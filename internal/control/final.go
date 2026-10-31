package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type StopProof struct {
	Name        string `json:"name"`
	ContainerID string `json:"container_id"`
	Image       string `json:"image_id"`
	DataPath    string `json:"data_path"`
	FinishedAt  string `json:"finished_at"`
}
type FinalReceipt struct {
	Schema          int    `json:"schema"`
	Operation       string `json:"operation_id"`
	Cluster         string `json:"cluster_id"`
	Instance        string `json:"source_instance_id"`
	Epoch           uint64 `json:"source_epoch"`
	Cutoff          string `json:"cutoff_time"`
	Snapshot        string `json:"snapshot_id"`
	PointKey        string `json:"point_key"`
	PointHash       string `json:"point_sha256"`
	Verified        bool   `json:"full_restore_verified"`
	StartupVerified bool   `json:"database_startup_verified"`
	VerifiedAt      string `json:"verified_at"`
}
type FinalOperation struct {
	Isolation *Isolation    `json:"external_isolation,omitempty"`
	ID        string        `json:"id"`
	Mode      string        `json:"mode"`
	Source    string        `json:"source"`
	Epoch     uint64        `json:"source_epoch"`
	Phase     string        `json:"phase"`
	StartedAt string        `json:"started_at"`
	Cutoff    string        `json:"cutoff_time,omitempty"`
	Proof     []StopProof   `json:"stopped_proof,omitempty"`
	Receipt   *FinalReceipt `json:"recovery_set,omitempty"`
	Target    string        `json:"target,omitempty"`
	NextEpoch uint64        `json:"next_epoch,omitempty"`
}
type stoppedInspector interface {
	Stopped(context.Context, Service) (StopProof, error)
}

func (c *Controller) drainingPermittedLocked(fresh bool) bool {
	if fresh && c.state != nil && c.state.Owner == c.cfg.Instance && c.state.Epoch == c.journal.BoundEpoch && c.state.Phase == "REQUESTED" {
		var op FinalOperation
		return json.Unmarshal(c.state.Operation, &op) == nil && op.Mode == "planned" && op.Source == c.cfg.Instance && op.Target != "" && op.Target != op.Source && op.Epoch == c.journal.BoundEpoch
	}
	if !fresh || c.journal.Final == nil || c.journal.Final.Phase != "DRAINING" || c.state == nil || c.state.Owner != c.cfg.Instance || c.state.Epoch != c.journal.BoundEpoch {
		return false
	}
	if c.state.Phase == "ONLINE" && string(c.state.Operation) == "null" {
		return true
	}
	var op FinalOperation
	return c.state.Phase == "QUIESCING" && json.Unmarshal(c.state.Operation, &op) == nil && op.ID == c.journal.Final.ID && op.Source == c.cfg.Instance && op.Epoch == c.journal.BoundEpoch
}

