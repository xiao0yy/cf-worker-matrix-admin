// Package engine executes host workflows. State transitions remain in control;
// long tasks run as separate invocations of the same executable.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/storage"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Host struct {
	Schema           int    `json:"schema"`
	Root             string `json:"root"`
	Docker           string `json:"docker"`
	SQLite           string `json:"sqlite"`
	Nginx            string `json:"nginx"`
	Systemctl        string `json:"systemctl"`
	NginxConfig      string `json:"nginx_config"`
	NginxDirectory   string `json:"nginx_directory"`
	NginxService     string `json:"nginx_service"`
	SystemdDirectory string `json:"systemd_directory"`
	RuntimeDirectory string `json:"runtime_directory"`
}

func Defaults(root string) Host {
	return Host{1, root, "docker", "sqlite3", "nginx", "systemctl", "/etc/nginx/nginx.conf", "/etc/nginx/conf.d", "nginx", "/etc/systemd/system", "/run/matrix-control"}
}
func LoadHost(root string) (Host, error) {
	h := Defaults(root)
	p := filepath.Join(root, "agent/host.json")
	if _, e := os.Lstat(p); e == nil {
		b, e := local.Private(p)
		if e != nil {
			return h, e
		}
		if e = local.Decode(b, &h); e != nil {
			return h, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return h, e
	}
	if h.Schema != 1 || h.Root != root {
		return h, errors.New("host registry belongs to another root")
	}
	for _, p := range []string{h.Root, h.NginxConfig, h.NginxDirectory, h.SystemdDirectory, h.RuntimeDirectory} {
		if !regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`).MatchString(p) {
			return h, errors.New("unsupported host path")
		}
		if e := local.SafePath(p); e != nil {
			return h, e
		}
	}
	if h.RuntimeDirectory != "/run/matrix-control" {
		return h, errors.New("controller runtime directory must be /run/matrix-control")
	}
	for _, p := range []string{h.Docker, h.SQLite, h.Nginx, h.Systemctl, h.NginxService} {
		if !regexp.MustCompile(`^[a-zA-Z0-9_/][a-zA-Z0-9_./@-]*$`).MatchString(p) {
			return h, errors.New("invalid executable or service name")
		}
	}
	return h, nil
}

type Command struct {
	Name     string
	Args     []string
	Dir      string
	Env      []string
	Timeout  time.Duration
	Combined bool
}
type Runner interface {
	Run(context.Context, Command) ([]byte, error)
}
type ExecRunner struct{ LogDir string }
type bounded struct {
	bytes.Buffer
	limit int
}

func (b *bounded) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("command output limit exceeded")
	}
	return b.Buffer.Write(p)
}
func (r ExecRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C.UTF-8"}, c.Env...)
	cmd.WaitDelay = 10 * time.Second
	out := &bounded{limit: 32 << 20}
	errout := &bounded{limit: 4 << 20}
	cmd.Stdout = out
	cmd.Stderr = errout
	if c.Combined {
		cmd.Stderr = out
	}
	if e := cmd.Run(); e != nil {
		if r.LogDir != "" {
			_ = local.Atomic(filepath.Join(r.LogDir, "command-"+local.RandomID()+".log"), append(out.Bytes(), errout.Bytes()...), 0600)
		}
		return nil, fmt.Errorf("%s failed; inspect private command logs: %w", filepath.Base(c.Name), e)
	}
	return out.Bytes(), nil
}

type Objects interface {
	Get(context.Context, string) ([]byte, string, error)
	Put(context.Context, string, []byte, string) error
}
type Engine struct {
	Host       Host
	Runner     Runner
	Store      Objects
	Cluster    string
	Binary     string
	ReleaseDir string
	Now        func() time.Time
}

func New(root string) (*Engine, error) {
	h, e := LoadHost(root)
	if e != nil {
		return nil, e
	}
	bin, e := os.Executable()
	if e != nil {
		return nil, e
	}
	return &Engine{Host: h, Runner: ExecRunner{filepath.Join(root, "agent/logs")}, Binary: bin, ReleaseDir: filepath.Dir(bin), Now: time.Now}, nil
}
func (e *Engine) connect() error {
	if e.Store != nil {
		return nil
	}
	s, c, err := storage.FromFile(filepath.Join(e.Host.Root, "config/s3.env"))
	if err != nil {
		return err
	}
	e.Store = s
	e.Cluster = c
	return nil
}
func (e *Engine) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return e.Runner.Run(ctx, Command{Name: name, Args: args})
}
func (e *Engine) docker(ctx context.Context, args ...string) ([]byte, error) {
	return e.Runner.Run(ctx, Command{Name: e.Host.Docker, Args: args, Combined: len(args) > 0 && args[0] == "logs"})
}
func (e *Engine) system(ctx context.Context, args ...string) error {
	_, err := e.run(ctx, e.Host.Systemctl, args...)
	return err
}
func (e *Engine) compose(ctx context.Context, project string, args ...string) ([]byte, error) {
	prefix := []string{"compose", "--project-directory", project, "-f", filepath.Join(project, "docker-compose.yml")}
	return e.docker(ctx, append(prefix, args...)...)
}
func (e *Engine) controlConfig() string {
	return filepath.Join(e.Host.Root, "deploy/control/local.json")
}
func (e *Engine) controlCall(ctx context.Context, action string, v any) ([]byte, error) {
	var cfg control.Config
	if err := local.ReadJSON(e.controlConfig(), &cfg); err != nil {
		return nil, err
	}
	c := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(cfg.RuntimeDir, "admin.sock"))
	}}}
	method := "GET"
	var b []byte
	if v != nil {
		method = "POST"
		b, _ = json.Marshal(v)
	}
	r, err := http.NewRequestWithContext(ctx, method, "http://controller/"+action, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(r)
	if err != nil {
		return nil, errors.New("local controller unavailable")
	}
	defer resp.Body.Close()
	b, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return nil, errors.New("invalid controller response")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("controller rejected %s (HTTP %d)", action, resp.StatusCode)
	}
	return b, nil
}

type Status struct {
	Instance   string                   `json:"instance_id"`
	Cluster    string                   `json:"cluster_id"`
	Epoch      uint64                   `json:"bound_epoch"`
	Blocked    bool                     `json:"blocked"`
	Serving    bool                     `json:"serving"`
	Control    *control.State           `json:"control"`
	Final      *control.FinalOperation  `json:"final_backup"`
	Handover   *control.TargetOperation `json:"handover"`
	Deployment string                   `json:"deployment_root"`
}

func (e *Engine) status(ctx context.Context) (Status, error) {
	var s Status
	b, err := e.controlCall(ctx, "status", nil)
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	return s, err
}
func (e *Engine) action(ctx context.Context, a, op, id string, epoch uint64) error {
	_, err := e.controlCall(ctx, a, map[string]any{"expected_instance_id": id, "expected_epoch": epoch, "operation_id": op})
	return err
}
func (e *Engine) active(ctx context.Context) (string, Status, error) {
	s, err := e.status(ctx)
	root := s.Deployment
	if root == "" {
		root = e.Host.Root
	}
	if err == nil {
		if root != e.Host.Root && !strings.HasPrefix(root, e.Host.Root+"/generations/") {
			return "", s, errors.New("active data outside foundation")
		}
		err = local.SafePath(root)
	}
	return root, s, err
}
func (e *Engine) wait(ctx context.Context, limit time.Duration, fn func() error) error {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		if err := fn(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("operation deadline reached; state retained")
		case <-time.After(time.Second):
		}
	}
}
func (e *Engine) stamp() string  { return e.Now().UTC().Format(time.RFC3339Nano) }
func (e *Engine) prefix() string { return "cluster/" + e.Cluster + "/recovery/v2" }
func immutable(ctx context.Context, s Objects, key string, b []byte) error {
	err := s.Put(ctx, key, b, "*")
	if err != nil && !errors.Is(err, storage.Conflict) {
		return err
	}
	got, _, err := s.Get(ctx, key)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, b) {
		return errors.New("immutable R2 object differs")
	}
	return nil
}
