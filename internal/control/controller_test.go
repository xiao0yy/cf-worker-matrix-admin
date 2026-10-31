package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeAuthority struct {
	mu    sync.Mutex
	state State
	err   error
}

func (f *fakeAuthority) Read(context.Context) (State, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, "\"etag\"", f.err
}
func (f *fakeAuthority) Write(context.Context, State, string) error   { return nil }
func (f *fakeAuthority) Report(context.Context, string, []byte) error { return nil }

type fakeDocker struct {
	mu         sync.Mutex
	running    map[string]bool
	actions    []string
	afterStart func()
}

func (f *fakeDocker) Inspect(_ context.Context, s Service) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[s.Name], nil
}
func (f *fakeDocker) Start(_ context.Context, s Service) error {
	f.mu.Lock()
	f.actions = append(f.actions, "start:"+s.Name)
	f.running[s.Name] = true
	f.mu.Unlock()
	if f.afterStart != nil {
		f.afterStart()
	}
	return nil
}
func (f *fakeDocker) Stop(_ context.Context, s Service) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, "stop:"+s.Name)
	f.running[s.Name] = false
	return nil
}
func (f *fakeDocker) Healthy(context.Context, Service) error { return nil }
func fixture(t *testing.T) (*Controller, *fakeAuthority, *fakeDocker, *time.Time) {
	t.Helper()
	now := time.Now()
	s := State{Schema: 1, ClusterID: "test", Revision: 4, Epoch: 2, Owner: "a", Phase: "ONLINE", Operation: json.RawMessage("null")}
	a := &fakeAuthority{state: s}
	d := &fakeDocker{running: map[string]bool{}}
	cfg := Config{Cluster: "test", Instance: "a", StateDir: t.TempDir(), StaleSeconds: 90, Services: []Service{{Name: "matrix"}, {Name: "bridge"}}}
	c := &Controller{cfg: cfg, store: a, docker: d, journal: Journal{Schema: 1, Cluster: "test", Instance: "a", BoundEpoch: 2, HighEpoch: 2, HighRevision: 4}, wake: make(chan struct{}, 1), now: func() time.Time { return now }}
	if err := c.saveLocked(); err != nil {
		t.Fatal(err)
	}
	return c, a, d, &now
}
func gateCode(c *Controller) int {
	w := httptest.NewRecorder()
	c.gate(w, httptest.NewRequest("GET", "/authorize", nil))
	return w.Code
}
func TestColdStartNeverUsesPersistedGrant(t *testing.T) {
	c, _, _, _ := fixture(t)
	c.ready = true
	if gateCode(c) != 403 {
		t.Fatal("cold boot accepted a persisted grant")
	}
}
func TestStaleStateClosesGateAndLatchesFence(t *testing.T) {
	c, a, _, now := fixture(t)
	c.observe(a.state, nil)
	c.ready = true
	if gateCode(c) != 204 {
		t.Fatal("healthy owner denied")
	}
	*now = now.Add(91 * time.Second)
	if gateCode(c) != 403 {
		t.Fatal("expired authorization still serving")
	}
	c.observe(State{}, errors.New("R2 down"))
	if !c.journal.Blocked {
		t.Fatal("stale state did not latch")
	}
	c.observe(a.state, nil)
	if gateCode(c) != 403 {
		t.Fatal("reconnected storage silently resumed")
	}
}
func TestOwnershipLossAndNewEpochNeverStartOldData(t *testing.T) {
	for _, change := range []string{"owner", "epoch", "phase"} {
		t.Run(change, func(t *testing.T) {
			c, a, d, _ := fixture(t)
			c.observe(a.state, nil)
			c.ready = true
			s := a.state
			s.Revision++
			switch change {
			case "owner":
				s.Owner = "b"
			case "epoch":
				s.Epoch++
			case "phase":
				s.Phase = "RESTORING"
			}
			c.observe(s, nil)
			if gateCode(c) != 403 || !c.journal.Blocked {
				t.Fatal("invalid grant was accepted")
			}
			a.state = s
			c.runServices()
			if len(d.actions) != 0 {
				t.Fatal("started old data without authorization")
			}
		})
	}
}
func TestStartOrderAndRevocationBetweenServices(t *testing.T) {
	c, a, d, _ := fixture(t)
	c.observe(a.state, nil)
	d.afterStart = func() { a.mu.Lock(); a.state.Owner = "b"; a.state.Revision++; a.mu.Unlock() }
	c.runServices()
	if len(d.actions) != 1 || d.actions[0] != "start:matrix" || gateCode(c) != 403 {
		t.Fatal("bridge started after grant revoked", d.actions)
	}
	if err := c.stopServices(); err != nil {
		t.Fatal(err)
	}
	if d.actions[1] != "stop:bridge" || d.actions[2] != "stop:matrix" {
		t.Fatal("incorrect shutdown order", d.actions)
	}
}
func TestResumeRequiresExactIdentityAndBoundEpoch(t *testing.T) {
	c, a, _, _ := fixture(t)
	if err := c.fence("manual"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		id string
		e  uint64
	}{{"b", 2}, {"a", 1}, {"a", 3}} {
		if c.resume(context.Background(), v.id, v.e) == nil {
			t.Fatal("unsafe resume accepted")
		}
	}
	if err := c.resume(context.Background(), "a", 2); err != nil {
		t.Fatal(err)
	}
	c.runServices()
	if gateCode(c) != 204 {
		t.Fatal("valid resume denied")
	}
	a.state.Epoch = 3
	a.state.Revision++
	if c.resume(context.Background(), "a", 3) == nil {
		t.Fatal("new epoch adopted without restoring data")
	}
}
func TestManualFenceSurvivesRestart(t *testing.T) {
	c, _, _, _ := fixture(t)
	if err := c.fence("manual"); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(c.cfg)
	if err != nil || !j.Blocked {
		t.Fatal("manual fence lost after restart", err)
	}
}
func TestRegressionAndPersistenceFailureDenyGrant(t *testing.T) {
	c, a, _, _ := fixture(t)
	s := a.state
	s.Revision--
	c.observe(s, nil)
	if !c.journal.Blocked || gateCode(c) != 403 {
		t.Fatal("state regression accepted")
	}
	c, a, _, _ = fixture(t)
	if err := os.RemoveAll(c.cfg.StateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.cfg.StateDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	c.observe(a.state, nil)
	if c.grantLocked() {
		t.Fatal("authorization granted without durable journal")
	}
}
func TestInvalidStateAndPrivateConfig(t *testing.T) {
	s := State{Schema: 1, ClusterID: "x", Revision: 1, Epoch: 1, Owner: "a", Phase: "ONLINE", Operation: json.RawMessage("{}")}
	if s.validate("x") == nil {
		t.Fatal("ONLINE with operation accepted")
	}
	p := filepath.Join(t.TempDir(), "s3.env")
	os.WriteFile(p, []byte("AWS_SECRET_ACCESS_KEY='do-not-leak'"), 0644)
	_, err := newStore(p, "x")
	if err == nil {
		t.Fatal("public credentials accepted")
	}
}

func TestStandbyJournalHasNoDataRunGrant(t *testing.T) {
	c, a, d, _ := fixture(t)
	c.journal.BoundEpoch = 0
	c.journal.Blocked = true
	c.journal.Reason = "standby"
	if err := c.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(c.cfg); err != nil {
		t.Fatal(err)
	}
	c.observe(a.state, nil)
	c.runServices()
	if len(d.actions) != 0 || gateCode(c) != 403 {
		t.Fatal("empty standby started application")
	}
}

func TestR2InputClusterMustMatchController(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s3.env")
	input := "S3_ENDPOINT=https://fixture.r2.cloudflarestorage.com\nS3_REGION=auto\nS3_BUCKET=example\nAWS_ACCESS_KEY_ID=fixture\nAWS_SECRET_ACCESS_KEY=fixture\nCLUSTER_ID=test\n"
	if e := os.WriteFile(p, []byte(input), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := newStore(p, "test"); e != nil {
		t.Fatal(e)
	}
	if _, e := newStore(p, "other"); e == nil {
		t.Fatal("accepted R2 input for another cluster")
	}
}
