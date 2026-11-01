package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func requireSpace(path string, bytes uint64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return err
	}
	if st.Bavail*uint64(st.Bsize) < bytes {
		return errors.New("insufficient working space; existing data and recovery point retained")
	}
	return nil
}
func treeSize(path string) (uint64, error) {
	var size uint64
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		s, err := d.Info()
		if err != nil {
			return err
		}
		if !s.Mode().IsRegular() {
			return errors.New("nonregular payload")
		}
		size += uint64(s.Size())
		return nil
	})
	return size, err
}

// A normal restore needs one database plus working headroom. Only an explicit
// drill creates probe copies of mutable files (SSTs remain hard linked).
func (e *Engine) snapshotSpace(ctx context.Context, repo Repository, snapshot string) (uint64, error) {
	return e.snapshotSpaceWithCache(ctx, repo, snapshot, "", false)
}
func (e *Engine) snapshotSpaceWithCache(ctx context.Context, repo Repository, snapshot, cache string, deep bool) (uint64, error) {
	stats, err := e.rest(ctx, repo, "", "stats", snapshot, "--mode", "restore-size", "--json")
	if err != nil {
		return 0, err
	}
	var size struct {
		Bytes uint64 `json:"total_size"`
	}
	if json.Unmarshal(stats, &size) != nil || size.Bytes == 0 || size.Bytes > 1<<60 {
		return 0, errors.New("invalid snapshot size")
	}
	files, err := e.databaseInventory(ctx, repo, snapshot)
	if err != nil {
		return 0, err
	}
	var total, mutable uint64
	for path, n := range files {
		if uint64(n) > 1<<60 || total > (1<<60)-uint64(n) {
			return 0, errors.New("invalid snapshot inventory size")
		}
		total += uint64(n)
		if !strings.HasPrefix(path, "/bundle/payload/database/") || !strings.HasSuffix(path, ".sst") {
			mutable += uint64(n)
		}
	}
	if total != size.Bytes {
		return 0, errors.New("snapshot inventory and restore size differ")
	}
	need := total + (1 << 30)
	if deep {
		need += mutable
	}
	remaining := need
	if cache != "" {
		allocated, err := allocatedSize(cache)
		if err != nil {
			return 0, err
		}
		if allocated > total {
			allocated = total
		}
		remaining -= allocated
	}
	return need, requireSpace(e.Host.Root, remaining)
}
func allocatedSize(path string) (uint64, error) {
	var size uint64
	seen := map[[2]uint64]bool{}
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) && p == path {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return errors.New("nonregular restore cache")
		}
		s := st.Sys().(*syscall.Stat_t)
		key := [2]uint64{uint64(s.Dev), s.Ino}
		if !seen[key] {
			seen[key] = true
			size += uint64(s.Blocks) * 512
		}
		return nil
	})
	return size, err
}
