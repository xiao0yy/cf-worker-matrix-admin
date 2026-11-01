package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type beforeCommit struct {
	Authority
	fail bool
}

func TestRequestedCommitAcknowledgementSurvivesMissedAbortHistory(t *testing.T) {
	_, b, s, _, _, _ := pairFixture(t)
	s.loseNext = true
	if hcall(b, "request") == nil || b.journal.Target.Phase != "REQUESTING" {
		t.Fatal("lost request acknowledgement was not reproduced")
	}
	// Retry is an observation of the committed request, not another CAS.
	writes := s.writes
	if err := hcall(b, "request"); err != nil || s.writes != writes || b.journal.Target.Phase != "REQUESTED" {
		t.Fatal("committed request was not reconciled", err)
	}
	b = restartController(t, b)
	// Source rolled back while this target was offline; a later rollback
	// replaced the optional last_aborted receipt in the meantime.
	s.state.Revision += 4
	s.state.Phase, s.state.Operation = "ONLINE", json.RawMessage("null")
	s.state.Aborted = &FinalOperation{ID: "later-abort", Phase: "ABORTED"}
	b.observe(s.state, nil)
	if b.journal.Target.Phase != "CANCELLED" || b.journal.Target.MayWrite || gateCode(b) != 403 {
		t.Fatal("target remained stranded after committed request was closed")
	}
}

// Exercise real controller journals, CAS transitions, stop proofs and admission
// across A -> B -> A. Docker and the backup producer are isolated fixtures.
func TestRoundTripHandoverUsesNewEpochAndGeneration(t *testing.T) {
	a, b, s, da, db, _ := pairFixture(t)
	a.cfg.Generations = t.TempDir()
	ctx := context.Background()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, pair := range [][2]*Controller{{a, b}, {b, a}} {
		source, target := pair[0], pair[1]
		epoch := s.state.Epoch
		id := []string{"move-out", "move-back"}[i]
		check(atomicJSON(filepath.Join(target.cfg.StateDir, "preflight.json"), map[string]any{"schema": 1, "instance_id": target.cfg.Instance, "cluster_id": "test", "epoch": epoch, "checked_at": target.now().UTC().Format(time.RFC3339Nano), "ready": true, "handover_receipt_schema": 2}))
		h := func(c *Controller, action string) {
			check(c.handoverAction(ctx, "/handover-"+action, c.cfg.Instance, epoch, id))
		}
		f := func(action string) {
			if action == "begin" {
				action = "begin-planned"
			}
			check(source.finalAction(ctx, "/final-"+action, source.cfg.Instance, epoch, id))
		}
		h(target, "request")
		f("begin")
		f("stop")
		check(source.stopServices())
		f("frozen")
		f("snapshot")
		root := filepath.Join(target.cfg.Generations, id, "rebuilt")
		check(os.MkdirAll(root, 0700))
		images := source.services()
		point := map[string]any{"protocol": 2, "point_schema": 2, "snapshot_id": strings.Repeat("a", 64), "image_id": images[0].Image, "telegram": map[string]string{"image_id": images[1].Image}, "operation_id": id, "consistency": "globally-quiesced", "instance_control": map[string]any{"cluster_id": "test", "source_instance_id": source.cfg.Instance, "source_epoch": epoch}}
		dir := filepath.Join(target.cfg.StateDir, "handover", id)
		check(atomicJSON(filepath.Join(dir, "point.json"), point))
		raw, err := os.ReadFile(filepath.Join(dir, "point.json"))
		check(err)
		r := receiptFor(source)
		r.Operation = id
		r.Instance = source.cfg.Instance
		r.Epoch = epoch
		r.PointHash = digest(raw)
		check(atomicJSON(filepath.Join(source.cfg.StateDir, "final", id, "receipt.json"), r))
		f("complete")
		check(atomicJSON(filepath.Join(dir, "deployment.json"), map[string]any{"schema": 1, "deployment_root": root, "operation_id": id, "instance_id": target.cfg.Instance, "epoch": epoch + 1}))
		check(atomicJSON(filepath.Join(root, "recovery-state/result.json"), map[string]any{"schema": 1, "restore_ready": true, "snapshot_id": r.Snapshot, "point_sha256": r.PointHash, "deployment_root": root, "production_started": false, "consistency": "globally-quiesced"}))
		h(source, "release")
		h(target, "claim")
		h(target, "validating")
		h(target, "bind")
		h(target, "start")
		target.runServices()
		h(target, "online")
		target.runServices()
		source.observe(s.state, nil)
		if gateCode(source) != 403 || gateCode(target) != 204 || s.state.Epoch != epoch+1 || target.journal.Deployment != root {
			t.Fatal("round trip admission mismatch")
		}
		var op FinalOperation
		_ = json.Unmarshal(s.state.Operation, &op)
		if s.state.Completed == nil || s.state.Completed.ID != id || op.ID != "" {
			t.Fatal("operation not committed")
		}
		for name, on := range da.running {
			if on && db.running[name] {
				t.Fatal("two writers")
			}
		}
	}
}

