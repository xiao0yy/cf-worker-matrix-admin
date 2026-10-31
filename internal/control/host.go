package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Service struct {
	Name      string `json:"name"`
	Image     string `json:"image_id"`
	DataPath  string `json:"data_path"`
	MountPath string `json:"mount_path"`
	HealthURL string `json:"health_url"`
}
type Config struct {
	Schema       int       `json:"schema"`
	Cluster      string    `json:"cluster_id"`
	Instance     string    `json:"instance_id"`
	S3Config     string    `json:"s3_config"`
	StateDir     string    `json:"state_dir"`
	RuntimeDir   string    `json:"runtime_dir"`
	DockerSocket string    `json:"docker_socket"`
	GateAddress  string    `json:"gate_address"`
	PollSeconds  int       `json:"poll_seconds"`
	StaleSeconds int       `json:"stale_seconds"`
	Services     []Service `json:"services"`
	Generations  string    `json:"generations_dir,omitempty"`
}

func safeID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return s != "." && s != ".."
}
func loadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, errors.New("invalid local controller config")
	}
	if c.Schema != 1 || !safeID(c.Cluster) || !safeID(c.Instance) || c.PollSeconds < 1 || c.StaleSeconds < c.PollSeconds*3 || len(c.Services) != 2 {
		return c, errors.New("invalid local controller settings")
	}
	for _, p := range []string{c.StateDir, c.RuntimeDir, c.DockerSocket, c.S3Config} {
		if !filepath.IsAbs(p) || filepath.Clean(p) == "/" {
			return c, errors.New("local paths must be absolute")
		}
	}
	host, _, err := net.SplitHostPort(c.GateAddress)
	if err != nil || host != "127.0.0.1" {
		return c, errors.New("gate must bind IPv4 loopback")
	}
	seen := map[string]bool{}
	if c.Generations != "" && (!filepath.IsAbs(c.Generations) || filepath.Clean(c.Generations) == "/") {
		return c, errors.New("invalid generations directory")
	}
	for _, s := range c.Services {
		if !safeID(s.Name) || seen[s.Name] || !strings.HasPrefix(s.Image, "sha256:") || len(s.Image) != 71 || !filepath.IsAbs(s.DataPath) || !filepath.IsAbs(s.MountPath) || !strings.HasPrefix(s.HealthURL, "http://127.0.0.1:") {
			return c, errors.New("invalid local service identity")
		}
		seen[s.Name] = true
	}
	return c, nil
}
func atomicJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type Journal struct {
	Schema       int              `json:"schema"`
	Cluster      string           `json:"cluster_id"`
	Instance     string           `json:"instance_id"`
	BoundEpoch   uint64           `json:"bound_epoch"`
	HighRevision uint64           `json:"high_revision"`
	HighEpoch    uint64           `json:"high_epoch"`
	Blocked      bool             `json:"blocked"`
	Reason       string           `json:"reason"`
	Final        *FinalOperation  `json:"final_backup,omitempty"`
	Target       *TargetOperation `json:"handover,omitempty"`
	Services     []Service        `json:"active_services,omitempty"`
	Deployment   string           `json:"deployment_root,omitempty"`
}

func readJournal(c Config) (Journal, error) {
	var j Journal
	b, err := os.ReadFile(filepath.Join(c.StateDir, "journal.json"))
	if err != nil {
		return j, err
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, errors.New("malformed local journal")
	}
	if j.Schema != 1 || j.Cluster != c.Cluster || j.Instance != c.Instance || (j.BoundEpoch == 0 && !j.Blocked) || j.HighEpoch < j.BoundEpoch || j.HighRevision == 0 {
		return j, errors.New("invalid local identity or journal")
	}
	return j, nil
}

type Containers interface {
	Inspect(context.Context, Service) (bool, error)
	Start(context.Context, Service) error
	Stop(context.Context, Service) error
	Healthy(context.Context, Service) error
}
type Docker struct {
	client *http.Client
	health *http.Client
}

var errContainerMissing = errors.New("container does not exist")

func newDocker(socket string) *Docker {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Docker{client: &http.Client{Transport: tr}, health: &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (d *Docker) call(ctx context.Context, method, path string) ([]byte, error) {
	r, _ := http.NewRequestWithContext(ctx, method, "http://docker/v1.41"+path, nil)
	resp, err := d.client.Do(r)
	if err != nil {
		return nil, errors.New("Docker API unavailable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 404 {
		return nil, errContainerMissing
	}
	if resp.StatusCode >= 300 && resp.StatusCode != 304 {
		return nil, fmt.Errorf("Docker HTTP %d", resp.StatusCode)
	}
	return b, nil
}
func (d *Docker) Inspect(ctx context.Context, s Service) (bool, error) {
	b, err := d.call(ctx, "GET", "/containers/"+s.Name+"/json")
	if err != nil {
		return false, err
	}
	var info struct {
		Image      string
		State      struct{ Running, Restarting bool }
		HostConfig struct{ RestartPolicy struct{ Name string } }
		Mounts     []struct{ Source, Destination string }
	}
	if json.Unmarshal(b, &info) != nil {
		return false, errors.New("invalid Docker inspection")
	}
	if info.Image != s.Image || info.HostConfig.RestartPolicy.Name != "no" {
		return false, errors.New("container image or restart policy differs from guarded configuration")
	}
	found := false
	for _, m := range info.Mounts {
		if m.Source == s.DataPath && m.Destination == s.MountPath {
			found = true
		}
	}
	if !found {
		return false, errors.New("container data mount differs from guarded configuration")
	}
	return info.State.Running && !info.State.Restarting, nil
}
func (d *Docker) Start(ctx context.Context, s Service) error {
	_, err := d.call(ctx, "POST", "/containers/"+s.Name+"/start")
	return err
}
func (d *Docker) Stop(ctx context.Context, s Service) error {
	// t=-1 requests graceful termination without Docker's timeout-to-SIGKILL.
	_, err := d.call(ctx, "POST", "/containers/"+s.Name+"/stop?t=-1")
	if errors.Is(err, errContainerMissing) {
		return nil
	}
	return err
}
func (d *Docker) Healthy(ctx context.Context, s Service) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", s.HealthURL, nil)
	resp, err := d.health.Do(req)
	if err != nil {
		return errors.New("service health endpoint unavailable")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
	if resp.StatusCode != 200 {
		return fmt.Errorf("service health HTTP %d", resp.StatusCode)
	}
	return nil
}
