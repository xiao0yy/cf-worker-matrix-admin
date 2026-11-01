package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requireFileOp(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func statFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(path)
	requireFileOp(t, err)
	return st
}
func TestFrozenViewLinksSparseTerabyteAndResumesWithoutCopies(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dst := filepath.Join(root, "view")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(src, "000001.sst"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(1 << 40); err != nil {
		f.Close()
		t.Skipf("filesystem cannot create sparse terabyte: %v", err)
	}
	f.Close()
	for n := 0; n < 3; n++ {
		if err = LinkFrozenTree(context.Background(), src, dst); err != nil {
			t.Fatal(err)
		}
		a := statFile(t, filepath.Join(src, "000001.sst"))
		b := statFile(t, filepath.Join(dst, "000001.sst"))
		if !os.SameFile(a, b) || b.Size() != 1<<40 {
			t.Fatal("database was copied")
		}
	}
	requireFileOp(t, os.WriteFile(filepath.Join(src, "CURRENT"), []byte("MANIFEST-1"), 0600))
	if err = LinkFrozenTree(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	requireFileOp(t, os.WriteFile(filepath.Join(dst, "unexpected"), nil, 0600))
	if err = LinkFrozenTree(context.Background(), src, dst); err == nil {
		t.Fatal("accepted additional view data")
	}
}
func TestFrozenViewRejectsCopiedFilesAndSymlinks(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		root := t.TempDir()
		src := filepath.Join(root, "src")
		dst := filepath.Join(root, "dst")
		requireFileOp(t, os.Mkdir(src, 0700))
		requireFileOp(t, os.Mkdir(dst, 0700))
		requireFileOp(t, os.WriteFile(filepath.Join(src, "CURRENT"), []byte("a"), 0600))
		if symlink {
			requireFileOp(t, os.Symlink(filepath.Join(src, "CURRENT"), filepath.Join(dst, "CURRENT")))
		} else {
			requireFileOp(t, os.WriteFile(filepath.Join(dst, "CURRENT"), []byte("a"), 0600))
		}
		if err := LinkFrozenTree(context.Background(), src, dst); err == nil {
			t.Fatal("accepted a different snapshot")
		}
	}
}
func TestProbeSharesOnlyImmutableTables(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "probe")
	requireFileOp(t, os.Mkdir(src, 0700))
	for _, name := range []string{"000001.sst", "CURRENT", "000002.log"} {
		requireFileOp(t, os.WriteFile(filepath.Join(src, name), []byte(name), 0600))
	}
	if err := CloneRocksDB(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"000001.sst", "CURRENT", "000002.log"} {
		a := statFile(t, filepath.Join(src, name))
		b := statFile(t, filepath.Join(dst, name))
		if os.SameFile(a, b) != (name == "000001.sst") {
			t.Fatal("wrong sharing", name)
		}
	}
	requireFileOp(t, os.WriteFile(filepath.Join(dst, "CURRENT"), []byte("changed"), 0600))
	b, err := os.ReadFile(filepath.Join(src, "CURRENT"))
	requireFileOp(t, err)
	if string(b) != "CURRENT" {
		t.Fatal("probe modified original metadata")
	}
}

func TestFrozenViewExcludesOnlyExplicitSourceDirectories(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "source"), filepath.Join(root, "view")
	requireFileOp(t, os.MkdirAll(filepath.Join(src, "media"), 0700))
	requireFileOp(t, os.WriteFile(filepath.Join(src, "CURRENT"), []byte("MANIFEST-1"), 0600))
	requireFileOp(t, os.Symlink("missing-media-cache", filepath.Join(src, "media", "cache")))
	for n := 0; n < 2; n++ {
		requireFileOp(t, LinkFrozenTree(context.Background(), src, dst, "media"))
	}
	if _, err := os.Lstat(filepath.Join(dst, "media")); !os.IsNotExist(err) {
		t.Fatal("traversed external media")
	}
	if !os.SameFile(statFile(t, filepath.Join(src, "CURRENT")), statFile(t, filepath.Join(dst, "CURRENT"))) {
		t.Fatal("database copied")
	}
	requireFileOp(t, os.Mkdir(filepath.Join(dst, "media"), 0700))
	if err := LinkFrozenTree(context.Background(), src, dst, "media"); err == nil || !strings.Contains(err.Error(), "already staged") {
		t.Fatalf("accepted excluded directory from an older stage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "media")); err != nil {
		t.Fatal("removed previously staged media")
	}
}

func TestFrozenViewDoesNotHideExcludedSymlink(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "source"), filepath.Join(root, "view")
	requireFileOp(t, os.Mkdir(src, 0700))
	requireFileOp(t, os.Symlink("outside", filepath.Join(src, "media")))
	if err := LinkFrozenTree(context.Background(), src, dst, "media"); err == nil || !strings.Contains(err.Error(), "must be a directory") {
		t.Fatalf("accepted an unexpected media link: %v", err)
	}
}
