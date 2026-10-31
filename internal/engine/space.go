package engine

import (
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