func (a *beforeCommit) Write(ctx context.Context, s State, tag string) error {
	if a.fail {
		a.fail = false
		return errors.New("transport failed before commit")
	}
	return a.Authority.Write(ctx, s, tag)
}
func restartController(t *testing.T, c *Controller) *Controller {
	t.Helper()
	j, err := readJournal(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	n := &Controller{cfg: c.cfg, store: c.store, docker: c.docker, journal: j, now: c.now, wake: make(chan struct{}, 1)}
	s, _, err := n.store.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n.observe(s, nil)
	return n
}
func TestStartIntentConvergesAfterFailedCommitAndRestart(t *testing.T) {
	for _, retry := range []string{"start", "resume"} {
		t.Run(retry, func(t *testing.T) {
			a, b, s, da, _, root := pairFixture(t)
			makePairReady(t, a, b, root)
			if err := hcall(a, "release"); err != nil {
				t.Fatal(err)
			}
			for _, action := range []string{"claim", "validating", "bind"} {
				if err := hcall(b, action); err != nil {
					t.Fatal(err)
				}
			}
			b.store = &beforeCommit{Authority: s, fail: true}
			if hcall(b, "start") == nil {
				t.Fatal("missing fault")
			}
			b = restartController(t, b)
			if s.state.Phase != "VALIDATING" || !b.journal.Target.MayWrite || b.journal.Deployment != root {
				t.Fatal("lost durable intent")
			}
			if hcall(b, "bind") == nil || hcall(b, "cancel") == nil {
				t.Fatal("possible writes could be replaced")
			}
			if err := hcall(b, retry); err != nil {
				t.Fatal(err)
			}
			b.runServices()
			if err := hcall(b, "online"); err != nil {
				t.Fatal(err)
			}
			b.runServices()
			if s.state.Owner != "b" || s.state.Epoch != 3 || gateCode(b) != 204 || b.journal.Deployment != root {
				t.Fatal("did not converge to original generation")
			}
			for _, running := range da.running {
				if running {
					t.Fatal("second writer")
				}
			}
		})
	}
}

func TestRollbackClosesBothParticipantsAcrossCommitFailures(t *testing.T) {
	for _, phase := range []string{"REQUESTED", "SNAPSHOTTING", "READY"} {
		for _, fault := range []string{"before", "after"} {
			t.Run(phase+"/"+fault, func(t *testing.T) {
				a, b, s, _, _, root := pairFixture(t)
				if phase == "READY" {
					makePairReady(t, a, b, root)
				} else {
					if err := hcall(b, "request"); err != nil {
						t.Fatal(err)
					}
					if phase == "SNAPSHOTTING" {
						toSnapshot(t, a)
					}
				}
				if fault == "before" {
					a.store = &beforeCommit{Authority: s, fail: true}
				} else {
					s.loseNext = true
				}
				if callFinal(a, "resume") == nil {
					t.Fatal("missing rollback fault")
				}
				a = restartController(t, a)
				if err := callFinal(a, "resume"); err != nil {
					t.Fatal(err)
				}
				if err := callFinal(a, "resume"); err != nil {
					t.Fatal("close is not idempotent", err)
				}
				a.runServices()
				b = restartController(t, b)
				if s.state.Owner != "a" || s.state.Epoch != 2 || s.state.Phase != "ONLINE" || s.state.Aborted == nil || s.state.Aborted.ID != "test-final" || s.state.Aborted.Phase != "ABORTED" || b.journal.Target.Phase != "CANCELLED" || b.journal.Target.MayWrite || gateCode(a) != 204 || gateCode(b) != 403 {
					t.Fatal("rollback did not converge", s.state, b.journal)
				}
				if hcall(a, "release") == nil || hcall(b, "request") == nil {
					t.Fatal("closed backup could be reused")
				}
				if err := b.handoverAction(context.Background(), "/handover-request", "b", 2, "next-move"); err != nil {
					t.Fatal("next handover blocked", err)
				}
				if callFinal(a, "resume") == nil {
					t.Fatal("old rollback cancelled a new operation")
				}
			})
		}
	}
}

func TestCancelBeforeFinalWorkerRestoresSourceWithoutRemovingManualFence(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		a, b, s, _, _, _ := pairFixture(t)
		if fenced {
			if err := a.fence("operator stop"); err != nil {
				t.Fatal(err)
			}
		}
		if err := hcall(b, "request"); err != nil {
			t.Fatal(err)
		}
		a.observe(s.state, nil)
		if gateCode(a) != 403 {
			t.Fatal("REQUESTED left ingress open")
		}
		if err := hcall(b, "cancel"); err != nil {
			t.Fatal(err)
		}
		a.observe(s.state, nil)
		a.runServices()
		if a.journal.Blocked != fenced || (gateCode(a) == 204) == fenced {
			t.Fatal("cancel changed pre-existing fence", fenced, a.snapshot())
		}
	}
}

