package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in integration test: real R2, throwaway containers, and a separate systemd
// unit. The production cluster ID and production containers are never used.
func TestLiveGuard(t *testing.T) {
	credentials := os.Getenv("MATRIX_CONTROL_LIVE_S3")
	if credentials == "" {
		t.Skip("set MATRIX_CONTROL_LIVE_S3 for isolated live test")
	}
	binary := os.Getenv("MATRIX_CONTROL_LIVE_BINARY")
	image := os.Getenv("MATRIX_CONTROL_LIVE_IMAGE")
	if binary == "" || !strings.HasPrefix(image, "sha256:") {
		t.Fatal("explicit test binary and image required")
	}
	id := fmt.Sprintf("guard-probe-%d", time.Now().UnixNano())
	unit := id + ".service"
	root := t.TempDir()
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer health.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	ln.Close()
	cfg := Config{Schema: 1, Cluster: id, Instance: id, S3Config: credentials, StateDir: filepath.Join(root, "state"), RuntimeDir: "/run/" + id, DockerSocket: "/var/run/docker.sock", GateAddress: address, PollSeconds: 1, StaleSeconds: 6}
	for _, suffix := range []string{"matrix", "bridge"} {
		data := filepath.Join(root, suffix)
		if err = os.Mkdir(data, 0755); err != nil {
			t.Fatal(err)
		}
		cfg.Services = append(cfg.Services, Service{Name: id + "-" + suffix, Image: image, DataPath: data, MountPath: "/data", HealthURL: health.URL})
	}
	cfgPath := filepath.Join(root, "local.json")
	if err = atomicJSON(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := newStore(credentials, id)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		// Always stop the transient controller before removing its exact containers.
		exec.Command("systemctl", "stop", unit).Run()
		for _, s := range cfg.Services {
			exec.Command("docker", "rm", "--force", s.Name).Run()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for _, key := range []string{store.key(), "cluster/" + id + "/instances/" + id + ".json"} {
			if _, _, e := store.request(ctx, "DELETE", key, nil, ""); e != nil && !strings.Contains(e.Error(), "missing") {
				t.Errorf("test object cleanup: %s", e)
			}
		}
		exec.Command("systemctl", "reset-failed", unit).Run()
	}
	defer cleanup()
	command := func(name string, args ...string) []byte {
		t.Helper()
		b, e := exec.Command(name, args...).CombinedOutput()
		if e != nil {
			t.Fatalf("%s failed: %s\n%s", name, e, b)
		}
		return b
	}
	for _, s := range cfg.Services {
		command("docker", "create", "--name", s.Name, "--restart", "no", "--user", "0", "--mount", "type=bind,src="+s.DataPath+",dst=/data", "--entrypoint", "/bin/sh", image, "-c", "trap 'exit 0' TERM INT; while :; do sleep 1; done")
		command("docker", "start", s.Name)
	}
	command(binary, "adopt-existing", "--config", cfgPath, "--expected-instance", id)
	command("systemd-run", "--unit", unit, "--service-type=notify", "--property=Restart=on-failure", "--property=RestartSec=2", "--property=WatchdogSec=30", "--property=TimeoutStopSec=120", "--property=RuntimeDirectory="+id, "--property=ExecStopPost="+binary+" emergency-stop --config "+cfgPath, binary, "daemon", "--config", cfgPath)
	query := func() map[string]any {
		b, e := exec.Command(binary, "status", "--config", cfgPath).Output()
		if e != nil {
			return nil
		}
		var v map[string]any
		json.Unmarshal(b, &v)
		return v
	}
	wait := func(label string, condition func() bool) {
		t.Helper()
		until := time.Now().Add(45 * time.Second)
		for time.Now().Before(until) {
			if condition() {
				t.Log("PASS:", label)
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatalf("timeout: %s, status=%v", label, query())
	}
	serving := func() bool { v := query(); return v != nil && v["serving"] == true }
	d := newDocker(cfg.DockerSocket)
	allStopped := func() bool {
		for _, s := range cfg.Services {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			running, e := d.Inspect(ctx, s)
			cancel()
			if e != nil || running {
				return false
			}
		}
		return true
	}
	wait("fresh authority and healthy services open gate", serving)
	command(binary, "fence-local", "--config", cfgPath, "--expected-instance", id)
	wait("manual fence stops both test services", allStopped)
	command("systemctl", "restart", unit)
	wait("persistent fence remains after controller restart", func() bool {
		v := query()
		return v != nil && v["blocked"] == true && v["serving"] == false && allStopped()
	})
	if e := exec.Command(binary, "resume-local", "--config", cfgPath, "--expected-instance", id, "--expected-epoch", "2").Run(); e == nil {
		t.Fatal("wrong epoch accepted")
	}
	command(binary, "resume-local", "--config", cfgPath, "--expected-instance", id, "--expected-epoch", "1")
	wait("exact-epoch resume restarts services", serving)
	// Kill main process, exercising systemd ExecStopPost then fresh authorization.
	pidBefore := string(command("systemctl", "show", "--property=MainPID", "--value", unit))
	command("systemctl", "kill", "--kill-whom=main", "--signal=KILL", unit)
	wait("crashed controller restarts after fresh authorization", func() bool {
		pid := string(command("systemctl", "show", "--property=MainPID", "--value", unit))
		return pid != "0\n" && pid != pidBefore && serving()
	})
	// A malformed control object exercises prolonged unknown authority. Restoring
	// the same object must not silently clear the persisted local stale fence.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	state, etag, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.request(ctx, "PUT", store.key(), []byte("not-json"), etag); err != nil {
		t.Fatal(err)
	}
	wait("unknown authority expires and stops services", func() bool { v := query(); return v != nil && v["blocked"] == true && allStopped() })
	_, brokenETag, err := store.request(ctx, "GET", store.key(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Write(ctx, state, brokenETag); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if serving() || !allStopped() {
		t.Fatal("unknown authority fence automatically cleared")
	}
	command(binary, "resume-local", "--config", cfgPath, "--expected-instance", id, "--expected-epoch", "1")
	wait("explicit recovery from unknown authority", serving)
	finalID := "final-" + id
	finalCommand := func(action string) {
		command(binary, "final-"+action, "--config", cfgPath, "--expected-instance", id, "--expected-epoch", "1", "--operation-id", finalID)
	}
	finalCommand("begin")
	if serving() {
		t.Fatal("final begin left ingress open")
	}
	command("systemctl", "restart", unit)
	wait("final hold persists across controller restart", func() bool { v := query(); return v != nil && v["final_backup"] != nil && !serving() && allStopped() })
	finalCommand("stop")
	wait("final writer reconciliation finished", func() bool { v := query(); return v != nil && v["action"] == "" && allStopped() })
	finalCommand("frozen")
	finalCommand("snapshot")
	finalCommand("verify")
	v := query()
	opBytes, _ := json.Marshal(v["final_backup"])
	var finalOp FinalOperation
	json.Unmarshal(opBytes, &finalOp)
	receipt := FinalReceipt{Schema: 1, Operation: finalID, Cluster: id, Instance: id, Epoch: 1, Cutoff: finalOp.Cutoff, Snapshot: strings.Repeat("c", 64), PointHash: strings.Repeat("d", 64), PointKey: "cluster/" + id + "/recovery/v2/points/" + strings.Repeat("c", 64) + ".json", Verified: true, StartupVerified: true, VerifiedAt: time.Now().UTC().Format(time.RFC3339)}
	if e := atomicJSON(filepath.Join(cfg.StateDir, "final", finalID, "receipt.json"), receipt); e != nil {
		t.Fatal(e)
	}
	finalCommand("complete")
	if serving() || !allStopped() {
		t.Fatal("final completion started production identity")
	}
	finalCommand("resume")
	wait("explicit final resume reopens only original owner", serving)
	state, etag, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = id + "-other"
	state.Epoch++
	state.Revision++
	if err = store.Write(ctx, state, etag); err != nil {
		t.Fatal(err)
	}
	wait("ownership change closes gate and stops services", func() bool { return !serving() && allStopped() })
	if e := exec.Command(binary, "resume-local", "--config", cfgPath, "--expected-instance", id, "--expected-epoch", "2").Run(); e == nil {
		t.Fatal("old data resumed under new epoch")
	}
	if _, _, err = store.request(ctx, "DELETE", store.key(), nil, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if _, _, err = store.Read(ctx); err != errMissing {
		t.Fatal("missing control object automatically recreated", err)
	}
	if !allStopped() {
		t.Fatal("services started without control state")
	}
	t.Log("PASS: missing state never initializes an empty cluster")
}
