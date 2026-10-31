package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixture(t *testing.T) (Origin, Manifest, map[string][]byte) {
	t.Helper()
	o := Origin{1, "matrix-admin", "https://releases.example/project/download", "linux/amd64", Protocols{1, 1, 2}, "0.19.1"}
	names := []string{"release.json", "backup/recover.sh", "backup/recover-main.sh", "backup/release-common.sh", "backup/tuwunel-restic.sh", "backup/prepare-standby.sh", "backup/foundation-software.sh", "backup/bin/restic", "backup/bin/matrix-release", "control/bin/matrix-control", "control/bin/matrix-mailbox", "control/handover-worker.sh", "control/matrix-control.service", "control/matrix-handover.service", "control/matrix-handover.timer", "control/matrix-mailbox.service", "control/matrix-mailbox.timer", "control/mailbox-install.sh", "control/nginx-server.conf", "control/nginx-location.conf"}
	m := Manifest{Schema: 1, Project: "matrix-admin", Version: "0.3.0", Commit: strings.Repeat("a", 40), Platform: o.Platform, Protocols: o.Protocols, Restic: o.Restic}
	data := map[string][]byte{}
	for _, p := range names {
		b := []byte("fixture " + p + "\n")
		if p == "release.json" {
			b, _ = json.Marshal(o)
		}
		data[p] = b
		m.Files = append(m.Files, File{p, digest(b), int64(len(b)), strings.Contains(p, "/bin/") || strings.HasSuffix(p, ".sh")})
	}
	return o, m, data
}
func archiveBytes(t *testing.T, m Manifest, data map[string][]byte, mutate func(*tar.Header, int), omit bool) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for i, f := range m.Files {
		if omit && i == 0 {
			continue
		}
		h := &tar.Header{Name: f.Path, Size: f.Size, Mode: 0600, Typeflag: tar.TypeReg}
		if mutate != nil {
			mutate(h, i)
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(data[f.Path][:h.Size]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func clientFor(t *testing.T, m Manifest, archive []byte) (*http.Client, Descriptor) {
	t.Helper()
	m.Archive = Archive{"matrix-admin-linux-amd64.tar.gz", digest(archive), int64(len(archive))}
	raw, _ := json.Marshal(m)
	return &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "releases.example" {
			t.Fatalf("untrusted URL: %s", r.URL)
		}
		var b []byte
		switch r.URL.Path {
		case "/project/download/v0.3.0/matrix-admin-linux-amd64.manifest.json":
			b = raw
		case "/project/download/v0.3.0/matrix-admin-linux-amd64.tar.gz":
			b = archive
		default:
			t.Fatalf("unexpected URL %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{}}, nil
	})}, Descriptor{"matrix-admin", "0.3.0", digest(raw)}
}
func TestFetchAndDrift(t *testing.T) {
	o, m, data := fixture(t)
	archive := archiveBytes(t, m, data, nil, false)
	c, d := clientFor(t, m, archive)
	root := filepath.Join(t.TempDir(), "release")
	if err := fetch(c, o, d, root); err != nil {
		t.Fatal(err)
	}
	var installed Manifest
	if err := readJSON(filepath.Join(root, "release-manifest.json"), &installed); err != nil {
		t.Fatal(err)
	}
	if err := verify(root, installed); err != nil {
		t.Fatal(err)
	}
	if err := fetch(c, o, d, root); err == nil {
		t.Fatal("overwrote existing destination")
	}
	// Private runtime files coexist but can never be part of the public inventory.
	os.WriteFile(filepath.Join(root, "control", "s3.env"), []byte("private"), 0600)
	if err := verify(root, installed); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "backup", "recover.sh"), []byte("modified"), 0700)
	if err := verify(root, installed); err == nil {
		t.Fatal("accepted modified executable")
	}
}
func TestFetchRejectsUnsafeArchivesAtomically(t *testing.T) {
	cases := map[string]func(*tar.Header, int){
		"traversal": func(h *tar.Header, i int) {
			if i == 0 {
				h.Name = "../escape"
			}
		},
		"absolute": func(h *tar.Header, i int) {
			if i == 0 {
				h.Name = "/escape"
			}
		},
		"duplicate": func(h *tar.Header, i int) {
			if i == 1 {
				h.Name = "release.json"
			}
		},
		"symlink": func(h *tar.Header, i int) {
			if i == 0 {
				h.Typeflag = tar.TypeSymlink
				h.Linkname = "/etc/passwd"
				h.Size = 0
			}
		},
		"hardlink": func(h *tar.Header, i int) {
			if i == 0 {
				h.Typeflag = tar.TypeLink
				h.Linkname = "/etc/passwd"
				h.Size = 0
			}
		},
		"short": func(h *tar.Header, i int) {
			if i == 0 {
				h.Size = 1
			}
		},
		"unlisted": func(h *tar.Header, i int) {
			if i == 0 {
				h.Name = "control/s3.env"
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o, m, data := fixture(t)
			c, d := clientFor(t, m, archiveBytes(t, m, data, mutate, false))
			parent := t.TempDir()
			dest := filepath.Join(parent, "release")
			if err := fetch(c, o, d, dest); err == nil {
				t.Fatal("unsafe extraction succeeded")
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 0 {
				t.Fatal("failed fetch left a partial installation")
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		o, m, data := fixture(t)
		c, d := clientFor(t, m, archiveBytes(t, m, data, nil, true))
		if fetch(c, o, d, filepath.Join(t.TempDir(), "out")) == nil {
			t.Fatal("accepted incomplete archive")
		}
	})
}
func TestManifestPolicy(t *testing.T) {
	_, m, _ := fixture(t)
	m.Archive = Archive{"matrix-admin-linux-amd64.tar.gz", strings.Repeat("b", 64), 123}
	if err := validateManifest(m); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"control/s3.env", "control/local.json", "control/state/journal.json", "backup/restic.env", "../recover.sh"} {
		t.Run(p, func(t *testing.T) {
			n := m
			n.Files = append(append([]File(nil), m.Files...), File{p, strings.Repeat("a", 64), 10, false})
			if validateManifest(n) == nil {
				t.Fatal("allowed private/unsafe file")
			}
		})
	}
	n := m
	n.Protocols.Recovery = 99
	if validateManifest(n) == nil {
		t.Fatal("accepted unknown protocol")
	}
	n = m
	n.Archive.Size = maxArchive + 1
	if validateManifest(n) == nil {
		t.Fatal("accepted oversized archive")
	}
}
func TestPinAndOrigin(t *testing.T) {
	o, m, data := fixture(t)
	c, d := clientFor(t, m, archiveBytes(t, m, data, nil, false))
	dest := filepath.Join(t.TempDir(), "release")
	bad := d
	bad.SHA = strings.Repeat("f", 64)
	if fetch(c, o, bad, dest) == nil {
		t.Fatal("accepted wrong manifest hash")
	}
	bad = d
	bad.Version = "../../evil"
	if fetch(c, o, bad, dest) == nil {
		t.Fatal("accepted untrusted version path")
	}
	for _, base := range []string{"http://releases.example/x", "https://user:pass@releases.example/x", "https://releases.example/x?x=y"} {
		n := o
		n.Base = base
		if fetch(c, n, d, dest) == nil {
			t.Fatal("accepted unsafe origin")
		}
	}
	if decode([]byte(`{"project":"matrix-admin","version":"0.3.0","manifest_sha256":"a","url":"https://evil.example"}`), &bad) == nil {
		t.Fatal("accepted redirect URL from data bucket")
	}
}

func TestOldReleaseRetainsItsOwnResticDependency(t *testing.T) {
	o, m, data := fixture(t)
	c, d := clientFor(t, m, archiveBytes(t, m, data, nil, false))
	// A later bootstrap may use a newer dependency. It must still fetch the
	// exact earlier runtime, including that release's own pinned restic.
	o.Restic = "0.20.0"
	if err := fetch(c, o, d, filepath.Join(t.TempDir(), "old-release")); err != nil {
		t.Fatal(err)
	}
}
func TestVerifyRejectsSymlinkRootAndMembers(t *testing.T) {
	o, m, data := fixture(t)
	c, d := clientFor(t, m, archiveBytes(t, m, data, nil, false))
	dir := t.TempDir()
	root := filepath.Join(dir, "release")
	if err := fetch(c, o, d, root); err != nil {
		t.Fatal(err)
	}
	readJSON(filepath.Join(root, "release-manifest.json"), &m)
	alias := filepath.Join(dir, "alias")
	os.Symlink(root, alias)
	if verify(alias, m) == nil {
		t.Fatal("accepted symlink root")
	}
	p := filepath.Join(root, "backup", "recover.sh")
	os.Remove(p)
	os.Symlink(filepath.Join(root, "backup", "recover-main.sh"), p)
	if verify(root, m) == nil {
		t.Fatal("accepted symlink member")
	}
}
