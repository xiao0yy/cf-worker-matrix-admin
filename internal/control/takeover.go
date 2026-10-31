package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"os"
	"path/filepath"
	"time"
)

// External fencing is a human assertion, never inferred from a stale heartbeat.
// The receipt identifies an older committed point, not a fictitious final backup.
type Isolation struct {
	Instances    []string `json:"instances"`
	Method       string   `json:"method"`
	Note         string   `json:"note"`
	AcceptedLoss bool     `json:"accepted_data_loss"`
}

func validRecovery(op FinalOperation, cluster string) bool {
	if op.Mode == "planned" {
		return validFinalReceipt(op, cluster)
	}
	r := op.Receipt
	return op.Mode == "forced" && op.Isolation != nil && op.Isolation.AcceptedLoss && len(op.Isolation.Instances) > 0 && r != nil && r.Schema == 1 && r.Cluster == cluster && safeID(r.Instance) && r.Epoch > 0 && r.Epoch <= op.Epoch && hex64(r.Snapshot) && hex64(r.PointHash) && r.PointKey == "cluster/"+cluster+"/recovery/v2/points/"+r.Snapshot+".json"
}
func (c *Controller) forceClaim(ctx context.Context, instance string, epoch uint64, id string) error {
	c.finalMu.Lock()
	defer c.finalMu.Unlock()
	c.authorityMu.Lock()
	defer c.authorityMu.Unlock()
	if instance != c.cfg.Instance || !safeID(id) || epoch == 0 || epoch == ^uint64(0) {
		return errors.New("invalid takeover identity")
	}
	dir := filepath.Join(c.cfg.StateDir, "handover", id)
	raw, err := os.ReadFile(filepath.Join(dir, "force-request.json"))
	if err != nil {
		return err
	}
	var req management.Request
	if json.Unmarshal(raw, &req) != nil || req.Validate() != nil || req.ID != id || req.Target != instance || req.Cluster != c.cfg.Cluster || req.Action != "force-takeover" || req.Expected.Epoch != epoch {
		return errors.New("invalid explicit takeover request")
	}
	s, etag, err := c.store.Read(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	j := c.journal
	busy := c.busy
	c.mu.Unlock()
	if s.Epoch < j.HighEpoch || s.Revision < j.HighRevision {
		return errors.New("control regression")
	}
	var remote FinalOperation
	_ = json.Unmarshal(s.Operation, &remote)
	if s.Owner == instance && s.Epoch == epoch+1 && remote.ID == id && remote.Mode == "forced" && j.Target != nil && j.Target.ID == id {
		return nil
	}
	if s.Phase == "ONLINE" && s.Owner == instance && s.Completed != nil && s.Completed.ID == id && j.Target != nil && j.Target.ID == id {
		return nil
	}
	if s.Revision != req.Expected.Revision || s.Epoch != epoch || s.Owner != req.Expected.Owner || s.Phase != req.Expected.Phase || s.Owner == instance || busy {
		return errors.New("takeover authority changed or target is not idle")
	}
	if j.Target != nil && j.Target.MayWrite && j.BoundEpoch >= epoch {
		return errors.New("target may contain newer writes; choose another isolated standby")
	}
	isolated := map[string]bool{}
	for _, v := range req.Parameters.Isolated {
		isolated[v] = true
	}
	// An interrupted planned handover may have two relevant hosts. Require both
	// to be accounted for, including a previous target which might have started.
	for _, v := range []string{s.Owner, remote.Source, remote.Target} {
		if v != "" && v != instance && !isolated[v] {
			return errors.New("all previous possible writers must be externally isolated")
		}
	}
	for _, svc := range c.services() {
		running, er := c.docker.Inspect(ctx, svc)
		if !errors.Is(er, errContainerMissing) && (er != nil || running) {
			return errors.New("target applications must be absent or stopped")
		}
	}
	raw, err = os.ReadFile(filepath.Join(dir, "point.json"))
	if err != nil || digest(raw) != req.Parameters.PointHash {
		return errors.New("selected recovery point hash mismatch")
	}
	var p struct {
		Snapshot string `json:"snapshot_id"`
		Time     string `json:"data_time"`
		Binding  struct {
			Cluster  string `json:"cluster_id"`
			Instance string `json:"source_instance_id"`
			Epoch    uint64 `json:"source_epoch"`
		} `json:"instance_control"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Snapshot != req.Parameters.Snapshot || p.Binding.Cluster != c.cfg.Cluster || p.Binding.Epoch == 0 || p.Binding.Epoch > epoch || !safeID(p.Binding.Instance) {
		return errors.New("selected point belongs to another authority")
	}
	source := s.Owner
	if source == "" {
		source = remote.Source
	}
	if source == "" {
		source = p.Binding.Instance
	}
	op := FinalOperation{ID: id, Mode: "forced", Source: source, Target: instance, Epoch: epoch, NextEpoch: epoch + 1, Phase: "RESTORING", StartedAt: c.now().UTC().Format(time.RFC3339Nano), Isolation: &Isolation{req.Parameters.Isolated, req.Parameters.Isolation, req.Parameters.Note, true}, Receipt: &FinalReceipt{Schema: 1, Cluster: c.cfg.Cluster, Instance: p.Binding.Instance, Epoch: p.Binding.Epoch, Snapshot: p.Snapshot, PointKey: "cluster/" + c.cfg.Cluster + "/recovery/v2/points/" + p.Snapshot + ".json", PointHash: req.Parameters.PointHash, Cutoff: p.Time}}
	t := &TargetOperation{FinalOperation: op}
	t.Phase = "CLAIMING"
	if err = atomicJSON(filepath.Join(dir, "request.json"), op); err != nil {
		return err
	}
	if err = c.saveTarget(t, true); err != nil {
		return err
	}
	s.Revision++
	s.Epoch = op.NextEpoch
	s.Owner = instance
	s.Phase = "RESTORING"
	s.Operation, _ = json.Marshal(op)
	if err = c.store.Write(ctx, s, etag); err != nil {
		return err
	}
	c.observe(s, nil)
	return c.saveTarget(&TargetOperation{FinalOperation: op}, true)
}
