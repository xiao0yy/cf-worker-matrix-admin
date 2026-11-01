package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestCacheOnlyReusesUnchangedImmutableTables(t *testing.T) {
	root := t.TempDir()
	payload := filepath.Join(root, "payload")
	cache := filepath.Join(root, "hashes.json")
	put := func(name, body string) {
		t.Helper()
		if err := Atomic(filepath.Join(payload, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put("database/000001.sst", "table")
	put("database/CURRENT", "mutable")
	reads := map[string]int{}
	hash := func(p string) (string, error) { reads[filepath.Base(p)]++; return FileHash(p) }
	scan := func() {
		t.Helper()
		got, err := manifestCached(context.Background(), payload, cache, hash)
		if err != nil {
			t.Fatal(err)
		}
		want, err := Manifest(payload)
		if err != nil || got != want {
			t.Fatalf("manifest mismatch: %v", err)
		}
	}
	scan()
	if err := os.Link(filepath.Join(payload, "database/000001.sst"), filepath.Join(root, "checkpoint-table")); err != nil {
		t.Fatal(err)
	}
	scan()
	if reads["000001.sst"] != 1 || reads["CURRENT"] != 2 {
		t.Fatal(reads)
	}
	// A replacement inode with the same name, size and mtime must be rehashed.
	st, err := os.Stat(filepath.Join(payload, "database/000001.sst"))
	if err != nil {
		t.Fatal(err)
	}
	put("database/000001.sst", "other")
	if err = os.Chtimes(filepath.Join(payload, "database/000001.sst"), st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	put("database/CURRENT", "changed")
	scan()
	if reads["000001.sst"] != 2 || reads["CURRENT"] != 3 {
		t.Fatal(reads)
	}
	// In-place changes invalidate a table by size/mtime as well.
	if err = os.WriteFile(filepath.Join(payload, "database/000001.sst"), []byte("bigger table"), 0600); err != nil {
		t.Fatal(err)
	}
	scan()
	if reads["000001.sst"] != 3 {
		t.Fatal(reads)
	}
}
func TestManifestCacheDetectsChangingSourceAndSymlink(t *testing.T) {
	root := t.TempDir()
	payload := filepath.Join(root, "payload")
	p := filepath.Join(payload, "database/000001.sst")
	cache := filepath.Join(root, "cache.json")
	if err := Atomic(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := manifestCached(context.Background(), payload, cache, func(p string) (string, error) {
		h, e := FileHash(p)
		if e == nil {
			e = os.WriteFile(p, []byte("changed size"), 0600)
		}
		return h, e
	})
	if err == nil {
		t.Fatal("accepted concurrent mutation")
	}
	if err = os.Symlink(p, filepath.Join(payload, "database/000002.sst")); err != nil {
		t.Fatal(err)
	}
	if _, err = ManifestCached(context.Background(), payload, cache); err == nil {
		t.Fatal("accepted symlink")
	}
}
