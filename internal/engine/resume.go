package engine

import (
	"context"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"os"
	"path/filepath"
	"time"
)

// Resume never rewrites the authority. Only the already admitted owner can resume.
func (e *Engine) Resume(ctx context.Context, id string, epoch uint64) error {
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(e.Host.Root, "agent/pending-update.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("complete the pending code update before resuming")
	}

	if !local.SafeID(id) || epoch == 0 {
		return errors.New("explicit instance and epoch required")
	}
	if err := e.action(ctx, "resume", "", id, epoch); err != nil {
		return err
	}
	if err := e.wait(ctx, 3*time.Minute, func() error {
		s, err := e.status(ctx)
		if err != nil {
			return err
		}
		if !s.Serving || s.Blocked || s.Instance != id || s.Epoch != epoch {
			return errors.New("owner not healthy")
		}
		return nil
	}); err != nil {
		return err
	}
	return e.system(ctx, "enable", "--now", "matrix-backup.timer")
}
