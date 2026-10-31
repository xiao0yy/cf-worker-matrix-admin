package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/payload"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/storage"
)

type runFunc func(context.Context, Command) ([]byte, error)

func (f runFunc) Run(c context.Context, v Command) ([]byte, error) { return f(c, v) }
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func write(t *testing.T, p, s string) { t.Helper(); must(t, local.Atomic(p, []byte(s), 0600)) }
func testEngine(t *testing.T) *Engine {
	t.Helper()
	root := t.TempDir()
	return &Engine{Host: Defaults(root), Runner: ExecRunner{filepath.Join(root, "logs")}, Cluster: "fixture", Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
}
func runtimeFixture(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	self, e := os.Executable()
	must(t, e)
	must(t, local.Copy(self, filepath.Join(dir, "matrix-admin")))
	must(t, os.Chmod(filepath.Join(dir, "matrix-admin"), 0700))
	m := release.Manifest{Schema: 1, Project: "matrix-admin", Version: version, Commit: strings.Repeat("a", 40), Platform: "linux/amd64", Runtime: "native-go-v1", RecoveryReaders: []int{2}, Control: 1, Mailbox: 1, Restic: release.ResticVersion}
	for _, n := range []string{"matrix-admin", "restic", "AGENT.md", "LICENSE", "THIRD-PARTY.md"} {
		if n != "matrix-admin" {
			write(t, filepath.Join(dir, n), "fixture "+n)
		}
		ex := n == "matrix-admin" || n == "restic"
		if ex {
			must(t, os.Chmod(filepath.Join(dir, n), 0700))
		}
		s, e := os.Stat(filepath.Join(dir, n))
		must(t, e)
		hash, e := local.FileHash(filepath.Join(dir, n))
		must(t, e)
		m.Files = append(m.Files, release.File{Path: n, SHA: hash, Size: s.Size(), Executable: ex})
	}
	must(t, local.JSON(filepath.Join(dir, "manifest.json"), m))
	return dir
}
func fakeController(t *testing.T, e *Engine, s *Status, action func(string) error) control.Config {
	t.Helper()
	run := t.TempDir()
	cfg := control.Config{Schema: 1, Cluster: e.Cluster, Instance: s.Instance, StateDir: filepath.Join(e.Host.Root, "deploy/control/state"), RuntimeDir: run, S3Config: filepath.Join(e.Host.Root, "deploy/control/s3.env"), GateAddress: "127.0.0.1:29318", PollSeconds: 5, StaleSeconds: 30}
	must(t, local.JSON(e.controlConfig(), cfg))
	ln, err := net.Listen("unix", filepath.Join(run, "admin.sock"))
	must(t, err)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			json.NewEncoder(w).Encode(s)
			return
		}
		if action != nil {
			if err := action(r.URL.Path); err != nil {
				http.Error(w, "denied", 409)
				return
			}
		}
		// The real controller acknowledges successful actions asynchronously.
		w.WriteHeader(http.StatusAccepted)
	})}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return cfg
}
func sqlite(t *testing.T) string {
	t.Helper()
	p, e := exec.LookPath("sqlite3")
	if e != nil {
		t.Fatal("SQLite CLI is required for native backup tests")
	}
	return p
}
func database(t *testing.T, p string) {
	t.Helper()
	var sql strings.Builder
	for _, name := range tables {
		fmt.Fprintf(&sql, "CREATE TABLE %s (id INTEGER PRIMARY KEY, body TEXT); INSERT INTO %s VALUES(1,'state');", name, name)
	}
	b, e := exec.Command(sqlite(t), p, sql.String()).CombinedOutput()
	if e != nil {
		t.Fatal(e, string(b))
	}
}
func TestSQLiteSnapshotIncludesCommittedWAL(t *testing.T) {
	e := testEngine(t)
	e.Host.SQLite = sqlite(t)
	project := filepath.Join(e.Host.Root, "telegram")
	src := filepath.Join(project, "data")
	must(t, os.MkdirAll(src, 0700))
	database(t, filepath.Join(src, "telegram.db"))
	write(t, filepath.Join(src, "config.yaml"), "test configuration")
	write(t, filepath.Join(src, "registration.yaml"), "test registration")
	write(t, filepath.Join(project, "docker-compose.yml"), "test compose")
	// Keep a writer open so the committed row remains in WAL, not checkpointed on close.
	writer := exec.Command(e.Host.SQLite, "-batch", filepath.Join(src, "telegram.db"))
	in, err := writer.StdinPipe()
	must(t, err)
	out, err := writer.StdoutPipe()
	must(t, err)
	must(t, writer.Start())
	defer func() { in.Close(); writer.Wait() }()
	fmt.Fprintln(in, "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO message VALUES(2,'committed WAL'); SELECT 'ready';")
	scan := bufio.NewScanner(out)
	ready := false
	for scan.Scan() {
		if scan.Text() == "ready" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("WAL writer not ready")
	}
	st, err := os.Stat(filepath.Join(src, "telegram.db-wal"))
	must(t, err)
	if st.Size() == 0 {
		t.Fatal("no WAL")
	}
	var container Container
	container.ID = "same-container"
	container.Image = "sha256:" + strings.Repeat("b", 64)
	container.State.Running = true
	container.State.StartedAt = "fixed"
	container.Mounts = append(container.Mounts, struct{ Source, Destination string }{src, "/data"})
	must(t, json.Unmarshal([]byte(`{"Networks":{"test-net":{"IPAddress":"172.30.0.3"}}}`), &container.NetworkSettings))
	real := e.Runner
	e.Runner = runFunc(func(ctx context.Context, c Command) ([]byte, error) {
		if c.Name != e.Host.Docker {
			return real.Run(ctx, c)
		}
		switch c.Args[0] {
		case "inspect":
			return json.Marshal([]Container{container})
		case "network":
			return []byte(`[{"Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.30.0.0/24","Gateway":"172.30.0.1"}]}}]`), nil
		}
		return nil, errors.New("unexpected Docker mutation")
	})
	dest := filepath.Join(e.Host.Root, "snapshot")
	meta, err := e.telegramSnapshot(context.Background(), project, dest, false)
	must(t, err)
	if meta.Counts["message"] != 2 || meta.Restarted {
		t.Fatal(meta)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err = os.Stat(filepath.Join(dest, "data/telegram.db"+suffix)); !os.IsNotExist(err) {
			t.Fatal("WAL/SHM copied")
		}
	}
	must(t, e.verifyTelegram(context.Background(), filepath.Join(dest, "data/telegram.db"), meta))
	write(t, filepath.Join(src, "new-auth-file"), "unknown")
	if _, err = e.telegramSnapshot(context.Background(), project, filepath.Join(e.Host.Root, "other"), false); err == nil {
		t.Fatal("unclassified persistent state ignored")
	}
}
func bundleFixture(t *testing.T, base string) (string, Metadata) {
	t.Helper()
	bundle := filepath.Join(base, "bundle")
	db := filepath.Join(bundle, "payload/telegram/data/telegram.db")
	must(t, os.MkdirAll(filepath.Dir(db), 0700))
	database(t, db)
	for name, content := range map[string]string{"database/CURRENT": "MANIFEST-1\n", "database/MANIFEST-1": "rocks fixture", "config/docker-compose.yml": "compose fixture", "config/s3/media.env": "S3=fixture", "control/source-local.json": "{}", "nginx/matrix.conf": "nginx fixture", "nginx/00-matrix-common.conf": "common fixture", "telegram/data/config.yaml": "bridge config", "telegram/data/registration.yaml": "bridge registration", "telegram/config/docker-compose.yml": "bridge compose"} {
		write(t, filepath.Join(bundle, "payload", name), content)
	}
	e := testEngine(t)
	counts, err := e.counts(context.Background(), db)
	must(t, err)
	image := Image{"registry.example/image@sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64)}
	meta := Metadata{Format: "tuwunel-checkpoint-v2", Recovery: 2, Layout: "database-only-v1", Time: "2026-01-01T00:00:00Z", Image: image.ID, Server: "matrix.example.com", Checkpoint: checkpointCommand, Images: map[string]Image{"tuwunel": image, "telegram": image}, Media: Media{"media_s3", "media", "external-s3", "reuse-existing"}, TelegramIncluded: true, Consistency: "independent-online-snapshots", Binding: Binding{1, "fixture", "node-a", 1, "controller-admission"}, Telegram: Telegram{Schema: 1, Method: "sqlite-online-backup", Image: image.ID, Counts: counts, Network: Network{"test-net", "172.30.0.3", "172.30.0.0/24", "172.30.0.1"}}}
	must(t, local.JSON(filepath.Join(bundle, "payload/telegram/metadata.json"), meta.Telegram))
	saveBundle(t, bundle, meta)
	return bundle, meta
}
func saveBundle(t *testing.T, b string, m Metadata) {
	t.Helper()
	must(t, local.JSON(filepath.Join(b, "metadata.json"), m))
	manifest, err := local.Manifest(filepath.Join(b, "payload"))
	must(t, err)
	write(t, filepath.Join(b, "manifest.sha256"), manifest)
	must(t, payload.Verify(b))
}
func TestRealResticDatabaseOnlyRoundTrip(t *testing.T) {
	bin := os.Getenv("RESTIC_TEST_BINARY")
	if bin == "" {
		t.Skip("set RESTIC_TEST_BINARY; mandatory in release CI")
	}
	e := testEngine(t)
	e.ReleaseDir = filepath.Dir(bin)
	repo := filepath.Join(t.TempDir(), "repository")
	real := e.Runner
	e.Runner = runFunc(func(ctx context.Context, c Command) ([]byte, error) {
		if filepath.Base(c.Name) == "restic" {
			c.Name = bin
			c.Env = append(c.Env, "RESTIC_REPOSITORY="+repo)
		}
		return real.Run(ctx, c)
	})
	r := Repository{Password: "test-only-password"}
	_, err := e.rest(context.Background(), r, "", "init")
	must(t, err)
	base := t.TempDir()
	bundle, meta := bundleFixture(t, base)
	capsule := filepath.Join(base, "capsule.json")
	must(t, payload.Pack(bundle, capsule))
	_, err = e.rest(context.Background(), r, base, "backup", "--tag", "native-roundtrip", "bundle/payload/database", "bundle/payload/telegram/data/telegram.db")
	must(t, err)
	b, err := e.rest(context.Background(), r, "", "snapshots", "--json", "--tag", "native-roundtrip")
	must(t, err)
	var snaps []struct{ ID string }
	must(t, json.Unmarshal(b, &snaps))
	if len(snaps) != 1 {
		t.Fatal("ambiguous snapshot")
	}
	must(t, e.verifyTree(context.Background(), r, snaps[0].ID, bundle))
	work := t.TempDir()
	merged := filepath.Join(work, "bundle")
	must(t, payload.Unpack(capsule, merged))
	must(t, e.downloadData(context.Background(), r, snaps[0].ID, merged, work))
	must(t, e.verifyTelegram(context.Background(), filepath.Join(merged, "payload/telegram/data/telegram.db"), meta.Telegram))
	if _, err = os.Stat(filepath.Join(work, "download/bundle/payload/config")); !os.IsNotExist(err) {
		t.Fatal("configuration duplicated in B2")
	}
	if err = e.downloadData(context.Background(), r, snaps[0].ID, merged, work); err == nil {
		t.Fatal("overwrote prior restore")
	}
}

type memoryObjects struct {
	data       map[string][]byte
	failLatest bool
	puts       int
}

func (s *memoryObjects) Get(_ context.Context, k string) ([]byte, string, error) {
	b, ok := s.data[k]
	if !ok {
		return nil, "", storage.Missing
	}
	return append([]byte(nil), b...), `"` + local.Digest(b) + `"`, nil
}
func (s *memoryObjects) Put(_ context.Context, k string, b []byte, match string) error {
	old, ok := s.data[k]
	if s.failLatest && strings.HasSuffix(k, "/latest.json") {
		return errors.New("simulated offline R2")
	}
	if match == "*" && ok || match != "" && match != "*" && (!ok || match != `"`+local.Digest(old)+`"`) {
		return storage.Conflict
	}
	s.data[k] = append([]byte(nil), b...)
	s.puts++
	return nil
}
func TestPublicationRetryAndForeignEpoch(t *testing.T) {
	e := testEngine(t)
	s := Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 1, Serving: true, Control: &control.State{Owner: "node-a", Epoch: 1, Phase: "ONLINE"}}
	fakeController(t, e, &s, nil)
	e.ReleaseDir = runtimeFixture(t, "0.2.0")
	descriptor, err := release.Describe(e.ReleaseDir)
	must(t, err)
	must(t, local.CopyTree(e.ReleaseDir, filepath.Join(e.Host.Root, "agent/cache/releases", descriptor.SHA)))
	pending := t.TempDir()
	bundle, m := bundleFixture(t, pending)
	m.Software = descriptor
	saveBundle(t, bundle, m)
	must(t, payload.Pack(bundle, filepath.Join(pending, "configuration.json")))
	objects := &memoryObjects{data: map[string][]byte{e.prefix() + "/latest.json": []byte(`{"old":true}`)}, failLatest: true}
	e.Store = objects
	_, etag, err := objects.Get(context.Background(), e.prefix()+"/latest.json")
	must(t, err)
	job := BackupJob{Snapshot: strings.Repeat("c", 64), Repository: Repository{Schema: 1, ID: strings.Repeat("d", 64)}, IndexMatch: etag, Full: true, Verified: true, VerifiedAt: e.stamp()}
	if _, _, err = e.publish(context.Background(), m, pending, job); err == nil {
		t.Fatal("publication succeeded offline")
	}
	old, _, _ := objects.Get(context.Background(), e.prefix()+"/latest.json")
	if string(old) != `{"old":true}` {
		t.Fatal("lost prior point")
	}
	objects.failLatest = false
	p, _, err := e.publish(context.Background(), m, pending, job)
	must(t, err)
	_, _, err = e.publish(context.Background(), m, pending, job)
	must(t, err)
	got, _, err := e.readPoint(context.Background(), "", "")
	must(t, err)
	if got.Snapshot != p.Snapshot {
		t.Fatal("wrong point")
	}
	s.Epoch = 2
	s.Control.Epoch = 2
	if _, _, err = e.publish(context.Background(), m, pending, job); err == nil {
		t.Fatal("old epoch published")
	}
	if _, _, err = e.readPoint(context.Background(), p.Snapshot, strings.Repeat("0", 64)); err == nil {
		t.Fatal("bad point hash accepted")
	}
}
func TestRestoreRebindsEveryPersistentMount(t *testing.T) {
	e := testEngine(t)
	project := filepath.Join(t.TempDir(), "new-project")
	must(t, os.Mkdir(project, 0700))
	input := `{"services":{"tuwunel":{"environment":{"TUWUNEL_SERVER_NAME":"matrix.example.com"},"volumes":[{"type":"bind","source":"/old/data","target":"/var/lib/tuwunel"},{"type":"bind","source":"/old/token","target":"/etc/tuwunel/registration-token"},{"type":"bind","source":"/old/appservices","target":"/etc/tuwunel/appservices"}]}},"networks":{"default":{}}}`
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		if strings.HasSuffix(strings.Join(c.Args, " "), "config --format json") {
			return []byte(input), nil
		}
		return nil, nil
	})
	must(t, e.rewriteCompose(context.Background(), project, Image{Ref: "fixture"}, false, Network{Name: "fixture-net"}))
	b, err := os.ReadFile(filepath.Join(project, "docker-compose.yml"))
	must(t, err)
	if strings.Contains(string(b), "/old/") {
		t.Fatal("source paths survived")
	}
	for _, n := range []string{"data", "registration-token", "appservices"} {
		if !strings.Contains(string(b), filepath.Join(project, n)) {
			t.Fatal("mount not rebound", n)
		}
	}
}
func TestUnitsContainOnlyOneRuntimeAndNoShell(t *testing.T) {
	e := testEngine(t)
	units := e.units(Installed{Directory: "/opt/matrix/releases/0.2.0"})
	for name, text := range units {
		if strings.Contains(text, ".sh") || strings.Contains(text, "bash") || strings.Contains(text, "matrix-release") {
			t.Fatal("legacy runtime", name)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "Exec") {
				if !strings.Contains(line, "/opt/matrix/releases/0.2.0/matrix-admin ") {
					t.Fatal(line)
				}
			}
		}
	}
}
func TestFinalFailureKeepsSourceFrozen(t *testing.T) {
	e := testEngine(t)
	e.Store = &memoryObjects{data: map[string][]byte{}}
	var actions []string
	s := Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 1, Blocked: true, Control: &control.State{Owner: "node-a", Epoch: 1, Phase: "SNAPSHOTTING"}, Final: &control.FinalOperation{ID: "move-1", Phase: "SNAPSHOTTING"}}
	fakeController(t, e, &s, func(a string) error { actions = append(actions, a); return nil })
	e.Runner = runFunc(func(context.Context, Command) ([]byte, error) { return nil, nil })
	err := e.Final(context.Background(), "move-1", "node-a", 1)
	if err == nil {
		t.Fatal("unexpected success without source database")
	}
	for _, a := range actions {
		if a == "/resume" || a == "/final-resume" || a == "/final-complete" {
			t.Fatal("unfroze/committed on failure")
		}
	}
	if _, err = os.Stat(filepath.Join(e.Host.Root, "deploy/control/state/final/move-1/error.json")); err != nil {
		t.Fatal("missing operation error")
	}
}
func TestUpgradeResumesAfterGuardStartFailureWithoutNewIdentity(t *testing.T) {
	e := testEngine(t)
	e.Host.SystemdDirectory = filepath.Join(e.Host.Root, "units")
	must(t, os.MkdirAll(e.Host.SystemdDirectory, 0700))
	source := runtimeFixture(t, "0.2.0")
	s := Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 7, Blocked: true, Control: &control.State{Owner: "node-a", Epoch: 7, Phase: "ONLINE"}}
	cfg := fakeController(t, e, &s, nil)
	journal := control.Journal{Schema: 1, Cluster: e.Cluster, Instance: s.Instance, BoundEpoch: 7, HighEpoch: 7, HighRevision: 20, Blocked: true}
	must(t, local.JSON(filepath.Join(cfg.StateDir, "journal.json"), journal))
	identity, err := local.FileHash(e.controlConfig())
	must(t, err)
	write(t, filepath.Join(e.Host.SystemdDirectory, "tuwunel-restic.timer"), "legacy")
	fail := true
	var calls []string
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		args := strings.Join(c.Args, " ")
		calls = append(calls, args)
		if args == "is-active --quiet matrix-backup.timer" {
			return nil, errors.New("inactive")
		}
		if args == "start matrix-control.service" && fail {
			fail = false
			return nil, errors.New("injected start failure")
		}
		return nil, nil
	})
	if _, err = e.Upgrade(context.Background(), source, true); err == nil {
		t.Fatal("injected failure ignored")
	}
	var update Update
	must(t, local.ReadJSON(filepath.Join(e.Host.Root, "agent/pending-update.json"), &update))
	if update.Phase != "INSTALLED" {
		t.Fatal(update.Phase)
	}
	// The restarted guard may advance observation state while staying fenced.
	journal.HighRevision++
	journal.Reason = "new observation"
	must(t, local.JSON(filepath.Join(cfg.StateDir, "journal.json"), journal))
	_, err = e.Upgrade(context.Background(), source, true)
	must(t, err)
	got, err := local.FileHash(e.controlConfig())
	must(t, err)
	if got != identity {
		t.Fatal("identity replaced")
	}
	must(t, local.ReadJSON(filepath.Join(cfg.StateDir, "journal.json"), &journal))
	if journal.HighRevision != 21 || journal.BoundEpoch != 7 || !journal.Blocked {
		t.Fatal("journal reset")
	}
	if _, err = os.Stat(filepath.Join(e.Host.Root, "agent/pending-update.json")); !os.IsNotExist(err) {
		t.Fatal("pending update not completed")
	}
	for _, call := range calls {
		if call == "enable --now matrix-backup.timer" {
			t.Fatal("backup started while fenced")
		}
	}
	journal.Target = &control.TargetOperation{MayWrite: true}
	must(t, local.JSON(filepath.Join(cfg.StateDir, "journal.json"), journal))
	if _, err = e.Upgrade(context.Background(), source, true); err == nil {
		t.Fatal("updated potentially written target")
	}
}
func TestUpgradeRetryAfterReloadFailurePreservesDataBinding(t *testing.T) {
	for _, changeBinding := range []bool{false, true} {
		t.Run(fmt.Sprint(changeBinding), func(t *testing.T) {
			e := testEngine(t)
			e.Host.SystemdDirectory = filepath.Join(e.Host.Root, "units")
			must(t, os.MkdirAll(e.Host.SystemdDirectory, 0700))
			source := runtimeFixture(t, "0.3.0")
			s := Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 7, Blocked: true, Control: &control.State{Owner: "node-a", Epoch: 7, Phase: "ONLINE"}}
			cfg := fakeController(t, e, &s, nil)
			journal := control.Journal{Schema: 1, Cluster: e.Cluster, Instance: s.Instance, BoundEpoch: 7, HighEpoch: 7, HighRevision: 20, Blocked: true}
			path := filepath.Join(cfg.StateDir, "journal.json")
			must(t, local.JSON(path, journal))
			fail, stops := true, 0
			e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
				args := strings.Join(c.Args, " ")
				if args == "stop matrix-control.service" {
					stops++
				}
				if args == "daemon-reload" && fail {
					fail = false
					return nil, errors.New("injected reload failure")
				}
				return nil, nil
			})
			if _, err := e.Upgrade(context.Background(), source, true); err == nil {
				t.Fatal("reload failure ignored")
			}
			var update Update
			must(t, local.ReadJSON(filepath.Join(e.Host.Root, "agent/pending-update.json"), &update))
			if update.Phase != "STOPPED" || update.Generation == "" {
				t.Fatal("missing durable stopped data binding", update.Phase)
			}
			journal.HighRevision++
			journal.Reason = "new observation after restarting the management channel"
			if changeBinding {
				journal.Deployment = filepath.Join(e.Host.Root, "generations/replacement")
			}
			must(t, local.JSON(path, journal))
			_, err := e.Upgrade(context.Background(), source, true)
			if changeBinding {
				if err == nil {
					t.Fatal("changed data binding accepted")
				}
				return
			}
			must(t, err)
			if stops != 2 {
				t.Fatal("guard not stopped again before installing", stops)
			}
			must(t, local.ReadJSON(path, &journal))
			if journal.HighRevision != 21 || journal.BoundEpoch != 7 || !journal.Blocked {
				t.Fatal("journal or generation overwritten")
			}
		})
	}
}
func TestPreparePreflightDoesNotContactB2OrCreateIdentity(t *testing.T) {
	e := testEngine(t)
	e.ReleaseDir = runtimeFixture(t, "0.2.0")
	old := release.Version
	release.Version = "0.2.0"
	defer func() { release.Version = old }()
	e.Store = &memoryObjects{data: map[string][]byte{}}
	e.Host.NginxDirectory = t.TempDir()
	e.Host.SystemdDirectory = t.TempDir()
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		if c.Name == filepath.Join(e.ReleaseDir, "restic") || strings.Contains(strings.Join(c.Args, " "), "start") {
			t.Fatal("preflight mutates or downloads")
		}
		return nil, nil
	})
	must(t, e.Prepare(context.Background(), true))
	if _, err := os.Stat(e.controlConfig()); !os.IsNotExist(err) {
		t.Fatal("preflight created identity")
	}
}
func TestResumeRejectsPendingUpdateBeforeUnfencing(t *testing.T) {
	e := testEngine(t)
	write(t, filepath.Join(e.Host.Root, "agent/pending-update.json"), `{}`)
	if err := e.Resume(context.Background(), "node-a", 1); err == nil || !strings.Contains(err.Error(), "pending code update") {
		t.Fatal(err)
	}
}

