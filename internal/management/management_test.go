package management

import (
	"context"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/storage"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func request(id string) Request {
	return Request{ID: id, Cluster: "test", Target: "b", Action: "preflight", Expected: Expected{1, 1, "a", "ONLINE"}, Created: time.Now().UTC().Format(time.RFC3339Nano)}
}
func TestQueueAndRetryCannotRunTwoMaintenanceJobs(t *testing.T) {
	root := t.TempDir()
	r := request("one")
	if err := Enqueue(root, r); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(root, r); err != nil {
		t.Fatal(err)
	}
	if Enqueue(root, request("two")) == nil {
		t.Fatal("second task accepted")
	}
	if err := Cancel(root, "one"); err != nil {
		t.Fatal(err)
	}
	if Retry(root, "one") == nil {
		t.Fatal("queued task retried")
	}
	job, _ := Read(root, "one")
	job.State = "FAILED"
	if err := local.JSON(Path(root, "one"), job); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(root, request("two")); err != nil {
		t.Fatal(err)
	}
	if Retry(root, "one") == nil {
		t.Fatal("retry bypassed queue")
	}
}
func TestTaskParametersRejectArbitraryCommandsAndUnconfirmedTakeover(t *testing.T) {
	r := request("one")
	r.Parameters.Note = "unexpected"
	if r.Validate() == nil {
		t.Fatal("extra parameter")
	}
	r.Action = "force-takeover"
	r.Parameters = Parameters{Snapshot: strings.Repeat("a", 64), PointHash: strings.Repeat("b", 64), Isolated: []string{"a"}, Isolation: "powered-off", Note: "provider confirmed"}
	if r.Validate() == nil {
		t.Fatal("data loss not acknowledged")
	}
	r.Parameters.AcceptLoss = true
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Parameters.Isolated = []string{"b"}
	if r.Validate() == nil {
		t.Fatal("isolated target accepted")
	}
	r = request("../escape")
	if r.Validate() == nil {
		t.Fatal("path escaped")
	}
}

type memory struct {
	b      []byte
	tag    string
	writes int
}

func (m *memory) Get(context.Context, string) ([]byte, string, error) {
	if m.b == nil {
		return nil, "", storage.Missing
	}
	return m.b, m.tag, nil
}
func (m *memory) Put(_ context.Context, _ string, b []byte, tag string) error {
	if m.b != nil && tag != m.tag {
		return storage.Conflict
	}
	m.b = b
	m.tag = local.Digest(b)
	m.writes++
	return nil
}
func TestStaleReportCannotOverwriteCompletionAndUnchangedReportsAreFree(t *testing.T) {
	s := &memory{}
	root := t.TempDir()
	job := Task{Schema: 1, Request: request("one"), State: "SUCCEEDED", Updated: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := Publish(context.Background(), root, job, s, "task"); err != nil {
		t.Fatal(err)
	}
	if err := Publish(context.Background(), root, job, s, "task"); err != nil || s.writes != 1 {
		t.Fatal("unchanged task republished")
	}
	job.State = "RUNNING"
	job.Updated = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if err := Publish(context.Background(), filepath.Join(root, "another"), job, s, "task"); err != nil || s.writes != 1 {
		t.Fatal("stale task hid completion", err)
	}
}
