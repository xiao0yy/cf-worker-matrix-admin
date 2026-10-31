package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	m := Manifest{Schema: 1, Project: "matrix-admin", Version: "0.2.0", Commit: strings.Repeat("a", 40), Platform: "linux/amd64", Runtime: "native-go-v1", RecoveryReaders: []int{2}, Control: 1, Mailbox: 1, Restic: ResticVersion}
	for _, n := range []string{"matrix-admin", "restic", "AGENT.md", "LICENSE", "THIRD-PARTY.md"} {
		b := []byte("fixture-" + n)
		ex := n == "matrix-admin" || n == "restic"
		mode := os.FileMode(0600)
		if ex {
			mode = 0700
		}
		if e := os.WriteFile(filepath.Join(dir, n), b, mode); e != nil {
			t.Fatal(e)
		}
		m.Files = append(m.Files, File{n, local.Digest(b), int64(len(b)), ex})
	}
	if e := local.JSON(filepath.Join(dir, "manifest.json"), m); e != nil {
		t.Fatal(e)
	}
	return dir
}
func archive(t *testing.T, dir, extra string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		tw.WriteHeader(&tar.Header{Name: f.Name(), Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg})
		tw.Write(data)
	}
	if extra != "" {
		tw.WriteHeader(&tar.Header{Name: extra, Typeflag: tar.TypeSymlink, Linkname: "/tmp/escape"})
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

type fakeHTTP func(*http.Request) (*http.Response, error)

func (f fakeHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestFetchRejectsUntrustedLayoutAndPinsVersion(t *testing.T) {
	for _, extra := range []string{"", "../escape", "backup/run.sh", "matrix-admin"} {
		t.Run(extra, func(t *testing.T) {
			dir := fixture(t)
			data := archive(t, dir, extra)
			c := &http.Client{Transport: fakeHTTP(func(r *http.Request) (*http.Response, error) {
				var b []byte
				switch {
				case strings.HasSuffix(r.URL.Path, "SHA256SUMS"):
					b = []byte(local.Digest(data) + "  matrix-admin-linux-amd64.tar.gz\n")
				case strings.HasSuffix(r.URL.Path, ".tar.gz"):
					b = data
				default:
					return nil, errors.New("unexpected URL")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}, nil
			})}
			target := filepath.Join(t.TempDir(), "new")
			hash, _ := local.FileHash(filepath.Join(dir, "manifest.json"))
			e := fetch(context.Background(), c, "0.2.0", hash, target)
			if extra == "" {
				if e != nil {
					t.Fatal(e)
				}
				if _, e = Verify(target); e != nil {
					t.Fatal(e)
				}
				if e = fetch(context.Background(), c, "0.2.0", hash, target); e == nil {
					t.Fatal("overwrote existing runtime")
				}
			} else {
				if e == nil {
					t.Fatal("unsafe member accepted")
				}
				if _, e = os.Stat(target); !os.IsNotExist(e) {
					t.Fatal("published partial runtime")
				}
			}
		})
	}
}
func TestInventoryRejectsChanges(t *testing.T) {
	for _, kind := range []string{"missing", "extra", "corrupt", "link", "mode"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixture(t)
			p := filepath.Join(dir, "matrix-admin")
			switch kind {
			case "missing":
				os.Remove(p)
			case "extra":
				os.WriteFile(filepath.Join(dir, "run.sh"), []byte("no"), 0600)
			case "corrupt":
				os.WriteFile(p, []byte("corrupt"), 0700)
			case "link":
				os.Remove(p)
				os.Symlink("/bin/true", p)
			case "mode":
				os.Chmod(p, 0600)
			}
			if _, e := Verify(dir); e == nil {
				t.Fatal("accepted modified runtime")
			}
		})
	}
}
