package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real conditional R2 writes and four throwaway containers. The databases and
// probe report are fixtures; real database restore is a separate S3 acceptance.
func TestLivePlannedHandover(t *testing.T) {
	credentials := os.Getenv("MATRIX_CONTROL_LIVE_S3")
	if credentials == "" {
		t.Skip("opt-in real R2/Docker test")
	}
	image := os.Getenv("MATRIX_CONTROL_LIVE_IMAGE")
	if !strings.HasPrefix(image, "sha256:") {
		t.Fatal("test image required")
	}
	a, b, _, _, _, root := pairFixture(t)
	id := fmt.Sprintf("handover-probe-%d", time.Now().UnixNano())
	store, err := newStore(credentials, id)
	if err != nil {
		t.Fatal(err)
	}
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer health.Close()
	names := []string{}
	for _, c := range []*Controller{a, b} {
		c.cfg.Cluster = id
		c.journal.Cluster = id
		c.store = store
		c.docker = newDocker("/var/run/docker.sock")
		for i := range c.cfg.Services {
			s := &c.cfg.Services[i]
			s.Name = fmt.Sprintf("%s-%s-%d", id, c.cfg.Instance, i)
			s.Image = image
			s.MountPath = "/data"
			s.HealthURL = health.URL
			s.DataPath = filepath.Join(c.cfg.StateDir, fmt.Sprintf("old-%d", i))
			if err = os.MkdirAll(s.DataPath, 0755); err != nil {
				t.Fatal(err)
			}
			names = append(names, s.Name)
		}
		if err = c.saveLocked(); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	defer func() {
		for _, name := range names {
			exec.Command("docker", "rm", "-f", name).Run()
		}
		for _, key := range []string{store.key(), "cluster/" + id + "/instances/a.json", "cluster/" + id + "/instances/b.json"} {
			if _, _, e := store.request(context.Background(), "DELETE", key, nil, ""); e != nil && !strings.Contains(e.Error(), "missing") {
				t.Errorf("test object cleanup: %v", e)
			}
		}
	}()
	run := func(name string, args ...string) {
		t.Helper()
		if out, e := exec.Command(name, args...).CombinedOutput(); e != nil {
			t.Fatalf("%s: %v %s", name, e, out)
		}
	}
	create := func(c *Controller) {
		for _, s := range c.services() {
			run("docker", "create", "--name", s.Name, "--restart", "no", "--user", "0", "--mount", "type=bind,src="+s.DataPath+",dst=/data", "--entrypoint", "/bin/sh", image, "-c", "trap 'exit 0' TERM INT; while :; do sleep 1; done")
		}
	}
	initial := State{Schema: 1, ClusterID: id, Revision: 4, Epoch: 2, Owner: "a", Phase: "ONLINE", Operation: json.RawMessage("null")}
	if err = store.Write(ctx, initial, "*"); err != nil {
		t.Fatal(err)
	}
	a.observe(initial, nil)
	create(a)
	a.runServices()
	if gateCode(a) != 204 {
		t.Fatal("source not online")
	}
	if err = hcall(b, "request"); err != nil {
		t.Fatal(err)
	}
	a.tick(ctx)
	if gateCode(a) != 403 {
		t.Fatal("request did not close source ingress")
	}
	for _, s := range a.services() {
		running, e := a.docker.Inspect(ctx, s)
		if e != nil || !running {
			t.Fatal("source stopped before claim/drain")
		}
	}
	for _, action := range []string{"begin", "stop"} {
		if err = callFinal(a, action); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.stopServices(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"frozen", "snapshot"} {
		if err = callFinal(a, action); err != nil {
			t.Fatal(err)
		}
	}
	point := map[string]any{"protocol": 2, "point_schema": 2, "snapshot_id": strings.Repeat("a", 64), "image_id": image, "telegram": map[string]string{"image_id": image}, "operation_id": "test-final", "consistency": "globally-quiesced", "instance_control": map[string]any{"cluster_id": id, "source_instance_id": "a", "source_epoch": 2}}
	dir := filepath.Join(b.cfg.StateDir, "handover/test-final")
	if err = atomicJSON(filepath.Join(dir, "point.json"), point); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "point.json"))
	receipt := receiptFor(a)
	receipt.Cluster = id
	receipt.PointHash = digest(data)
	putReceipt(t, a, receipt)
	if err = callFinal(a, "complete"); err != nil {
		t.Fatal(err)
	}
	if hcall(b, "claim") == nil {
		t.Fatal("target claimed before release")
	}
	if err = hcall(a, "release"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"claim", "validating"} {
		if err = hcall(b, action); err != nil {
			t.Fatal(err)
		}
	}
	if err = atomicJSON(filepath.Join(dir, "deployment.json"), map[string]any{"schema": 1, "operation_id": "test-final", "instance_id": "b", "epoch": 3, "deployment_root": root}); err != nil {
		t.Fatal(err)
	}
	if err = hcall(b, "bind"); err != nil {
		t.Fatal(err)
	}
	create(b)
	if err = hcall(b, "start"); err != nil {
		t.Fatal(err)
	}
	// Rebuild the in-memory controller solely from the durable STARTING journal.
	journal, err := readJournal(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	b = &Controller{cfg: b.cfg, store: store, docker: b.docker, journal: journal, wake: make(chan struct{}, 1), now: time.Now}
	b.runServices()
	if !b.ready || gateCode(b) != 403 {
		t.Fatal("restart lost STARTING intent or opened gate")
	}
	if err = hcall(b, "online"); err != nil {
		t.Fatal(err)
	}
	b.runServices()
	if gateCode(b) != 204 {
		t.Fatal("target not online")
	}
	if callFinal(a, "resume") == nil {
		t.Fatal("retired source resumed")
	}
	for _, s := range a.services() {
		running, e := a.docker.Inspect(ctx, s)
		if e != nil || running {
			t.Fatal("source writer remained running")
		}
	}
	current, _, err := store.Read(ctx)
	if err != nil || current.Owner != "b" || current.Epoch != 3 || current.Completed == nil {
		t.Fatal("invalid final ownership", err)
	}
	t.Log("PASS: real R2/Docker planned release -> one epoch claim -> durable STARTING restart -> ONLINE, source stays stopped")
}
