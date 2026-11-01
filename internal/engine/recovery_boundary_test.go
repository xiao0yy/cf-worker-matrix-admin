package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
)

func TestHandoverReconcilesIntentWithoutCallingBackupOrHost(t *testing.T) {
	e := testEngine(t)
	op := control.FinalOperation{ID: "move-start", Mode: "planned", Source: "node-a", Target: "node-b", Epoch: 1, NextEpoch: 2, Phase: "VALIDATING"}
	s := Status{Instance: "node-b", Cluster: e.Cluster, Epoch: 2, Blocked: true, Control: &control.State{Owner: "node-b", Epoch: 2, Phase: "VALIDATING", Operation: release.JSON(op)}, Handover: &control.TargetOperation{FinalOperation: op, Root: filepath.Join(e.Host.Root, "generations/handover-move-start/rebuilt"), MayWrite: true}}
	s.Handover.Phase = "STARTING"
	calls := 0
	fakeController(t, e, &s, func(action string) error {
		if action != "/handover-start" {
			t.Fatal("replayed data stage", action)
		}
		calls++
		return nil
	})
	e.Backups = &readyBackup{}
	e.Runner = runFunc(func(context.Context, Command) ([]byte, error) {
		t.Fatal("host operation during intent reconciliation")
		return nil, nil
	})
	must(t, e.Handover(context.Background()))
	if calls != 1 {
		t.Fatal("start intent was not reconciled")
	}
}

func TestRollbackRetryAuthorityAcceptsOnlyItsOwnClosure(t *testing.T) {
	e := testEngine(t)
	s := Status{Instance: "node-a", Cluster: e.Cluster, Control: &control.State{Owner: "node-a", Epoch: 1, Revision: 5, Phase: "ONLINE", Aborted: &control.FinalOperation{ID: "move-test", Source: "node-a", Epoch: 1, Phase: "ABORTED"}}}
	fakeController(t, e, &s, nil)
	r := taskRequest(e, "rollback-handover")
	r.Parameters.Operation = "move-test"
	must(t, e.taskAuthority(context.Background(), r))
	s.Control.Aborted.ID = "another-move"
	if e.taskAuthority(context.Background(), r) == nil {
		t.Fatal("old rollback could close another operation")
	}
	s.Control.Aborted.ID = "move-test"
	s.Control.Epoch = 2
	if e.taskAuthority(context.Background(), r) == nil {
		t.Fatal("old rollback could cross epoch")
	}
}

func TestRegisteredCleanupProtectsUnclassifiedAndCurrentData(t *testing.T) {
	e := testEngine(t)
	work := filepath.Join(e.Host.Root, "generations/handover-retired")
	root := filepath.Join(work, "rebuilt")
	s := Status{Instance: "node-a", Cluster: e.Cluster, Epoch: 2, Deployment: root, Blocked: true, Control: &control.State{Revision: 5, Owner: "node-b", Epoch: 3, Phase: "ONLINE", Completed: &control.FinalOperation{Target: "node-b", NextEpoch: 3, Receipt: &control.FinalReceipt{Ready: true}}}}
	fakeController(t, e, &s, nil)
	e.Store = &memoryObjects{data: map[string][]byte{"cluster/fixture/instances/node-b.json": release.JSON(map[string]any{"serving": true, "bound_epoch": 3, "reported_at": e.stamp()})}}
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		if c.Args[0] == "inspect" {
			return nil, errors.New("absent")
		}
		return nil, nil
	})
	unknown := filepath.Join(e.Host.Root, "generations/recovery-unclassified/data")
	write(t, unknown, "preserve")
	write(t, filepath.Join(root, "deploy/tuwunel/data/CURRENT"), "retired")
	g := generation{1, e.Cluster, s.Instance, root, work, strings.Repeat("a", 64), strings.Repeat("b", 64), 2}
	must(t, e.registerGeneration(g))
	// Register a possibly writable current generation: it cannot be deleted.
	current := filepath.Join(e.Host.Root, "generations/handover-uncertain")
	protected := g
	protected.Work = current
	protected.Root = filepath.Join(current, "rebuilt")
	protected.Epoch = 3
	write(t, filepath.Join(current, "data"), "preserve")
	must(t, e.registerGeneration(protected))
	r := taskRequest(e, "cleanup-plan")
	r.Expected = management.Expected{Revision: 5, Epoch: 3, Owner: "node-b", Phase: "ONLINE"}
	res, err := e.CleanupPlan(context.Background(), r)
	must(t, err)
	p := res.(map[string]any)["plan"].(CleanupPlan)
	if len(p.Paths) != 1 || p.Paths[0] != work {
		t.Fatal("registry inventory differs", p.Paths)
	}
	r.Action = "cleanup-apply"
	r.Parameters = management.Parameters{Plan: p.ID, PlanHash: res.(map[string]any)["plan_sha256"].(string)}
	producer, err := local.Lock(filepath.Join(root, "backup-state/tuwunel-restic/operation.lock"))
	must(t, err)
	if _, err = e.CleanupApply(context.Background(), r); err == nil {
		t.Fatal("cleanup removed a running producer's inputs")
	}
	producer()
	_, err = e.CleanupApply(context.Background(), r)
	must(t, err)
	_, err = e.CleanupApply(context.Background(), r)
	must(t, err) // retained registry supports retries
	for _, path := range []string{unknown, filepath.Join(current, "data"), e.generationPath(root)} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal("protected data or identity removed", path, err)
		}
	}
	if _, err = os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retired handover workspace survived", err)
	}
}

func TestClosedOperationPauseClearsWithoutTouchingLaterPause(t *testing.T) {
	e := testEngine(t)
	path := filepath.Join(e.Host.Root, "agent/handover-paused.json")
	for _, id := range []string{"closed-op", "new-op", ""} {
		must(t, local.JSON(path, map[string]string{"operation_id": id}))
		must(t, e.clearOperationPause("closed-op"))
		_, err := os.Stat(path)
		if (id == "closed-op") != errors.Is(err, os.ErrNotExist) {
			t.Fatal("pause ownership not respected", id, err)
		}
	}
}
