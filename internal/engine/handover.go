package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (e *Engine) requireFinalReader(ctx context.Context, target string) error {
	if !local.SafeID(target) {
		return errors.New("invalid target identity")
	}
	if err := e.connect(); err != nil {
		return err
	}
	b, _, err := e.Store.Get(ctx, "cluster/"+e.Cluster+"/agents/"+target+".json")
	if err != nil {
		return err
	}
	var r struct {
		Instance string `json:"instance_id"`
		Cluster  string `json:"cluster_id"`
		Schema   int    `json:"handover_receipt_schema"`
		At       string `json:"reported_at"`
	}
	if json.Unmarshal(b, &r) != nil || r.Instance != target || r.Cluster != e.Cluster || r.Schema != 2 {
		return errors.New("target must support final receipt schema 2 before handover")
	}
	at, err := time.Parse(time.RFC3339Nano, r.At)
	age := e.Now().Sub(at)
	if err != nil || age < -30*time.Second || age > 90*time.Second {
		return errors.New("target capability report is stale")
	}
	return nil
}
func (e *Engine) Final(ctx context.Context, op, id string, epoch uint64) (err error) {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return e.final(ctx, op, id, epoch, false)
}
func (e *Engine) final(ctx context.Context, op, id string, epoch uint64, planned bool) (err error) {
	if !local.SafeID(op) || strings.Contains(op, ".") || !local.SafeID(id) || epoch == 0 {
		return errors.New("explicit operation/instance/epoch required")
	}
	if err = e.connect(); err != nil {
		return err
	}
	root, _, err := e.active(ctx)
	if err != nil {
		return err
	}
	base := filepath.Join(e.Host.Root, "deploy/control/state/final", op)
	unlock, err := local.Lock(filepath.Join(base, "worker.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	step := "close-ingress"
	defer func() { e.recordError(base, op, step, err) }()
	timer := filepath.Join(base, "timer.json")
	if _, err = os.Stat(timer); errors.Is(err, os.ErrNotExist) {
		_, active := e.run(ctx, e.Host.Systemctl, "is-active", "--quiet", "matrix-backup.timer")
		if err = local.JSON(timer, map[string]bool{"timer_was_active": active == nil}); err != nil {
			return err
		}
	}
	begin := "final-begin"
	if planned {
		begin = "final-begin-planned"
	}
	if err = e.action(ctx, begin, op, id, epoch); err != nil {
		return err
	}
	step = "pause-backup"
	if err = e.system(ctx, "stop", "matrix-backup.timer"); err != nil {
		return err
	}
	if err = e.wait(ctx, 15*time.Minute, func() error {
		b, err := e.run(ctx, e.Host.Systemctl, "show", "matrix-backup.service", "--property=ActiveState", "--value")
		if err != nil {
			return err
		}
		switch strings.TrimSpace(string(b)) {
		case "active", "activating", "deactivating":
			return errors.New("daily producer still running")
		}
		return nil
	}); err != nil {
		return err
	}
	producer, err := local.Lock(filepath.Join(root, "backup-state/tuwunel-restic/operation.lock"))
	if err != nil {
		return err
	}
	defer producer()
	step = "stop-writers"
	if err = e.action(ctx, "final-stop", op, id, epoch); err != nil {
		return err
	}
	if err = e.wait(ctx, 75*time.Minute, func() error { return e.action(ctx, "final-frozen", op, id, epoch) }); err != nil {
		return err
	}
	if err = e.action(ctx, "final-snapshot", op, id, epoch); err != nil {
		return err
	}
	if err = e.action(ctx, "final-verify", op, id, epoch); err != nil {
		return err
	}
	s, err := e.status(ctx)
	if err != nil {
		return err
	}
	if s.Final == nil || s.Final.ID != op {
		return errors.New("final operation changed")
	}
	if s.Final.Phase != "READY" {
		step = "wait-backup-ready"
		receipt, backupErr := e.backupService().Final(ctx, root, op)
		if backupErr != nil {
			return backupErr
		}
		if receipt.Operation != op || receipt.Instance != id || receipt.Epoch != epoch || !receipt.BackupReady() {
			return errors.New("final receipt identity differs")
		}
		if err = e.action(ctx, "final-verify", op, id, epoch); err != nil {
			return err
		}
		if err = local.JSON(filepath.Join(base, "receipt.json"), receipt); err != nil {
			return err
		}
		step = "commit-final-receipt"
		if err = e.action(ctx, "final-complete", op, id, epoch); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) SourceResume(ctx context.Context, op, id string, epoch uint64) error {
	if !local.SafeID(op) || strings.Contains(op, ".") || !local.SafeID(id) || epoch == 0 {
		return errors.New("explicit operation/instance/epoch required")
	}
	// Stop progression before waiting on the operation lock. A rollback must
	// work while a long transfer is active, not require a hidden second action.
	s, err := e.status(ctx)
	if err != nil {
		return err
	}
	var remote control.FinalOperation
	if s.Control != nil {
		_ = json.Unmarshal(s.Control.Operation, &remote)
	}
	if s.Control == nil || s.Instance != id || s.Control.Owner != id || s.Control.Epoch != epoch || (s.Final != nil && s.Final.ID != op) || (s.Final == nil && s.Control.Phase != "ONLINE" && !(remote.ID == op && remote.Source == id && remote.Epoch == epoch && s.Control.Phase == "REQUESTED")) {
		return errors.New("source rollback requires the matching unreleased operation and ownership")
	}
	if s.Final != nil || remote.ID == op {
		e.progress("stop-handover-worker")
		if err = e.PauseHandover(ctx, id, true); err != nil {
			return err
		}
		if _, err = e.Runner.Run(ctx, Command{Name: e.Host.Systemctl, Args: []string{"stop", "matrix-handover.service"}, Timeout: 35 * time.Minute}); err != nil {
			return err
		}
	}
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	e.progress("close-handover-authority")
	if err = e.action(ctx, "final-resume", op, id, epoch); err != nil {
		return err
	}
	if err = e.action(ctx, "resume", "", id, epoch); err != nil {
		return err
	}
	e.progress("wait-source-health")
	if err = e.wait(ctx, 3*time.Minute, func() error {
		s, err := e.status(ctx)
		if err != nil {
			return err
		}
		if !s.Serving || s.Blocked || s.Instance != id || s.Epoch != epoch || s.Final != nil {
			return errors.New("source not healthy yet")
		}
		return nil
	}); err != nil {
		return err
	}
	var timer struct {
		Active bool `json:"timer_was_active"`
	}
	e.progress("resume-backup-schedule")
	if err = local.ReadJSON(filepath.Join(e.Host.Root, "deploy/control/state/final", op, "timer.json"), &timer); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if timer.Active {
		if err = e.system(ctx, "start", "matrix-backup.timer"); err != nil {
			return err
		}
	}
	return e.clearOperationPause(op)
}
func (e *Engine) Handover(ctx context.Context) (err error) {
	// Closing an aborted operation must still work when progression is paused.
	if s, er := e.status(ctx); er == nil && s.Control != nil && s.Control.Phase == "ONLINE" {
		if closed := s.Control.Aborted; closed != nil && closed.Source == s.Instance && closed.Epoch == s.Epoch && s.Control.Owner == s.Instance && s.Final == nil && s.Serving {
			if er = e.clearOperationPause(closed.ID); er != nil {
				return er
			}
		}
		if h := s.Handover; h != nil && ((h.Phase == "CANCELLED" && !h.MayWrite) || (h.Phase == "RETIRED" && s.Control.Epoch > h.NextEpoch)) {
			if er = e.clearOperationPause(h.ID); er != nil {
				return er
			}
		}
		if f := s.Final; f != nil && f.Phase == "RETIRED" && s.Control.Epoch > f.Epoch {
			if er = e.clearOperationPause(f.ID); er != nil {
				return er
			}
		}
	}
	if _, er := os.Stat(filepath.Join(e.Host.Root, "agent/handover-paused.json")); er == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, er := os.Stat(filepath.Join(e.Host.Root, "agent/handover-paused.json")); er == nil {
					cancel()
					return
				}
			}
		}
	}()
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	worker, err := local.Lock(filepath.Join(e.Host.Root, "deploy/control/state/handover-worker.lock"))
	if err != nil {
		return err
	}
	defer worker()
	s, err := e.status(ctx)
	if err != nil {
		return err
	}
	if s.Control == nil {
		return errors.New("authority unavailable")
	}
	if closed := s.Control.Aborted; closed != nil && s.Control.Phase == "ONLINE" && string(s.Control.Operation) == "null" && closed.Epoch == s.Control.Epoch && closed.Source == s.Control.Owner {
		return nil // Do not replay the older successful operation after rollback.
	}
	var op control.FinalOperation
	if string(s.Control.Operation) != "null" {
		if err = json.Unmarshal(s.Control.Operation, &op); err != nil {
			return err
		}
	} else if s.Control.Completed != nil {
		op = *s.Control.Completed
	}
	if op.ID == "" || (op.Mode != "planned" && op.Mode != "forced") {
		return nil
	}
	if !local.SafeID(op.ID) {
		return errors.New("invalid handover ID")
	}
	base := filepath.Join(e.Host.Root, "deploy/control/state/handover", op.ID)
	if err = os.MkdirAll(base, 0700); err != nil {
		return err
	}
	step := "inspect"
	previousProgress := e.Progress
	e.Progress = func(stage string) {
		step = stage
		_ = local.JSON(filepath.Join(base, "progress.json"), map[string]any{"operation_id": op.ID, "step": stage, "at": e.stamp(), "state": "RUNNING"})
		if previousProgress != nil {
			previousProgress(stage)
		}
	}
	e.progress(step)
	defer func() {
		e.Progress = previousProgress
		e.recordError(base, op.ID, step, err)
		state := "SUCCEEDED"
		if err != nil {
			state = "FAILED"
		}
		if errors.Is(err, context.Canceled) {
			state = "INTERRUPTED"
		}
		_ = local.JSON(filepath.Join(base, "progress.json"), map[string]any{"operation_id": op.ID, "step": step, "at": e.stamp(), "state": state})
		if err != nil {
			pauseErr := local.JSON(filepath.Join(e.Host.Root, "agent/handover-paused.json"), map[string]any{"at": e.stamp(), "operation_id": op.ID, "reason": "handover failed; inspect error and explicitly continue", "step": step})
			if pauseErr != nil {
				err = errors.Join(err, pauseErr)
			}
		}
	}()
	ctl := func(action string) error { return e.action(ctx, action, op.ID, s.Instance, op.Epoch) }
	if op.Mode == "planned" && op.Source == s.Instance {
		switch s.Control.Phase {
		case "REQUESTED", "QUIESCING", "QUIESCED", "SNAPSHOTTING":
			step = "check-target-reader"
			if err = e.requireFinalReader(ctx, op.Target); err != nil {
				return err
			}
			step = "final-backup"
			if err = e.final(ctx, op.ID, s.Instance, op.Epoch, true); err != nil {
				return err
			}
			step = "release"
			return ctl("handover-release")
		case "RELEASED", "RESTORING", "VALIDATING", "STARTING", "ONLINE":
			return ctl("handover-release")
		}
		return nil
	}
	if op.Target != s.Instance {
		return nil
	}
	switch s.Control.Phase {
	case "RELEASED":
		step = "check-reader"
		if op.Receipt == nil {
			return errors.New("released recovery set missing")
		}
		if _, _, err = e.readPoint(ctx, op.Receipt.Snapshot, op.Receipt.PointHash); err != nil {
			return err
		}
		return ctl("handover-claim")
	case "RESTORING", "VALIDATING":
		if s.Handover != nil && s.Handover.MayWrite && s.Handover.Phase == "STARTING" && s.Control.Phase == "VALIDATING" {
			step = "reconcile-start-intent"
			return ctl("handover-start")
		}
		if s.Handover == nil || s.Handover.MayWrite {
			return errors.New("generation may have written; restore forbidden")
		}
		step = "restore-and-validate"
		if err = ctl("handover-validating"); err != nil {
			return err
		}
		s, err = e.status(ctx)
		if err != nil {
			return err
		}
		if s.Handover == nil {
			return errors.New("local handover missing")
		}
		receipt := s.Handover.Receipt
		if receipt == nil {
			return errors.New("recovery set missing")
		}
		var restored Restored
		record := filepath.Join(base, "recovery.json")
		if _, err = os.Stat(record); errors.Is(err, os.ErrNotExist) {
			restored, err = e.backupService().Restore(ctx, receipt.Snapshot, receipt.PointHash, filepath.Join(e.Host.Root, "generations", "handover-"+op.ID))
			if err != nil {
				return err
			}
			if err = local.JSON(record, restored); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if err = local.ReadJSON(record, &restored); err != nil {
			return err
		}
		if restored.Snapshot != receipt.Snapshot || restored.Hash != receipt.PointHash || !restored.Ready || restored.Production {
			return errors.New("restore receipt differs")
		}
		raw, err := os.ReadFile(filepath.Join(restored.Work, "point.json"))
		if err != nil {
			return err
		}
		if local.Digest(raw) != receipt.PointHash {
			return errors.New("restored point changed")
		}
		if err = local.Atomic(filepath.Join(base, "point.json"), raw, 0600); err != nil {
			return err
		}
		var point Point
		if err = local.Decode(raw, &point); err != nil {
			return err
		}
		step = "bind-generation"
		var cfg control.Config
		if err = local.ReadJSON(e.controlConfig(), &cfg); err != nil {
			return err
		}
		var journal control.Journal
		if err = local.ReadJSON(filepath.Join(cfg.StateDir, "journal.json"), &journal); err != nil {
			return err
		}
		services := journal.Services
		if len(services) == 0 {
			services = cfg.Services
		}
		for _, service := range services {
			b, err := e.docker(ctx, "container", "ls", "-a", "--format", "{{.Names}}", "--filter", "name=^/"+service.Name+"$")
			if err != nil {
				return err
			}
			if len(strings.TrimSpace(string(b))) == 0 {
				continue
			}
			c, err := e.inspect(ctx, service.Name)
			if err != nil {
				return err
			}
			if c.State.Running || c.State.Restarting || c.Image != service.Image || c.mount(service.MountPath) != service.DataPath {
				return errors.New("old container is not safely stopped")
			}
			if _, err = e.docker(ctx, "rm", service.Name); err != nil {
				return err
			}
		}
		if err = local.JSON(filepath.Join(base, "deployment.json"), map[string]any{"schema": 1, "deployment_root": restored.Root, "operation_id": op.ID, "instance_id": s.Instance, "epoch": op.NextEpoch}); err != nil {
			return err
		}
		if err = e.registerDeployment(s); err != nil {
			return err
		}
		if err = e.bindGeneration(restored, s.Instance, op.NextEpoch); err != nil {
			return err
		}
		if err = ctl("handover-bind"); err != nil {
			return err
		}
		step = "prepare-containers"
		if err = e.activateFiles(ctx, restored, point); err != nil {
			return err
		}
		step = "start-intent"
		return ctl("handover-start")
	case "STARTING":
		step = "health-and-online"
		if err = e.wait(ctx, 3*time.Minute, func() error { return ctl("handover-online") }); err != nil {
			return err
		}
	case "ONLINE":
		if s.Handover != nil && s.Handover.Phase == "COMMITTING" {
			if err = ctl("handover-online"); err != nil {
				return err
			}
		}
	}
	s, err = e.status(ctx)
	if err != nil {
		return err
	}
	if s.Serving && s.Handover != nil && s.Handover.ID == op.ID && s.Handover.Phase == "ONLINE" {
		step = "enable-backup"
		return e.system(ctx, "enable", "--now", "matrix-backup.timer")
	}
	return nil
}
