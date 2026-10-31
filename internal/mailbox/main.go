package mailbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/storage"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const protocol = 1

var errMissing = storage.Missing
var errConflict = storage.Conflict

type Store interface {
	Get(context.Context, string) ([]byte, string, error)
	Put(context.Context, string, []byte, string) error
}
type S3 struct {
	endpoint, bucket, region, access, secret string
	client                                   *http.Client
}

func hash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func (s *S3) request(ctx context.Context, method, key string, body []byte, match string) ([]byte, string, error) {
	c := &storage.Client{Endpoint: s.endpoint, Bucket: s.bucket, Region: s.region, Access: s.access, Secret: s.secret, HTTP: s.client}
	b, tag, err := c.Request(ctx, method, key, body, match)
	if errors.Is(err, storage.Missing) {
		err = errMissing
	}
	if errors.Is(err, storage.Conflict) {
		err = errConflict
	}
	return b, tag, err
}

func (s *S3) Get(ctx context.Context, k string) ([]byte, string, error) {
	return s.request(ctx, "GET", k, nil, "")
}
func (s *S3) Put(ctx context.Context, k string, b []byte, m string) error {
	_, _, e := s.request(ctx, "PUT", k, b, m)
	return e
}
func safe(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
func private(path string) ([]byte, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private regular file (0600) required")
	}
	return os.ReadFile(path)
}
func storeFrom(path string) (*S3, error) {
	b, e := private(path)
	if e != nil {
		return nil, e
	}
	v := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, x, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		x = strings.TrimSpace(x)
		if !ok || v[k] != "" {
			return nil, errors.New("invalid credentials assignment")
		}
		if len(x) >= 2 && ((x[0] == '\'' && x[len(x)-1] == '\'') || (x[0] == '"' && x[len(x)-1] == '"')) {
			x = x[1 : len(x)-1]
		}
		v[k] = x
	}
	for _, k := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if v[k] == "" {
			return nil, errors.New("incomplete R2 configuration")
		}
	}
	u, e := url.Parse(v["S3_ENDPOINT"])
	if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || !strings.HasSuffix(u.Hostname(), ".r2.cloudflarestorage.com") || !safe(v["S3_BUCKET"]) {
		return nil, errors.New("invalid private R2 endpoint")
	}
	return &S3{strings.TrimRight(v["S3_ENDPOINT"], "/"), v["S3_BUCKET"], v["S3_REGION"], v["AWS_ACCESS_KEY_ID"], v["AWS_SECRET_ACCESS_KEY"], &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".pending-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return errors.New("invalid protocol fields")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing protocol content")
	}
	return nil
}

type Config struct {
	Schema   int    `json:"schema"`
	Cluster  string `json:"cluster_id"`
	Instance string `json:"instance_id"`
	S3Config string `json:"s3_config"`
	StateDir string `json:"state_dir"`
}
type Expected struct {
	Revision uint64 `json:"revision"`
	Epoch    uint64 `json:"epoch"`
	Owner    string `json:"owner"`
	Phase    string `json:"phase"`
}
type Command struct {
	Schema      int                   `json:"schema"`
	ID          string                `json:"id"`
	Cluster     string                `json:"cluster_id"`
	Target      string                `json:"target"`
	Action      string                `json:"action"`
	Created     string                `json:"created_at"`
	Expires     string                `json:"expires_at"`
	Expected    Expected              `json:"expected"`
	Bound       uint64                `json:"bound_epoch"`
	Operation   string                `json:"operation_id"`
	SourceEpoch uint64                `json:"source_epoch"`
	Parameters  management.Parameters `json:"parameters,omitempty"`
}
type Receipt struct {
	Schema    int    `json:"schema"`
	ID        string `json:"id"`
	Target    string `json:"target"`
	Hash      string `json:"command_sha256"`
	State     string `json:"state"`
	At        string `json:"at"`
	Error     string `json:"error,omitempty"`
	Action    string `json:"action"`
	Operation string `json:"operation_id,omitempty"`
}
type Status struct {
	Schema     int    `json:"schema"`
	Cluster    string `json:"cluster_id"`
	Instance   string `json:"instance_id"`
	Bound      uint64 `json:"bound_epoch"`
	Deployment string `json:"deployment_root"`
	Handover   *Op    `json:"handover"`
	Final      *Op    `json:"final_backup"`
}
type Op struct {
	ID    string `json:"id"`
	Epoch uint64 `json:"source_epoch"`
}
type Runner func(context.Context, Command, Status) error
type Poller struct {
	foundation string
	cfg        Config
	store      Store
	now        func() time.Time
	status     func(context.Context) (Status, error)
	run        Runner
}

