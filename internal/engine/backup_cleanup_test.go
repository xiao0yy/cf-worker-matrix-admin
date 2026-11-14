package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
)

func cleanupFixture(t *testing.T) (*Engine, *Status) {
	e := testEngine(t)
	e.Store = &memoryObjects{data: map[string][]byte{}}
	s := &Status{Instance: "node-a", Cluster: e.Cluster, Epoch: 1, Serving: true, Control: &control.State{Epoch: 1, Owner: "node-a", Phase: "ONLINE", Operation: json.RawMessage("null")}}
	fakeController(t, e, s, nil)
	return e, s
}
func workspaceRead(t *testing.T, base string) backupWorkspace {
	t.Helper()
	var w backupWorkspace
	must(t, local.ReadJSON(filepath.Join(base, "workspace.json"), &w))
	return w
}
func requireRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch remains: %s (%v)", path, err)
	}
}

func TestFailedBackupScratchExpiresWithoutExtendingOnRetry(t *testing.T) {
	e, _ := cleanupFixture(t)
	ctx := context.Background()
	e.Runner = runFunc(func(context.Context, Command) ([]byte, error) { return nil, errors.New("offline Docker fixture") })
	base := backupBase(e.Host.Root, "")
	live := filepath.Join(e.Host.Root, "deploy/tuwunel/data/CURRENT")
	write(t, live, "live database")
	write(t, filepath.Join(base, "last-success.json"), `{"snapshot_id":"previous"}`)
	if err := e.Backup(ctx, false); err == nil {
		t.Fatal("expected staging failure")
	}
	w := workspaceRead(t, base)
	if w.State != "RETRY_PENDING" || w.Expires == "" {
		t.Fatal("failed task has no bounded retention", w)
	}
	initial := e.Now()
	e.Now = func() time.Time { return initial.Add(23 * time.Hour) }
	if err := e.Backup(ctx, false); err == nil {
		t.Fatal("expected repeated failure")
	}
	if workspaceRead(t, base).Expires != w.Expires {
		t.Fatal("retry extended retention")
	}
	must(t, e.SweepBackupScratch(ctx))
	if _, err := os.Stat(filepath.Join(base, "preparing")); err != nil {
		t.Fatal("retriable scratch removed early")
	}
	e.Now = func() time.Time { return initial.Add(24 * time.Hour) }
	must(t, e.SweepBackupScratch(ctx))
	if got := workspaceRead(t, base); got.State != "CLEANED" {
		t.Fatalf("cleanup failed: %+v", got)
	}
	requireRemoved(t, filepath.Join(base, "preparing"))
	for _, p := range []string{live, filepath.Join(base, "last-success.json")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal("non-scratch removed", p)
		}
	}
	// A later independent backup gets its own retention window.
	if err := e.Backup(ctx, false); err == nil {
		t.Fatal("expected failure")
	}
	if workspaceRead(t, base).Expires == w.Expires {
		t.Fatal("new backup inherited old expiry")
	}
}

