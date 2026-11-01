package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Installed struct {
	Schema    int    `json:"schema"`
	Directory string `json:"directory"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
}

// Saved before publishing any host configuration. An interrupted prepare must
// resume the same identity and fixed configuration, never generate a new node.
type Preparation struct {
	Schema  int               `json:"schema"`
	Phase   string            `json:"phase"`
	Host    Host              `json:"host"`
	Runtime Installed         `json:"runtime"`
	Config  control.Config    `json:"config"`
	Nginx   map[string]string `json:"nginx"`
}

func (e *Engine) Doctor(ctx context.Context) (any, error) {
	report := map[string]any{"root": e.Host.Root, "host": e.Host, "dependencies": map[string]any{}, "packages_installed": false}
	deps := report["dependencies"].(map[string]any)
	for _, name := range []string{e.Host.Docker, e.Host.SQLite, e.Host.Nginx, e.Host.Systemctl} {
		p, err := exec.LookPath(name)
		deps[name] = map[string]any{"path": p, "found": err == nil}
	}
	_, dockerErr := e.docker(ctx, "info")
	deps["docker_daemon"] = map[string]any{"ready": dockerErr == nil}
	var preparation Preparation
	if err := local.ReadJSON(filepath.Join(e.Host.Root, "agent/pending-prepare.json"), &preparation); err == nil {
		report["phase"] = "PREPARING"
		report["preparation"] = map[string]any{"phase": preparation.Phase, "instance_id": preparation.Config.Instance, "version": preparation.Runtime.Version}
		return report, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s, err := e.status(ctx); err == nil {
		report["instance"] = s
	} else if _, err := os.Stat(e.controlConfig()); err == nil {
		report["phase"] = "NEEDS_REPAIR"
	} else {
		report["phase"] = "UNPREPARED"
	}
	return report, nil
}
func (e *Engine) prerequisites(ctx context.Context) error {
	for _, test := range []struct {
		name string
		args []string
	}{{e.Host.Docker, []string{"info"}}, {e.Host.Docker, []string{"compose", "version"}}, {e.Host.SQLite, []string{"-version"}}, {e.Host.Nginx, []string{"-c", e.Host.NginxConfig, "-t"}}, {e.Host.Systemctl, []string{"--version"}}} {
		if _, err := e.run(ctx, test.name, test.args...); err != nil {
			return fmt.Errorf("Agent must prepare %s: %w", test.name, err)
		}
	}
	for _, dir := range []string{e.Host.NginxDirectory, e.Host.SystemdDirectory} {
		s, err := os.Stat(dir)
		if err != nil || !s.IsDir() {
			return errors.New("Agent must prepare nginx/systemd paths")
		}
	}
	return nil
}
func (e *Engine) installRuntime(source string) (Installed, error) {
	m, err := release.Verify(source)
	if err != nil {
		return Installed{}, err
	}
	target := filepath.Join(e.Host.Root, "releases", m.Version)
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return Installed{}, err
	}
	if _, err = os.Stat(target); errors.Is(err, os.ErrNotExist) {
		stage, err := os.MkdirTemp(filepath.Dir(target), ".install-")
		if err != nil {
			return Installed{}, err
		}
		defer os.RemoveAll(stage)
		for _, f := range append(m.Files, release.File{Path: "manifest.json"}) {
			if err = local.Copy(filepath.Join(source, f.Path), filepath.Join(stage, f.Path)); err != nil {
				return Installed{}, err
			}
		}
		if _, err = release.Verify(stage); err != nil {
			return Installed{}, err
		}
		if err = os.Rename(stage, target); err != nil {
			return Installed{}, err
		}
	} else if err != nil {
		return Installed{}, err
	}
	actual, err := release.Verify(target)
	if err != nil || !sameJSON(actual, m) {
		return Installed{}, errors.New("existing version directory differs")
	}
	return Installed{1, target, m.Version, m.Commit}, nil
}

func (e *Engine) units(runtime Installed) map[string]string {
	bin := filepath.Join(runtime.Directory, "matrix-admin")
	cfg := e.controlConfig()
	common := "[Service]\nType=oneshot\nUser=root\nUMask=0077\nEnvironment=GOMAXPROCS=1\nNice=10\nTimeoutStartSec=infinity\nTimeoutStopSec=30min\n"
	units := map[string]string{"matrix-control.service": fmt.Sprintf(`[Unit]
Description=Matrix runtime authority guard
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service
PartOf=docker.service
StartLimitIntervalSec=300
StartLimitBurst=5
[Service]
Type=notify
NotifyAccess=main
User=root
UMask=0077
Environment=GOMAXPROCS=1
Environment=GOMEMLIMIT=32MiB
ExecStart=%s control daemon --config %s
ExecStopPost=%s control emergency-stop --config %s
WatchdogSec=90
Restart=on-failure
RestartSec=10
TimeoutStartSec=45
TimeoutStopSec=75min
RuntimeDirectory=matrix-control
RuntimeDirectoryMode=0700
MemoryMax=64M
TasksMax=64
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=%s /run/matrix-control
[Install]
WantedBy=multi-user.target
`, bin, cfg, bin, cfg, filepath.Join(e.Host.Root, "deploy/control/state"))}
	for _, job := range []string{"mailbox", "handover", "backup", "tasks"} {
		args := job + " --root " + e.Host.Root
		if job == "mailbox" {
			args = "mailbox --config " + cfg
		}
		units["matrix-"+job+".service"] = "[Unit]\nDescription=Matrix " + job + " task\nAfter=matrix-control.service network-online.target\nRequires=matrix-control.service\n" + common + "ExecStart=" + bin + " " + args + "\n"
		if job == "tasks" {
			units["matrix-"+job+".service"] = strings.ReplaceAll(units["matrix-"+job+".service"], "Requires=matrix-control.service\n", "")
		}
		interval := "15s"
		if job == "backup" {
			interval = "15min"
		}
		units["matrix-"+job+".timer"] = "[Unit]\nDescription=Schedule Matrix " + job + "\n[Timer]\nOnBootSec=" + interval + "\nOnUnitInactiveSec=" + interval + "\nAccuracySec=1s\n[Install]\nWantedBy=timers.target\n"
	}
	return units
}
func (e *Engine) writeUnits(i Installed) error {
	for name, text := range e.units(i) {
		if err := local.Atomic(filepath.Join(e.Host.SystemdDirectory, name), []byte(text), 0644); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) Prepare(ctx context.Context, preflight bool) error {
	if _, err := release.Current(e.ReleaseDir); err != nil {
		return err
	}
	if err := e.connect(); err != nil {
		return err
	}
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	pending := filepath.Join(e.Host.Root, "agent/pending-prepare.json")
	var plan Preparation
	if err = local.ReadJSON(pending, &plan); err == nil {
		if preflight {
			return errors.New("standby preparation is incomplete; rerun prepare to resume the same instance")
		}
		return e.finishPreparation(ctx, plan)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := e.prerequisites(ctx); err != nil {
		return err
	}
	if _, err = os.Stat(e.controlConfig()); err == nil {
		s, err := e.status(ctx)
		if err != nil {
			return errors.New("existing controller needs repair; identity preserved")
		}
		if s.Cluster != e.Cluster {
			return errors.New("existing instance belongs to another cluster")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(e.Host.Root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "config", "agent", "generations", "releases":
		default:
			return errors.New("unknown or partial existing deployment; inspect without resetting")
		}
	}
	if files, _ := os.ReadDir(filepath.Join(e.Host.Root, "generations")); len(files) > 0 {
		return errors.New("existing generations cannot be reinitialized")
	}
	for _, n := range []string{"matrix.conf", "00-matrix-common.conf"} {
		if _, err = os.Lstat(filepath.Join(e.Host.NginxDirectory, n)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("existing nginx Matrix configuration requires inspection")
		}
	}
	for name := range e.units(Installed{}) {
		if _, err = os.Lstat(filepath.Join(e.Host.SystemdDirectory, name)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("existing Matrix systemd unit requires inspection")
		}
	}
	if preflight {
		return nil
	}
	point, _, err := e.readPoint(ctx, "", "")
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(filepath.Join(e.Host.Root, "agent"), "prepare-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	bundle, _, err := e.configuration(ctx, point, work)
	if err != nil {
		return err
	}
	var cfg control.Config
	if err = local.ReadJSON(filepath.Join(bundle, "payload/control/source-local.json"), &cfg); err != nil {
		return err
	}
	if cfg.Cluster != e.Cluster {
		return errors.New("source controller belongs to another cluster")
	}
	if len(cfg.Services) != 2 || cfg.Services[0].Name != "tuwunel" || cfg.Services[1].Name != "telegram-bridge" || cfg.GateAddress != "127.0.0.1:29318" {
		return errors.New("unsupported service topology")
	}
	for _, service := range cfg.Services {
		b, err := e.docker(ctx, "container", "ls", "-a", "--format", "{{.Names}}", "--filter", "name=^/"+service.Name+"$")
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(b))) != 0 {
			return errors.New("existing application containers require inspection")
		}
	}
	installed, err := e.installRuntime(e.ReleaseDir)
	if err != nil {
		return err
	}
	base := filepath.Join(e.Host.Root, "deploy/control")
	sourceControl := filepath.Dir(cfg.S3Config)
	cfg.Instance = "matrix-" + local.RandomID()
	cfg.S3Config = filepath.Join(base, "s3.env")
	cfg.StateDir = filepath.Join(base, "state")
	cfg.RuntimeDir = e.Host.RuntimeDirectory
	cfg.Generations = filepath.Join(e.Host.Root, "generations")
	cfg.Services[0].DataPath = filepath.Join(e.Host.Root, "deploy/tuwunel/data")
	cfg.Services[1].DataPath = filepath.Join(e.Host.Root, "deploy/telegram/data")
	plan = Preparation{Schema: 1, Phase: "INSTALLING", Host: e.Host, Runtime: installed, Config: cfg, Nginx: map[string]string{}}
	for _, n := range []string{"matrix.conf", "00-matrix-common.conf"} {
		b, err := os.ReadFile(filepath.Join(bundle, "payload/nginx", n))
		if err != nil {
			return err
		}
		plan.Nginx[n] = strings.ReplaceAll(string(b), sourceControl, base)
	}
	if err = local.JSON(pending, plan); err != nil {
		return err
	}
	return e.finishPreparation(ctx, plan)
}

// Only create missing files or reuse byte-identical ones. If a person or another
// operation changed a partial installation, preserve it and report the path.
func prepareFile(path string, data []byte, mode os.FileMode) error {
	if err := local.SafePath(path); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err == nil {
		if !bytes.Equal(b, data) {
			return fmt.Errorf("preparation file changed; inspect %s before retry", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return local.Atomic(path, data, mode)
}

func (e *Engine) finishPreparation(ctx context.Context, p Preparation) error {
	if p.Schema != 1 || p.Host != e.Host || p.Config.Cluster != e.Cluster || !local.SafeID(p.Config.Instance) ||
		(p.Phase != "INSTALLING" && p.Phase != "READY") || p.Config.StateDir != filepath.Join(e.Host.Root, "deploy/control/state") {
		return errors.New("preparation identity or host changed; preserve the pending installation")
	}
	installed, err := e.installRuntime(e.ReleaseDir)
	if err != nil {
		return err
	}
	if installed != p.Runtime {
		return errors.New("resume preparation with the same runtime version")
	}
	pending := filepath.Join(e.Host.Root, "agent/pending-prepare.json")
	if p.Phase == "READY" {
		// Timers may already have accepted work. Do not rewrite the journal,
		// units or instance config after reaching this durable boundary.
		if err = e.system(ctx, "enable", "--now", "matrix-mailbox.timer", "matrix-handover.timer", "matrix-tasks.timer"); err != nil {
			return err
		}
		return os.Rename(pending, filepath.Join(e.Host.Root, "agent/preparation.json"))
	}
	cfg := p.Config
	if err = os.MkdirAll(cfg.Generations, 0700); err != nil {
		return err
	}
	if err = prepareFile(e.controlConfig(), release.JSON(cfg), 0600); err != nil {
		return err
	}
	input, err := local.Private(filepath.Join(e.Host.Root, "config/s3.env"))
	if err != nil {
		return err
	}
	if err = local.Atomic(cfg.S3Config, input, 0600); err != nil {
		return err
	}
	// Host configuration may have been supplied by the operator with different
	// formatting. Compare its decoded value above, then preserve it.
	if err = local.JSON(filepath.Join(e.Host.Root, "agent/host.json"), e.Host); err != nil {
		return err
	}
	if err = prepareFile(filepath.Join(e.Host.Root, "agent/runtime.json"), release.JSON(installed), 0600); err != nil {
		return err
	}
	journalPath := filepath.Join(cfg.StateDir, "journal.json")
	var j control.Journal
	if err = local.ReadJSON(journalPath, &j); errors.Is(err, os.ErrNotExist) {
		_, err = e.run(ctx, filepath.Join(installed.Directory, "matrix-admin"), "control", "prepare-standby", "--config", e.controlConfig(), "--expected-instance", cfg.Instance)
		if err != nil {
			return err
		}
		err = local.ReadJSON(journalPath, &j)
	}
	if err != nil {
		return err
	}
	if j.Schema != 1 || j.Instance != cfg.Instance || j.Cluster != cfg.Cluster || !j.Blocked || j.BoundEpoch != 0 || j.Final != nil || j.Target != nil || j.Deployment != "" || len(j.Services) != 0 {
		return errors.New("partial standby already has bound data or operations; initialization will not reset it")
	}
	base := filepath.Dir(e.controlConfig())
	if err = prepareFile(filepath.Join(base, "managed"), release.JSON(map[string]any{"schema": 1, "standby": true}), 0600); err != nil {
		return err
	}
	for path, text := range map[string]string{"nginx-server/guard.conf": control.NginxServer, "nginx-location/guard.conf": control.NginxLocation} {
		if err = prepareFile(filepath.Join(base, path), []byte(text), 0600); err != nil {
			return err
		}
	}
	for _, name := range []string{"matrix.conf", "00-matrix-common.conf"} {
		text, ok := p.Nginx[name]
		if !ok || text == "" {
			return errors.New("incomplete preparation nginx configuration")
		}
		for path, mode := range map[string]os.FileMode{filepath.Join(e.Host.Root, "deploy/nginx", name): 0600, filepath.Join(e.Host.NginxDirectory, name): 0644} {
			if err = prepareFile(path, []byte(text), mode); err != nil {
				return err
			}
		}
	}
	for name, text := range e.units(installed) {
		if err = prepareFile(filepath.Join(e.Host.SystemdDirectory, name), []byte(text), 0644); err != nil {
			return err
		}
	}
	if _, err = e.run(ctx, e.Host.Nginx, "-c", e.Host.NginxConfig, "-t"); err != nil {
		return err
	}
	if err = e.system(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err = e.system(ctx, "enable", "--now", "matrix-control.service"); err != nil {
		return err
	}
	if err = e.system(ctx, "reload", e.Host.NginxService); err != nil {
		return err
	}
	if err = e.wait(ctx, 30*time.Second, func() error {
		s, err := e.status(ctx)
		if err != nil {
			return err
		}
		if s.Instance != cfg.Instance || s.Cluster != cfg.Cluster || !s.Blocked || s.Serving || s.Epoch != 0 {
			return errors.New("prepared controller is not an empty fenced standby")
		}
		return nil
	}); err != nil {
		return err
	}
	p.Phase = "READY"
	if err = local.JSON(pending, p); err != nil {
		return err
	}
	return e.finishPreparation(ctx, p)
}
