package main

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

type mem struct {
	objects   map[string][]byte
	failFinal bool
	loseFinal bool
	beforePut func(string, []byte, string)
}

func (m *mem) Get(_ context.Context, k string) ([]byte, string, error) {
	v, ok := m.objects[k]
	if !ok {
		return nil, "", errMissing
	}
	return append([]byte(nil), v...), hash(v), nil
}
func (m *mem) Put(_ context.Context, k string, b []byte, match string) error {
	if m.beforePut != nil {
		m.beforePut(k, b, match)
	}
	old, exists := m.objects[k]
	if match == "*" && exists {
		return errConflict
	}
	if match != "" && match != "*" && (!exists || match != hash(old)) {
		return errConflict
	}
	var r Receipt
	_ = json.Unmarshal(b, &r)
	if m.failFinal && r.State == "APPLIED" {
		return errors.New("network unavailable")
	}
	m.objects[k] = append([]byte(nil), b...)
	if m.loseFinal && r.State == "APPLIED" {
		m.loseFinal = false
		return errors.New("lost ACK")
	}
	return nil
}
func setup(t *testing.T) (*Poller, *mem, Command, *int) {
	t.Helper()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	m := &mem{objects: map[string][]byte{}}
	calls := 0
	p := &Poller{cfg: Config{Schema: 1, Cluster: "test", Instance: "B", StateDir: t.TempDir()}, store: m, now: func() time.Time { return now }, status: func(context.Context) (Status, error) {
		return Status{Schema: 1, Cluster: "test", Instance: "B", Bound: 0}, nil
	}, run: func(context.Context, Command, Status) error { calls++; return nil }}
	c := Command{Schema: 1, ID: "cmd-one", Cluster: "test", Target: "B", Action: "handover-request", Created: now.Format(time.RFC3339), Expires: now.Add(5 * time.Minute).Format(time.RFC3339), Expected: Expected{Revision: 4, Epoch: 1, Owner: "A", Phase: "ONLINE"}, Bound: 0, Operation: "move-one", SourceEpoch: 1}
	setCommand(t, p, m, c)
	m.objects[p.prefix()+"control.json"] = []byte(`{"schema":1,"cluster_id":"test","revision":4,"epoch":1,"owner":"A","phase":"ONLINE","operation":null}`)
	return p, m, c, &calls
}
func setCommand(t *testing.T, p *Poller, m *mem, c Command) {
	t.Helper()
	b, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	m.objects[p.prefix()+"commands/B/pending.json"] = b
}
func receipt(t *testing.T, p *Poller, m *mem, id string) Receipt {
	t.Helper()
	b := m.objects[p.receiptKey(id)]
	var r Receipt
	if json.Unmarshal(b, &r) != nil {
		t.Fatalf("missing receipt: %s", b)
	}
	return r
}
func TestSingleExecutionAndReplay(t *testing.T) {
	p, m, c, calls := setup(t)
	for i := 0; i < 3; i++ {
		if e := p.poll(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if *calls != 1 || receipt(t, p, m, c.ID).State != "APPLIED" {
		t.Fatal("not exactly one local dispatch")
	}
}
func TestChangedAuthorityRejected(t *testing.T) {
	p, m, c, calls := setup(t)
	c.Expected.Revision--
	setCommand(t, p, m, c)
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 0 || receipt(t, p, m, c.ID).State != "REJECTED" {
		t.Fatal("stale command dispatched")
	}
}
func TestExpiredRejected(t *testing.T) {
	p, m, c, calls := setup(t)
	c.Created = p.now().Add(-6 * time.Minute).Format(time.RFC3339)
	c.Expires = p.now().Add(-time.Minute).Format(time.RFC3339)
	setCommand(t, p, m, c)
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 0 || receipt(t, p, m, c.ID).State != "REJECTED" {
		t.Fatal("expired command dispatched")
	}
}
func TestCrashClaimBecomesUnknown(t *testing.T) {
	p, m, c, calls := setup(t)
	raw := m.objects[p.prefix()+"commands/B/pending.json"]
	r := Receipt{Schema: 1, ID: c.ID, Target: "B", Hash: hash(raw), State: "CLAIMED"}
	if e := writeJSON(p.local(c.ID), r); e != nil {
		t.Fatal(e)
	}
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 0 || receipt(t, p, m, c.ID).State != "UNKNOWN" {
		t.Fatal("ambiguous command replayed")
	}
}
func TestRemoteClaimWithoutLocalRecordUnknown(t *testing.T) {
	p, m, c, calls := setup(t)
	r := Receipt{Schema: 1, ID: c.ID, Target: "B", Hash: hash(m.objects[p.prefix()+"commands/B/pending.json"]), State: "CLAIMED"}
	m.objects[p.receiptKey(c.ID)], _ = json.Marshal(r)
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 0 || receipt(t, p, m, c.ID).State != "UNKNOWN" {
		t.Fatal("remote claim replayed")
	}
}
func TestReceiptNetworkFailureDoesNotReplaySideEffect(t *testing.T) {
	p, m, c, calls := setup(t)
	m.failFinal = true
	if p.poll(context.Background()) == nil {
		t.Fatal("expected publication failure")
	}
	m.failFinal = false
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 1 || receipt(t, p, m, c.ID).State != "APPLIED" {
		t.Fatal("effect replayed")
	}
}
func TestLostAckReconciles(t *testing.T) {
	p, m, c, calls := setup(t)
	m.loseFinal = true
	if p.poll(context.Background()) == nil {
		t.Fatal("expected lost ack")
	}
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 1 || receipt(t, p, m, c.ID).State != "APPLIED" {
		t.Fatal("lost ack replayed")
	}
}
func TestCancelledReceiptWinsRaceBeforeDispatch(t *testing.T) {
	p, m, c, calls := setup(t)
	m.beforePut = func(k string, b []byte, match string) {
		if k == p.receiptKey(c.ID) && match == "*" {
			m.beforePut = nil
			var r Receipt
			_ = json.Unmarshal(b, &r)
			r.State = "REJECTED"
			m.objects[k], _ = json.Marshal(r)
		}
	}
	if p.poll(context.Background()) == nil {
		t.Fatal("expected race conflict")
	}
	if *calls != 0 {
		t.Fatal("cancel race executed")
	}
}
func TestReusedIDWithDifferentBodyRejected(t *testing.T) {
	p, m, c, calls := setup(t)
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	c.Action = "fence-local"
	setCommand(t, p, m, c)
	if p.poll(context.Background()) == nil || *calls != 1 {
		t.Fatal("conflicting replay accepted")
	}
}
func TestWrongOperationRejected(t *testing.T) {
	p, m, c, calls := setup(t)
	c.Action = "handover-resume"
	setCommand(t, p, m, c)
	p.status = func(context.Context) (Status, error) {
		return Status{Schema: 1, Instance: "B", Cluster: "test", Handover: &Op{ID: "other", Epoch: 1}}, nil
	}
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 0 || receipt(t, p, m, c.ID).State != "REJECTED" {
		t.Fatal("wrong op executed")
	}
}
func TestOnlyKnownCommandsAccepted(t *testing.T) {
	p, m, c, calls := setup(t)
	c.Action = "handover-claim"
	setCommand(t, p, m, c)
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 0 || receipt(t, p, m, c.ID).State != "REJECTED" {
		t.Fatal("internal transition exposed")
	}
}
func TestNoCommandDoesNotStartServices(t *testing.T) {
	p, m, _, calls := setup(t)
	delete(m.objects, p.prefix()+"commands/B/pending.json")
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 0 {
		t.Fatal("idle starts services")
	}
}
func TestMarkerPermissions(t *testing.T) {
	p, _, c, _ := setup(t)
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	st, e := os.Stat(p.local(c.ID))
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal("marker not private")
	}
}

func TestAuthorityChangeBetweenClaimAndDispatch(t *testing.T) {
	p, m, c, calls := setup(t)
	m.beforePut = func(k string, b []byte, match string) {
		if k == p.receiptKey(c.ID) {
			var r Receipt
			_ = json.Unmarshal(b, &r)
			if r.State == "CLAIMED" {
				m.objects[p.prefix()+"control.json"] = []byte(`{"schema":1,"cluster_id":"test","revision":5,"epoch":1,"owner":"A","phase":"QUIESCING"}`)
			}
		}
	}
	if e := p.poll(context.Background()); e != nil {
		t.Fatal(e)
	}
	if *calls != 0 || receipt(t, p, m, c.ID).State != "REJECTED" {
		t.Fatal("authority change dispatched")
	}
}
func TestRunLocalUsesSourceEpochForHandoverResume(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "deploy/control")
	if e := os.MkdirAll(filepath.Join(base, "bin"), 0700); e != nil {
		t.Fatal(e)
	}
	output := filepath.Join(root, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + output + "'\n"
	if e := os.WriteFile(filepath.Join(base, "bin/matrix-control"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	_, run := runLocal(filepath.Join(base, "local.json"))
	if e := run(context.Background(), Command{Action: "handover-resume", Target: "B", Bound: 4, SourceEpoch: 3, Operation: "move-one"}, Status{}); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(output)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(b), "--expected-epoch\n3\n") || !strings.Contains(string(b), "--operation-id\nmove-one\n") {
		t.Fatal(string(b))
	}
}
func TestProbeHasNoSubprocess(t *testing.T) {
	_, run := runLocal("/does-not-exist/local.json")
	if e := run(context.Background(), Command{Action: "probe"}, Status{}); e != nil {
		t.Fatal(e)
	}
}