func TestResumeAcceptsAcknowledgementAndStartsBackupTimer(t *testing.T) {
	e := testEngine(t)
	s := &Status{Instance: "node-a", Cluster: e.Cluster, Epoch: 1, Blocked: true}
	fakeController(t, e, s, func(action string) error {
		if action != "/resume" {
			return errors.New("unexpected action")
		}
		s.Blocked, s.Serving = false, true
		return nil
	})
	started := false
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		if c.Name != e.Host.Systemctl || strings.Join(c.Args, " ") != "enable --now matrix-backup.timer" {
			return nil, fmt.Errorf("unexpected command: %s %v", c.Name, c.Args)
		}
		started = true
		return nil, nil
	})
	must(t, e.Resume(context.Background(), "node-a", 1))
	if !started {
		t.Fatal("accepted, healthy resume did not enable scheduled backups")
	}
}

func TestResumeRejectionDoesNotStartBackupTimer(t *testing.T) {
	e := testEngine(t)
	s := &Status{Instance: "node-a", Cluster: e.Cluster, Epoch: 1, Blocked: true}
	fakeController(t, e, s, func(string) error { return errors.New("ownership mismatch") })
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		t.Errorf("rejected resume ran a host command: %s %v", c.Name, c.Args)
		return nil, errors.New("unexpected command")
	})
	if err := e.Resume(context.Background(), "node-a", 1); err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("expected controller rejection, got %v", err)
	}
}