func TestFinalScratchRetainedUntilClosedAndHardlinksDoNotDeleteSource(t *testing.T) {
	e, s := cleanupFixture(t)
	ctx := context.Background()
	op := control.FinalOperation{ID: "move-scratch", Source: s.Instance, Epoch: 1, Phase: "SNAPSHOTTING"}
	s.Final = &op
	s.Control.Operation, _ = json.Marshal(op)
	w, err := e.beginBackupWorkspace(e.Host.Root, op.ID, *s)
	must(t, err)
	base := backupBase(e.Host.Root, op.ID)
	source := filepath.Join(e.Host.Root, "deploy/tuwunel/data/000001.sst")
	write(t, source, "original immutable table")
	view := filepath.Join(base, "preparing/bundle/payload/database")
	must(t, os.MkdirAll(view, 0700))
	must(t, os.Link(source, filepath.Join(view, "000001.sst")))
	write(t, filepath.Join(base, "interrupted-checkpoint/000001.sst"), "checkpoint copy")
	e.finishBackupWorkspace(e.Host.Root, w, errors.New("failed upload"))
	initial := e.Now()
	e.Now = func() time.Time { return initial.Add(72 * time.Hour) }
	must(t, e.SweepBackupScratch(ctx))
	if workspaceRead(t, base).State != "RETRY_PENDING" {
		t.Fatal("active final expired")
	}
	// A durable close record alone cannot override a still-active reference.
	must(t, local.JSON(filepath.Join(e.Host.Root, "deploy/control/state/final", op.ID, "closed.json"), map[string]any{"operation": op, "closed_at": e.stamp(), "recovery_set_valid_for_handover": false}))
	must(t, e.SweepBackupScratch(ctx))
	if _, err := os.Stat(view); err != nil {
		t.Fatal("active operation lost scratch")
	}
	s.Final, s.Control.Operation = nil, json.RawMessage("null")
	must(t, e.SweepBackupScratch(ctx))
	if got := workspaceRead(t, base); got.State != "CLEANED" {
		t.Fatalf("closed final not reclaimed: %+v", got)
	}
	requireRemoved(t, view)
	requireRemoved(t, filepath.Join(base, "interrupted-checkpoint"))
	b, err := os.ReadFile(source)
	must(t, err)
	if string(b) != "original immutable table" {
		t.Fatal("source changed")
	}
}

func TestScratchRecoveryHonorsProducerAndOrphanProcess(t *testing.T) {
	e, s := cleanupFixture(t)
	ctx := context.Background()
	base := backupBase(e.Host.Root, "")
	_, err := e.beginBackupWorkspace(e.Host.Root, "", *s)
	must(t, err)
	write(t, filepath.Join(base, "pending/data"), "staged data")
	lock, err := local.Lock(filepath.Join(base, "operation.lock"))
	must(t, err)
	must(t, e.SweepBackupScratch(ctx))
	if workspaceRead(t, base).State != "RUNNING" {
		t.Fatal("sweeper touched live producer")
	}
	lock() // A killed producer releases this lock but cannot run deferred cleanup.
	must(t, e.SweepBackupScratch(ctx))
	until := workspaceRead(t, base).Expires
	when, err := time.Parse(time.RFC3339Nano, until)
	must(t, err)
	e.Now = func() time.Time { return when }
	// An orphan subprocess can outlive its parent and still read this data.
	child := exec.Command("sleep", "60")
	child.Dir = filepath.Join(base, "pending")
	must(t, child.Start())
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	must(t, e.SweepBackupScratch(ctx))
	w := workspaceRead(t, base)
	if w.State != "CLEANUP_PENDING" || !strings.Contains(w.Error, "仍使用临时数据") {
		t.Fatalf("in-use scratch not protected: %+v", w)
	}
	must(t, child.Process.Kill())
	_ = child.Wait()
	must(t, e.SweepBackupScratch(ctx))
	if w = workspaceRead(t, base); w.State != "CLEANED" || w.Error != "" {
		t.Fatalf("failed cleanup not retried: %+v", w)
	}
	requireRemoved(t, filepath.Join(base, "pending"))
}

func TestScratchSweepRejectsUnknownAndSymlinkedDirectories(t *testing.T) {
	e, s := cleanupFixture(t)
	base := backupBase(e.Host.Root, "")
	write(t, filepath.Join(base, "preparing/unclassified"), "unknown")
	if _, err := e.beginBackupWorkspace(e.Host.Root, "", *s); err == nil {
		t.Fatal("unknown directory adopted")
	}
	must(t, e.SweepBackupScratch(context.Background()))
	var report backupCleanupReport
	must(t, local.ReadJSON(filepath.Join(e.Host.Root, "agent/backup-cleanup.json"), &report))
	if report.State != "BLOCKED" || len(report.Items) != 1 {
		t.Fatal("unclassified scratch hidden")
	}
	must(t, os.RemoveAll(filepath.Join(base, "preparing")))
	w, err := e.beginBackupWorkspace(e.Host.Root, "", *s)
	must(t, err)
	out := t.TempDir()
	write(t, filepath.Join(out, "sentinel"), "keep")
	must(t, os.Symlink(out, filepath.Join(base, "pending")))
	e.finishBackupWorkspace(e.Host.Root, w, nil)
	if workspaceRead(t, base).State != "CLEANUP_PENDING" {
		t.Fatal("symlink accepted")
	}
	if _, err := os.Stat(filepath.Join(out, "sentinel")); err != nil {
		t.Fatal("followed symlink")
	}
}

