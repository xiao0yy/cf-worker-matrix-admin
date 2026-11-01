package engine

import (
	"context"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/payload"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"os"
	"path/filepath"
	"strings"
)

type Restored struct {
	Schema     int    `json:"schema"`
	Snapshot   string `json:"snapshot_id"`
	Hash       string `json:"point_sha256"`
	Root       string `json:"deployment_root"`
	Work       string `json:"work_dir"`
	Matrix     bool   `json:"database_startup_verified"`
	Telegram   bool   `json:"telegram_database_startup_verified"`
	Production bool   `json:"production_started"`
	Ready      bool   `json:"restore_ready"`
}

func (e *Engine) Restore(ctx context.Context, snapshot, hash string) (Restored, error) {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return Restored{}, err
	}
	defer unlock()
	// The public restore command is an explicit, isolated recovery drill.
	return e.restoreAt(ctx, snapshot, hash, "", true)
}
func (e *Engine) restoreAt(ctx context.Context, snapshot, hash, work string, deep bool) (Restored, error) {
	var result Restored
	if _, err := release.Current(e.ReleaseDir); err != nil {
		return result, err
	}
	e.progress("read-recovery-point")
	point, raw, err := e.readPoint(ctx, snapshot, hash)
	if err != nil {
		return result, err
	}
	dir := filepath.Join(e.Host.Root, "generations")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return result, err
	}
	if err = requireSpace(dir, 1<<30); err != nil {
		return result, err
	}
	if work == "" {
		work, err = os.MkdirTemp(dir, "recovery-")
		if err != nil {
			return result, err
		}
	} else {
		if filepath.Dir(work) != dir || local.SafePath(work) != nil {
			return result, errors.New("unsafe recovery workspace")
		}
		if err = os.MkdirAll(work, 0700); err != nil {
			return result, err
		}
	}
	active, s, err := e.active(ctx)
	if err != nil {
		return result, err
	}
	if active == filepath.Join(work, "rebuilt") || (s.Handover != nil && s.Handover.Root == filepath.Join(work, "rebuilt")) {
		return result, errors.New("recovery workspace is already bound; cannot replay restore")
	}
	if previous, er := os.ReadFile(filepath.Join(work, "point.json")); er == nil {
		if string(previous) != string(raw) {
			return result, errors.New("recovery workspace belongs to another point")
		}
	} else if !errors.Is(er, os.ErrNotExist) {
		return result, er
	} else {
		entries, er := os.ReadDir(work)
		if er != nil {
			return result, er
		}
		if len(entries) != 0 {
			return result, errors.New("unclassified recovery workspace; retained for explicit reconciliation")
		}
	}
	resultFile := filepath.Join(work, "result.json")
	if _, er := os.Lstat(resultFile); er == nil {
		if err = local.ReadJSON(resultFile, &result); err != nil {
			return result, err
		}
		if result.Work != work || result.Root != filepath.Join(work, "rebuilt") || result.Snapshot != point.Snapshot || result.Hash != local.Digest(raw) || !result.Ready || result.Production {
			return result, errors.New("completed restore identity differs")
		}
		return result, finishRestore(work)
	} else if !errors.Is(er, os.ErrNotExist) {
		return result, er
	}
	if err = local.Atomic(filepath.Join(work, "point.json"), raw, 0600); err != nil {
		return result, err
	}
	if err = e.registerGeneration(generation{Schema: 1, Cluster: e.Cluster, Instance: s.Instance, Root: filepath.Join(work, "rebuilt"), Work: work, Snapshot: point.Snapshot, Hash: local.Digest(raw)}); err != nil {
		return result, err
	}
	// PREPARED is durable before discarding download/probe views. If publication
	// fails after cleanup, retry publishes the existing generation without
	// downloading, probing or replacing databases again.
	prepared := filepath.Join(work, "prepared.json")
	if _, er := os.Stat(prepared); er == nil {
		if err = local.ReadJSON(prepared, &result); err != nil {
			return result, err
		}
		if result.Work != work || result.Root != filepath.Join(work, "rebuilt") || result.Snapshot != point.Snapshot || result.Hash != local.Digest(raw) || result.Production || !result.Ready {
			return result, errors.New("prepared generation differs")
		}
		return result, e.publishRestore(result, point)
	} else if !errors.Is(er, os.ErrNotExist) {
		return result, er
	}
	// Only this unbound generation's disposable views are rebuilt. The pinned
	// restic download cache remains, so an interrupted transfer resumes in place.
	for _, name := range []string{"bundle", "probe", "rebuilt"} {
		if err = os.RemoveAll(filepath.Join(work, name)); err != nil {
			return result, err
		}
	}
	bundle, meta, err := e.configuration(ctx, point, work)
	if err != nil {
		return result, err
	}
	repo, err := e.repository(ctx, point)
	if err != nil {
		return result, err
	}
	if s, er := e.status(ctx); er != nil || s.Control == nil || s.Control.Phase == "MAINTENANCE" {
		return result, errors.New("restore requires available authority outside storage maintenance")
	}
	if err = e.verifyRepository(ctx, repo); err != nil {
		return result, err
	}
	if _, err = e.snapshotSpaceWithCache(ctx, repo, point.Snapshot, filepath.Join(work, "download"), deep); err != nil {
		return result, err
	}
	e.progress("download-databases")
	if err = e.downloadData(ctx, repo, point.Snapshot, bundle, work); err != nil {
		return result, err
	}
	if deep {
		e.progress("deep-data-check")
		if err = payload.Verify(bundle); err != nil {
			return result, err
		}
		if err = e.verifyTelegram(ctx, filepath.Join(bundle, "payload/telegram/data/telegram.db"), meta.Telegram); err != nil {
			return result, err
		}
	}
	for _, image := range point.Images {
		if err = e.checkImage(ctx, image, true); err != nil {
			return result, err
		}
	}
	if deep {
		e.progress("isolated-database-startup")
		if err = e.probe(ctx, bundle, meta, filepath.Join(work, "probe")); err != nil {
			return result, err
		}
	}
	root := filepath.Join(work, "rebuilt")
	copies := map[string]string{"config": "deploy/tuwunel", "telegram/config": "deploy/telegram", "nginx": "deploy/nginx"}
	for src, dst := range copies {
		if err = local.CopyTree(filepath.Join(bundle, "payload", src), filepath.Join(root, dst)); err != nil {
			return result, err
		}
	}
	for src, dst := range map[string]string{"database": "deploy/tuwunel/data", "telegram/data": "deploy/telegram/data"} {
		if err = os.Rename(filepath.Join(bundle, "payload", src), filepath.Join(root, dst)); err != nil {
			return result, err
		}
	}
	tmp := filepath.Join(root, "deploy/telegram/data/tmp")
	if err = os.Mkdir(tmp, 0700); err != nil {
		return result, err
	}
	if err = copyOwner(filepath.Dir(tmp), tmp); err != nil {
		return result, err
	}
	for name, image := range point.Images {
		project := filepath.Join(root, "deploy", name)
		if err = e.rewriteCompose(ctx, project, image, name == "telegram", point.Telegram.Network); err != nil {
			return result, err
		}
	}
	result = Restored{1, point.Snapshot, local.Digest(raw), root, work, deep, deep, false, true}
	if err = local.JSON(prepared, result); err != nil {
		return result, err
	}
	return result, e.publishRestore(result, point)
}
func (e *Engine) publishRestore(result Restored, point Point) error {
	if err := finishRestore(result.Work); err != nil {
		return err
	}
	proof := map[string]any{"schema": 1, "restore_ready": true, "snapshot_id": result.Snapshot, "point_sha256": result.Hash, "deployment_root": result.Root, "database_startup_verified": result.Matrix, "telegram_database_startup_verified": result.Telegram, "production_started": false, "consistency": point.Consistency, "media_included": false, "media_reused_from_s3": true}
	if err := local.JSON(filepath.Join(result.Root, "recovery-state/result.json"), proof); err != nil {
		return err
	}
	return local.JSON(filepath.Join(result.Work, "result.json"), result)
}
func finishRestore(work string) error {
	// Remove cache/probe links before the generation can be admitted as a writer.
	for _, name := range []string{"download", "probe"} {
		if err := os.RemoveAll(filepath.Join(work, name)); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) activateFiles(ctx context.Context, r Restored, p Point) error {
	if !strings.HasPrefix(r.Root, e.Host.Root+"/generations/") || r.Root != filepath.Join(r.Work, "rebuilt") {
		return errors.New("invalid restored generation")
	}
	for _, name := range []string{"tuwunel", "telegram"} {
		if _, err := e.compose(ctx, filepath.Join(r.Root, "deploy", name), "create", "--no-build"); err != nil {
			return err
		}
	}
	return e.refreshNginx(ctx, filepath.Join(r.Work, "bundle"))
}
