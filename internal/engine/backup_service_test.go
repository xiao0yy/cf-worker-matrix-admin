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
)

type readyBackup struct {
	receipt control.FinalReceipt
	calls   int
}

func (b *readyBackup) Final(context.Context, string, string) (control.FinalReceipt, error) {
	b.calls++
	return b.receipt, nil
}
func (*readyBackup) Restore(context.Context, string, string, string) (Restored, error) {
	return Restored{}, errors.New("unexpected restore")
}

func TestFinalConsumesProducerReadinessWithoutReadingDatabase(t *testing.T) {
	e := testEngine(t)
	e.Store = &memoryObjects{data: map[string][]byte{}}
	op := &control.FinalOperation{ID: "move-test", Source: "node-a", Epoch: 1, Phase: "SNAPSHOTTING"}
	s := Status{Instance: "node-a", Cluster: e.Cluster, Epoch: 1, Blocked: true, Final: op, Control: &control.State{Owner: "node-a", Epoch: 1, Phase: "SNAPSHOTTING"}}
	completed := false
	fakeController(t, e, &s, func(action string) error {
		if action == "/final-complete" {
			completed = true
		}
		return nil
	})
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		if c.Name != e.Host.Systemctl {
			t.Fatalf("cluster performed data work: %v", c)
		}
		return []byte("inactive"), nil
	})
	b := &readyBackup{receipt: control.FinalReceipt{Schema: 2, Ready: true, Operation: op.ID, Instance: s.Instance, Epoch: s.Epoch}}
	e.Backups = b
	must(t, e.Final(context.Background(), op.ID, s.Instance, s.Epoch))
	if b.calls != 1 || !completed {
		t.Fatal("did not consume readiness", b.calls, completed)
	}
	// No database fixture is present: the coordinator must not read, restore,
	// hash or probe data after the trusted backup service says it is ready.
}

func TestSourceRollbackStopsWorkerBeforeAcquiringItsLock(t *testing.T) {
	e := testEngine(t)
	op := &control.FinalOperation{ID: "move-test", Source: "node-a", Epoch: 1, Phase: "SNAPSHOTTING"}
	s := Status{Instance: "node-a", Epoch: 1, Blocked: true, Final: op, Control: &control.State{Owner: "node-a", Epoch: 1, Phase: "SNAPSHOTTING"}}
	fakeController(t, e, &s, func(action string) error {
		if action == "/final-resume" {
			s.Final = nil
			s.Serving = true
			s.Blocked = false
			s.Control.Phase = "ONLINE"
		}
		return nil
	})
	base := filepath.Join(e.Host.Root, "deploy/control/state/final", op.ID)
	must(t, local.JSON(filepath.Join(base, "timer.json"), map[string]bool{"timer_was_active": true}))
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	must(t, err)
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		args := strings.Join(c.Args, " ")
		if args == "stop matrix-handover.service" {
			if _, err := os.Stat(filepath.Join(e.Host.Root, "agent/handover-paused.json")); err != nil {
				t.Fatal("worker stopped before durable pause", err)
			}
			unlock()
			unlock = nil
		} else if args != "start matrix-backup.timer" {
			t.Fatal("unexpected host mutation", args)
		}
		return nil, nil
	})
	must(t, e.SourceResume(context.Background(), op.ID, s.Instance, 1))
	if !s.Serving || s.Final != nil {
		t.Fatal("source not resumed")
	}
}

func TestSourceRollbackRefusesReleasedAuthorityBeforeAnyMutation(t *testing.T) {
	e := testEngine(t)
	s := Status{Instance: "node-a", Epoch: 1, Final: &control.FinalOperation{ID: "move-test"}, Control: &control.State{Owner: "", Epoch: 1, Phase: "RELEASED"}}
	fakeController(t, e, &s, func(string) error { t.Fatal("controller mutated"); return nil })
	e.Runner = runFunc(func(context.Context, Command) ([]byte, error) { t.Fatal("host mutated"); return nil, nil })
	if e.SourceResume(context.Background(), "move-test", "node-a", 1) == nil {
		t.Fatal("rolled back released source")
	}
	if _, err := os.Stat(filepath.Join(e.Host.Root, "agent/handover-paused.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pause changed", err)
	}
}
