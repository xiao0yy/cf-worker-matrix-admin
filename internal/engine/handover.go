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

func (e *Engine) recordError(path, op, step string, err error) {
	if err == nil {
		_ = os.Remove(filepath.Join(path, "error.json"))
		return
	}
	_ = local.JSON(filepath.Join(path, "error.json"), map[string]any{"operation_id": op, "step": step, "at": e.stamp(), "error": err.Error()})
}
func (e *Engine) Final(ctx context.Context, op, id string, epoch uint64) (err error) {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return e.final(ctx, op, id, epoch)
}
func (e *Engine) final(ctx context.Context, op, id string, epoch uint64) (err error) {
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
	if err = e.action(ctx, "final-begin", op, id, epoch); err != nil {
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
		step = "backup-and-verify"
		state := filepath.Join(root, "backup-state/tuwunel-restic")
		checkpoint := filepath.Join(root, "deploy/tuwunel/data/checkpoint-restic")
		if _, err = os.Stat(checkpoint); err == nil {
			if _, err = os.Stat(filepath.Join(state, "preparing/request.json")); err != nil {
				return errors.New("unclassified interrupted checkpoint; source retained")
			}
			dst := filepath.Join(state, "finals", op, "interrupted-checkpoint")
			if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
				return err
			}
			if err = os.Rename(checkpoint, dst); err != nil {
				return err
			}
		}
		published := filepath.Join(state, "finals", op, "published.json")
		if _, err = os.Stat(published); errors.Is(err, os.ErrNotExist) {
			if err = e.backup(ctx, root, op, true); err != nil {
				return err
			}
		}
		var receipt control.FinalReceipt
		if err = local.ReadJSON(published, &receipt); err != nil {
			return err
		}
		if receipt.Operation != op || receipt.Instance != id || receipt.Epoch != epoch || !receipt.StartupVerified || !receipt.Verified {
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
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if !local.SafeID(op) || !local.SafeID(id) || epoch == 0 {
		return errors.New("explicit operation/instance/epoch required")
	}
	if err = e.action(ctx, "final-resume", op, id, epoch); err != nil {
		return err
	}
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
	if err = local.ReadJSON(filepath.Join(e.Host.Root, "deploy/control/state/final", op, "timer.json"), &timer); err == nil && timer.Active {
		return e.system(ctx, "start", "matrix-backup.timer")
	}
	return err
}
func (e *Engine) Handover(ctx context.Context) (err error) {
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
	defer func() { e.recordError(base, op.ID, step, err) }()
	ctl := func(action string) error { return e.action(ctx, action, op.ID, s.Instance, op.Epoch) }
	if op.Mode == "planned" && op.Source == s.Instance {
		switch s.Control.Phase {
		case "REQUESTED", "QUIESCING", "QUIESCED", "SNAPSHOTTING":
			step = "final-backup"
			if err = e.final(ctx, op.ID, s.Instance, op.Epoch); err != nil {
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
			restored, err = e.restore(ctx, receipt.Snapshot, receipt.PointHash)
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
		if restored.Snapshot != receipt.Snapshot || restored.Hash != receipt.PointHash || !restored.Matrix || !restored.Telegram || restored.Production {
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
