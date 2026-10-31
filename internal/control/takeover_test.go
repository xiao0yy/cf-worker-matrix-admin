package control

import (
	"context"
	"encoding/json"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func forceFixture(t *testing.T) (*Controller, *Controller, *finalAuthority, string, management.Request) {
	a, b, s, da, _, root := pairFixture(t)
	for k := range da.running {
		da.running[k] = false
	} // operator powered off the source
	raw, _ := json.Marshal(map[string]any{"protocol": 2, "point_schema": 2, "snapshot_id": strings.Repeat("a", 64), "data_time": "2026-01-01T00:00:00Z", "image_id": a.cfg.Services[0].Image, "telegram": map[string]string{"image_id": a.cfg.Services[1].Image}, "consistency": "independent-online-snapshots", "instance_control": map[string]any{"cluster_id": "test", "source_instance_id": "a", "source_epoch": 1}})
	r := management.Request{ID: "test-final", Cluster: "test", Target: "b", Action: "force-takeover", Created: b.now().UTC().Format(time.RFC3339Nano), Expected: management.Expected{Revision: s.state.Revision, Epoch: 2, Owner: "a", Phase: "ONLINE"}, Parameters: management.Parameters{Snapshot: strings.Repeat("a", 64), PointHash: digest(raw), Isolated: []string{"a"}, Isolation: "powered-off", Note: "powered off in provider console", AcceptLoss: true}}
	dir := filepath.Join(b.cfg.StateDir, "handover", r.ID)
	if err := atomicJSON(filepath.Join(dir, "force-request.json"), r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "point.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(root, "recovery-state/result.json"), map[string]any{"database_startup_verified": true, "telegram_database_startup_verified": true, "production_started": false, "consistency": "independent-online-snapshots"}); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(dir, "deployment.json"), map[string]any{"schema": 1, "deployment_root": root, "operation_id": r.ID, "instance_id": "b", "epoch": 3}); err != nil {
		t.Fatal(err)
	}
	return a, b, s, root, r
}
func TestForcedTakeoverUsesOlderPointWithoutInventingFinalBackup(t *testing.T) {
	a, b, s, _, r := forceFixture(t)
	if err := b.forceClaim(context.Background(), "b", 2, r.ID); err != nil {
		t.Fatal(err)
	}
	if s.state.Epoch != 3 || s.state.Owner != "b" || s.state.Phase != "RESTORING" || gateCode(b) != 403 {
		t.Fatal("claim did not fence target")
	}
	if b.journal.Target.Receipt.Verified || b.journal.Target.Receipt.Operation != "" || len(b.journal.Target.Proof) != 0 {
		t.Fatal("manufactured a clean source stop")
	}
	for _, action := range []string{"validating", "bind", "start"} {
		if err := hcall(b, action); err != nil {
			t.Fatal(action, err)
		}
	}
	b.runServices()
	if gateCode(b) != 403 {
		t.Fatal("opened during STARTING")
	}
	if err := hcall(b, "online"); err != nil {
		t.Fatal(err)
	}
	b.runServices()
	if gateCode(b) != 204 || s.state.Completed.Mode != "forced" {
		t.Fatal("forced recovery not completed")
	}
	a.observe(s.state, nil)
	if a.resume(context.Background(), "a", 2) == nil {
		t.Fatal("old owner resumed")
	}
	if hcall(b, "bind") == nil {
		t.Fatal("restored over new writes")
	}
}
func TestForcedTakeoverLostAcknowledgementAndStaleRequest(t *testing.T) {
	_, b, s, _, r := forceFixture(t)
	s.loseNext = true
	if b.forceClaim(context.Background(), "b", 2, r.ID) == nil {
		t.Fatal("fault missing")
	}
	n := s.writes
	if err := b.forceClaim(context.Background(), "b", 2, r.ID); err != nil || s.writes != n {
		t.Fatal("claim replayed", err)
	}
	r.ID = "another"
	r.Expected.Revision = 4
	if err := atomicJSON(filepath.Join(b.cfg.StateDir, "handover", r.ID, "force-request.json"), r); err != nil {
		t.Fatal(err)
	}
	if b.forceClaim(context.Background(), "b", 2, r.ID) == nil {
		t.Fatal("stale takeover accepted")
	}
}
func TestForcedTakeoverRequiresExplicitIsolationOfPreviousTarget(t *testing.T) {
	_, b, s, _, r := forceFixture(t)
	s.state.Phase = "STARTING"
	s.state.Owner = "c"
	s.state.Operation, _ = json.Marshal(FinalOperation{ID: "previous", Mode: "planned", Source: "a", Target: "c", Epoch: 1, NextEpoch: 2})
	r.Expected.Owner = "c"
	r.Expected.Phase = "STARTING"
	if err := atomicJSON(filepath.Join(b.cfg.StateDir, "handover", r.ID, "force-request.json"), r); err != nil {
		t.Fatal(err)
	}
	if b.forceClaim(context.Background(), "b", 2, r.ID) == nil {
		t.Fatal("forgotten previous writer was accepted")
	}
	r.Parameters.Isolated = append(r.Parameters.Isolated, "c")
	if err := atomicJSON(filepath.Join(b.cfg.StateDir, "handover", r.ID, "force-request.json"), r); err != nil {
		t.Fatal(err)
	}
	if err := b.forceClaim(context.Background(), "b", 2, r.ID); err != nil {
		t.Fatal(err)
	}
}
func TestPlannedRequestNeedsFreshPreflightBeforeSourceStops(t *testing.T) {
	a, b, s, da, _, _ := pairFixture(t)
	_ = a
	if err := os.Remove(filepath.Join(b.cfg.StateDir, "preflight.json")); err != nil {
		t.Fatal(err)
	}
	if hcall(b, "request") == nil || s.writes != 0 {
		t.Fatal("requested without target preflight")
	}
	for _, running := range da.running {
		if !running {
			t.Fatal("source stopped")
		}
	}
}

func TestForceCanRecoverAnAbandonedReleasedHandover(t *testing.T) {
	_, b, s, _, r := forceFixture(t)
	s.state.Owner = ""
	s.state.Phase = "RELEASED"
	s.state.Operation, _ = json.Marshal(FinalOperation{ID: "previous", Mode: "planned", Source: "a", Target: "c", Epoch: 2, NextEpoch: 3})
	r.Expected.Owner = ""
	r.Expected.Phase = "RELEASED"
	r.Parameters.Isolated = append(r.Parameters.Isolated, "c")
	if err := atomicJSON(filepath.Join(b.cfg.StateDir, "handover", r.ID, "force-request.json"), r); err != nil {
		t.Fatal(err)
	}
	if err := b.forceClaim(context.Background(), "b", 2, r.ID); err != nil {
		t.Fatal(err)
	}
	var op FinalOperation
	json.Unmarshal(s.state.Operation, &op)
	if op.Source != "a" || op.NextEpoch != 3 || op.Target != "b" {
		t.Fatal(op)
	}
}
func TestStorageMaintenanceFencesAndCannotBeEndedByAnotherOperation(t *testing.T) {
	a, _, s, da, _, _ := pairFixture(t)
	r := management.Request{ID: "prune-one", Cluster: "test", Target: "a", Action: "storage-apply", Created: a.now().UTC().Format(time.RFC3339Nano), Expected: management.Expected{Revision: s.state.Revision, Epoch: 2, Owner: "a", Phase: "ONLINE"}, Parameters: management.Parameters{Plan: "plan-one", PlanHash: strings.Repeat("c", 64)}}
	if err := atomicJSON(filepath.Join(a.cfg.StateDir, "maintenance", r.ID+".json"), r); err != nil {
		t.Fatal(err)
	}
	if a.maintenance(context.Background(), "/maintenance-begin", r.ID, "a", 2) == nil {
		t.Fatal("maintenance while online")
	}
	a.journal.Blocked = true
	for k := range da.running {
		da.running[k] = false
	}
	if err := a.maintenance(context.Background(), "/maintenance-begin", r.ID, "a", 2); err != nil {
		t.Fatal(err)
	}
	if s.state.Phase != "MAINTENANCE" || gateCode(a) != 403 {
		t.Fatal("maintenance admitted traffic")
	}
	if a.resume(context.Background(), "a", 2) == nil {
		t.Fatal("resumed during pruning")
	}
	if a.maintenance(context.Background(), "/maintenance-end", "other", "a", 2) == nil {
		t.Fatal("ended different operation")
	}
	if err := a.maintenance(context.Background(), "/maintenance-end", r.ID, "a", 2); err != nil {
		t.Fatal(err)
	}
	if s.state.Phase != "ONLINE" || !a.journal.Blocked || gateCode(a) != 403 {
		t.Fatal("maintenance auto-resumed business")
	}
}