func TestBackupCopiesRuntimeS3ConfigurationWithoutAdjacentFiles(t *testing.T) {
	root := t.TempDir()
	project, dest := filepath.Join(root, "project"), filepath.Join(root, "configuration")
	write(t, filepath.Join(project, "s3/media.env"), "fixture runtime configuration")
	write(t, filepath.Join(project, "appservices/telegram.yaml"), "fixture registration")
	for _, name := range []string{"media.env.example", "PLAN.md", ".gitignore", "unrelated-secret.env"} {
		write(t, filepath.Join(project, "s3", name), "must not be backed up")
	}
	must(t, copyApplicationConfiguration(project, dest))
	for _, name := range []string{"s3/media.env", "appservices/telegram.yaml"} {
		want, err := os.ReadFile(filepath.Join(project, name))
		must(t, err)
		got, err := os.ReadFile(filepath.Join(dest, name))
		must(t, err)
		if string(got) != string(want) {
			t.Fatalf("runtime configuration changed: %s", name)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dest, "s3"))
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != "media.env" {
		t.Fatal("non-runtime files entered the S3 recovery configuration")
	}
}
func TestBackupRetryReusesSnapshotAndCommitsSuccessLast(t *testing.T) {
	bin := os.Getenv("RESTIC_TEST_BINARY")
	if bin == "" {
		t.Skip("set RESTIC_TEST_BINARY; mandatory in release CI")
	}
	e := testEngine(t)
	s := Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 1, Serving: true, Control: &control.State{Owner: "node-a", Epoch: 1, Phase: "ONLINE"}}
	fakeController(t, e, &s, nil)
	e.ReleaseDir = runtimeFixture(t, "0.2.0")
	desc, err := release.Describe(e.ReleaseDir)
	must(t, err)
	must(t, local.CopyTree(e.ReleaseDir, filepath.Join(e.Host.Root, "agent/cache/releases", desc.SHA)))
	repoDir := filepath.Join(t.TempDir(), "repository")
	real := e.Runner
	e.Runner = runFunc(func(ctx context.Context, c Command) ([]byte, error) {
		if filepath.Base(c.Name) == "restic" {
			c.Name = bin
			c.Env = append(c.Env, "RESTIC_REPOSITORY="+repoDir)
		}
		return real.Run(ctx, c)
	})
	repo := Repository{Schema: 1, Endpoint: "https://s3.example.com", Region: "region", Bucket: "fixture", Path: "database", Access: "test-access", Secret: "test-secret", Password: "test-password"}
	_, err = e.rest(context.Background(), repo, "", "init")
	must(t, err)
	raw, err := e.rest(context.Background(), repo, "", "cat", "config")
	must(t, err)
	var config struct{ ID string }
	must(t, json.Unmarshal(raw, &config))
	repo.ID = config.ID
	state := filepath.Join(e.Host.Root, "backup-state/tuwunel-restic")
	pending := filepath.Join(state, "pending")
	bundle, meta := bundleFixture(t, pending)
	meta.Software = desc
	saveBundle(t, bundle, meta)
	must(t, payload.Pack(bundle, filepath.Join(pending, "configuration.json")))
	capsule, err := os.ReadFile(filepath.Join(pending, "configuration.json"))
	must(t, err)
	repoBytes := release.JSON(repo)
	old := Point{Protocol: 2, Schema: 2, Catalog: 1, Layout: meta.Layout, Format: meta.Format, Recovery: 2, Platform: "linux/amd64", Snapshot: strings.Repeat("e", 64), RepositoryID: repo.ID, RepositoryHash: local.Digest(repoBytes), ConfigurationHash: local.Digest(capsule), Software: desc, Server: meta.Server, Time: meta.Time, Images: meta.Images, Image: meta.Image, Consistency: meta.Consistency, Telegram: meta.Telegram, Media: meta.Media, Binding: meta.Binding}
	old.Scope.Telegram = true
	old.RepositoryObject = e.prefix() + "/repositories/" + repo.ID + "-" + old.RepositoryHash + ".json"
	old.ConfigurationObject = e.prefix() + "/configurations/" + old.ConfigurationHash + ".json"
	oldRaw := release.JSON(old)
	key := e.prefix() + "/points/" + old.Snapshot + ".json"
	objects := &memoryObjects{data: map[string][]byte{old.RepositoryObject: repoBytes, old.ConfigurationObject: capsule, key: oldRaw, e.prefix() + "/latest.json": release.JSON(Index{2, 2, key, local.Digest(oldRaw)})}, failLatest: true}
	e.Store = objects
	success := filepath.Join(state, "last-success.json")
	write(t, success, `{"previous":true}`)
	if err = e.Backup(context.Background(), true); err == nil {
		t.Fatal("publication failure ignored")
	}
	b, err := os.ReadFile(success)
	must(t, err)
	if string(b) != `{"previous":true}` {
		t.Fatal("success moved before publication")
	}
	var first BackupJob
	must(t, local.ReadJSON(filepath.Join(pending, "job.json"), &first))
	if !first.Verified || !local.SHA.MatchString(first.Snapshot) {
		t.Fatal("retry checkpoint missing")
	}
	objects.failLatest = false
	must(t, e.Backup(context.Background(), true))
	if _, err = os.Stat(pending); !os.IsNotExist(err) {
		t.Fatal("completed pending retained")
	}
	b, err = e.rest(context.Background(), repo, "", "snapshots", "--json", "--tag", first.Run)
	must(t, err)
	var snapshots []struct{ ID string }
	must(t, json.Unmarshal(b, &snapshots))
	if len(snapshots) != 1 || snapshots[0].ID != first.Snapshot {
		t.Fatal("retry duplicated backup")
	}
	b, err = os.ReadFile(success)
	must(t, err)
	var done struct {
		Snapshot string `json:"snapshot_id"`
	}
	must(t, json.Unmarshal(b, &done))
	if done.Snapshot != first.Snapshot {
		t.Fatal("wrong success receipt")
	}
}
