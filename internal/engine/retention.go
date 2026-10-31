package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/storage"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type StoragePlan struct {
	Schema     int      `json:"schema"`
	ID         string   `json:"id"`
	Instance   string   `json:"instance_id"`
	Cluster    string   `json:"cluster_id"`
	Epoch      uint64   `json:"epoch"`
	At         string   `json:"at"`
	Repository string   `json:"repository_id"`
	Delete     []string `json:"delete_snapshots"`
	Keep       []string `json:"keep_snapshots"`
}

func (e *Engine) list(ctx context.Context, prefix string) ([]string, error) {
	l, ok := e.Store.(interface {
		List(context.Context, string) ([]string, error)
	})
	if !ok {
		return nil, errors.New("object listing unavailable")
	}
	return l.List(ctx, prefix)
}
func (e *Engine) protected(ctx context.Context) (map[string]bool, Point, error) {
	p, _, err := e.readPoint(ctx, "", "")
	if err != nil {
		return nil, p, err
	}
	s, err := e.status(ctx)
	if err != nil || s.Control == nil {
		return nil, p, errors.New("authority unavailable")
	}
	protect := map[string]bool{p.Snapshot: true}
	for _, op := range []*control.FinalOperation{s.Control.Completed, s.Final} {
		if op != nil && op.Receipt != nil {
			protect[op.Receipt.Snapshot] = true
		}
	}
	var op control.FinalOperation
	_ = json.Unmarshal(s.Control.Operation, &op)
	if op.Receipt != nil {
		protect[op.Receipt.Snapshot] = true
	}
	// Explicitly selected queued/running restores are protected, across nodes.
	keys, err := e.list(ctx, "cluster/"+e.Cluster+"/tasks/")
	if err != nil {
		return nil, p, err
	}
	for _, k := range keys {
		raw, _, er := e.Store.Get(ctx, k)
		if er != nil {
			return nil, p, er
		}
		var t management.Task
		if json.Unmarshal(raw, &t) != nil {
			return nil, p, errors.New("invalid task catalog")
		}
		if !management.Terminal(t.State) && t.Request.Parameters.Snapshot != "" {
			protect[t.Request.Parameters.Snapshot] = true
		}
	}
	return protect, p, nil
}
func (e *Engine) RetentionPlan(ctx context.Context, r management.Request) (any, error) {
	protect, current, err := e.protected(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := e.list(ctx, e.prefix()+"/points/")
	if err != nil {
		return nil, err
	}
	var points []Point
	for _, k := range keys {
		raw, _, er := e.Store.Get(ctx, k)
		if er != nil {
			return nil, er
		}
		var p Point
		if local.Decode(raw, &p) != nil || p.validate(e.Cluster, e.prefix()) != nil {
			return nil, errors.New("invalid recovery catalog")
		}
		if p.RepositoryID != current.RepositoryID {
			return nil, errors.New("multiple repositories require separate retention review")
		}
		if _, _, er = e.Store.Get(ctx, e.prefix()+"/retired/"+p.Snapshot+".json"); er == nil {
			continue
		} else if !errors.Is(er, storage.Missing) {
			return nil, er
		}
		if _, er = time.Parse(time.RFC3339Nano, p.Time); er != nil {
			return nil, errors.New("invalid recovery timestamp")
		}
		points = append(points, p)
	}
	sort.Slice(points, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, points[i].Time)
		b, _ := time.Parse(time.RFC3339Nano, points[j].Time)
		return a.After(b)
	})
	plan := StoragePlan{Schema: 1, ID: r.ID, Instance: r.Target, Cluster: r.Cluster, Epoch: r.Expected.Epoch, At: e.stamp(), Repository: current.RepositoryID, Delete: []string{}, Keep: []string{}}
	for i, p := range points {
		at, _ := time.Parse(time.RFC3339Nano, p.Time)
		if i < r.Parameters.KeepLast || e.Now().Sub(at) < time.Duration(r.Parameters.KeepDays)*24*time.Hour || protect[p.Snapshot] {
			plan.Keep = append(plan.Keep, p.Snapshot)
		} else {
			plan.Delete = append(plan.Delete, p.Snapshot)
		}
	}
	raw := release.JSON(plan)
	if err = local.Atomic(filepath.Join(e.Host.Root, "agent/plans", r.ID+".json"), raw, 0600); err != nil {
		return nil, err
	}
	return map[string]any{"plan": plan, "plan_sha256": local.Digest(raw), "requires_fenced_owner": true, "media_deleted": false}, nil
}
func (e *Engine) RetentionApply(ctx context.Context, r management.Request) (any, error) {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	raw, err := local.Private(filepath.Join(e.Host.Root, "agent/plans", r.Parameters.Plan+".json"))
	if err != nil || local.Digest(raw) != r.Parameters.PlanHash {
		return nil, errors.New("reviewed storage plan differs")
	}
	var p StoragePlan
	if local.Decode(raw, &p) != nil || p.Schema != 1 || p.ID != r.Parameters.Plan || p.Instance != r.Target || p.Cluster != r.Cluster || p.Epoch != r.Expected.Epoch {
		return nil, errors.New("plan belongs to another instance or epoch")
	}
	at, err := time.Parse(time.RFC3339Nano, p.At)
	if err != nil || e.Now().Sub(at) > 24*time.Hour || e.Now().Before(at) {
		return nil, errors.New("storage plan expired")
	}
	root, _, err := e.active(ctx)
	if err != nil {
		return nil, err
	}
	backup, err := local.Lock(filepath.Join(root, "backup-state/tuwunel-restic/operation.lock"))
	if err != nil {
		return nil, err
	}
	defer backup()
	if err = local.JSON(filepath.Join(e.Host.Root, "deploy/control/state/maintenance", r.ID+".json"), r); err != nil {
		return nil, err
	}
	if err = e.action(ctx, "maintenance-begin", r.ID, r.Target, r.Expected.Epoch); err != nil {
		return nil, err
	}
	protect, current, err := e.protected(ctx)
	if err != nil {
		return nil, err
	}
	if current.RepositoryID != p.Repository {
		return nil, errors.New("repository changed")
	}
	for _, id := range p.Delete {
		if !local.SHA.MatchString(id) || protect[id] {
			return nil, errors.New("deletion would remove a protected recovery point")
		}
	}
	repo, err := e.repository(ctx, current)
	if err != nil {
		return nil, err
	}
	if err = e.verifyRepository(ctx, repo); err != nil {
		return nil, err
	}
	e.progress("retire-reviewed-snapshots")
	for _, id := range p.Delete {
		if err = e.maintenanceOwned(ctx, r); err != nil {
			return nil, err
		}
		if err = immutable(ctx, e.Store, e.prefix()+"/retired/"+id+".json", release.JSON(map[string]any{"schema": 1, "snapshot_id": id, "plan_id": p.ID, "plan_sha256": r.Parameters.PlanHash})); err != nil {
			return nil, err
		}
		// An interrupted forget can safely be repeated for the exact IDs. No --keep
		// policy is reevaluated during apply and no unknown orphan snapshot is erased.
		if _, err = e.rest(ctx, repo, "", "forget", id); err != nil {
			return nil, err
		}
	}
	e.progress("prune-and-check")
	if err = e.maintenanceOwned(ctx, r); err != nil {
		return nil, err
	}
	if len(p.Delete) > 0 {
		if _, err = e.rest(ctx, repo, "", "prune"); err != nil {
			return nil, err
		}
	}
	if _, err = e.rest(ctx, repo, "", "check"); err != nil {
		return nil, err
	}
	if err = e.action(ctx, "maintenance-end", r.ID, r.Target, r.Expected.Epoch); err != nil {
		return nil, err
	}
	return map[string]any{"removed_snapshots": p.Delete, "repository_checked": true, "remains_fenced": true}, nil
}
func (e *Engine) maintenanceOwned(ctx context.Context, r management.Request) error {
	s, err := e.status(ctx)
	if err != nil || s.Control == nil || s.Control.Owner != r.Target || s.Control.Epoch != r.Expected.Epoch || s.Control.Phase != "MAINTENANCE" || !s.Blocked {
		return errors.New("storage maintenance no longer owns authority")
	}
	var op control.FinalOperation
	if json.Unmarshal(s.Control.Operation, &op) != nil || op.ID != r.ID {
		return errors.New("storage maintenance superseded")
	}
	return nil
}