func (d *Docker) Stopped(ctx context.Context, s Service) (StopProof, error) {
	running, err := d.Inspect(ctx, s)
	if err != nil {
		return StopProof{}, err
	}
	if running {
		return StopProof{}, errors.New("writer is still running")
	}
	b, err := d.call(ctx, "GET", "/containers/"+s.Name+"/json")
	if err != nil {
		return StopProof{}, err
	}
	var v struct {
		Id    string
		Image string
		State struct {
			Running, Restarting, OOMKilled bool
			Pid, ExitCode                  int
			FinishedAt                     string
		}
	}
	if json.Unmarshal(b, &v) != nil || v.State.Running || v.State.Restarting || v.State.Pid != 0 || v.State.OOMKilled || v.State.ExitCode != 0 {
		return StopProof{}, errors.New("clean shutdown not proven (running, OOM, or nonzero exit)")
	}
	finish, err := time.Parse(time.RFC3339Nano, v.State.FinishedAt)
	if err != nil || finish.Year() < 2000 {
		return StopProof{}, errors.New("missing shutdown timestamp")
	}
	return StopProof{Name: s.Name, ContainerID: v.Id, Image: v.Image, DataPath: s.DataPath, FinishedAt: v.State.FinishedAt}, nil
}
func (c *Controller) saveFinal(op *FinalOperation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
	c.journal.Blocked = true
	c.journal.Reason = "final backup: explicit final-resume required"
	c.journal.Final = op
	if err := c.saveLocked(); err != nil {
		return err
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}
func (c *Controller) finalAction(ctx context.Context, action, instance string, epoch uint64, id string) error {
	c.finalMu.Lock()
	defer c.finalMu.Unlock()
	c.authorityMu.Lock()
	defer c.authorityMu.Unlock()
	if instance != c.cfg.Instance || !safeID(id) || len(id) > 100 || strings.Contains(id, ".") || epoch == 0 {
		return errors.New("invalid expected final backup identity")
	}
	c.mu.Lock()
	bound := c.journal.BoundEpoch
	highRevision, highEpoch := c.journal.HighRevision, c.journal.HighEpoch
	local := c.journal.Final
	busy := c.busy
	starting := busy && c.action == "checking-or-starting"
	c.mu.Unlock()
	if epoch != bound {
		return errors.New("final backup data epoch mismatch")
	}
	if local != nil && (local.ID != id || local.Epoch != epoch) {
		return errors.New("another final operation holds this instance")
	}
	state, etag, err := c.store.Read(ctx)
	if err != nil {
		return err
	}
	if state.Owner != instance || state.Epoch != epoch {
		return errors.New("final action has no source ownership")
	}
	if state.Revision < highRevision || state.Epoch < highEpoch {
		return errors.New("control state regression")
	}
	var remote FinalOperation
	if string(state.Operation) != "null" {
		if json.Unmarshal(state.Operation, &remote) != nil || remote.ID != id || remote.Source != instance || remote.Epoch != epoch || (remote.Mode != "final-backup" && remote.Mode != "planned") {
			return errors.New("remote operation mismatch")
		}
	}
	update := func(phase string, op *FinalOperation) error {
		state.Revision++
		state.Phase = phase
		if op == nil {
			state.Operation = json.RawMessage("null")
		} else {
			state.Operation, _ = json.Marshal(op)
		}
		if err := c.store.Write(ctx, state, etag); err != nil {
			return err
		}
		c.observe(state, nil)
		return nil
	}
	if action == "/final-begin" {
		if local == nil {
			if _, e := os.Stat(filepath.Join(c.cfg.StateDir, "final", id, "closed.json")); !os.IsNotExist(e) {
				return errors.New("operation ID already closed or cannot be inspected")
			}
			planned := state.Phase == "REQUESTED" && remote.Mode == "planned" && remote.Phase == "REQUESTED" && remote.NextEpoch == epoch+1 && remote.Target != instance
			if !planned && (state.Phase != "ONLINE" || string(state.Operation) != "null") {
				return errors.New("source not idle ONLINE")
			}
			op := &FinalOperation{ID: id, Mode: "final-backup", Source: instance, Epoch: epoch, Phase: "DRAINING", StartedAt: c.now().UTC().Format(time.RFC3339Nano)}
			if planned {
				copy := remote
				copy.Phase = "DRAINING"
				op = &copy
			}
			if err = c.saveFinal(op); err != nil {
				return err
			}
			local = op
		}
		if local.Phase == "DRAINING" && ((state.Phase == "ONLINE" && string(state.Operation) == "null") || (state.Phase == "REQUESTED" && remote.Mode == "planned")) {
			return update("QUIESCING", local)
		}
		if remote.ID == id {
			return nil
		}
		return errors.New("begin acknowledgment cannot be reconciled")
	}
	if local == nil {
		if action == "/final-resume" && state.Phase == "ONLINE" && string(state.Operation) == "null" {
			b, e := os.ReadFile(filepath.Join(c.cfg.StateDir, "final", id, "closed.json"))
			var closed struct {
				Operation FinalOperation `json:"operation"`
			}
			if e == nil && json.Unmarshal(b, &closed) == nil && closed.Operation.ID == id && closed.Operation.Source == instance && closed.Operation.Epoch == epoch {
				return nil
			}
		}
		return errors.New("no local final operation")
	}
	// Reconcile a committed remote transition whose acknowledgment/local fsync
	// was lost. Never replay an earlier phase or create a second recovery set.
	rank := map[string]int{"DRAINING": 1, "STOPPING": 2, "FROZEN": 3, "SNAPSHOTTING": 4, "READY": 5, "RESUMING": 6}
	if remote.ID == id && rank[remote.Phase] > rank[local.Phase] {
		if err = c.saveFinal(&remote); err != nil {
			return err
		}
		local = &remote
	}
	op := *local
	if action == "/final-resume" {
		if starting {
			return errors.New("service reconciliation still active; retry")
		}
		dir := filepath.Join(c.cfg.StateDir, "final", id)
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(dir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return errors.New("final backup worker still active")
		}
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		if state.Phase == "ONLINE" && string(state.Operation) == "null" && (op.Phase == "RESUMING" || op.Phase == "DRAINING") {
			// Successful CAS with a lost acknowledgment: finish only the local commit.
		} else {
			if remote.ID != id {
				return errors.New("cannot resume another or unknown operation")
			}
			op.Phase = "RESUMING"
			if err = c.saveFinal(&op); err != nil {
				return err
			}
			if err = update("ONLINE", nil); err != nil {
				return err
			}
		}
		if err = atomicJSON(filepath.Join(dir, "closed.json"), map[string]any{"operation": op, "closed_at": c.now().UTC().Format(time.RFC3339Nano), "recovery_set_valid_for_handover": false}); err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.journal.Final = nil
		c.journal.Blocked = false
		c.journal.Reason = ""
		c.ready = false
		if err = c.saveLocked(); err != nil {
			c.journal.Final = &op
			c.journal.Blocked = true
			return err
		}
		select {
		case c.wake <- struct{}{}:
		default:
		}
		return nil
	}
	if remote.ID != id {
		return errors.New("remote final operation is not acknowledged")
	}
	switch action {
	case "/final-verify":
		if op.Cutoff == "" || len(op.Proof) != len(c.services()) {
			return errors.New("source freeze proof missing")
		}
		inspector, ok := c.docker.(stoppedInspector)
		if !ok {
			return errors.New("clean-stop inspection unavailable")
		}
		for i, s := range c.services() {
			p, e := inspector.Stopped(ctx, s)
			if e != nil {
				return e
			}
			if p != op.Proof[i] {
				return errors.New("source changed or restarted after freeze")
			}
		}
		return nil
	case "/final-stop":
		if op.Phase != "DRAINING" {
			return nil
		}
		op.Phase = "STOPPING"
		return c.saveFinal(&op)
	case "/final-frozen":
		if op.Cutoff != "" {
			return nil
		}
		if op.Phase != "STOPPING" || starting {
			return errors.New("writers have not finished stopping")
		}
		inspector, ok := c.docker.(stoppedInspector)
		if !ok {
			return errors.New("clean-stop inspection unavailable")
		}
		var latest time.Time
		for _, service := range c.services() {
			proof, e := inspector.Stopped(ctx, service)
			if e != nil {
				return e
			}
			op.Proof = append(op.Proof, proof)
			t, _ := time.Parse(time.RFC3339Nano, proof.FinishedAt)
			if t.After(latest) {
				latest = t
			}
		}
		op.Phase = "FROZEN"
		op.Cutoff = latest.UTC().Format(time.RFC3339Nano)
		if err = update("QUIESCED", &op); err != nil {
			return err
		}
		return c.saveFinal(&op)
	case "/final-snapshot":
		if op.Phase == "SNAPSHOTTING" || op.Phase == "READY" {
			return nil
		}
		if op.Phase != "FROZEN" {
			return errors.New("final snapshot requires verified frozen source")
		}
		op.Phase = "SNAPSHOTTING"
		if err = update("SNAPSHOTTING", &op); err != nil {
			return err
		}
		return c.saveFinal(&op)
	case "/final-complete":
		if op.Phase == "READY" {
			return nil
		}
		if op.Phase != "SNAPSHOTTING" {
			return errors.New("not snapshotting this operation")
		}
		if err = c.verifyStopped(ctx, op); err != nil {
			return err
		}
		b, e := os.ReadFile(filepath.Join(c.cfg.StateDir, "final", id, "receipt.json"))
		if e != nil {
			return errors.New("verified final receipt missing")
		}
		var r FinalReceipt
		if json.Unmarshal(b, &r) != nil || r.Schema != 1 || r.Operation != id || r.Cluster != c.cfg.Cluster || r.Instance != instance || r.Epoch != epoch || r.Cutoff != op.Cutoff || !r.Verified || !r.StartupVerified || !hex64(r.Snapshot) || !hex64(r.PointHash) || r.PointKey != "cluster/"+c.cfg.Cluster+"/recovery/v2/points/"+r.Snapshot+".json" || r.VerifiedAt == "" {
			return errors.New("invalid or mismatched final receipt")
		}
		op.Receipt = &r
		op.Phase = "READY"
		if err = update("QUIESCED", &op); err != nil {
			return err
		}
		return c.saveFinal(&op)
	default:
		return fmt.Errorf("unsupported final action %s", action)
	}
}
func hex64(s string) bool { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }
