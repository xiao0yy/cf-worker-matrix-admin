package engine

import (
	"context"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"os"
	"path/filepath"
	"time"
)

type Update struct {
	Schema          int       `json:"schema"`
	Runtime         Installed `json:"runtime"`
	Identity        string    `json:"identity_sha256"`
	Journal         string    `json:"journal_sha256"`
	Generation      string    `json:"generation_sha256,omitempty"`
	Phase           string    `json:"phase"`
	BackupWasActive bool      `json:"backup_was_active"`
}

// Polling may advance observations while a failed update remains fenced. Keep
// data ownership, service binding and unfinished operations immutable on retry.
func upgradeGeneration(j control.Journal) string {
	j.HighRevision, j.HighEpoch, j.Reason = 0, 0, ""
	return local.Digest(release.JSON(j))
}

func sameUpdateGeneration(update Update, journal control.Journal, path string) bool {
	if update.Generation != "" {
		return update.Generation == upgradeGeneration(journal)
	}
	hash, err := local.FileHash(path)
	return err == nil && hash == update.Journal
}

func (e *Engine) Upgrade(ctx context.Context, source string, apply bool) (result any, err error) {
	manifest, err := release.Verify(source)
	if err != nil {
		return nil, err
	}
	unlock, err := local.Lock(filepath.Join(e.Host.Root, "agent/operation.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	pending := filepath.Join(e.Host.Root, "agent/pending-update.json")
	identity, err := local.FileHash(e.controlConfig())
	if err != nil {
		return nil, err
	}
	var cfg control.Config
	if err = local.ReadJSON(e.controlConfig(), &cfg); err != nil {
		return nil, err
	}
	locks := []string{filepath.Join(cfg.StateDir, "handover-worker.lock"), filepath.Join(cfg.StateDir, "mailbox.lock")}
	for _, path := range locks {
		close, err := local.Lock(path)
		if err != nil {
			return nil, err
		}
		defer close()
	}
	var journal control.Journal
	if err = local.ReadJSON(filepath.Join(cfg.StateDir, "journal.json"), &journal); err != nil {
		return nil, err
	}
	if journal.Cluster != cfg.Cluster || journal.Instance != cfg.Instance || !journal.Blocked || journal.Target != nil && (journal.Target.MayWrite && journal.Target.Phase != "ONLINE" || journal.Target.Phase == "STARTING" || journal.Target.Phase == "COMMITTING") {
		return nil, errors.New("upgrade requires a fenced instance outside STARTING; preserve identity/data")
	}
	root := journal.Deployment
	if root == "" {
		root = e.Host.Root
	}
	close, err := local.Lock(filepath.Join(root, "backup-state/tuwunel-restic/operation.lock"))
	if err != nil {
		return nil, err
	}
	defer close()
	var update Update
	resume := false
	if _, err = os.Stat(pending); err == nil {
		if err = local.ReadJSON(pending, &update); err != nil {
			return nil, err
		}
		if update.Schema != 1 || update.Identity != identity || update.Runtime.Version != manifest.Version || update.Runtime.Commit != manifest.Commit {
			return nil, errors.New("resume the same pending update and instance")
		}
		if update.Journal != "" && update.Phase == "STOPPED" {
			if !sameUpdateGeneration(update, journal, filepath.Join(cfg.StateDir, "journal.json")) {
				return nil, errors.New("journal changed after update stop; inspect before retry")
			}
		}
		resume = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !resume {
		s, err := e.status(ctx)
		if err != nil {
			return nil, err
		}
		if !s.Blocked || s.Serving || s.Control == nil || s.Control.Phase != "ONLINE" || s.Final != nil && s.Final.Phase != "RETIRED" {
			return nil, errors.New("controller does not authorize a code upgrade in this phase")
		}
	}
	result = map[string]any{"version": manifest.Version, "commit": manifest.Commit, "apply": apply, "identity_preserved": true, "data_restore": false}
	if !apply {
		return result, nil
	}
	installed, err := e.installRuntime(source)
	if err != nil {
		return nil, err
	}

	// Even a failed installation must restore the management channel. Business
	// admission remains fenced; a pending update is retried explicitly.
	defer func() {
		if err != nil {
			recovery, done := context.WithTimeout(context.Background(), 2*time.Minute)
			defer done()
			_ = e.system(recovery, "daemon-reload")
			_ = e.system(recovery, "start", "matrix-control.service")
			_ = e.system(recovery, "enable", "--now", "matrix-mailbox.timer", "matrix-tasks.timer")
		}
	}()
	if !resume {
		_, active := e.run(ctx, e.Host.Systemctl, "is-active", "--quiet", "matrix-backup.timer")
		if active != nil {
			_, active = e.run(ctx, e.Host.Systemctl, "is-active", "--quiet", "tuwunel-restic.timer")
		}
		update = Update{Schema: 1, Runtime: installed, Identity: identity, Phase: "PREPARED", BackupWasActive: active == nil}
		if err = local.JSON(pending, update); err != nil {
			return nil, err
		}
	}
	if update.Phase == "PREPARED" {
		for _, timer := range []string{"matrix-mailbox.timer", "matrix-handover.timer", "matrix-backup.timer"} {
			if _, statErr := os.Stat(filepath.Join(e.Host.SystemdDirectory, timer)); errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if err = e.system(ctx, "stop", timer); err != nil {
				return nil, err
			}
		}
		if _, err = os.Stat(filepath.Join(e.Host.SystemdDirectory, "tuwunel-restic.timer")); err == nil {
			if err = e.system(ctx, "disable", "--now", "tuwunel-restic.timer"); err != nil {
				return nil, err
			}
		}
		if err = e.system(ctx, "stop", "matrix-control.service"); err != nil {
			return nil, err
		}
		var stopped control.Journal
		if err = local.ReadJSON(filepath.Join(cfg.StateDir, "journal.json"), &stopped); err != nil {
			return nil, err
		}
		if !stopped.Blocked || stopped.Instance != cfg.Instance || stopped.BoundEpoch != journal.BoundEpoch {
			return nil, errors.New("unsafe stopped journal")
		}
		update.Journal, err = local.FileHash(filepath.Join(cfg.StateDir, "journal.json"))
		if err != nil {
			return nil, err
		}
		update.Phase = "STOPPED"
		update.Generation = upgradeGeneration(stopped)
		if err = local.JSON(pending, update); err != nil {
			return nil, err
		}
	}
	if update.Phase == "STOPPED" {
		if resume {
			// Failure recovery restarts the management channel. Stop its guard
			// again and recheck the durable binding before replacing unit files.
			if err = e.system(ctx, "stop", "matrix-control.service"); err != nil {
				return nil, err
			}
			var stopped control.Journal
			path := filepath.Join(cfg.StateDir, "journal.json")
			if err = local.ReadJSON(path, &stopped); err != nil {
				return nil, err
			}
			if !sameUpdateGeneration(update, stopped, path) {
				return nil, errors.New("data binding changed during update retry")
			}
		}
		if err = e.writeUnits(installed); err != nil {
			return nil, err
		}
		if err = local.JSON(filepath.Join(e.Host.Root, "agent/host.json"), e.Host); err != nil {
			return nil, err
		}
		if err = local.JSON(filepath.Join(e.Host.Root, "agent/runtime.json"), installed); err != nil {
			return nil, err
		}
		if err = e.system(ctx, "daemon-reload"); err != nil {
			return nil, err
		}
		update.Phase = "INSTALLED"
		if err = local.JSON(pending, update); err != nil {
			return nil, err
		}
	}
	if update.Phase == "INSTALLED" {
		if err = e.system(ctx, "start", "matrix-control.service"); err != nil {
			return nil, err
		}
		s, err := e.status(ctx)
		if err != nil {
			return nil, err
		}
		if !s.Blocked || s.Serving || s.Instance != cfg.Instance || s.Epoch != journal.BoundEpoch {
			return nil, errors.New("updated guard has unexpected authorization")
		}
		after, err := local.FileHash(e.controlConfig())
		if err != nil || after != identity {
			return nil, errors.New("instance identity changed")
		}
		update.Phase = "COMPLETE"
		if err = local.JSON(pending, update); err != nil {
			return nil, err
		}
	}
	if update.Phase != "COMPLETE" {
		return nil, errors.New("unknown update phase")
	}
	if err = e.system(ctx, "enable", "--now", "matrix-mailbox.timer", "matrix-handover.timer", "matrix-tasks.timer"); err != nil {
		return nil, err
	}
	if update.BackupWasActive {
		if err = e.system(ctx, "enable", "matrix-backup.timer"); err != nil {
			return nil, err
		}
	}
	if err = local.JSON(filepath.Join(e.Host.Root, "agent/updates", installed.Version+".json"), update); err != nil {
		return nil, err
	}
	return result, os.Remove(pending)
}
