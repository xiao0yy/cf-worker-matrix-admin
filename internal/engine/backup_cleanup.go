package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
)

const backupRetryRetention = 24 * time.Hour

// This journal owns only scratch data, never a deployment or remote snapshot.
// It is durable BEFORE staging begins and survives both deletion and SIGKILL.
type backupWorkspace struct {
	Schema     int     `json:"schema"`
	Binding    Binding `json:"binding"`
	Operation  string  `json:"operation_id,omitempty"`
	State      string  `json:"state"`
	Updated    string  `json:"updated_at"`
	Expires    string  `json:"retry_until,omitempty"`
	Reason     string  `json:"reason,omitempty"`
	Error      string  `json:"error,omitempty"`
	Checkpoint string  `json:"checkpoint_cleanup,omitempty"`
}

func backupBase(root, op string) string {
	p := filepath.Join(root, "backup-state/tuwunel-restic")
	if op != "" {
		p = filepath.Join(p, "finals", op)
	}
	return p
}

func (e *Engine) beginBackupWorkspace(root, op string, s Status) (backupWorkspace, error) {
	base := backupBase(root, op)
	w := backupWorkspace{Schema: 1, Binding: Binding{1, s.Cluster, s.Instance, s.Epoch, "controller-admission"}, Operation: op}
	var old backupWorkspace
	err := local.ReadJSON(filepath.Join(base, "workspace.json"), &old)
	if err == nil {
		compatible := old.Binding == w.Binding
		if old.State == "CLEANED" && old.Binding.Epoch <= w.Binding.Epoch {
			previous := old.Binding
			previous.Epoch = w.Binding.Epoch
			compatible = previous == w.Binding
		}
		if old.Schema != 1 || !compatible || old.Operation != op {
			return w, errors.New("backup scratch identity differs; wait for automatic cleanup or inspect its report")
		}
		if old.State == "CLEANUP_PENDING" {
			return w, errors.New("backup scratch cleanup is pending; inspect automatic cleanup report")
		}
		if old.State != "CLEANED" {
			w = old
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return w, err
	} else {
		// Adopt an older staged backup only with its complete, matching binding.
		// Unknown pre-checkpoint directories must never become deletion candidates.
		for _, name := range []string{"preparing", "pending", "interrupted-checkpoint"} {
			path := filepath.Join(base, name)
			if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return w, err
			}
			var meta Metadata
			if local.ReadJSON(filepath.Join(path, "bundle/metadata.json"), &meta) != nil || meta.Binding != w.Binding || meta.Operation != op {
				return w, errors.New("untracked backup scratch requires inspection before adoption")
			}
		}
	}
	w.State, w.Updated, w.Error, w.Reason = "RUNNING", e.stamp(), "", ""
	return w, local.JSON(filepath.Join(base, "workspace.json"), w)
}

func (e *Engine) finishBackupWorkspace(root string, w backupWorkspace, result error) {
	w.Updated = e.stamp()
	w.State = "RETRY_PENDING"
	if w.Expires == "" {
		w.Expires = e.Now().Add(backupRetryRetention).UTC().Format(time.RFC3339Nano)
	}
	if result == nil {
		w.State, w.Reason = "CLEANUP_PENDING", "备份已发布"
	}
	base := backupBase(root, w.Operation)
	if local.JSON(filepath.Join(base, "workspace.json"), w) != nil {
		return
	}
	if result == nil {
		// Publication has succeeded. A local unlink failure is cleanup debt, not
		// a failed backup and must not roll back or republish the recovery point.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		e.cleanBackupWorkspace(ctx, root, base, &w)
	}
}

