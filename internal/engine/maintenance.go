package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type PreflightReport struct {
	Schema        int    `json:"schema"`
	Instance      string `json:"instance_id"`
	Cluster       string `json:"cluster_id"`
	Epoch         uint64 `json:"epoch"`
	Snapshot      string `json:"snapshot_id"`
	Hash          string `json:"point_sha256"`
	Configuration string `json:"configuration_sha256"`
	At            string `json:"checked_at"`
	Required      uint64 `json:"required_bytes"`
	Ready         bool   `json:"ready"`
}

func (e *Engine) Preflight(ctx context.Context, snapshot, hash string) (PreflightReport, error) {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return PreflightReport{}, err
	}
	defer unlock()
	return e.preflight(ctx, snapshot, hash)
}
func (e *Engine) preflight(ctx context.Context, snapshot, hash string) (out PreflightReport, err error) {
	// A failed recheck must invalidate the previous successful report immediately.
	if err = local.JSON(filepath.Join(e.Host.Root, "deploy/control/state/preflight.json"), PreflightReport{Schema: 1, Cluster: e.Cluster, At: e.stamp()}); err != nil {
		return out, err
	}
	e.progress("check-dependencies")
	if err = e.prerequisites(ctx); err != nil {
		return out, err
	}
	s, err := e.status(ctx)
	if err != nil || s.Control == nil {
		return out, errors.New("fresh control state required")
	}
	if s.Control.Phase == "MAINTENANCE" {
		return out, errors.New("storage maintenance must finish before preflight")
	}
	if _, err = release.Current(e.ReleaseDir); err != nil {
		return out, err
	}
	e.progress("verify-recovery-catalog")
	p, raw, err := e.readPoint(ctx, snapshot, hash)
	if err != nil {
		return out, err
	}
	work, err := os.MkdirTemp(filepath.Join(e.Host.Root, "agent"), "preflight-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(work)
	bundle, _, err := e.configuration(ctx, p, work)
	if err != nil {
		return out, err
	}
	var source control.Config
	if err = local.ReadJSON(filepath.Join(bundle, "payload/control/source-local.json"), &source); err != nil {
		return out, err
	}
	if source.Cluster != e.Cluster || len(source.Services) != 2 || source.GateAddress != "127.0.0.1:29318" {
		return out, errors.New("unsupported source topology")
	}
	e.progress("check-backup-storage")
	repo, err := e.repository(ctx, p)
	if err != nil {
		return out, err
	}
	if err = e.verifyRepository(ctx, repo); err != nil {
		return out, err
	}
	need, err := e.snapshotSpace(ctx, repo, p.Snapshot)
	if err != nil {
		return out, err
	}
	e.progress("pull-pinned-images")
	for _, name := range []string{"tuwunel", "telegram"} {
		if err = e.checkImage(ctx, p.Images[name], true); err != nil {
			return out, err
		}
	}
	// Target nginx is refreshed while admission remains closed. Final recovery
	// may contain newer configuration and is independently validated at bind.
	if s.Control.Owner != s.Instance || s.Blocked {
		if err = e.refreshNginx(ctx, bundle); err != nil {
			return out, err
		}
	}
	out = PreflightReport{1, s.Instance, e.Cluster, s.Control.Epoch, p.Snapshot, local.Digest(raw), p.ConfigurationHash, e.stamp(), need, true}
	err = local.JSON(filepath.Join(e.Host.Root, "deploy/control/state/preflight.json"), out)
	return out, err
}
func (e *Engine) Refresh(ctx context.Context, snapshot, hash string) (any, error) {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, _, err := e.readPoint(ctx, snapshot, hash)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Join(e.Host.Root, "agent"), "refresh-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	bundle, _, err := e.configuration(ctx, p, dir)
	if err != nil {
		return nil, err
	}
	if err = e.refreshNginx(ctx, bundle); err != nil {
		return nil, err
	}
	return map[string]any{"snapshot_id": p.Snapshot, "configuration_sha256": p.ConfigurationHash, "refreshed_at": e.stamp()}, nil
}
func (e *Engine) refreshNginx(ctx context.Context, bundle string) error {
	s, err := e.status(ctx)
	if err != nil || s.Serving || (!s.Blocked && s.Control != nil && s.Control.Owner == s.Instance) {
		return errors.New("nginx refresh requires closed admission")
	}
	var source control.Config
	if err = local.ReadJSON(filepath.Join(bundle, "payload/control/source-local.json"), &source); err != nil {
		return err
	}
	names := []string{"matrix.conf", "00-matrix-common.conf"}
	old := map[string][]byte{}
	next := map[string][]byte{}
	for _, name := range names {
		b, er := os.ReadFile(filepath.Join(bundle, "payload/nginx", name))
		if er != nil {
			return er
		}
		next[name] = []byte(strings.ReplaceAll(string(b), filepath.Dir(source.S3Config), filepath.Join(e.Host.Root, "deploy/control")))
		old[name], er = os.ReadFile(filepath.Join(e.Host.NginxDirectory, name))
		if er != nil {
			return er
		}
	}
	rollback := func() {
		for _, name := range names {
			_ = local.Atomic(filepath.Join(e.Host.NginxDirectory, name), old[name], 0644)
		}
		recovery, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = e.system(recovery, "reload", e.Host.NginxService)
	}
	for _, name := range names {
		if err = local.Atomic(filepath.Join(e.Host.NginxDirectory, name), next[name], 0644); err != nil {
			rollback()
			return err
		}
	}
	if _, err = e.run(ctx, e.Host.Nginx, "-c", e.Host.NginxConfig, "-t"); err != nil {
		rollback()
		return err
	}
	if err = e.system(ctx, "reload", e.Host.NginxService); err != nil {
		rollback()
		return err
	}
	for _, name := range names {
		if err = local.Atomic(filepath.Join(e.Host.Root, "deploy/nginx", name), next[name], 0600); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) Takeover(ctx context.Context, r management.Request) (any, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	s, err := e.status(ctx)
	if err != nil || s.Control == nil {
		return nil, errors.New("authority unavailable")
	}
	if s.Control.Completed != nil && s.Control.Completed.ID == r.ID && s.Control.Owner == s.Instance {
		return map[string]any{"online": s.Serving, "epoch": s.Control.Epoch}, nil
	}
	var op control.FinalOperation
	_ = json.Unmarshal(s.Control.Operation, &op)
	if op.ID != r.ID {
		if _, err = e.Preflight(ctx, r.Parameters.Snapshot, r.Parameters.PointHash); err != nil {
			return nil, err
		}
		p, raw, er := e.readPoint(ctx, r.Parameters.Snapshot, r.Parameters.PointHash)
		if er != nil {
			return nil, er
		}
		if p.Binding.Epoch > r.Expected.Epoch {
			return nil, errors.New("recovery point is from a future epoch")
		}
		base := filepath.Join(e.Host.Root, "deploy/control/state/handover", r.ID)
		if err = local.Atomic(filepath.Join(base, "point.json"), raw, 0600); err != nil {
			return nil, err
		}
		if err = local.JSON(filepath.Join(base, "force-request.json"), r); err != nil {
			return nil, err
		}
		e.progress("claim-new-epoch")
		if err = e.action(ctx, "handover-force", r.ID, r.Target, r.Expected.Epoch); err != nil {
			return nil, err
		}
	}
	if err = os.Remove(filepath.Join(e.Host.Root, "agent/handover-paused.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	defer func() {
		if ctx.Err() != nil {
			_ = local.JSON(filepath.Join(e.Host.Root, "agent/handover-paused.json"), map[string]any{"at": e.stamp(), "reason": "takeover task interrupted"})
		}
	}()
	for {
		if _, paused := os.Stat(filepath.Join(e.Host.Root, "agent/handover-paused.json")); paused == nil {
			return nil, errors.New("takeover paused; inspect the current stage and explicitly retry to continue")
		}
		s, err = e.status(ctx)
		if err != nil {
			return nil, err
		}
		if s.Control == nil || s.Control.Owner != r.Target || s.Control.Epoch != r.Expected.Epoch+1 {
			return nil, errors.New("takeover superseded; no further side effects")
		}
		if s.Serving && s.Control.Phase == "ONLINE" {
			return map[string]any{"online": true, "epoch": s.Control.Epoch, "snapshot_id": r.Parameters.Snapshot, "data_loss_acknowledged": true}, nil
		}
		e.progress("takeover-" + strings.ToLower(s.Control.Phase))
		if err = e.Handover(ctx); err != nil && !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
func (e *Engine) PauseHandover(ctx context.Context, id string, pause bool) error {
	s, err := e.status(ctx)
	if err != nil || s.Instance != id {
		return errors.New("instance identity mismatch")
	}
	path := filepath.Join(e.Host.Root, "agent/handover-paused.json")
	if pause {
		return local.JSON(path, map[string]any{"at": e.stamp(), "reason": "operator paused progression"})
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (e *Engine) Health(ctx context.Context) (any, error) {
	s, err := e.status(ctx)
	if err != nil {
		return nil, err
	}
	report := map[string]any{"checked_at": e.stamp(), "instance_id": s.Instance, "epoch": s.Epoch, "admission_open": s.Serving, "telegram_delivery": "not_verified", "checks": map[string]any{}}
	checks := report["checks"].(map[string]any)
	for _, name := range []string{"tuwunel", "telegram-bridge"} {
		names, er := e.docker(ctx, "container", "ls", "-a", "--format", "{{.Names}}", "--filter", "name=^/"+name+"$")
		if er == nil && strings.TrimSpace(string(names)) == "" {
			checks[name] = map[string]any{"container_running": false, "absent": true}
			continue
		}
		c, er := e.inspect(ctx, name)
		checks[name] = map[string]any{"container_running": er == nil && c.State.Running, "error": errorText(er)}
	}
	// Container/HTTP health cannot establish Telegram delivery. Only an explicit
	// two-way observation is shown, bound to the current generation and epoch.
	var confirmation struct {
		Instance string `json:"instance_id"`
		Epoch    uint64 `json:"epoch"`
		At       string `json:"at"`
	}
	if local.ReadJSON(filepath.Join(e.Host.Root, "agent/delivery-confirmation.json"), &confirmation) == nil && confirmation.Instance == s.Instance && confirmation.Epoch == s.Epoch {
		at, er := time.Parse(time.RFC3339Nano, confirmation.At)
		if er == nil && e.Now().Sub(at) >= 0 && e.Now().Sub(at) < 15*time.Minute {
			report["telegram_delivery"] = "operator_verified_both_directions"
			report["delivery_verified_at"] = confirmation.At
		}
	}
	err = local.JSON(filepath.Join(e.Host.Root, "agent/health.json"), report)
	return report, err
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
func (e *Engine) Observe(ctx context.Context) error { _, err := e.Health(ctx); return err }
