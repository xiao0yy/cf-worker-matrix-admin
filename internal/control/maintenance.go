package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"os"
	"path/filepath"
	"time"
)

func (c *Controller) maintenance(ctx context.Context, action, id, instance string, epoch uint64) error {
	c.finalMu.Lock()
	defer c.finalMu.Unlock()
	c.authorityMu.Lock()
	defer c.authorityMu.Unlock()
	if !safeID(id) || instance != c.cfg.Instance || epoch == 0 {
		return errors.New("invalid maintenance identity")
	}
	s, etag, err := c.store.Read(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	j := c.journal
	busy := c.busy
	c.mu.Unlock()
	if s.Epoch < j.HighEpoch || s.Revision < j.HighRevision || s.Owner != instance || s.Epoch != epoch || j.BoundEpoch != epoch || !j.Blocked || busy {
		return errors.New("maintenance requires a fenced current owner")
	}
	for _, svc := range c.services() {
		running, er := c.docker.Inspect(ctx, svc)
		if er != nil || running {
			return errors.New("writers must be stopped before storage maintenance")
		}
	}
	var op FinalOperation
	_ = json.Unmarshal(s.Operation, &op)
	if action == "/maintenance-begin" {
		if s.Phase == "MAINTENANCE" && op.ID == id && op.Mode == "maintenance" {
			return nil
		}
		raw, er := os.ReadFile(filepath.Join(c.cfg.StateDir, "maintenance", id+".json"))
		if er != nil {
			return er
		}
		var r management.Request
		if json.Unmarshal(raw, &r) != nil || r.Validate() != nil || r.Action != "storage-apply" || r.ID != id || r.Target != instance || r.Cluster != c.cfg.Cluster || r.Expected.Epoch != s.Epoch || r.Expected.Revision != s.Revision || r.Expected.Owner != s.Owner || r.Expected.Phase != s.Phase || s.Phase != "ONLINE" {
			return errors.New("maintenance request or authority changed")
		}
		op = FinalOperation{ID: id, Mode: "maintenance", Source: instance, Epoch: epoch, Phase: "MAINTENANCE", StartedAt: c.now().UTC().Format(time.RFC3339Nano)}
		s.Phase = "MAINTENANCE"
		s.Operation, _ = json.Marshal(op)
	} else {
		if s.Phase == "ONLINE" && string(s.Operation) == "null" {
			return nil
		}
		if s.Phase != "MAINTENANCE" || op.ID != id || op.Mode != "maintenance" {
			return errors.New("another maintenance operation is active")
		}
		s.Phase = "ONLINE"
		s.Operation = json.RawMessage("null")
	}
	s.Revision++
	if err = c.store.Write(ctx, s, etag); err != nil {
		return err
	}
	c.observe(s, nil)
	return nil
}