func TestReturnedFormerWriterRetiresWithoutLosingWriteHistory(t *testing.T) {
	_, b, s, _, _, _ := pairFixture(t)
	b.journal.Target = &TargetOperation{FinalOperation: FinalOperation{ID: "old-move", Mode: "planned", Source: "a", Target: "b", Epoch: 2, NextEpoch: 3, Phase: "STARTING"}, Root: "/retained/generation", MayWrite: true}
	b.journal.BoundEpoch = 3
	b.journal.HighEpoch = 3
	s.state = State{Schema: 1, ClusterID: "test", Revision: 20, Epoch: 4, Owner: "a", Phase: "ONLINE", Operation: json.RawMessage("null"), Completed: &FinalOperation{ID: "force-new", Mode: "forced", Target: "a", NextEpoch: 4}}
	b.observe(s.state, nil)
	if b.journal.Target.Phase != "RETIRED" || !b.journal.Target.MayWrite || b.journal.Target.Root != "/retained/generation" || !b.journal.Blocked || gateCode(b) != 403 {
		t.Fatal("returning writer not fenced and retained")
	}
}

func TestCancelledRequestCannotRestartAsStandaloneBackup(t *testing.T) {
	a, b, s, _, _, _ := pairFixture(t)
	if err := hcall(b, "request"); err != nil {
		t.Fatal(err)
	}
	if err := hcall(b, "cancel"); err != nil {
		t.Fatal(err)
	}
	// Even if later aborts replaced the display history, a delayed planned
	// worker must match a currently active planned operation before stopping.
	s.state.Aborted = nil
	if err := a.finalAction(context.Background(), "/final-begin-planned", "a", 2, "test-final"); err == nil {
		t.Fatal("cancelled request started a new backup")
	}
	if a.journal.Final != nil || s.state.Phase != "ONLINE" {
		t.Fatal("closed handover changed state")
	}
}
