package management

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/storage"
	"os"
	"path/filepath"
	"time"
)

type Store interface {
	Get(context.Context, string) ([]byte, string, error)
	Put(context.Context, string, []byte, string) error
}

// Progress and terminal completion may be published by different processes.
// Conditional writes prevent an older heartbeat from hiding a final result.
func Publish(ctx context.Context, root string, t Task, s Store, key string) error {
	raw := release.JSON(t)
	digest := local.Digest(raw)
	marker := filepath.Join(Directory(root), t.Request.ID+".uploaded")
	if b, err := os.ReadFile(marker); err == nil && string(b) == digest {
		return nil
	}
	previous, etag, err := s.Get(ctx, key)
	if errors.Is(err, storage.Missing) {
		etag = "*"
	} else if err != nil {
		return err
	} else {
		var prior Task
		if json.Unmarshal(previous, &prior) != nil || string(release.JSON(prior.Request)) != string(release.JSON(t.Request)) {
			return errors.New("remote task identity conflict")
		}
		before, er := time.Parse(time.RFC3339Nano, prior.Updated)
		if er != nil {
			return er
		}
		now, er := time.Parse(time.RFC3339Nano, t.Updated)
		if er != nil {
			return er
		}
		if before.After(now) || (before.Equal(now) && Terminal(prior.State) && !Terminal(t.State)) {
			return nil
		}
	}
	if err = s.Put(ctx, key, raw, etag); err != nil {
		return err
	}
	return local.Atomic(marker, []byte(digest), 0600)
}