func (p *Poller) prefix() string { return "cluster/" + p.cfg.Cluster + "/" }
func (p *Poller) receiptKey(id string) string {
	return p.prefix() + "commands/" + p.cfg.Instance + "/results/" + id + ".json"
}
func (p *Poller) local(id string) string { return filepath.Join(p.cfg.StateDir, "mailbox", id+".json") }
func (p *Poller) validate(c Command, now time.Time) error {
	if c.Schema != protocol || !safe(c.ID) || c.Cluster != p.cfg.Cluster || c.Target != p.cfg.Instance {
		return errors.New("command identity or schema mismatch")
	}
	created, e := time.Parse(time.RFC3339, c.Created)
	if e != nil {
		return errors.New("invalid created_at")
	}
	expires, e := time.Parse(time.RFC3339, c.Expires)
	if e != nil || !expires.After(created) || expires.Sub(created) > 5*time.Minute || created.After(now.Add(30*time.Second)) {
		return errors.New("invalid command lifetime")
	}
	if !now.Before(expires) {
		return errors.New("command expired")
	}
	if c.Expected.Epoch == 0 || c.Expected.Revision == 0 {
		return errors.New("missing expected authority")
	}
	if management.IsAction(c.Action) {
		if c.Operation != "" || c.SourceEpoch != 0 {
			return errors.New("management tasks do not accept legacy operation fields")
		}
		return taskRequest(c).Validate()
	}
	if string(release.JSON(c.Parameters)) != string(release.JSON(management.Parameters{})) {
		return errors.New("unexpected parameters")
	}
	switch c.Action {
	case "probe", "fence-local", "resume-local":
	case "handover-request", "handover-resume", "handover-cancel", "source-resume":
		if !safe(c.Operation) || strings.Contains(c.Operation, ".") || c.SourceEpoch == 0 {
			return errors.New("invalid operation identity")
		}
	default:
		return errors.New("unsupported action")
	}
	return nil
}
func (p *Poller) publish(ctx context.Context, r Receipt) error {
	b, _ := json.Marshal(r)
	key := p.receiptKey(r.ID)
	old, etag, e := p.store.Get(ctx, key)
	if errors.Is(e, errMissing) {
		return p.store.Put(ctx, key, b, "*")
	}
	if e != nil {
		return e
	}
	var prev Receipt
	if json.Unmarshal(old, &prev) != nil || prev.ID != r.ID || prev.Target != r.Target || prev.Hash != r.Hash {
		return errors.New("receipt identity conflict")
	}
	if prev.State == r.State {
		return nil
	}
	if prev.State != "CLAIMED" {
		return errors.New("terminal receipt cannot be overwritten")
	}
	return p.store.Put(ctx, key, b, etag)
}
func (p *Poller) poll(ctx context.Context) error {
	raw, _, e := p.store.Get(ctx, p.prefix()+"commands/"+p.cfg.Instance+"/pending.json")
	if errors.Is(e, errMissing) {
		return nil
	}
	if e != nil {
		return e
	}
	if len(raw) > 16384 {
		return errors.New("oversized command")
	}
	var c Command
	if e = decode(raw, &c); e != nil {
		return e
	}
	if !safe(c.ID) || c.Target != p.cfg.Instance || c.Cluster != p.cfg.Cluster {
		return errors.New("invalid command identity")
	}
	digest := hash(raw)
	r := Receipt{Schema: 1, ID: c.ID, Target: p.cfg.Instance, Hash: digest, State: "CLAIMED", At: p.now().UTC().Format(time.RFC3339), Action: c.Action, Operation: c.Operation}
	// Durable local marker and remote receipt both precede the side effect. A crash
	// in the ambiguous interval becomes UNKNOWN; never silently replay a command.
	if b, e := os.ReadFile(p.local(c.ID)); e == nil {
		var prev Receipt
		if json.Unmarshal(b, &prev) != nil || prev.Hash != digest {
			return errors.New("local command replay conflict")
		}
		if prev.State == "CLAIMED" {
			prev.State = "UNKNOWN"
			prev.Error = "previous execution interrupted; inspect authority and submit a new explicit command"
			prev.At = r.At
			if e = writeJSON(p.local(c.ID), prev); e != nil {
				return e
			}
		}
		return p.publish(ctx, prev)
	} else if !os.IsNotExist(e) {
		return e
	}
	if b, _, e := p.store.Get(ctx, p.receiptKey(c.ID)); e == nil {
		var prev Receipt
		if json.Unmarshal(b, &prev) != nil || prev.Hash != digest {
			return errors.New("remote command replay conflict")
		}
		if prev.State == "CLAIMED" {
			prev.State = "UNKNOWN"
			prev.Error = "remote claim exists without local result; no replay"
			prev.At = r.At
		}
		if e = writeJSON(p.local(c.ID), prev); e != nil {
			return e
		}
		return p.publish(ctx, prev)
	} else if !errors.Is(e, errMissing) {
		return e
	}
	finish := func(state string, err error) error {
		r.State = state
		r.At = p.now().UTC().Format(time.RFC3339)
		if err != nil {
			r.Error = err.Error()
		}
		if e := writeJSON(p.local(c.ID), r); e != nil {
			return e
		}
		return p.publish(ctx, r)
	}
	if e = p.validate(c, p.now()); e != nil {
		return finish("REJECTED", e)
	}
	control, _, e := p.store.Get(ctx, p.prefix()+"control.json")
	if e != nil {
		return e
	}
	var authority struct {
		Schema  int    `json:"schema"`
		Cluster string `json:"cluster_id"`
		Expected
	}
	if json.Unmarshal(control, &authority) != nil || authority.Schema != 1 || authority.Cluster != p.cfg.Cluster || authority.Expected != c.Expected {
		return finish("REJECTED", errors.New("authority changed; refresh before submitting a new command"))
	}
	st, e := p.status(ctx)
	if e != nil {
		return e
	}
	if st.Schema != 1 || st.Cluster != c.Cluster || st.Instance != c.Target || st.Bound != c.Bound {
		return finish("REJECTED", errors.New("local instance or bound epoch mismatch"))
	}
	if c.Action == "handover-request" && c.SourceEpoch != c.Expected.Epoch {
		return finish("REJECTED", errors.New("handover source epoch mismatch"))
	}
	if c.Action == "handover-resume" || c.Action == "handover-cancel" || c.Action == "source-resume" {
		op := st.Handover
		if c.Action == "source-resume" {
			op = st.Final
		}
		if op == nil || op.ID != c.Operation || op.Epoch != c.SourceEpoch {
			return finish("REJECTED", errors.New("local operation identity mismatch"))
		}
	}
	if e = writeJSON(p.local(c.ID), r); e != nil {
		return e
	}
	if e = p.publish(ctx, r); e != nil {
		return e
	}
	// Check lifetime/authority again immediately before dispatch. The existing
	// controller then performs its own ETag/owner/epoch/stop-proof validation.
	if e = p.validate(c, p.now()); e != nil {
		return finish("REJECTED", e)
	}
	current, _, e := p.store.Get(ctx, p.prefix()+"control.json")
	if e != nil {
		return finish("REJECTED", errors.New("authority unavailable before dispatch"))
	}
	var again struct{ Expected }
	if json.Unmarshal(current, &again) != nil || again.Expected != c.Expected {
		return finish("REJECTED", errors.New("authority changed before dispatch"))
	}
	if e = p.run(ctx, c, st); e != nil {
		return finish("FAILED", e)
	}
	return finish("APPLIED", nil)
}

