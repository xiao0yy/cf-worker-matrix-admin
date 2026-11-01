package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
)

// BackupService owns data preparation, transfer, verification and recovery.
// Handover only consumes its durable readiness receipts. Implementations must
// return Ready only after their own policy succeeds; the cluster never repeats
// that policy or inspects database contents to decide whether to promote.
type BackupService interface {
	Final(context.Context, string, string) (control.FinalReceipt, error)
	Restore(context.Context, string, string, string) (Restored, error)
}

type nativeBackupService struct{ e *Engine }

func (e *Engine) backupService() BackupService {
	if e.Backups != nil {
		return e.Backups
	}
	return nativeBackupService{e}
}

func (b nativeBackupService) Final(ctx context.Context, root, op string) (control.FinalReceipt, error) {
	var receipt control.FinalReceipt
	state := filepath.Join(root, "backup-state/tuwunel-restic")
	checkpoint := filepath.Join(root, "deploy/tuwunel/data/checkpoint-restic")
	if _, err := os.Stat(checkpoint); err == nil {
		if _, err = os.Stat(filepath.Join(state, "preparing/request.json")); err != nil {
			return receipt, errors.New("unclassified interrupted checkpoint; source retained")
		}
		dst := filepath.Join(state, "finals", op, "interrupted-checkpoint")
		if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return receipt, err
		}
		if err = os.Rename(checkpoint, dst); err != nil {
			return receipt, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return receipt, err
	}
	published := filepath.Join(state, "finals", op, "published.json")
	if _, err := os.Stat(published); errors.Is(err, os.ErrNotExist) {
		if err = b.e.backup(ctx, root, op, false); err != nil {
			return receipt, err
		}
	} else if err != nil {
		return receipt, err
	}
	err := local.ReadJSON(published, &receipt)
	return receipt, err
}

func (b nativeBackupService) Restore(ctx context.Context, snapshot, hash, work string) (Restored, error) {
	return b.e.restoreAt(ctx, snapshot, hash, work)
}