// Only explicit evidence can retire final scratch. A failed/paused handover
// remains resumable; its final snapshot must not silently change after a TTL.
func (e *Engine) workspaceDisposition(w backupWorkspace, s Status) (bool, string, error) {
	if w.Schema != 1 || w.Binding.Schema != 1 || w.Binding.Activation != "controller-admission" || w.Binding.Cluster != e.Cluster || w.Binding.Instance != s.Instance || w.Binding.Epoch == 0 || w.Binding.Epoch > s.Control.Epoch {
		return false, "", errors.New("backup scratch identity or epoch differs")
	}
	switch w.State {
	case "RUNNING", "RETRY_PENDING", "CLEANUP_PENDING", "CLEANED":
	default:
		return false, "", errors.New("unknown backup scratch state")
	}
	if w.State == "CLEANED" {
		return false, "", nil
	}
	if w.State == "CLEANUP_PENDING" {
		return true, w.Reason, nil
	}
	var op control.FinalOperation
	if len(s.Control.Operation) > 0 && string(s.Control.Operation) != "null" {
		if err := json.Unmarshal(s.Control.Operation, &op); err != nil {
			return false, "", err
		}
	}
	// An externally fenced/forced takeover can leave an old local final
	// journal behind. Its epoch is irrevocably obsolete even if still paused.
	if s.Control.Epoch > w.Binding.Epoch {
		return true, "原归属已失效", nil
	}
	if w.Operation != "" {
		if op.ID == w.Operation || (s.Final != nil && s.Final.ID == w.Operation && s.Final.Phase != "RETIRED") {
			return false, "交接尚未结束；继续交接或撤销后自动回收", nil
		}
		var closed struct {
			Operation control.FinalOperation `json:"operation"`
			At        string                 `json:"closed_at"`
			Valid     *bool                  `json:"recovery_set_valid_for_handover"`
		}
		err := local.ReadJSON(filepath.Join(e.Host.Root, "deploy/control/state/final", w.Operation, "closed.json"), &closed)
		if err == nil && closed.Operation.ID == w.Operation && closed.Operation.Source == w.Binding.Instance && closed.Operation.Epoch == w.Binding.Epoch && closed.Valid != nil && !*closed.Valid {
			if _, err = time.Parse(time.RFC3339Nano, closed.At); err != nil {
				return false, "", err
			}
			return true, "交接已撤销", nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, "", err
		}
		return false, "尚无交接结束凭据，保留临时数据", nil
	}
	if w.Expires == "" {
		return false, "等待重试", nil
	}
	until, err := time.Parse(time.RFC3339Nano, w.Expires)
	if err != nil {
		return false, "", err
	}
	if !e.Now().Before(until) {
		return true, "失败重试保留期已到", nil
	}
	return false, "保留供重试，到期自动回收", nil
}

func (e *Engine) cleanBackupWorkspace(ctx context.Context, root, base string, w *backupWorkspace) {
	err := e.removeBackupScratch(ctx, root, base, w)
	w.Updated, w.Error = e.stamp(), ""
	if err == nil {
		w.State = "CLEANED"
	} else {
		w.State, w.Error = "CLEANUP_PENDING", err.Error()
	}
	_ = local.JSON(filepath.Join(base, "workspace.json"), w)
}