// Host identity is an observation only; never use it as run authorization.
func hostInfo() map[string]any {
	hostname, _ := os.Hostname()
	addresses := []string{}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 || strings.HasPrefix(iface.Name, "docker") || strings.HasPrefix(iface.Name, "veth") || strings.HasPrefix(iface.Name, "br-") {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.IsGlobalUnicast() {
				addresses = append(addresses, ip.String())
			}
		}
	}
	sort.Strings(addresses)
	if len(addresses) > 32 {
		addresses = addresses[:32]
	}
	return map[string]any{"hostname": hostname, "addresses": addresses}
}
func (p *Poller) report(ctx context.Context, err error) error {
	report := map[string]any{"schema": 1, "mailbox_protocol": protocol, "instance_id": p.cfg.Instance, "cluster_id": p.cfg.Cluster, "reported_at": p.now().UTC().Format(time.RFC3339), "actions": []string{"probe", "handover-request", "handover-resume", "handover-cancel", "fence-local", "resume-local", "source-resume"}, "force_takeover": false, "software": release.Info(), "host": hostInfo()}
	if err != nil {
		report["error"] = err.Error()
	}
	st, e := p.status(ctx)
	if e == nil {
		details := map[string]any{}
		for name, op := range map[string]*Op{"handover": st.Handover, "final": st.Final} {
			if op != nil && safe(op.ID) {
				b, er := os.ReadFile(filepath.Join(p.cfg.StateDir, name, op.ID, "error.json"))
				if er == nil && len(b) < 16384 {
					var v any
					if json.Unmarshal(b, &v) == nil {
						details[name] = v
					}
				}
			}
		}
		report["worker_errors"] = details
		root := st.Deployment
		if root == "" {
			root = p.foundation
		}
		if b, er := os.ReadFile(filepath.Join(root, "backup-state/tuwunel-restic/job-status.json")); er == nil && len(b) < 65536 {
			var v any
			if json.Unmarshal(b, &v) == nil {
				report["scheduled_backup"] = v
			}
		}
		// Point summaries are observations, not authorization or restore selection.
		b, er := os.ReadFile(filepath.Join(root, "backup-state/tuwunel-restic/last-success.json"))
		if er == nil && len(b) < 65536 {
			var v map[string]any
			if json.Unmarshal(b, &v) == nil {
				summary := map[string]any{}
				for _, k := range []string{"snapshot_id", "data_time", "verification", "database_startup_verified", "consistency", "media_included", "telegram_included", "verified_at", "instance_control"} {
					if x, ok := v[k]; ok {
						summary[k] = x
					}
				}
				report["backup_observation"] = summary
			}
		}
	}
	p.observations(ctx, report)
	b, _ := json.Marshal(report)
	return p.store.Put(ctx, p.prefix()+"agents/"+p.cfg.Instance+".json", b, "")
}
func runLocal(configPath string) (func(context.Context) (Status, error), Runner) {
	bin, _ := os.Executable()
	return runLocalBinary(configPath, bin)
}
func runLocalBinary(configPath, bin string) (func(context.Context) (Status, error), Runner) {
	base := filepath.Dir(configPath)
	command := func(ctx context.Context, args []string) ([]byte, error) {
		out, e := exec.CommandContext(ctx, bin, append(append([]string{"control"}, args...), "--config", configPath)...).Output()
		if e != nil {
			if ctx.Err() != nil {
				return nil, errors.New("local controller timed out; inspect status before any further action")
			}
			if ex, ok := e.(*exec.ExitError); ok {
				return nil, fmt.Errorf("controller rejected/failed: %s", strings.TrimSpace(string(ex.Stderr)))
			}
			return nil, errors.New("local controller unavailable")
		}
		return out, nil
	}
	status := func(ctx context.Context) (Status, error) {
		var st Status
		b, e := command(ctx, []string{"status"})
		if e != nil {
			return st, e
		}
		e = json.Unmarshal(b, &st)
		return st, e
	}
	run := func(ctx context.Context, c Command, st Status) error {
		if management.IsAction(c.Action) {
			root := pRoot(base)
			switch c.Action {
			case "task-cancel":
				return management.Cancel(root, c.Parameters.Task)
			case "task-retry":
				return management.Retry(root, c.Parameters.Task)
			case "handover-pause", "handover-continue":
				cmd := exec.CommandContext(ctx, bin, c.Action, "--root", root, "--instance", c.Target)
				if _, err := cmd.Output(); err != nil {
					return errors.New("handover intervention failed; inspect the local task log")
				}
				return nil
			default:
				return management.Enqueue(root, taskRequest(c))
			}
		}
		if c.Action == "probe" {
			return nil
		}
		args := []string{c.Action, "--expected-instance", c.Target}
		if c.Action != "fence-local" {
			epoch := c.Bound
			if strings.HasPrefix(c.Action, "handover-") {
				epoch = c.SourceEpoch
			}
			args = append(args, "--expected-epoch", fmt.Sprint(epoch))
		}
		if c.Operation != "" {
			args = append(args, "--operation-id", c.Operation)
		}
		if c.Action == "source-resume" || c.Action == "resume-local" {
			if c.Action == "source-resume" {
				args = []string{"source-resume", "--root", pRoot(base), "--operation", c.Operation, "--instance", c.Target, "--epoch", fmt.Sprint(c.SourceEpoch)}
			} else {
				args = []string{"resume", "--root", pRoot(base), "--instance", c.Target, "--epoch", fmt.Sprint(c.Bound)}
			}
			cmd := exec.CommandContext(ctx, bin, args...)
			if _, e := cmd.Output(); e != nil {
				return errors.New("resume failed or interrupted; inspect instance and worker error before retrying")
			}
			return nil
		}
		_, e := command(ctx, args)
		return e
	}
	return status, run
}