type CleanupPlan struct {
	Schema   int      `json:"schema"`
	ID       string   `json:"id"`
	Instance string   `json:"instance_id"`
	Epoch    uint64   `json:"epoch"`
	At       string   `json:"at"`
	Paths    []string `json:"paths"`
	Bytes    uint64   `json:"bytes"`
}

func (e *Engine) cleanupCandidates(ctx context.Context) ([]string, uint64, error) {
	s, err := e.status(ctx)
	if err != nil || s.Control == nil || !s.Blocked || s.Serving || s.Control.Owner == s.Instance || s.Control.Phase != "ONLINE" {
		return nil, 0, errors.New("cleanup requires an inactive fenced instance and an ONLINE replacement")
	}
	raw, _, err := e.Store.Get(ctx, "cluster/"+e.Cluster+"/instances/"+s.Control.Owner+".json")
	if err != nil {
		return nil, 0, err
	}
	var remote struct {
		Serving bool   `json:"serving"`
		Epoch   uint64 `json:"bound_epoch"`
		At      string `json:"reported_at"`
	}
	if json.Unmarshal(raw, &remote) != nil || !remote.Serving || remote.Epoch != s.Control.Epoch {
		return nil, 0, errors.New("replacement is not serving the current epoch")
	}
	at, err := time.Parse(time.RFC3339Nano, remote.At)
	if err != nil || e.Now().Sub(at) > 90*time.Second || e.Now().Sub(at) < -30*time.Second {
		return nil, 0, errors.New("replacement health report expired")
	}
	for _, name := range []string{"tuwunel", "telegram-bridge"} {
		c, er := e.inspect(ctx, name)
		if er == nil && c.State.Running {
			return nil, 0, errors.New("local container still running")
		}
		if er != nil {
			names, er := e.docker(ctx, "container", "ls", "-a", "--format", "{{.Names}}", "--filter", "name=^/"+name+"$")
			if er != nil || strings.TrimSpace(string(names)) != "" {
				return nil, 0, errors.New("cannot prove local container absent")
			}
		}
	}
	paths := []string{}
	var total uint64
	dirs, err := os.ReadDir(filepath.Join(e.Host.Root, "generations"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	for _, d := range dirs {
		if !d.IsDir() || !strings.HasPrefix(d.Name(), "recovery-") {
			continue
		}
		path := filepath.Join(e.Host.Root, "generations", d.Name())
		if s.Epoch >= s.Control.Epoch && strings.HasPrefix(s.Deployment, path+"/") {
			continue
		}
		paths = append(paths, path)
	}
	// Only retired source databases are eligible; foundation config/identity and
	// backup journals remain. A fresh standby never qualifies for these paths.
	if s.Epoch > 0 && s.Epoch < s.Control.Epoch {
		if s.Control.Completed == nil || s.Control.Completed.NextEpoch != s.Control.Epoch || s.Control.Completed.Receipt == nil {
			return nil, 0, errors.New("completed replacement recovery receipt required")
		}
		for _, path := range []string{"deploy/tuwunel/data", "deploy/telegram/data"} {
			full := filepath.Join(e.Host.Root, path)
			if _, er := os.Stat(full); er == nil {
				paths = append(paths, full)
			}
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err = local.SafePath(path); err != nil {
			return nil, 0, err
		}
		n, er := treeSize(path)
		if er != nil {
			return nil, 0, er
		}
		total += n
	}
	return paths, total, nil
}
func (e *Engine) CleanupPlan(ctx context.Context, r management.Request) (any, error) {
	paths, size, err := e.cleanupCandidates(ctx)
	if err != nil {
		return nil, err
	}
	p := CleanupPlan{1, r.ID, r.Target, r.Expected.Epoch, e.stamp(), paths, size}
	raw := release.JSON(p)
	if err = local.Atomic(filepath.Join(e.Host.Root, "agent/plans", r.ID+".json"), raw, 0600); err != nil {
		return nil, err
	}
	return map[string]any{"plan": p, "plan_sha256": local.Digest(raw)}, nil
}
func (e *Engine) CleanupApply(ctx context.Context, r management.Request) (any, error) {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	raw, err := local.Private(filepath.Join(e.Host.Root, "agent/plans", r.Parameters.Plan+".json"))
	if err != nil || local.Digest(raw) != r.Parameters.PlanHash {
		return nil, errors.New("reviewed cleanup plan differs")
	}
	var p CleanupPlan
	if local.Decode(raw, &p) != nil || p.Schema != 1 || p.Instance != r.Target || p.ID != r.Parameters.Plan || p.Epoch != r.Expected.Epoch {
		return nil, errors.New("cleanup identity mismatch")
	}
	at, err := time.Parse(time.RFC3339Nano, p.At)
	if err != nil || e.Now().Sub(at) > time.Hour || e.Now().Before(at) {
		return nil, errors.New("cleanup plan expired")
	}
	paths, _, err := e.cleanupCandidates(ctx)
	if err != nil {
		return nil, err
	}
	allow := map[string]bool{}
	for _, v := range paths {
		allow[v] = true
	}
	for _, path := range p.Paths {
		if !allow[path] {
			if _, er := os.Stat(path); errors.Is(er, os.ErrNotExist) {
				continue
			}
			return nil, errors.New("cleanup target changed")
		}
		if err = e.taskAuthority(ctx, r); err != nil {
			return nil, err
		}
		if err = local.SafePath(path); err != nil {
			return nil, err
		}
		e.progress("remove-retired-local-data")
		if err = os.RemoveAll(path); err != nil {
			return nil, err
		}
	}
	return map[string]any{"removed_paths": p.Paths, "identity_preserved": true, "remote_media_untouched": true}, nil
}

// Exit an interrupted maintenance window without silently repeating deletion.
// Restic's check must pass before the owner is allowed to be resumed separately.
func (e *Engine) RecoverMaintenance(ctx context.Context, r management.Request) (any, error) {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	s, err := e.status(ctx)
	if err != nil || s.Control == nil || s.Control.Owner != r.Target || s.Control.Phase != "MAINTENANCE" || !s.Blocked {
		return nil, errors.New("fenced maintenance owner required")
	}
	var op control.FinalOperation
	if json.Unmarshal(s.Control.Operation, &op) != nil || op.Mode != "maintenance" || !local.SafeID(op.ID) {
		return nil, errors.New("maintenance identity missing")
	}
	p, _, err := e.readPoint(ctx, "", "")
	if err != nil {
		return nil, err
	}
	repo, err := e.repository(ctx, p)
	if err != nil {
		return nil, err
	}
	if err = e.verifyRepository(ctx, repo); err != nil {
		return nil, err
	}
	e.progress("check-interrupted-maintenance")
	if _, err = e.rest(ctx, repo, "", "check"); err != nil {
		return nil, err
	}
	if err = e.action(ctx, "maintenance-end", op.ID, r.Target, s.Control.Epoch); err != nil {
		return nil, err
	}
	return map[string]any{"maintenance_ended": true, "remains_fenced": true}, nil
}
