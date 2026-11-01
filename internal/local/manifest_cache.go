package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type tableHash struct {
	Device   uint64 `json:"device"`
	Inode    uint64 `json:"inode"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified_ns"`
	Hash     string `json:"sha256"`
}
type tableHashes struct {
	Schema int                  `json:"schema"`
	Files  map[string]tableHash `json:"files"`
}

func tableIdentity(st os.FileInfo) tableHash {
	s := st.Sys().(*syscall.Stat_t)
	return tableHash{Device: uint64(s.Dev), Inode: s.Ino, Size: st.Size(), Modified: st.ModTime().UnixNano()}
}

// ManifestCached is for a private, frozen RocksDB checkpoint only. SST contents
// are immutable; link creation changes ctime, not content. Cache by relative
// name, device, inode, size and mtime, never by filename/size alone. Mutable files
// are always hashed. This is change detection, not a bit-rot scrub. A cache miss
// (including a new host) reads the file normally. The cache is not transported.
func ManifestCached(ctx context.Context, root, cache string) (string, error) {
	return manifestCached(ctx, root, cache, FileHash)
}
func manifestCached(ctx context.Context, root, cache string, hash func(string) (string, error)) (string, error) {
	if err := SafePath(root); err != nil {
		return "", err
	}
	if err := SafePath(cache); err != nil {
		return "", err
	}
	var previous tableHashes
	if err := ReadJSON(cache, &previous); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if previous.Schema != 0 && previous.Schema != 1 {
		return "", errors.New("unsupported SST hash cache")
	}
	next := tableHashes{Schema: 1, Files: map[string]tableHash{}}
	var lines []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
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
			return errors.New("nonregular checkpoint file")
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if strings.ContainsAny(rel, "\r\n\\") {
			return errors.New("invalid payload path")
		}
		immutable := filepath.Dir(rel) == "database" && filepath.Ext(rel) == ".sst"
		id := tableIdentity(st)
		old := previous.Files[rel]
		savedHash := old.Hash
		old.Hash = ""
		h := ""
		if immutable && old == id && SHA.MatchString(savedHash) {
			h = savedHash
		} else {
			h, err = hash(p)
			if err != nil {
				return err
			}
			after, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if !after.Mode().IsRegular() || tableIdentity(after) != id {
				return errors.New("checkpoint file changed while hashing")
			}
		}
		if immutable {
			id.Hash = h
			next.Files[rel] = id
		}
		lines = append(lines, h+"  ./"+filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i][68:] < lines[j][68:] })
	if err = JSON(cache, next); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n") + "\n", nil
}