func taskRequest(c Command) management.Request {
	return management.Request{ID: c.ID, Cluster: c.Cluster, Target: c.Target, Action: c.Action, Created: c.Created,
		Expected: management.Expected{Revision: c.Expected.Revision, Epoch: c.Expected.Epoch, Owner: c.Expected.Owner, Phase: c.Expected.Phase}, Parameters: c.Parameters}
}
func Run(args []string) error {
	flags := flag.NewFlagSet("mailbox", flag.ContinueOnError)
	path := flags.String("config", "/opt/matrix/deploy/control/local.json", "existing controller config")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	b, e := private(*path)
	if e != nil {
		return e
	}
	var cfg Config
	if json.Unmarshal(b, &cfg) != nil || cfg.Schema != 1 || !safe(cfg.Instance) || !safe(cfg.Cluster) || !filepath.IsAbs(cfg.StateDir) || !filepath.IsAbs(cfg.S3Config) {
		return errors.New("invalid controller configuration")
	}
	if e = os.MkdirAll(filepath.Join(cfg.StateDir, "mailbox"), 0700); e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(cfg.StateDir, "mailbox.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return nil
	}
	store, e := storeFrom(cfg.S3Config)
	if e != nil {
		return e
	}
	status, run := runLocal(*path)
	p := &Poller{foundation: filepath.Dir(filepath.Dir(filepath.Dir(*path))), cfg: cfg, store: store, now: time.Now, status: status, run: run}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result := p.poll(ctx)
	reportCtx, done := context.WithTimeout(context.Background(), 20*time.Second)
	defer done()
	if e = p.report(reportCtx, result); e != nil && result == nil {
		return e
	}
	return result
}

func pRoot(base string) string { return filepath.Dir(filepath.Dir(base)) }
