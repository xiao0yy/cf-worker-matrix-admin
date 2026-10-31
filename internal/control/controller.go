package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Controller struct {
	cfg         Config
	store       Authority
	docker      Containers
	mu          sync.Mutex
	finalMu     sync.Mutex
	authorityMu sync.Mutex
	journal     Journal
	state       *State
	deadline    time.Time
	ready       bool
	lastError   string
	action      string
	busy        bool
	wake        chan struct{}
	now         func() time.Time
}

func (c *Controller) saveLocked() error {
	return atomicJSON(filepath.Join(c.cfg.StateDir, "journal.json"), c.journal)
}
func (c *Controller) services() []Service {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.journal.Services) != 0 {
		return append([]Service(nil), c.journal.Services...)
	}
	return append([]Service(nil), c.cfg.Services...)
}
func (c *Controller) processGrantLocked() bool {
	if c.grantLocked() {
		return true
	}
	t := c.journal.Target
	return t != nil && (t.Phase == "STARTING" || t.Phase == "COMMITTING") && !c.journal.Blocked && c.journal.Final == nil && c.state != nil && c.state.Owner == c.cfg.Instance && c.state.Epoch == c.journal.BoundEpoch && c.state.Epoch == t.NextEpoch && c.state.Phase == "STARTING" && c.now().Before(c.deadline) && operationMatches(c.state.Operation, t.FinalOperation)
}
func (c *Controller) allowedLocked() bool {
	return c.ready && c.grantLocked()
}
func (c *Controller) grantLocked() bool {
	return c.journal.Final == nil && !c.journal.Blocked && c.state != nil && c.state.Owner == c.cfg.Instance && c.state.Epoch == c.journal.BoundEpoch && c.state.Phase == "ONLINE" && c.now().Before(c.deadline)
}
func (c *Controller) observe(s State, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.lastError = err.Error()
		if !c.deadline.IsZero() && !c.now().Before(c.deadline) {
			c.ready = false
			c.journal.Blocked = true
			c.journal.Reason = "control state stale; explicit resume required"
			if e := c.saveLocked(); e != nil {
				c.lastError = "cannot persist stale-state fence"
			}
		}
		return
	}
	if s.Epoch < c.journal.HighEpoch || s.Revision < c.journal.HighRevision {
		c.ready = false
		c.deadline = time.Time{}
		c.journal.Blocked = true
		c.journal.Reason = "control state regression"
		c.lastError = "control state regression"
		_ = c.saveLocked()
		return
	}
	c.state = &s
	c.journal.HighEpoch = s.Epoch
	c.journal.HighRevision = s.Revision
	starting := c.journal.Target != nil && (c.journal.Target.Phase == "STARTING" || c.journal.Target.Phase == "COMMITTING") && s.Phase == "STARTING" && operationMatches(s.Operation, c.journal.Target.FinalOperation)
	if s.Owner != c.cfg.Instance || s.Epoch != c.journal.BoundEpoch || (s.Phase != "ONLINE" && !starting) {
		c.ready = false
		c.deadline = time.Time{}
		c.journal.Blocked = true
		c.journal.Reason = "ownership, epoch or phase does not authorize this instance"
	}
	// Persist high-water marks before granting any process start.
	if err = c.saveLocked(); err != nil {
		c.ready = false
		c.deadline = time.Time{}
		c.lastError = "cannot persist control observation"
		return
	}
	c.deadline = c.now().Add(time.Duration(c.cfg.StaleSeconds) * time.Second)
	c.lastError = ""
}
func (c *Controller) freshGrant(ctx context.Context) error {
	c.authorityMu.Lock()
	s, _, err := c.store.Read(ctx)
	c.observe(s, err)
	c.authorityMu.Unlock()
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.processGrantLocked() {
		return errors.New("instance has no current run authorization")
	}
	return nil
}
func (c *Controller) snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"schema": 1, "cluster_id": c.cfg.Cluster, "instance_id": c.cfg.Instance, "bound_epoch": c.journal.BoundEpoch, "blocked": c.journal.Blocked, "reason": c.journal.Reason, "control": c.state, "final_backup": c.journal.Final, "handover": c.journal.Target, "deployment_root": c.journal.Deployment, "serving": c.allowedLocked(), "ready": c.ready, "action": c.action, "error": c.lastError, "reported_at": c.now().UTC().Format(time.RFC3339), "planned_handover_implemented": true, "forced_takeover_implemented": false, "automatic_cleanup_implemented": false}
}
func (c *Controller) fence(reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
	c.deadline = time.Time{}
	c.journal.Blocked = true
	c.journal.Reason = reason
	err := c.saveLocked()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return err
}
func (c *Controller) resume(ctx context.Context, instance string, epoch uint64) error {
	c.authorityMu.Lock()
	defer c.authorityMu.Unlock()
	if instance != c.cfg.Instance {
		return errors.New("wrong expected instance")
	}
	s, _, err := c.store.Read(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.Owner != instance || s.Epoch != epoch || epoch != c.journal.BoundEpoch || s.Phase != "ONLINE" || s.Revision < c.journal.HighRevision || s.Epoch < c.journal.HighEpoch {
		return errors.New("resume rejected: ownership, epoch or phase mismatch")
	}
	if c.journal.Final != nil {
		return errors.New("final backup holds this instance; use final-resume with its operation ID")
	}
	if c.journal.Target != nil && c.journal.Target.Phase != "ONLINE" && c.journal.Target.Phase != "CANCELLED" {
		return errors.New("target handover is active; use its operation-specific action")
	}
	c.journal.Blocked = false
	c.journal.Reason = ""
	c.journal.HighRevision = s.Revision
	c.journal.HighEpoch = s.Epoch
	if err = c.saveLocked(); err != nil {
		c.journal.Blocked = true
		return err
	}
	c.state = &s
	c.deadline = c.now().Add(time.Duration(c.cfg.StaleSeconds) * time.Second)
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}
func (c *Controller) runServices() {
	defer func() { c.mu.Lock(); c.busy = false; c.action = ""; c.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, service := range c.services() {
		if err := c.freshGrant(ctx); err != nil {
			c.jobError(err)
			return
		}
		running, err := c.docker.Inspect(ctx, service)
		if err != nil {
			c.jobError(err)
			c.fence("container identity or restart policy validation failed")
			return
		}
		if !running {
			c.mu.Lock()
			c.ready = false
			c.mu.Unlock()
			if err = c.docker.Start(ctx, service); err != nil {
				c.jobError(err)
				return
			}
		}
		for {
			if err = c.docker.Healthy(ctx, service); err == nil {
				break
			}
			c.mu.Lock()
			c.ready = false
			c.mu.Unlock()
			c.mu.Lock()
			grant := c.processGrantLocked()
			c.mu.Unlock()
			if !grant {
				c.jobError(errors.New("authorization revoked during startup"))
				return
			}
			select {
			case <-ctx.Done():
				c.jobError(errors.New("startup health check timed out"))
				return
			case <-time.After(time.Second):
			}
		}
	}
	if err := c.freshGrant(ctx); err != nil {
		c.jobError(err)
		return
	}
	c.mu.Lock()
	if c.processGrantLocked() {
		c.ready = true
	}
	c.mu.Unlock()
}
func (c *Controller) jobError(err error) {
	c.mu.Lock()
	c.ready = false
	c.lastError = err.Error()
	c.mu.Unlock()
	log.Printf("service reconciliation: %s", err)
}
func (c *Controller) stopServices() error {
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	services := c.services()
	for i := len(services) - 1; i >= 0; i-- {
		s := services[i]
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
		err := c.docker.Stop(ctx, s)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
func (c *Controller) schedule(fresh bool) {
	c.mu.Lock()
	if c.busy {
		c.mu.Unlock()
		return
	}
	granted := c.processGrantLocked()
	if c.drainingPermittedLocked(fresh) {
		c.mu.Unlock()
		return
	}
	if granted && !fresh {
		c.mu.Unlock()
		return
	}
	c.busy = true
	if granted {
		c.action = "checking-or-starting"
	} else {
		c.action = "stopping"
		c.ready = false
	}
	c.mu.Unlock()
	if granted {
		go c.runServices()
	} else {
		go func() {
			err := c.stopServices()
			c.mu.Lock()
			c.busy = false
			c.action = ""
			if err != nil {
				c.lastError = err.Error()
			}
			c.mu.Unlock()
		}()
	}
}
func (c *Controller) tick(ctx context.Context) {
	c.authorityMu.Lock()
	s, _, err := c.store.Read(ctx)
	c.observe(s, err)
	c.authorityMu.Unlock()
	c.schedule(err == nil)
	// Reports are observations, never authorization. Failed report writes do not
	// extend the authoritative read deadline or change ownership.
	b, _ := json.Marshal(c.snapshot())
	if err := c.store.Report(ctx, c.cfg.Instance, b); err != nil {
		log.Printf("instance report: %s", err)
	}
}
func notify(msg string) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err == nil {
		defer conn.Close()
		_, _ = conn.Write([]byte(msg))
	}
}
func (c *Controller) gate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/authorize" || r.Method != "GET" {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	allowed := c.allowedLocked()
	c.mu.Unlock()
	if !allowed {
		w.WriteHeader(403)
		return
	}
	w.WriteHeader(204)
}
func (c *Controller) admin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" && r.URL.Path == "/status" {
		json.NewEncoder(w).Encode(c.snapshot())
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var request struct {
		Instance  string `json:"expected_instance_id"`
		Epoch     uint64 `json:"expected_epoch"`
		Operation string `json:"operation_id"`
	}
	decoder := json.NewDecoder(ioLimit(r))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.Instance != c.cfg.Instance {
		http.Error(w, "invalid request or expected instance", 400)
		return
	}
	var err error
	switch r.URL.Path {
	case "/fence":
		err = c.fence("manual fence; explicit resume required")
	case "/resume":
		err = c.resume(r.Context(), request.Instance, request.Epoch)
	case "/final-begin", "/final-stop", "/final-frozen", "/final-snapshot", "/final-complete", "/final-resume", "/final-verify":
		err = c.finalAction(r.Context(), r.URL.Path, request.Instance, request.Epoch, request.Operation)
	case "/handover-request", "/handover-release", "/handover-claim", "/handover-validating", "/handover-bind", "/handover-start", "/handover-online", "/handover-resume", "/handover-cancel":
		err = c.handoverAction(r.Context(), r.URL.Path, request.Instance, request.Epoch, request.Operation)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	w.WriteHeader(202)
}
func ioLimit(r *http.Request) *httpBodyReader { return &httpBodyReader{r: r, remaining: 4096} }

// Bounded decoding without exposing request payloads in logs.
type httpBodyReader struct {
	r         *http.Request
	remaining int
}

func (b *httpBodyReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, fmt.Errorf("request too large")
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, e := b.r.Body.Read(p)
	b.remaining -= n
	return n, e
}
func daemon(ctx context.Context, c *Controller) error {
	if err := os.MkdirAll(c.cfg.RuntimeDir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(c.cfg.StateDir, "controller.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another controller owns the local lock")
	}
	adminPath := filepath.Join(c.cfg.RuntimeDir, "admin.sock")
	_ = os.Remove(adminPath)
	adminListener, err := net.Listen("unix", adminPath)
	if err != nil {
		return err
	}
	defer adminListener.Close()
	if err = os.Chmod(adminPath, 0600); err != nil {
		return err
	}
	gateListener, err := net.Listen("tcp", c.cfg.GateAddress)
	if err != nil {
		return err
	}
	defer gateListener.Close()
	gate := &http.Server{Handler: http.HandlerFunc(c.gate), ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second}
	admin := &http.Server{Handler: http.HandlerFunc(c.admin), ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 15 * time.Second}
	serverErrors := make(chan error, 2)
	go func() { serverErrors <- gate.Serve(gateListener) }()
	go func() { serverErrors <- admin.Serve(adminListener) }()
	defer gate.Close()
	defer admin.Close()
	notify("READY=1")
	ticker := time.NewTicker(time.Duration(c.cfg.PollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		cycle, cancel := context.WithTimeout(ctx, 20*time.Second)
		c.tick(cycle)
		cancel()
		notify("WATCHDOG=1")
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.ready = false
			c.mu.Unlock()
			return nil
		case err := <-serverErrors:
			return err
		case <-ticker.C:
		case <-c.wake:
		}
	}
}