func TestForcedEpochRetiresFinalScratchWithoutDeletingDeployment(t *testing.T) {
	e, s := cleanupFixture(t)
	op := control.FinalOperation{ID: "move-lost-source", Source: s.Instance, Epoch: 1, Phase: "SNAPSHOTTING"}
	w, err := e.beginBackupWorkspace(e.Host.Root, op.ID, *s)
	must(t, err)
	base := backupBase(e.Host.Root, op.ID)
	write(t, filepath.Join(base, "pending/copy"), "partial snapshot")
	e.finishBackupWorkspace(e.Host.Root, w, errors.New("network failure"))
	s.Final = &op // local journal is stale after external fencing
	s.Control.Epoch, s.Control.Owner = 2, "node-b"
	must(t, e.SweepBackupScratch(context.Background()))
	if got := workspaceRead(t, base); got.State != "CLEANED" {
		t.Fatalf("superseded scratch retained: %+v", got)
	}
}

func TestScratchWaitsForAsynchronousCheckpointProducer(t *testing.T) {
	e, s := cleanupFixture(t)
	base := backupBase(e.Host.Root, "")
	w, err := e.beginBackupWorkspace(e.Host.Root, "", *s)
	must(t, err)
	var c Container
	c.ID, c.State.Running, c.State.StartedAt = "checkpoint-producer", true, "start"
	must(t, local.JSON(filepath.Join(base, "preparing/request.json"), checkpointRequest{At: e.stamp(), Container: c}))
	checkpoint := filepath.Join(e.Host.Root, "deploy/tuwunel/data/checkpoint-restic")
	write(t, filepath.Join(checkpoint, "part.sst"), "incomplete checkpoint")
	e.finishBackupWorkspace(e.Host.Root, w, errors.New("checkpoint timeout"))
	start := e.Now()
	e.Now = func() time.Time { return start.Add(24 * time.Hour) }
	complete := false
	e.Runner = runFunc(func(_ context.Context, cmd Command) ([]byte, error) {
		if cmd.Args[0] == "inspect" {
			return json.Marshal([]Container{c})
		}
		if cmd.Args[0] == "logs" {
			if complete {
				return []byte("Created checkpoint at `/var/lib/tuwunel/checkpoint-restic`."), nil
			}
			return nil, nil
		}
		return nil, errors.New("unexpected producer mutation")
	})
	must(t, e.SweepBackupScratch(context.Background()))
	if got := workspaceRead(t, base); got.State != "CLEANUP_PENDING" || !strings.Contains(got.Error, "检查点仍可能生成") {
		t.Fatalf("producer was not protected: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(base, "preparing/request.json")); err != nil {
		t.Fatal("producer proof lost")
	}
	complete = true
	// Hold the completed checkpoint open to interrupt deletion after its
	// ownership decision has been persisted, then lose the original logs.
	file, err := os.Open(filepath.Join(checkpoint, "part.sst"))
	must(t, err)
	must(t, e.SweepBackupScratch(context.Background()))
	if got := workspaceRead(t, base); got.Checkpoint != "remove" || got.State != "CLEANUP_PENDING" {
		t.Fatalf("cleanup proof not durable: %+v", got)
	}
	must(t, file.Close())
	complete = false
	e.Runner = runFunc(func(context.Context, Command) ([]byte, error) { return nil, errors.New("producer logs rotated") })
	must(t, e.SweepBackupScratch(context.Background()))
	if got := workspaceRead(t, base); got.State != "CLEANED" {
		t.Fatalf("completed producer not reclaimed: %+v", got)
	}
	requireRemoved(t, checkpoint)
	requireRemoved(t, filepath.Join(base, "preparing"))
}
