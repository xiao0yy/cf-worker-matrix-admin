// Package release owns one immutable runtime: matrix-admin and pinned restic.
package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var Version = "dev"
var Commit = "unknown"

const Origin = "https://github.com/xiao0yy/cf-worker-matrix-admin/releases/download"
const ResticVersion = "0.19.1"

var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[a-zA-Z0-9.-]+)?$`)

type File struct {
	Path       string `json:"path"`
	SHA        string `json:"sha256"`
	Size       int64  `json:"size"`
	Executable bool   `json:"executable"`
}
type Manifest struct {
	Schema          int    `json:"schema"`
	Project         string `json:"project"`
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	Platform        string `json:"platform"`
	Runtime         string `json:"runtime"`
	RecoveryReaders []int  `json:"recovery_readers"`
	Control         int    `json:"control_protocol"`
	Mailbox         int    `json:"mailbox_protocol"`
	Restic          string `json:"restic_version"`
	Files           []File `json:"files"`
}
type Descriptor struct {
	Project string `json:"project"`
	Version string `json:"version"`
	SHA     string `json:"manifest_sha256"`
}

func Info() any {
	return map[string]any{"version": Version, "commit": Commit, "runtime": "native-go-v1", "recovery_readers": []int{2}, "control_protocol": 1, "mailbox_protocol": 1, "restic_version": ResticVersion}
}
func ValidVersion(v string) bool { return versionRE.MatchString(v) }
func allowed(p string) bool {
	switch p {
	case "matrix-admin", "restic", "AGENT.md", "LICENSE", "THIRD-PARTY.md":
		return true
	}
	return false
}
func Verify(dir string) (Manifest, error) {
	var m Manifest
	if err := local.SafePath(dir); err != nil {
		return m, err
	}
	if err := local.ReadJSON(filepath.Join(dir, "manifest.json"), &m); err != nil {
		return m, err
	}
	if m.Schema != 1 || m.Project != "matrix-admin" || !ValidVersion(m.Version) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(m.Commit) || m.Runtime != "native-go-v1" || m.Platform != "linux/amd64" || m.Control != 1 || m.Mailbox != 1 || m.Restic != ResticVersion || len(m.RecoveryReaders) != 1 || m.RecoveryReaders[0] != 2 || len(m.Files) != 5 {
		return m, errors.New("unsupported runtime manifest")
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		p := filepath.Join(dir, f.Path)
		if !allowed(f.Path) || seen[f.Path] || !local.SHA.MatchString(f.SHA) || f.Size <= 0 || f.Size > 128<<20 || f.Executable != (f.Path == "matrix-admin" || f.Path == "restic") {
			return m, errors.New("invalid runtime inventory")
		}
		seen[f.Path] = true
		if err := local.SafePath(p); err != nil {
			return m, err
		}
		s, err := os.Lstat(p)
		if err != nil {
			return m, err
		}
		if !s.Mode().IsRegular() || s.Size() != f.Size || (s.Mode()&0111 != 0) != f.Executable {
			return m, errors.New("runtime file size/mode mismatch")
		}
		h, err := local.FileHash(p)
		if err != nil || h != f.SHA {
			return m, errors.New("runtime file hash mismatch")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return m, err
	}
	if len(entries) != 6 {
		return m, errors.New("unexpected runtime files")
	}
	return m, nil
}
func Describe(dir string) (Descriptor, error) {
	m, err := Verify(dir)
	if err != nil {
		return Descriptor{}, err
	}
	h, err := local.FileHash(filepath.Join(dir, "manifest.json"))
	return Descriptor{m.Project, m.Version, h}, err
}
func Current(dir string) (Descriptor, error) {
	d, err := Describe(dir)
	if err != nil {
		return d, err
	}
	if d.Version != Version {
		return d, errors.New("running binary and installed version differ")
	}
	h, err := local.FileHash(filepath.Join(dir, "matrix-admin"))
	if err != nil {
		return d, err
	}
	self, err := os.Executable()
	if err != nil {
		return d, err
	}
	got, err := local.FileHash(self)
	if err != nil || got != h {
		return d, errors.New("running binary differs from installed release")
	}
	return d, nil
}
func get(ctx context.Context, c *http.Client, url string, w io.Writer, limit int64) error {
	r, e := http.NewRequestWithContext(ctx, "GET", url, nil)
	if e != nil {
		return e
	}
	res, e := c.Do(r)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("release HTTP %d", res.StatusCode)
	}
	n, e := io.Copy(w, io.LimitReader(res.Body, limit+1))
	if n > limit {
		return errors.New("release size limit exceeded")
	}
	return e
}
func Fetch(ctx context.Context, version, wantHash, dest string) error {
	return fetch(ctx, &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || len(via) > 5 {
			return errors.New("unsafe release redirect")
		}
		return nil
	}}, version, wantHash, dest)
}
func fetch(ctx context.Context, c *http.Client, version, wantHash, dest string) error {
	if !ValidVersion(version) || (wantHash != "" && !local.SHA.MatchString(wantHash)) {
		return errors.New("fixed version and valid manifest hash required")
	}
	if err := local.SafePath(dest); err != nil {
		return err
	}
	if _, e := os.Lstat(dest); !errors.Is(e, os.ErrNotExist) {
		return errors.New("runtime destination must be new")
	}
	stage, e := os.MkdirTemp(filepath.Dir(dest), ".release-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	base := Origin + "/v" + version + "/"
	var sums strings.Builder
	if e = get(ctx, c, base+"SHA256SUMS", &sums, 16384); e != nil {
		return e
	}
	hash := ""
	for _, line := range strings.Split(strings.TrimSpace(sums.String()), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 || !local.SHA.MatchString(parts[0]) {
			return errors.New("invalid checksums")
		}
		if parts[1] == "matrix-admin-linux-amd64.tar.gz" {
			if hash != "" {
				return errors.New("duplicate archive checksum")
			}
			hash = parts[0]
		}
	}
	if hash == "" {
		return errors.New("archive checksum missing")
	}
	archive := filepath.Join(stage, "runtime.tar.gz")
	f, e := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	e = get(ctx, c, base+"matrix-admin-linux-amd64.tar.gz", f, 128<<20)
	f.Close()
	if e != nil {
		return e
	}
	h, e := local.FileHash(archive)
	if e != nil || h != hash {
		return errors.New("archive checksum mismatch")
	}
	tree := filepath.Join(stage, "tree")
	if e = os.Mkdir(tree, 0700); e != nil {
		return e
	}
	in, e := os.Open(archive)
	if e != nil {
		return e
	}
	defer in.Close()
	gz, e := gzip.NewReader(in)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, 256<<20))
	seen := map[string]bool{}
	for {
		head, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if (!allowed(head.Name) && head.Name != "manifest.json") || seen[head.Name] || head.Typeflag != tar.TypeReg || head.Size < 0 || head.Size > 128<<20 {
			return errors.New("unsafe runtime member")
		}
		seen[head.Name] = true
		mode := os.FileMode(0600)
		if head.Name == "matrix-admin" || head.Name == "restic" {
			mode = 0700
		}
		out, err := os.OpenFile(filepath.Join(tree, head.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		out.Close()
		if err != nil {
			return err
		}
	}
	m, e := Verify(tree)
	if e != nil {
		return e
	}
	if m.Version != version {
		return errors.New("downloaded version differs")
	}
	if wantHash != "" {
		h, e = local.FileHash(filepath.Join(tree, "manifest.json"))
		if e != nil || h != wantHash {
			return errors.New("pinned manifest differs")
		}
	}
	return os.Rename(tree, dest)
}
func JSON(v any) []byte { b, _ := json.MarshalIndent(v, "", "  "); return append(b, '\n') }
