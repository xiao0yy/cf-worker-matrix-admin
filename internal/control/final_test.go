package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type finalAuthority struct {
	mu       sync.Mutex
	state    State
	writes   int
	loseNext bool
}

func (f *finalAuthority) Read(context.Context) (State, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, fmt.Sprintf("\"%d\"", f.state.Revision), nil
}
func (f *finalAuthority) Write(_ context.Context, s State, etag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if etag != fmt.Sprintf("\"%d\"", f.state.Revision) {
		return errConflict
	}
	f.state = s
	f.writes++
	if f.loseNext {
		f.loseNext = false
		return errors.New("lost acknowledgment")
	}
	return nil
}
func (f *finalAuthority) Report(context.Context, string, []byte) error { return nil }

type finalDocker struct {
	*fakeDocker
	clean bool
}

func (f *finalDocker) Stopped(ctx context.Context, s Service) (StopProof, error) {
	running, _ := f.Inspect(ctx, s)
	if running || !f.clean {
		return StopProof{}, errors.New("not cleanly stopped")
	}
	return StopProof{Name: s.Name, ContainerID: s.Name + "-id", FinishedAt: "2026-10-25T07:00:00Z"}, nil
}
func finalFixture(t *testing.T) (*Controller, *finalAuthority, *finalDocker) {
	c, a, d, _ := fixture(t)
	store := &finalAuthority{state: a.state}
	host := &finalDocker{fakeDocker: d, clean: true}
	c.store = store
	c.docker = host
	c.observe(a.state, nil)
	c.ready = true
	return c, store, host
}
func callFinal(c *Controller, action string) error {
	return c.finalAction(context.Background(), "/final-"+action, "a", 2, "test-final")
}
func toSnapshot(t *testing.T, c *Controller) {
	t.Helper()
	for _, a := range []string{"begin", "stop"} {
		if e := callFinal(c, a); e != nil {
			t.Fatal(e)
		}
	}
	if e := c.stopServices(); e != nil {
		t.Fatal(e)
	}
	for _, a := range []string{"frozen", "snapshot"} {
		if e := callFinal(c, a); e != nil {
			t.Fatal(e)
		}
	}
}
func receiptFor(c *Controller) FinalReceipt {
	return FinalReceipt{Schema: 1, Operation: "test-final", Cluster: "test", Instance: "a", Epoch: 2, Cutoff: c.journal.Final.Cutoff, Snapshot: strings.Repeat("a", 64), PointHash: strings.Repeat("b", 64), PointKey: "cluster/" + "test" + "/recovery/v2/points/" + strings.Repeat("a", 64) + ".json", Verified: true, StartupVerified: true, VerifiedAt: time.Now().UTC().Format(time.RFC3339)}
}
func putReceipt(t *testing.T, c *Controller, r FinalReceipt) {
	t.Helper()
	if e := atomicJSON(filepath.Join(c.cfg.StateDir, "final", "test-final", "receipt.json"), r); e != nil {
		t.Fatal(e)
	}
}
func TestFinalCloseBeforeStopAndIdempotentBegin(t *testing.T) {
	c, s, d := finalFixture(t)
	if e := callFinal(c, "begin"); e != nil {
		t.Fatal(e)
	}
	if gateCode(c) != 403 || c.journal.Final.Phase != "DRAINING" || s.state.Phase != "QUIESCING" {
		t.Fatal("begin did not fence ingress")
	}
	c.schedule(true)
	if len(d.actions) != 0 {
		t.Fatal("writers stopped before producers drained")
	}
	writes := s.writes
	if e := callFinal(c, "begin"); e != nil || s.writes != writes {
		t.Fatal("duplicate begin changed operation", e)
	}
	if c.finalAction(context.Background(), "/final-begin", "a", 2, "conflict") == nil {
		t.Fatal("concurrent operation accepted")
	}
	if callFinal(c, "snapshot") == nil {
		t.Fatal("snapshot allowed before clean stop")
	}
	if c.resume(context.Background(), "a", 2) == nil {
		t.Fatal("normal resume bypassed final hold")
	}
}
func TestFinalRejectsUncleanStopAndBadReceipt(t *testing.T) {
	c, s, d := finalFixture(t)
	callFinal(c, "begin")
	callFinal(c, "stop")
	c.stopServices()
	d.clean = false
	if callFinal(c, "frozen") == nil || s.state.Phase != "QUIESCING" {
		t.Fatal("unclean shutdown accepted")
	}
	d.clean = true
	if e := callFinal(c, "frozen"); e != nil {
		t.Fatal(e)
	}
	if e := callFinal(c, "snapshot"); e != nil {
		t.Fatal(e)
	}
	r := receiptFor(c)
	r.Epoch = 3
	putReceipt(t, c, r)
	if callFinal(c, "complete") == nil || s.state.Phase != "SNAPSHOTTING" {
		t.Fatal("foreign receipt accepted")
	}
	r = receiptFor(c)
	r.PointKey = "matrix-recovery/v1/points/" + r.Snapshot + ".json"
	putReceipt(t, c, r)
	if callFinal(c, "complete") == nil {
		t.Fatal("old data namespace accepted by v2 controller")
	}
	putReceipt(t, c, receiptFor(c))
	if e := callFinal(c, "complete"); e != nil {
		t.Fatal(e)
	}
	if c.journal.Final.Phase != "READY" || gateCode(c) != 403 || s.state.Owner != "a" || s.state.Epoch != 2 {
		t.Fatal("final set reopened or transferred source")
	}
}
func TestFinalResumeWaitsForWorkerAndInvalidatesSet(t *testing.T) {
	c, s, _ := finalFixture(t)
	toSnapshot(t, c)
	putReceipt(t, c, receiptFor(c))
	callFinal(c, "complete")
	p := filepath.Join(c.cfg.StateDir, "final", "test-final", "worker.lock")
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	if callFinal(c, "resume") == nil {
		t.Fatal("resumed while worker could publish")
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if e = callFinal(c, "resume"); e != nil {
		t.Fatal(e)
	}
	if s.state.Phase != "ONLINE" || s.state.Epoch != 2 || c.journal.Final != nil {
		t.Fatal("bad resume transition")
	}
	writes := s.writes
	if e = callFinal(c, "resume"); e != nil || s.writes != writes {
		t.Fatal("resume retry is not idempotent", e)
	}
	if callFinal(c, "begin") == nil {
		t.Fatal("closed operation ID reused")
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "closed.json"))
	var v map[string]any
	json.Unmarshal(b, &v)
	if v["recovery_set_valid_for_handover"] != false {
		t.Fatal("resumed set still eligible for handover")
	}
}
func TestFinalLostAcknowledgmentReconcilesExactOperation(t *testing.T) {
	for _, step := range []string{"begin", "frozen", "snapshot", "complete", "resume"} {
		t.Run(step, func(t *testing.T) {
			c, s, _ := finalFixture(t)
			actions := []string{"begin", "stop", "frozen", "snapshot", "complete", "resume"}
			for _, a := range actions {
				if a == "frozen" {
					c.stopServices()
				}
				if a == "complete" {
					putReceipt(t, c, receiptFor(c))
				}
				if a == step {
					s.loseNext = true
				}
				e := callFinal(c, a)
				if a == step {
					if e == nil {
						t.Fatal("fault not injected")
					}
					writes := s.writes
					if e = callFinal(c, a); e != nil || s.writes != writes {
						t.Fatal("lost ack replayed a write", e)
					}
				} else if e != nil {
					t.Fatal(a, e)
				}
			}
		})
	}
}
