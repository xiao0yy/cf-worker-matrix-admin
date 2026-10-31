package engine

import (
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
)

type Installed struct {
	Schema    int    `json:"schema"`
	Directory string `json:"directory"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
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
	for _, job := range []string{"mailbox", "handover", "backup"} {
		args := job + " --root " + e.Host.Root
		if job == "mailbox" {
			args = "mailbox --config " + cfg
		}
		units["matrix-"+job+".service"] = "[Unit]\nDescription=Matrix " + job + " task\nAfter=matrix-control.service network-online.target\nRequires=matrix-control.service\n" + common + "ExecStart=" + bin + " " + args + "\n"
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
	if err := e.prerequisites(ctx); err != nil {
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
	if _, err = os.Stat(e.controlConfig()); err == nil {
		s, err := e.status(ctx)
		if err != nil {
			return errors.New("existing controller needs repair; identity preserved")
		}
		if s.Cluster != e.Cluster {
			return errors.New("existing instance belongs to another cluster")
		}
		return nil
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
	if err = os.MkdirAll(cfg.Generations, 0700); err != nil {
		return err
	}
	if err = local.JSON(e.controlConfig(), cfg); err != nil {
		return err
	}
	input, err := local.Private(filepath.Join(e.Host.Root, "config/s3.env"))
	if err != nil {
		return err
	}
	if err = local.Atomic(cfg.S3Config, input, 0600); err != nil {
		return err
	}
	if err = local.JSON(filepath.Join(e.Host.Root, "agent/host.json"), e.Host); err != nil {
		return err
	}
	if err = local.JSON(filepath.Join(e.Host.Root, "agent/runtime.json"), installed); err != nil {
		return err
	}
	if err = control.Run([]string{"prepare-standby", "--config", e.controlConfig(), "--expected-instance", cfg.Instance}); err != nil {
		return err
	}
	if err = local.JSON(filepath.Join(base, "managed"), map[string]any{"schema": 1, "standby": true}); err != nil {
		return err
	}
	for p, text := range map[string]string{"nginx-server/guard.conf": control.NginxServer, "nginx-location/guard.conf": control.NginxLocation} {
		if err = local.Atomic(filepath.Join(base, p), []byte(text), 0600); err != nil {
			return err
		}
	}
	for _, n := range []string{"matrix.conf", "00-matrix-common.conf"} {
		b, err := os.ReadFile(filepath.Join(bundle, "payload/nginx", n))
		if err != nil {
			return err
		}
		b = []byte(strings.ReplaceAll(string(b), sourceControl, base))
		if err = local.Atomic(filepath.Join(e.Host.Root, "deploy/nginx", n), b, 0600); err != nil {
			return err
		}
		if err = local.Atomic(filepath.Join(e.Host.NginxDirectory, n), b, 0644); err != nil {
			return err
		}
	}
	if err = e.writeUnits(installed); err != nil {
		return err
	}
	if _, err = e.run(ctx, e.Host.Nginx, "-c", e.Host.NginxConfig, "-t"); err != nil {
		return err
	}
	if err = e.system(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err = e.system(ctx, "enable", "--now", "matrix-control.service", "matrix-mailbox.timer", "matrix-handover.timer"); err != nil {
		return err
	}
	if err = e.system(ctx, "reload", e.Host.NginxService); err != nil {
		return err
	}
	return nil
}
