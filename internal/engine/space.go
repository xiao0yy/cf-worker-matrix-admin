package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// Bundle, probe copy and rebuilt generation coexist. Check the selected final
// point as well as the earlier preflight point: the database may have grown.
func (e *Engine) snapshotSpace(ctx context.Context, repo Repository, snapshot string) (uint64, error) {
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
	need := size.Bytes*4 + (1 << 30)
	return need, requireSpace(e.Host.Root, need)
}
