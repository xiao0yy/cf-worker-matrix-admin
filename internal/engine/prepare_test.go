package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/payload"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
)

// Run the public prepare entry point with an actual configuration capsule,
// persistent files and Unix HTTP controller endpoint. Only host commands and
// object storage are fixtures; no production service or credentials are used.
func preparationFixture(t *testing.T) *Engine {
	t.Helper()
	e := testEngine(t)
	e.ReleaseDir = runtimeFixture(t, "0.3.0")
	e.Host.NginxDirectory, e.Host.SystemdDirectory, e.Host.RuntimeDirectory = t.TempDir(), t.TempDir(), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(e.Host.Root, "agent"), 0700))
	write(t, filepath.Join(e.Host.Root, "config/s3.env"), "test-only-credentials")
	d, err := release.Describe(e.ReleaseDir)
	must(t, err)
	must(t, local.CopyTree(e.ReleaseDir, filepath.Join(e.Host.Root, "agent/cache/releases", d.SHA)))
	pending := t.TempDir()
	bundle, m := bundleFixture(t, pending)
	m.Software = d
	cfg := control.Config{Schema: 1, Cluster: e.Cluster, Instance: "node-a", S3Config: "/source/deploy/control/s3.env", GateAddress: "127.0.0.1:29318", Services: []control.Service{{Name: "tuwunel"}, {Name: "telegram-bridge"}}}
	must(t, local.JSON(filepath.Join(bundle, "payload/control/source-local.json"), cfg))
	saveBundle(t, bundle, m)
	must(t, payload.Pack(bundle, filepath.Join(pending, "configuration.json")))
	e.Store = &memoryObjects{data: map[string][]byte{}}
	source := testEngine(t)
	source.Store, source.ReleaseDir = e.Store, e.ReleaseDir
	fakeController(t, source, &Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 1, Serving: true, Control: &control.State{Owner: "node-a", Epoch: 1, Phase: "ONLINE"}}, nil)
	must(t, local.CopyTree(e.ReleaseDir, filepath.Join(source.Host.Root, "agent/cache/releases", d.SHA)))
	_, _, err = source.publish(context.Background(), m, pending, BackupJob{Snapshot: strings.Repeat("a", 64), Repository: Repository{Schema: 1, ID: strings.Repeat("d", 64)}, IndexMatch: "*", Verified: true, VerifiedAt: e.stamp()})
	must(t, err)
	ln, err := net.Listen("unix", filepath.Join(e.Host.RuntimeDirectory, "admin.sock"))
	must(t, err)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var cfg control.Config
		if local.ReadJSON(e.controlConfig(), &cfg) != nil {
			http.Error(w, "not prepared", 503)
			return
		}
		json.NewEncoder(w).Encode(Status{Cluster: cfg.Cluster, Instance: cfg.Instance, Blocked: true})
	})}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return e
}

func TestPrepareResumesEveryHostBoundaryWithoutResettingIdentity(t *testing.T) {
	old := release.Version
	release.Version = "0.3.0"
	defer func() { release.Version = old }()
	for _, boundary := range []string{"journal-response", "nginx-test", "daemon-reload", "guard-start", "nginx-reload", "timer-start"} {
		t.Run(boundary, func(t *testing.T) {
			e := preparationFixture(t)
			initializations, injected := 0, false
			e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
				step := ""
				args := strings.Join(c.Args, " ")
				if strings.HasPrefix(args, "control prepare-standby ") {
					initializations++
					var cfg control.Config
					must(t, local.ReadJSON(e.controlConfig(), &cfg))
					must(t, local.JSON(filepath.Join(cfg.StateDir, "journal.json"), control.Journal{Schema: 1, Cluster: cfg.Cluster, Instance: cfg.Instance, HighEpoch: 1, HighRevision: 1, Blocked: true}))
					step = "journal-response"
				}
				if _, err := os.Stat(e.controlConfig()); err == nil && c.Name == e.Host.Nginx {
					step = "nginx-test"
				}
				switch args {
				case "daemon-reload":
					step = "daemon-reload"
				case "enable --now matrix-control.service":
					step = "guard-start"
				case "reload nginx":
					step = "nginx-reload"
				case "enable --now matrix-mailbox.timer matrix-handover.timer matrix-tasks.timer":
					step = "timer-start"
				}
				if step == boundary && !injected {
					injected = true
					return nil, errors.New("injected " + boundary)
				}
				if c.Name == e.Host.Docker && !strings.HasPrefix(args, "info") && !strings.HasPrefix(args, "compose version") && !strings.HasPrefix(args, "container ls") {
					t.Fatal("prepare touched applications", args)
				}
				if filepath.Base(c.Name) == "restic" {
					t.Fatal("standby preparation downloaded databases")
				}
				return nil, nil
			})
			if err := e.Prepare(context.Background(), false); err == nil || !injected {
				t.Fatal("fault not exercised", err)
			}
			before, err := local.FileHash(e.controlConfig())
			must(t, err)
			must(t, e.Prepare(context.Background(), false))
			after, err := local.FileHash(e.controlConfig())
			must(t, err)
			if before != after || initializations != 1 {
				t.Fatal("retry reset identity/journal", initializations)
			}
			if _, err = os.Stat(filepath.Join(e.Host.Root, "agent/pending-prepare.json")); !os.IsNotExist(err) {
				t.Fatal("preparation did not finish", err)
			}
			must(t, e.Prepare(context.Background(), false))
		})
	}
}

func TestPrepareRetryPreservesChangedConfiguration(t *testing.T) {
	old := release.Version
	release.Version = "0.3.0"
	defer func() { release.Version = old }()
	e := preparationFixture(t)
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		if len(c.Args) > 0 && c.Args[0] == "control" {
			return nil, errors.New("interrupted before journal")
		}
		return nil, nil
	})
	if err := e.Prepare(context.Background(), false); err == nil {
		t.Fatal("missing interruption")
	}
	write(t, e.controlConfig(), "operator-edited-configuration")
	if err := e.Prepare(context.Background(), false); err == nil || !strings.Contains(err.Error(), "preparation file changed") {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.controlConfig())
	must(t, err)
	if string(b) != "operator-edited-configuration" {
		t.Fatal("overwrote operator configuration")
	}
}
