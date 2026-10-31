package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tasks is a separate systemd job. It never runs inside the mailbox, so a long
// restore or an upgrade cannot prevent cancellation or receipt delivery.
func (e *Engine) Tasks(ctx context.Context) error {
	unlock, err := local.Lock(filepath.Join(management.Directory(e.Host.Root), "worker.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if err = e.connect(); err != nil {
		return err
	}
	_ = e.Observe(ctx)
	entries, err := os.ReadDir(management.Directory(e.Host.Root))
	if err != nil {
		return err
	}
	var queued []management.Task
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		t, er := management.Read(e.Host.Root, strings.TrimSuffix(ent.Name(), ".json"))
		if er != nil {
			return er
		}
		if t.State == "RUNNING" {
			t.State, t.Error, t.Updated = "INTERRUPTED", "执行进程已中断；检查当前阶段后显式重试，系统不会自动重放。", e.stamp()
			if er = local.JSON(management.Path(e.Host.Root, t.Request.ID), t); er != nil {
				return er
			}
		}
		if t.State == "QUEUED" {
			queued = append(queued, t)
		}
		_ = e.publishTask(ctx, t)
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].Request.Created < queued[j].Request.Created })
	if len(queued) == 0 {
		return nil
	}
	t := queued[0]
	if _, err = os.Stat(filepath.Join(management.Directory(e.Host.Root), t.Request.ID+".cancel")); err == nil {
		t.State, t.Stage, t.Updated = "CANCELLED", "cancelled-before-start", e.stamp()
		if err = local.JSON(management.Path(e.Host.Root, t.Request.ID), t); err != nil {
			return err
		}
		return e.publishTask(ctx, t)
	}
	if err = t.Request.Validate(); err != nil {
		return err
	}
	t.State, t.Stage, t.Started, t.Updated = "RUNNING", "checking-authority", e.stamp(), e.stamp()
	t.Attempt++
	if err = local.JSON(management.Path(e.Host.Root, t.Request.ID), t); err != nil {
		return err
	}
	jobctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	setStage := func(stage string) {
		mu.Lock()
		defer mu.Unlock()
		t.Stage, t.Updated = stage, e.stamp()
		_ = local.JSON(management.Path(e.Host.Root, t.Request.ID), t)
	}
	e.Progress = setStage
	defer func() { e.Progress = nil }()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-jobctx.Done():
				return
			case <-tick.C:
				if _, er := os.Stat(filepath.Join(management.Directory(e.Host.Root), t.Request.ID+".cancel")); er == nil {
					cancel()
					return
				}
			}
		}
	}()
	var result any
	if err = e.taskAuthority(jobctx, t.Request); err == nil {
		result, err = e.executeTask(jobctx, t.Request)
	}
	close(done)
	<-stopped
	mu.Lock()
	t.State = "SUCCEEDED"
	t.Error = ""
	t.Result = result
	t.Finished = e.stamp()
	t.Updated = t.Finished
	if err != nil {
		t.State = "FAILED"
		t.Error = err.Error()
		if jobctx.Err() != nil {
			t.State = "INTERRUPTED"
			if _, er := os.Stat(filepath.Join(management.Directory(e.Host.Root), t.Request.ID+".cancel")); er == nil {
				t.State = "CANCELLED"
			}
		}
	}
	mu.Unlock()
	if err = local.JSON(management.Path(e.Host.Root, t.Request.ID), t); err != nil {
		return err
	}
	return e.publishTask(ctx, t)
}
func (e *Engine) progress(stage string) {
	if e.Progress != nil {
		e.Progress(stage)
	}
}
func (e *Engine) publishTask(ctx context.Context, t management.Task) error {
	return management.Publish(ctx, e.Host.Root, t, e.Store, "cluster/"+e.Cluster+"/tasks/"+t.Request.Target+"/"+t.Request.ID+".json")
}
func (e *Engine) taskAuthority(ctx context.Context, r management.Request) error {
	s, err := e.status(ctx)
	if err != nil {
		return err
	}
	if s.Cluster != r.Cluster || s.Instance != r.Target || s.Control == nil {
		return errors.New("task instance identity or authority unavailable")
	}
	c := s.Control
	if c.Revision == r.Expected.Revision && c.Epoch == r.Expected.Epoch && c.Owner == r.Expected.Owner && c.Phase == r.Expected.Phase {
		return nil
	}
	// Only the same operation may reconcile its own committed transition.
	var op struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(c.Operation, &op)
	if (r.Action == "force-takeover" || r.Action == "storage-apply") && op.ID == r.ID {
		return nil
	}
	if r.Action == "force-takeover" && c.Completed != nil && c.Completed.ID == r.ID {
		return nil
	}
	return errors.New("authority changed since task was queued; inspect and submit a new task")
}
func (e *Engine) executeTask(ctx context.Context, r management.Request) (any, error) {
	p := r.Parameters
	switch r.Action {
	case "preflight":
		return e.Preflight(ctx, p.Snapshot, p.PointHash)
	case "refresh-config":
		return e.Refresh(ctx, p.Snapshot, p.PointHash)
	case "backup-now":
		e.progress("checkpoint-and-upload")
		return nil, e.Backup(ctx, p.Full)
	case "restore-drill":
		e.progress("isolated-restore")
		return e.Restore(ctx, p.Snapshot, p.PointHash)
	case "confirm-delivery":
		s, err := e.status(ctx)
		if err != nil || !s.Serving {
			return nil, errors.New("online instance required")
		}
		err = local.JSON(filepath.Join(e.Host.Root, "agent/delivery-confirmation.json"), map[string]any{"instance_id": s.Instance, "epoch": s.Epoch, "at": e.stamp()})
		if err != nil {
			return nil, err
		}
		return e.Health(ctx)
	case "health-check":
		return e.Health(ctx)
	case "upgrade-runtime":
		e.progress("download-runtime")
		dir := filepath.Join(e.Host.Root, "agent/downloads", r.ID)
		if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
			return nil, err
		}
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			if err = release.Fetch(ctx, p.Version, p.ManifestHash, dir); err != nil {
				return nil, err
			}
		}
		d, err := release.Describe(dir)
		if err != nil || d.Version != p.Version || d.SHA != p.ManifestHash {
			return nil, errors.New("downloaded release does not match approved manifest")
		}
		e.progress("install-runtime")
		out, err := e.Runner.Run(context.WithoutCancel(ctx), Command{Name: filepath.Join(dir, "matrix-admin"), Args: []string{"upgrade", "--root", e.Host.Root, "--from", dir, "--apply"}, Timeout: 90 * time.Minute})
		if err != nil {
			return nil, err
		}
		var result any
		err = json.Unmarshal(out, &result)
		return result, err
	case "force-takeover":
		e.progress("prepare-disaster-recovery")
		return e.Takeover(ctx, r)
	case "maintenance-recover":
		return e.RecoverMaintenance(ctx, r)
	case "storage-plan":
		return e.RetentionPlan(ctx, r)
	case "storage-apply":
		return e.RetentionApply(ctx, r)
	case "cleanup-plan":
		return e.CleanupPlan(ctx, r)
	case "cleanup-apply":
		return e.CleanupApply(ctx, r)
	}
	return nil, errors.New("unknown maintenance task")
}
