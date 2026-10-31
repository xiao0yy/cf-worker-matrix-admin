package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pairFixture(t *testing.T) (*Controller, *Controller, *finalAuthority, *finalDocker, *finalDocker, string) {
	a, s, da := finalFixture(t)
	a.cfg.Services[0].Image = "sha256:" + strings.Repeat("1", 64)
	a.cfg.Services[1].Image = "sha256:" + strings.Repeat("2", 64)
	bcfg := a.cfg
	bcfg.Services = append([]Service(nil), a.cfg.Services...)
	bcfg.Instance = "b"
	bcfg.StateDir = t.TempDir()
	bcfg.Generations = t.TempDir()
	db := &finalDocker{fakeDocker: &fakeDocker{running: map[string]bool{}}, clean: true}
	b := &Controller{cfg: bcfg, store: s, docker: db, journal: Journal{Schema: 1, Cluster: "test", Instance: "b", HighEpoch: 2, HighRevision: 4, Blocked: true}, wake: make(chan struct{}, 1), now: a.now}
	if e := b.saveLocked(); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(bcfg.Generations, "generation", "rebuilt")
	for p, v := range map[string]string{"deploy/tuwunel/data/CURRENT": "MANIFEST-1", "deploy/telegram/data/telegram.db": "sqlite-fixture"} {
		path := filepath.Join(root, p)
		if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, []byte(v), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := atomicJSON(filepath.Join(root, "recovery-state/result.json"), map[string]any{"database_startup_verified": true, "telegram_database_startup_verified": true, "production_started": false, "consistency": "globally-quiesced"}); e != nil {
		t.Fatal(e)
	}
	return a, b, s, da, db, root
}
func hcall(c *Controller, action string) error {
	return c.handoverAction(context.Background(), "/handover-"+action, c.cfg.Instance, 2, "test-final")
}
func makePairReady(t *testing.T, a, b *Controller, root string) {
	t.Helper()
	if e := hcall(b, "request"); e != nil {
		t.Fatal(e)
	}
	toSnapshot(t, a)
	point := map[string]any{"protocol": 2, "point_schema": 2, "snapshot_id": strings.Repeat("a", 64), "image_id": a.cfg.Services[0].Image, "telegram": map[string]string{"image_id": a.cfg.Services[1].Image}, "operation_id": "test-final", "consistency": "globally-quiesced", "instance_control": map[string]any{"cluster_id": "test", "source_instance_id": "a", "source_epoch": 2}}
	dir := filepath.Join(b.cfg.StateDir, "handover", "test-final")
	if e := atomicJSON(filepath.Join(dir, "point.json"), point); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "point.json"))
	r := receiptFor(a)
	r.PointHash = digest(data)
	putReceipt(t, a, r)
	if e := callFinal(a, "complete"); e != nil {
		t.Fatal(e)
	}
	if e := atomicJSON(filepath.Join(dir, "deployment.json"), map[string]any{"schema": 1, "deployment_root": root, "operation_id": "test-final", "instance_id": "b", "epoch": 3}); e != nil {
		t.Fatal(e)
	}
}
func TestPlannedHandoverHasExactlyOneWriterAndAClosedGateDuringStartup(t *testing.T) {
	a, b, s, da, db, root := pairFixture(t)
	makePairReady(t, a, b, root)
	if hcall(b, "claim") == nil {
		t.Fatal("target claimed before source release")
	}
	if hcall(b, "online") == nil {
		t.Fatal("target opened without a bound generation")
	}
	if e := hcall(a, "release"); e != nil {
		t.Fatal(e)
	}
	if s.state.Owner != "" || s.state.Phase != "RELEASED" {
		t.Fatal("source did not release ownership")
	}
	if callFinal(a, "resume") == nil || a.resume(context.Background(), "a", 2) == nil {
		t.Fatal("retired source resumed")
	}
	for _, action := range []string{"claim", "validating", "bind", "start"} {
		if e := hcall(b, action); e != nil {
			t.Fatalf("%s: %v", action, e)
		}
	}
	b.runServices()
	if gateCode(b) != 403 || !b.ready || s.state.Epoch != 3 {
		t.Fatal("STARTING opened ingress or wrong epoch")
	}
	if hcall(b, "bind") == nil || hcall(b, "validating") == nil {
		t.Fatal("could restore after possible writes")
	}
	if e := hcall(b, "online"); e != nil {
		t.Fatal(e)
	}
	b.runServices()
	if gateCode(b) != 204 || s.state.Owner != "b" || s.state.Completed == nil || s.state.Completed.ID != "test-final" {
		t.Fatal("target admission incomplete")
	}
	for _, v := range da.running {
		if v {
			t.Fatal("source still writing")
		}
	}
	for _, v := range db.running {
		if !v {
			t.Fatal("target not running")
		}
	}
}
func TestPlannedHandoverLostAcknowledgmentsDoNotCreateAnotherEpoch(t *testing.T) {
	for _, step := range []string{"request", "release", "claim", "validating", "start", "online"} {
		t.Run(step, func(t *testing.T) {
			a, b, s, _, _, root := pairFixture(t)
			if step == "request" {
				s.loseNext = true
				if hcall(b, "request") == nil {
					t.Fatal("missing fault")
				}
				writes := s.writes
				if e := hcall(b, "request"); e != nil || s.writes != writes {
					t.Fatal("request replayed CAS", e)
				}
			}
			makePairReady(t, a, b, root)
			for _, action := range []string{"release", "claim", "validating", "bind", "start", "online"} {
				actor := b
				if action == "release" {
					actor = a
				}
				if action == "online" {
					b.runServices()
				}
				if action == step {
					s.loseNext = true
				}
				e := hcall(actor, action)
				if action == step {
					if e == nil {
						t.Fatal("missing fault")
					}
					writes := s.writes
					if e = hcall(actor, action); e != nil || s.writes != writes {
						t.Fatal("lost ack advanced state twice", e)
					}
				} else if e != nil {
					t.Fatalf("%s: %v", action, e)
				}
			}
			if s.state.Epoch != 3 || s.state.Owner != "b" {
				t.Fatal("epoch or ownership changed unexpectedly")
			}
		})
	}
}
func TestHandoverRejectsWrongGenerationAndStaleResume(t *testing.T) {
	a, b, _, _, _, root := pairFixture(t)
	makePairReady(t, a, b, root)
	for _, action := range []string{"release", "claim", "validating"} {
		actor := b
		if action == "release" {
			actor = a
		}
		if e := hcall(actor, action); e != nil {
			t.Fatal(e)
		}
	}
	dir := filepath.Join(b.cfg.StateDir, "handover", "test-final")
	raw, _ := os.ReadFile(filepath.Join(dir, "point.json"))
	os.WriteFile(filepath.Join(dir, "point.json"), append(raw, ' '), 0600)
	if hcall(b, "bind") == nil {
		t.Fatal("mismatched point accepted")
	}
	os.WriteFile(filepath.Join(dir, "point.json"), raw, 0600)
	if e := hcall(b, "bind"); e != nil {
		t.Fatal(e)
	}
	if e := hcall(b, "start"); e != nil {
		t.Fatal(e)
	}
	b.fence("manual operator fence")
	b.runServices()
	if b.ready || gateCode(b) != 403 {
		t.Fatal("manual fence bypassed")
	}
	if hcall(b, "online") == nil {
		t.Fatal("fenced target committed ONLINE")
	}
	if e := hcall(b, "resume"); e != nil {
		t.Fatal(e)
	}
	b.runServices()
	if e := hcall(b, "online"); e != nil {
		t.Fatal(e)
	}
	if _, e := readJournal(b.cfg); e != nil {
		t.Fatal(e)
	}
}
func TestHandoverCancelRequiresSourceToRemainUnclaimed(t *testing.T) {
	_, b, s, _, _, _ := pairFixture(t)
	if e := hcall(b, "request"); e != nil {
		t.Fatal(e)
	}
	if e := hcall(b, "cancel"); e != nil {
		t.Fatal(e)
	}
	if s.state.Phase != "ONLINE" || s.state.Owner != "a" {
		t.Fatal("cancel changed owner")
	}
	if hcall(b, "request") == nil {
		t.Fatal("closed request ID reused")
	}
}
