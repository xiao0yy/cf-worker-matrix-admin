package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type TargetOperation struct {
	FinalOperation
	Root     string `json:"deployment_root,omitempty"`
	MayWrite bool   `json:"may_have_written"`
}

func operationMatches(raw json.RawMessage, want FinalOperation) bool {
	var op FinalOperation
	return json.Unmarshal(raw, &op) == nil && op.Mode == "planned" && want.Mode == "planned" && op.ID == want.ID && op.Source == want.Source && op.Target == want.Target && op.Epoch == want.Epoch && op.NextEpoch == want.NextEpoch
}
func (c *Controller) saveTarget(t *TargetOperation, blocked bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
	c.journal.Target = t
	c.journal.Blocked = blocked
	c.journal.Reason = "planned handover: " + t.Phase
	if err := c.saveLocked(); err != nil {
		c.journal.Blocked = true
		return err
	}
	return nil
}
func (c *Controller) verifyStopped(ctx context.Context, op FinalOperation) error {
	services := c.services()
	inspector, ok := c.docker.(stoppedInspector)
	if !ok || len(op.Proof) != len(services) || op.Cutoff == "" {
		return errors.New("source freeze proof missing")
	}
	for i, s := range services {
		p, err := inspector.Stopped(ctx, s)
		if err != nil || p != op.Proof[i] {
			return errors.New("source restarted or no longer matches clean-stop proof")
		}
	}
	return nil
}
func validFinalReceipt(op FinalOperation, cluster string) bool {
	r := op.Receipt
	return r != nil && r.Schema == 1 && r.Operation == op.ID && r.Cluster == cluster && r.Instance == op.Source && r.Epoch == op.Epoch && r.Cutoff == op.Cutoff && r.Verified && r.StartupVerified && hex64(r.Snapshot) && hex64(r.PointHash) && r.PointKey == "cluster/"+cluster+"/recovery/v2/points/"+r.Snapshot+".json"
}
func (c *Controller) handoverAction(ctx context.Context, action, instance string, epoch uint64, id string) error {
	c.finalMu.Lock()
	defer c.finalMu.Unlock()
	c.authorityMu.Lock()
	defer c.authorityMu.Unlock()
	if instance != c.cfg.Instance || !safeID(id) || strings.Contains(id, ".") || len(id) > 100 || epoch == 0 {
		return errors.New("invalid handover identity")
	}
	s, etag, err := c.store.Read(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	j := c.journal
	busy := c.busy
	c.mu.Unlock()
	if s.Revision < j.HighRevision || s.Epoch < j.HighEpoch {
		return errors.New("control regression")
	}
	var remote FinalOperation
	_ = json.Unmarshal(s.Operation, &remote)
	write := func(phase, owner string, e uint64, op *FinalOperation) error {
		s.Revision++
		s.Phase, s.Owner, s.Epoch = phase, owner, e
		s.Operation = json.RawMessage("null")
		if op != nil {
			s.Operation, _ = json.Marshal(op)
		}
		if err := c.store.Write(ctx, s, etag); err != nil {
			return err
		}
		c.observe(s, nil)
		return nil
	}
	if action == "/handover-request" {
		if c.cfg.Generations == "" {
			return errors.New("Agent must prepare a generations directory before requesting handover")
		}
		if j.Target != nil && j.Target.ID == id && j.Target.Epoch == epoch {
			if operationMatches(s.Operation, j.Target.FinalOperation) || (s.Phase == "ONLINE" && s.Owner == instance && s.Epoch == j.Target.NextEpoch && s.Completed != nil && s.Completed.ID == id) {
				return nil
			}
			if j.Target.Phase != "REQUESTING" {
				return errors.New("request was closed or superseded")
			}
		} else if j.Target != nil && j.Target.Phase != "ONLINE" && j.Target.Phase != "CANCELLED" {
			return errors.New("another target operation is active")
		}
		if _, e := os.Stat(filepath.Join(c.cfg.StateDir, "handover", id, "request.json")); e == nil && (j.Target == nil || j.Target.ID != id) {
			return errors.New("operation ID was already used")
		}
		if s.Phase != "ONLINE" || s.Owner == instance || s.Owner == "" || s.Epoch != epoch || epoch == ^uint64(0) || (j.Final != nil && j.Final.Phase != "RETIRED") {
			return errors.New("source is not an idle owner or target is not a standby")
		}
		for _, service := range c.services() {
			running, e := c.docker.Inspect(ctx, service)
			if !errors.Is(e, errContainerMissing) && (e != nil || running) {
				return errors.New("target applications must be absent or stopped with the expected identity")
			}
		}
		op := FinalOperation{ID: id, Mode: "planned", Source: s.Owner, Target: instance, Epoch: epoch, NextEpoch: epoch + 1, Phase: "REQUESTED", StartedAt: c.now().UTC().Format("2006-01-02T15:04:05.999999999Z07:00")}
		t := &TargetOperation{FinalOperation: op}
		t.Phase = "REQUESTING"
		if err = atomicJSON(filepath.Join(c.cfg.StateDir, "handover", id, "request.json"), op); err != nil {
			return err
		}
		if err = c.saveTarget(t, true); err != nil {
			return err
		}
		if err = write("REQUESTED", s.Owner, epoch, &op); err != nil {
			return err
		}
		t = &TargetOperation{FinalOperation: op}
		return c.saveTarget(t, true)
	}
	if action == "/handover-release" {
		f := j.Final
		if f == nil || f.ID != id || f.Mode != "planned" || f.Epoch != epoch || f.Source != instance || f.Target == instance || f.NextEpoch != epoch+1 || !validFinalReceipt(*f, c.cfg.Cluster) {
			return errors.New("no complete planned source recovery set")
		}
		ack := operationMatches(s.Operation, *f) && ((s.Phase == "RELEASED" && s.Owner == "" && s.Epoch == epoch) || (s.Owner == f.Target && s.Epoch == f.NextEpoch))
		ack = ack || (s.Phase == "ONLINE" && s.Owner == f.Target && s.Epoch == f.NextEpoch && s.Completed != nil && s.Completed.ID == id)
		if !ack {
			if s.Owner != instance || s.Epoch != epoch || s.Phase != "QUIESCED" || f.Phase != "READY" || !operationMatches(s.Operation, *f) || remote.Phase != "READY" {
				return errors.New("source is not exclusively frozen and READY")
			}
			lock, e := os.OpenFile(filepath.Join(c.cfg.StateDir, "final", id, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if e != nil {
				return e
			}
			defer lock.Close()
			if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
				return errors.New("final producer is still active")
			}
			defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			if err = c.verifyStopped(ctx, *f); err != nil {
				return err
			}
			op := *f
			op.Phase = "RELEASED"
			if err = write("RELEASED", "", epoch, &op); err != nil {
				return err
			}
		}
		op := *f
		op.Phase = "RETIRED"
		return c.saveFinal(&op)
	}
	t := j.Target
	if t == nil || t.ID != id || t.Target != instance || t.Epoch != epoch || t.NextEpoch != epoch+1 {
		return errors.New("no matching local target operation")
	}
	if action == "/handover-cancel" {
		if t.MayWrite || s.Epoch != epoch {
			return errors.New("cannot cancel after ownership claim or possible writes")
		}
		copy := *t
		copy.Phase = "CANCELLED"
		if s.Phase == "ONLINE" && s.Owner == t.Source && string(s.Operation) == "null" {
			return c.saveTarget(&copy, true)
		}
		if s.Phase != "REQUESTED" || s.Owner != t.Source || !operationMatches(s.Operation, t.FinalOperation) {
			return errors.New("source has claimed the operation; source final-resume is required before release")
		}
		if err = write("ONLINE", t.Source, epoch, nil); err != nil {
			return err
		}
		return c.saveTarget(&copy, true)
	}
	if action == "/handover-online" && s.Phase == "ONLINE" && s.Owner == instance && s.Epoch == t.NextEpoch && s.Completed != nil && s.Completed.ID == id && (t.Phase == "COMMITTING" || t.Phase == "ONLINE") {
		if j.Blocked {
			return errors.New("target is fenced; explicit handover-resume is required")
		}
		copy := *t
		copy.Phase = "ONLINE"
		return c.saveTarget(&copy, false)
	}
	if action == "/handover-resume" && s.Phase == "ONLINE" && s.Owner == instance && s.Epoch == t.NextEpoch && s.Completed != nil && s.Completed.ID == id && t.MayWrite && j.BoundEpoch == t.NextEpoch {
		copy := *t
		if err = c.saveTarget(&copy, false); err != nil {
			return err
		}
		select {
		case c.wake <- struct{}{}:
		default:
		}
		return nil
	}
	if !operationMatches(s.Operation, t.FinalOperation) {
		return errors.New("remote operation is different or closed")
	}
	if !validFinalReceipt(remote, c.cfg.Cluster) {
		return errors.New("remote final recovery receipt is incomplete")
	}
	copy := *t
	copy.FinalOperation = remote
	if action == "/handover-claim" {
		if s.Owner == instance && s.Epoch == t.NextEpoch && (s.Phase == "RESTORING" || s.Phase == "VALIDATING" || s.Phase == "STARTING") {
			return nil
		}
		if s.Phase != "RELEASED" || s.Owner != "" || s.Epoch != epoch || remote.Phase != "RELEASED" || t.MayWrite {
			return errors.New("source has not released the exact verified recovery set")
		}
		copy.Phase = "CLAIMING"
		if err = c.saveTarget(&copy, true); err != nil {
			return err
		}
		op := remote
		op.Phase = "RESTORING"
		if err = write("RESTORING", instance, t.NextEpoch, &op); err != nil {
			return err
		}
		copy.FinalOperation = op
		return c.saveTarget(&copy, true)
	}
	if s.Owner != instance || s.Epoch != t.NextEpoch {
		return errors.New("target no longer owns the expected epoch")
	}
	switch action {
	case "/handover-validating":
		if s.Phase == "VALIDATING" {
			return nil
		}
		if s.Phase != "RESTORING" || t.MayWrite {
			return errors.New("validation cannot restart after possible writes")
		}
		op := remote
		op.Phase = "VALIDATING"
		if err = write("VALIDATING", instance, t.NextEpoch, &op); err != nil {
			return err
		}
		copy.FinalOperation = op
		return c.saveTarget(&copy, true)
	case "/handover-bind":
		if s.Phase != "VALIDATING" || t.MayWrite || busy {
			return errors.New("bind requires an idle pre-write target")
		}
		return c.bindDeployment(ctx, copy)
	case "/handover-start":
		if s.Phase == "STARTING" && t.MayWrite {
			return nil
		}
		if s.Phase != "VALIDATING" || t.Phase != "BOUND" || t.Root == "" || j.BoundEpoch != t.NextEpoch || t.MayWrite || busy {
			return errors.New("target is not durably bound and ready to start")
		}
		// Exclude an in-flight stop worker until the STARTING CAS is reconciled.
		c.mu.Lock()
		c.busy = true
		c.action = "admission-transition"
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			c.busy = false
			c.action = ""
			c.mu.Unlock()
			select {
			case c.wake <- struct{}{}:
			default:
			}
		}()
		copy.Phase = "STARTING"
		copy.MayWrite = true
		if err = c.saveTarget(&copy, false); err != nil {
			return err
		}
		op := remote
		op.Phase = "STARTING"
		return write("STARTING", instance, t.NextEpoch, &op)
	case "/handover-resume":
		if s.Phase != "STARTING" || !t.MayWrite || j.BoundEpoch != t.NextEpoch || t.Root == "" {
			return errors.New("resume only continues an already bound STARTING generation")
		}
		copy.Phase = "STARTING"
		if err = c.saveTarget(&copy, false); err != nil {
			return err
		}
		select {
		case c.wake <- struct{}{}:
		default:
		}
		return nil
	case "/handover-online":
		if s.Phase != "STARTING" || !t.MayWrite || (t.Phase != "STARTING" && t.Phase != "COMMITTING") || j.Blocked || j.BoundEpoch != t.NextEpoch {
			return errors.New("target is not authorized STARTING")
		}
		for _, service := range c.services() {
			running, e := c.docker.Inspect(ctx, service)
			if e != nil || !running {
				return errors.New("target service is not running with the pinned identity")
			}
			if e = c.docker.Healthy(ctx, service); e != nil {
				return e
			}
		}
		copy.Phase = "COMMITTING"
		if err = c.saveTarget(&copy, false); err != nil {
			return err
		}
		op := remote
		op.Phase = "ONLINE"
		s.Completed = &op
		if err = write("ONLINE", instance, t.NextEpoch, nil); err != nil {
			return err
		}
		copy.Phase = "ONLINE"
		return c.saveTarget(&copy, false)
	}
	return errors.New("unsupported target action")
}

// The root worker supplies fixed-path artifacts. The controller independently
// binds the R2 receipt hash, restored identity, probe report and canonical mounts.
func (c *Controller) bindDeployment(ctx context.Context, t TargetOperation) error {
	dir := filepath.Join(c.cfg.StateDir, "handover", t.ID)
	var m struct {
		Schema    int    `json:"schema"`
		Root      string `json:"deployment_root"`
		Operation string `json:"operation_id"`
		Instance  string `json:"instance_id"`
		Epoch     uint64 `json:"epoch"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "deployment.json"))
	if err != nil {
		return err
	}
	if json.Unmarshal(b, &m) != nil || m.Schema != 1 || m.Operation != t.ID || m.Instance != c.cfg.Instance || m.Epoch != t.NextEpoch {
		return errors.New("deployment identity mismatch")
	}
	root := filepath.Clean(m.Root)
	base, err := filepath.EvalSymlinks(c.cfg.Generations)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil || real != root || !strings.HasPrefix(root, base+string(os.PathSeparator)) {
		return errors.New("deployment must be a new generation below the prepared directory")
	}
	if t.Root != "" && t.Root != root {
		return errors.New("bound generation is immutable")
	}
	b, err = os.ReadFile(filepath.Join(dir, "point.json"))
	if err != nil || digest(b) != t.Receipt.PointHash {
		return errors.New("downloaded point does not match the released receipt")
	}
	var point struct {
		Protocol int    `json:"protocol"`
		Schema   int    `json:"point_schema"`
		Snapshot string `json:"snapshot_id"`
		Image    string `json:"image_id"`
		Telegram struct {
			Image string `json:"image_id"`
		} `json:"telegram"`
		Control struct {
			Cluster  string `json:"cluster_id"`
			Instance string `json:"source_instance_id"`
			Epoch    uint64 `json:"source_epoch"`
		} `json:"instance_control"`
		Operation   string `json:"operation_id"`
		Consistency string `json:"consistency"`
	}
	if json.Unmarshal(b, &point) != nil || point.Protocol != 2 || point.Schema != 2 || point.Snapshot != t.Receipt.Snapshot || point.Operation != t.ID || point.Consistency != "globally-quiesced" || point.Control.Cluster != c.cfg.Cluster || point.Control.Instance != t.Source || point.Control.Epoch != t.Epoch || !strings.HasPrefix(point.Image, "sha256:") || !hex64(strings.TrimPrefix(point.Image, "sha256:")) || !strings.HasPrefix(point.Telegram.Image, "sha256:") || !hex64(strings.TrimPrefix(point.Telegram.Image, "sha256:")) {
		return errors.New("released point identity or images mismatch")
	}
	var probe struct {
		Matrix      bool   `json:"database_startup_verified"`
		Telegram    bool   `json:"telegram_database_startup_verified"`
		Production  bool   `json:"production_started"`
		Consistency string `json:"consistency"`
	}
	b, err = os.ReadFile(filepath.Join(root, "recovery-state", "result.json"))
	if err != nil || json.Unmarshal(b, &probe) != nil || !probe.Matrix || !probe.Telegram || probe.Production || probe.Consistency != "globally-quiesced" {
		return errors.New("isolated database validation report missing")
	}
	services := append([]Service(nil), c.cfg.Services...)
	services[0].DataPath = filepath.Join(root, "deploy/tuwunel/data")
	services[0].Image = point.Image
	services[1].DataPath = filepath.Join(root, "deploy/telegram/data")
	services[1].Image = point.Telegram.Image
	for _, file := range []string{filepath.Join(services[0].DataPath, "CURRENT"), filepath.Join(services[1].DataPath, "telegram.db")} {
		st, e := os.Lstat(file)
		if e != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			return errors.New("restored database missing")
		}
	}
	for _, s := range c.services() {
		running, e := c.docker.Inspect(ctx, s)
		if !errors.Is(e, errContainerMissing) && (e != nil || running) {
			return errors.New("old application containers must be absent or stopped")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Phase = "BOUND"
	t.Root = root
	c.journal.Target = &t
	c.journal.Final = nil
	c.journal.BoundEpoch = t.NextEpoch
	c.journal.Services = services
	c.journal.Deployment = root
	c.journal.Blocked = true
	c.ready = false
	return c.saveLocked()
}
