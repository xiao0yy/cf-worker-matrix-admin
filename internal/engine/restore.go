package engine

import (
	"context"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
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
}

func (e *Engine) Restore(ctx context.Context, snapshot, hash string) (Restored, error) {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return Restored{}, err
	}
	defer unlock()
	return e.restore(ctx, snapshot, hash)
}
func (e *Engine) restore(ctx context.Context, snapshot, hash string) (Restored, error) {
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
	work, err := os.MkdirTemp(dir, "recovery-")
	if err != nil {
		return result, err
	}
	if err = local.Atomic(filepath.Join(work, "point.json"), raw, 0600); err != nil {
		return result, err
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
	if _, err = e.snapshotSpace(ctx, repo, point.Snapshot); err != nil {
		return result, err
	}
	e.progress("download-and-verify-databases")
	if err = e.downloadData(ctx, repo, point.Snapshot, bundle, work); err != nil {
		return result, err
	}
	for _, image := range point.Images {
		if err = e.checkImage(ctx, image, true); err != nil {
			return result, err
		}
	}
	e.progress("isolated-database-startup")
	if err = e.probe(ctx, bundle, meta, filepath.Join(work, "probe")); err != nil {
		return result, err
	}
	root := filepath.Join(work, "rebuilt")
	copies := map[string]string{"config": "deploy/tuwunel", "database": "deploy/tuwunel/data", "telegram/config": "deploy/telegram", "telegram/data": "deploy/telegram/data", "nginx": "deploy/nginx"}
	for src, dst := range copies {
		if err = local.CopyTree(filepath.Join(bundle, "payload", src), filepath.Join(root, dst)); err != nil {
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
	proof := map[string]any{"database_startup_verified": true, "telegram_database_startup_verified": true, "production_started": false, "consistency": point.Consistency, "media_included": false, "media_reused_from_s3": true}
	if err = local.JSON(filepath.Join(root, "recovery-state/result.json"), proof); err != nil {
		return result, err
	}
	result = Restored{1, point.Snapshot, local.Digest(raw), root, work, true, true, false}
	if err = local.JSON(filepath.Join(work, "result.json"), result); err != nil {
		return result, err
	}
	return result, nil
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
