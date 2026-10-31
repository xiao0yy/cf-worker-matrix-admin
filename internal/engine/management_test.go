package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/payload"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
)

func (s *memoryObjects) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for k := range s.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}
func taskRequest(e *Engine, action string) management.Request {
	return management.Request{ID: "job-one", Cluster: e.Cluster, Target: "node-a", Action: action, Created: e.stamp(), Expected: management.Expected{Revision: 1, Epoch: 1, Owner: "node-a", Phase: "ONLINE"}}
}
func TestTasksInterruptInsteadOfReplayingAndCancelBeforeSideEffects(t *testing.T) {
	for _, initial := range []string{"RUNNING", "QUEUED"} {
		t.Run(initial, func(t *testing.T) {
			e := testEngine(t)
			s := Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 1, Blocked: true, Control: &control.State{Revision: 1, Epoch: 1, Owner: "node-a", Phase: "ONLINE"}}
			fakeController(t, e, &s, nil)
			e.Store = &memoryObjects{data: map[string][]byte{}}
			e.Runner = runFunc(func(context.Context, Command) ([]byte, error) { return nil, errors.New("no business mutation allowed") })
			task := management.Task{Schema: 1, Request: taskRequest(e, "backup-now"), State: initial, Stage: "checkpoint-and-upload", Updated: e.stamp()}
			must(t, local.JSON(management.Path(e.Host.Root, task.Request.ID), task))
			if initial == "QUEUED" {
				must(t, management.Cancel(e.Host.Root, task.Request.ID))
			}
			must(t, e.Tasks(context.Background()))
			got, err := management.Read(e.Host.Root, task.Request.ID)
			must(t, err)
			want := "INTERRUPTED"
			if initial == "QUEUED" {
				want = "CANCELLED"
			}
			if got.State != want {
				t.Fatal(got)
			}
		})
	}
}
func TestTaskAuthorityAndPreflightFailClosed(t *testing.T) {
	e := testEngine(t)
	s := Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 1, Control: &control.State{Revision: 2, Epoch: 1, Owner: "node-a", Phase: "ONLINE"}}
	fakeController(t, e, &s, nil)
	if e.taskAuthority(context.Background(), taskRequest(e, "backup-now")) == nil {
		t.Fatal("stale task executed")
	}
	path := filepath.Join(e.Host.Root, "deploy/control/state/preflight.json")
	must(t, local.JSON(path, PreflightReport{Ready: true, Epoch: 1, At: e.stamp()}))
	e.Runner = runFunc(func(context.Context, Command) ([]byte, error) { return nil, errors.New("missing dependency") })
	if _, err := e.Preflight(context.Background(), "", ""); err == nil {
		t.Fatal("preflight unexpectedly passed")
	}
	var report PreflightReport
	must(t, local.ReadJSON(path, &report))
	if report.Ready {
		t.Fatal("old ready report survived failed recheck")
	}
}
func TestNginxRefreshRestoresBothFilesOnFailedValidation(t *testing.T) {
	e := testEngine(t)
	e.Host.NginxDirectory = t.TempDir()
	s := Status{Cluster: e.Cluster, Instance: "node-a", Blocked: true}
	fakeController(t, e, &s, nil)
	bundle := t.TempDir()
	must(t, local.JSON(filepath.Join(bundle, "payload/control/source-local.json"), control.Config{S3Config: "/old/deploy/control/s3.env"}))
	for _, name := range []string{"matrix.conf", "00-matrix-common.conf"} {
		write(t, filepath.Join(bundle, "payload/nginx", name), "new /old/deploy/control")
		write(t, filepath.Join(e.Host.NginxDirectory, name), "original "+name)
	}
	reloaded := false
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		if c.Name == e.Host.Nginx {
			return nil, errors.New("invalid nginx config")
		}
		if strings.Join(c.Args, " ") == "reload nginx" {
			reloaded = true
			return nil, nil
		}
		t.Fatalf("unexpected command %+v", c)
		return nil, nil
	})
	if e.refreshNginx(context.Background(), bundle) == nil {
		t.Fatal("invalid config accepted")
	}
	for _, name := range []string{"matrix.conf", "00-matrix-common.conf"} {
		b, err := os.ReadFile(filepath.Join(e.Host.NginxDirectory, name))
		must(t, err)
		if string(b) != "original "+name {
			t.Fatal("original lost")
		}
	}
	if !reloaded {
		t.Fatal("rollback was not reloaded")
	}
}
func catalogFixture(t *testing.T) (*Engine, *Status, *memoryObjects, Point) {
	e := testEngine(t)
	s := &Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 1, Serving: true, Control: &control.State{Revision: 1, Owner: "node-a", Epoch: 1, Phase: "ONLINE"}}
	fakeController(t, e, s, nil)
	e.ReleaseDir = runtimeFixture(t, "0.3.0")
	d, err := release.Describe(e.ReleaseDir)
	must(t, err)
	must(t, local.CopyTree(e.ReleaseDir, filepath.Join(e.Host.Root, "agent/cache/releases", d.SHA)))
	pending := t.TempDir()
	bundle, m := bundleFixture(t, pending)
	m.Software = d
	saveBundle(t, bundle, m)
	must(t, payload.Pack(bundle, filepath.Join(pending, "configuration.json")))
	store := &memoryObjects{data: map[string][]byte{}}
	e.Store = store
	p, _, err := e.publish(context.Background(), m, pending, BackupJob{Snapshot: strings.Repeat("a", 64), Repository: Repository{Schema: 1, ID: strings.Repeat("d", 64)}, IndexMatch: "*", Full: true, Verified: true, VerifiedAt: e.stamp()})
	must(t, err)
	return e, s, store, p
}
func TestRetentionProtectsLatestCompletedAndQueuedRestore(t *testing.T) {
	e, s, store, base := catalogFixture(t)
	for i, id := range []string{"b", "c", "d", "e", "f"} {
		p := base
		p.Snapshot = strings.Repeat(id, 64)
		p.Time = e.Now().Add(-time.Duration(i+10) * 24 * time.Hour).Format(time.RFC3339Nano)
		store.data[e.prefix()+"/points/"+p.Snapshot+".json"] = release.JSON(p)
	}
	s.Control.Completed = &control.FinalOperation{Receipt: &control.FinalReceipt{Snapshot: strings.Repeat("e", 64)}}
	task := management.Task{Schema: 1, Request: taskRequest(e, "restore-drill"), State: "RUNNING"}
	task.Request.Parameters.Snapshot = strings.Repeat("f", 64)
	store.data["cluster/fixture/tasks/node-a/job.json"] = release.JSON(task)
	r := taskRequest(e, "storage-plan")
	r.Parameters = management.Parameters{KeepLast: 2, KeepDays: 1}
	result, err := e.RetentionPlan(context.Background(), r)
	must(t, err)
	p := result.(map[string]any)["plan"].(StoragePlan)
	if strings.Join(p.Delete, ",") != strings.Repeat("c", 64)+","+strings.Repeat("d", 64) {
		t.Fatal(p)
	}
	if len(store.data) == 0 {
		t.Fatal("lost catalog")
	}
	// A tombstone closes access before physical forget, including interrupted pruning.
	store.data[e.prefix()+"/retired/"+base.Snapshot+".json"] = []byte("{}")
	if _, _, err = e.readPoint(context.Background(), "", ""); err == nil {
		t.Fatal("retired point readable")
	}
}
func TestCleanupChecksReplacementAndRetainsIdentity(t *testing.T) {
	e := testEngine(t)
	s := Status{Cluster: e.Cluster, Instance: "node-a", Blocked: true, Control: &control.State{Revision: 1, Epoch: 1, Owner: "node-b", Phase: "ONLINE"}}
	fakeController(t, e, &s, nil)
	store := &memoryObjects{data: map[string][]byte{}}
	e.Store = store
	store.data["cluster/fixture/instances/node-b.json"] = release.JSON(map[string]any{"serving": true, "bound_epoch": 1, "reported_at": e.stamp()})
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		if c.Args[0] == "inspect" {
			return nil, errors.New("missing")
		}
		return nil, nil
	})
	data := filepath.Join(e.Host.Root, "generations/recovery-old/data")
	write(t, data, "old data")
	identity := filepath.Join(e.Host.Root, "deploy/control/state/identity.json")
	write(t, identity, "preserve")
	req := taskRequest(e, "cleanup-plan")
	req.Expected.Owner = "node-b"
	res, err := e.CleanupPlan(context.Background(), req)
	must(t, err)
	hash := res.(map[string]any)["plan_sha256"].(string)
	apply := req
	apply.Action = "cleanup-apply"
	apply.Parameters = management.Parameters{Plan: req.ID, PlanHash: hash}
	s.Serving = true
	if _, err = e.CleanupApply(context.Background(), apply); err == nil {
		t.Fatal("cleaned serving instance")
	}
	s.Serving = false
	s.Control.Epoch = 2
	if _, err = e.CleanupApply(context.Background(), apply); err == nil {
		t.Fatal("cleaned after authority changed")
	}
	s.Control.Epoch = 1
	must(t, os.Symlink(identity, filepath.Join(e.Host.Root, "generations/recovery-old/link")))
	if _, err = e.CleanupApply(context.Background(), apply); err == nil {
		t.Fatal("symlink accepted")
	}
	must(t, os.Remove(filepath.Join(e.Host.Root, "generations/recovery-old/link")))
	_, err = e.CleanupApply(context.Background(), apply)
	must(t, err)
	if _, err = os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("planned data retained", err)
	}
	if b, err := os.ReadFile(identity); err != nil || string(b) != "preserve" {
		t.Fatal("identity damaged")
	}
}
func TestUpdaterIsIndependentOfControllerService(t *testing.T) {
	e := testEngine(t)
	unit := e.units(Installed{Directory: "/opt/matrix/releases/0.3.0"})["matrix-tasks.service"]
	if unit == "" || strings.Contains(unit, "Requires=matrix-control.service") {
		t.Fatal("upgrade would terminate itself", unit)
	}
}
func TestSnapshotCapacityRejectsInvalidAndOversizedBackup(t *testing.T) {
	e := testEngine(t)
	e.ReleaseDir = runtimeFixture(t, "0.3.0")
	for _, data := range []string{`{"total_size":0}`, `{"total_size":1152921504606846976}`, `bad`} {
		e.Runner = runFunc(func(context.Context, Command) ([]byte, error) { return []byte(data), nil })
		if _, err := e.snapshotSpace(context.Background(), Repository{}, "id"); err == nil {
			t.Fatal("invalid size passed", data)
		}
	}
}
