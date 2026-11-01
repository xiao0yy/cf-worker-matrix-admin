package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// LinkFrozenTree is only for a database whose writers have been stopped and
// fenced by the controller. It creates no second copy of database bytes. A retry
// may complete the same view, but cannot adopt different or additional files.
// Cross-filesystem layouts fail explicitly instead of falling back to a copy.
// excludedDirectories names direct child directories owned by another storage
// provider. They are never traversed; callers must establish that provider first.
func LinkFrozenTree(ctx context.Context, src, dst string, excludedDirectories ...string) error {
	excluded := make(map[string]bool, len(excludedDirectories))
	for _, name := range excludedDirectories {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
			return errors.New("excluded directory must be a direct child name")
		}
		excluded[name] = true
	}
	if err := SafePath(src); err != nil {
		return err
	}
	if err := SafePath(dst); err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("database source must be a directory")
	}

	if src == dst || strings.HasPrefix(dst, src+"/") || strings.HasPrefix(src, dst+"/") {
		return errors.New("database view overlaps source")
	}
	if err := filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) && p == dst {
			return nil
		}
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil {
			return err
		}
		if excluded[rel] {
			return fmt.Errorf("excluded directory %q is already staged; retain it and start a new backup operation", rel)
		}
		a, err := os.Lstat(filepath.Join(src, rel))
		if err != nil {
			return err
		}
		b, err := d.Info()
		if err != nil {
			return err
		}
		if a.IsDir() && b.IsDir() {
			return nil
		}
		if !a.Mode().IsRegular() || !b.Mode().IsRegular() || !os.SameFile(a, b) {
			return fmt.Errorf("existing frozen view differs at %q; retained for explicit reconciliation", rel)
		}
		return nil
	}); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if excluded[rel] {
			if !d.IsDir() {
				return fmt.Errorf("excluded path %q must be a directory", rel)
			}
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		st, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return mirrorDirectory(target, st)
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("nonregular database file: %q", rel)
		}
		if b, er := os.Lstat(target); er == nil {
			if b.Mode().IsRegular() && os.SameFile(st, b) {
				return nil
			}
			return fmt.Errorf("database view collision: %q", rel)
		} else if !os.IsNotExist(er) {
			return er
		}
		if err = os.Link(p, target); err != nil {
			return fmt.Errorf("database hard link requires the same filesystem: %w", err)
		}
		return nil
	})
}

func mirrorDirectory(path string, st os.FileInfo) error {
	if err := SafePath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		if err := os.Chown(path, int(s.Uid), int(s.Gid)); err != nil {
			return err
		}
	}
	return os.Chmod(path, st.Mode().Perm())
}

// CloneRocksDB copies mutable metadata and WAL, but shares immutable SST files
// with the read-only restore bundle. Probe compaction may create new files or
// unlink its links; it must never run against the original mutable metadata.
func CloneRocksDB(ctx context.Context, src, dst string) error {
	if err := SafePath(src); err != nil {
		return err
	}
	if err := SafePath(dst); err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("database source must be a directory")
	}

	if src == dst || strings.HasPrefix(dst, src+"/") || strings.HasPrefix(src, dst+"/") {
		return errors.New("probe database overlaps source")
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		return errors.New("probe database destination must be new")
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		st, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return mirrorDirectory(target, st)
		}
		if !st.Mode().IsRegular() {
			return errors.New("nonregular probe database file")
		}
		if filepath.Ext(rel) == ".sst" {
			return os.Link(p, target)
		}
		return Copy(p, target)
	})
}