func (e *Engine) removeBackupScratch(ctx context.Context, root, base string, w *backupWorkspace) error {
	paths := []string{filepath.Join(base, "preparing"), filepath.Join(base, "pending"), filepath.Join(base, "interrupted-checkpoint")}
	// A checkpoint can outlive the backup process that sent USR2. Do not race
	// Tuwunel's asynchronous producer or lose its request/identity evidence.
	if w.Checkpoint != "" && w.Checkpoint != "staged" && w.Checkpoint != "remove" {
		return errors.New("invalid checkpoint cleanup disposition")
	}
	if w.Operation == "" && w.Checkpoint == "" {
		var req checkpointRequest
		err := local.ReadJSON(filepath.Join(base, "preparing/request.json"), &req)
		if err == nil {
			if _, er := time.Parse(time.RFC3339Nano, req.At); er != nil || req.Container.ID == "" {
				return errors.New("invalid checkpoint producer record")
			}
			if _, er := os.Lstat(filepath.Join(base, "preparing/bundle/payload/database")); errors.Is(er, os.ErrNotExist) {
				// Inspect the actual producer, not a replacement using the same
				// container name: the old container might have been renamed.
				c, er := e.inspect(ctx, req.Container.ID)
				if er != nil {
					return fmt.Errorf("无法确认检查点生产者已结束：%w", er)
				}
				if sameContainer(req.Container, c) && c.State.Running {
					if er = e.checkpointComplete(ctx, req); er != nil {
						return fmt.Errorf("检查点仍可能生成，保留请求记录并稍后重试：%w", er)
					}
				}
				w.Checkpoint = "remove"
			} else if er != nil {
				return er
			} else {
				w.Checkpoint = "staged"
			}
			// Commit the producer decision before unlinking any of its evidence.
			// A restart during deletion must not depend on old Docker log lines.
			if err = local.JSON(filepath.Join(base, "workspace.json"), w); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if w.Operation == "" && w.Checkpoint == "remove" {
		// Delete the checkpoint before deleting its original request record.
		paths = append([]string{filepath.Join(root, "deploy/tuwunel/data/checkpoint-restic")}, paths...)
	}
	if err := scratchUnused(ctx, paths); err != nil {
		return err
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := local.SafePath(path); err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

// Check process/mount references, not database contents. This also catches a
// restic child left alive by a kill of a manually launched parent process.
func scratchUnused(ctx context.Context, paths []string) error {
	inside := func(path string) bool {
		for _, root := range paths {
			if path == root || strings.HasPrefix(path, root+"/") {
				return true
			}
		}
		return false
	}
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && inside(unescape.Replace(f[4])) {
			return errors.New("临时目录含挂载点，拒绝自动清理")
		}
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, p := range procs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !p.IsDir() || p.Name()[0] < '0' || p.Name()[0] > '9' {
			continue
		}
		// Scratch parents are private (0700). Other unprivileged users cannot
		// open them; procfs may legitimately hide their descriptors from us.
		info, er := p.Info()
		if errors.Is(er, os.ErrNotExist) {
			continue
		}
		if er != nil {
			return er
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
			continue
		}
		base := filepath.Join("/proc", p.Name())
		refs := []string{filepath.Join(base, "cwd"), filepath.Join(base, "root")}
		fds, er := os.ReadDir(filepath.Join(base, "fd"))
		if er != nil && !errors.Is(er, os.ErrNotExist) {
			return fmt.Errorf("无法确认进程 %s 是否仍使用临时数据：%w", p.Name(), er)
		}
		for _, fd := range fds {
			refs = append(refs, filepath.Join(base, "fd", fd.Name()))
		}
		for _, ref := range refs {
			target, er := os.Readlink(ref)
			if er == nil && inside(strings.TrimSuffix(target, " (deleted)")) {
				return fmt.Errorf("进程 %s 仍使用临时数据，稍后自动重试", p.Name())
			}
			if er != nil && !errors.Is(er, os.ErrNotExist) && !errors.Is(er, syscall.EINVAL) && !errors.Is(er, syscall.ESRCH) {
				return er
			}
		}
	}
	return nil
}

type backupCleanupReport struct {
	Schema  int               `json:"schema"`
	At      string            `json:"checked_at"`
	State   string            `json:"state"`
	Error   string            `json:"error,omitempty"`
	Items   []backupWorkspace `json:"items"`
	Omitted int               `json:"omitted,omitempty"`
}

// Called by the existing native task timer, independent of backup success or
// whether a handover is paused. No new shell, dependency or storage scan.
func (e *Engine) SweepBackupScratch(ctx context.Context) (err error) {
	report := backupCleanupReport{Schema: 1, At: e.stamp(), State: "OK", Items: []backupWorkspace{}}
	defer func() {
		if err != nil {
			report.State, report.Error = "BLOCKED", err.Error()
		}
		// Observations remain bounded. Pending/errors precede completed records;
		// full ownership journals remain local for every operation.
		sort.SliceStable(report.Items, func(i, j int) bool {
			a, b := report.Items[i], report.Items[j]
			if (a.State == "CLEANED") != (b.State == "CLEANED") {
				return b.State == "CLEANED"
			}
			return a.Updated > b.Updated
		})
		if len(report.Items) > 40 {
			report.Omitted = len(report.Items) - 40
			report.Items = report.Items[:40]
		}
		_ = local.JSON(filepath.Join(e.Host.Root, "agent/backup-cleanup.json"), report)
	}()
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if errors.Is(err, syscall.EWOULDBLOCK) {
		report.State = "BUSY"
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock()
	s, err := e.status(ctx)
	if err != nil {
		return err
	}
	if s.Control == nil || s.Cluster != e.Cluster || !local.SafeID(s.Instance) {
		return errors.New("controller identity unavailable for scratch cleanup")
	}
	roots := map[string]bool{e.Host.Root: true}
	entries, err := os.ReadDir(filepath.Join(e.Host.Root, "agent/generations"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		var g generation
		if err = local.ReadJSON(filepath.Join(e.Host.Root, "agent/generations", ent.Name()), &g); err != nil {
			return err
		}
		if err = e.validateGeneration(g); err != nil {
			return err
		}
		if g.Instance != s.Instance {
			return errors.New("generation belongs to another instance")
		}
		roots[g.Root] = true
	}
	if s.Deployment != "" && !roots[s.Deployment] {
		return errors.New("active deployment missing from generation registry")
	}
	ordered := make([]string, 0, len(roots))
	for root := range roots {
		ordered = append(ordered, root)
	}
	sort.Strings(ordered)
	for _, root := range ordered {
		base := backupBase(root, "")
		if _, err = os.Lstat(base); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err = local.SafePath(base); err != nil {
			return err
		}
		lock, er := local.Lock(filepath.Join(base, "operation.lock"))
		if errors.Is(er, syscall.EWOULDBLOCK) {
			report.State = "BUSY"
			continue
		}
		if er != nil {
			return er
		}
		er = e.sweepBackupRoot(ctx, root, s, &report)
		lock()
		if er != nil {
			return er
		}
	}
	return nil
}

func (e *Engine) sweepBackupRoot(ctx context.Context, root string, s Status, report *backupCleanupReport) error {
	bases := []string{backupBase(root, "")}
	entries, err := os.ReadDir(filepath.Join(bases[0], "finals"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() && local.SafeID(ent.Name()) {
			bases = append(bases, backupBase(root, ent.Name()))
		}
	}
	for _, base := range bases {
		if err = local.SafePath(base); err != nil {
			return err
		}
		var w backupWorkspace
		if err = local.ReadJSON(filepath.Join(base, "workspace.json"), &w); errors.Is(err, os.ErrNotExist) {
			for _, name := range []string{"preparing", "pending", "interrupted-checkpoint"} {
				if _, er := os.Lstat(filepath.Join(base, name)); er == nil {
					report.State = "BLOCKED"
					report.Items = append(report.Items, backupWorkspace{State: "UNTRACKED", Reason: "旧版临时目录缺少归属记录", Error: "请由 Agent 核实目录归属后清理；自动回收不会猜测删除"})
					break
				} else if !errors.Is(er, os.ErrNotExist) {
					return er
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		if (w.Operation != "" && !local.SafeID(w.Operation)) || backupBase(root, w.Operation) != base {
			return errors.New("backup scratch journal path differs")
		}
		// Lock is available: RUNNING means the producer was killed. Start a
		// bounded recovery window once; repeated sweeps never extend it.
		if w.State == "RUNNING" {
			w.State = "RETRY_PENDING"
			if w.Expires == "" {
				w.Expires = e.Now().Add(backupRetryRetention).UTC().Format(time.RFC3339Nano)
			}
			w.Updated = e.stamp()
		}
		remove, reason, er := e.workspaceDisposition(w, s)
		if er != nil {
			return er
		}
		if w.State != "CLEANED" {
			w.Reason = reason
			if remove {
				w.State = "CLEANUP_PENDING"
			}
			if er = local.JSON(filepath.Join(base, "workspace.json"), w); er != nil {
				return er
			}
			if remove {
				e.cleanBackupWorkspace(ctx, root, base, &w)
			}
		}
		if w.Error != "" {
			report.State = "BLOCKED"
		}
		report.Items = append(report.Items, w)
	}
	return nil
}
