// matrix-release verifies and fetches public release artifacts. S3 contains only
// a version/hash descriptor; it cannot redirect execution to a different origin.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxArchive = 256 << 20
const maxExpanded = 512 << 20

var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?$`)
var hashRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Protocols struct {
	Control  int `json:"control"`
	Mailbox  int `json:"mailbox"`
	Recovery int `json:"recovery"`
}
type Origin struct {
	Schema    int       `json:"schema"`
	Project   string    `json:"project"`
	Base      string    `json:"release_base_url"`
	Platform  string    `json:"platform"`
	Protocols Protocols `json:"protocols"`
	Restic    string    `json:"restic_version"`
}
type Descriptor struct {
	Project string `json:"project"`
	Version string `json:"version"`
	SHA     string `json:"manifest_sha256"`
}
type File struct {
	Path       string `json:"path"`
	SHA        string `json:"sha256"`
	Size       int64  `json:"size"`
	Executable bool   `json:"executable"`
}
type Archive struct {
	Name string `json:"name"`
	SHA  string `json:"sha256"`
	Size int64  `json:"size"`
}
type Manifest struct {
	Schema    int       `json:"schema"`
	Project   string    `json:"project"`
	Version   string    `json:"version"`
	Commit    string    `json:"commit"`
	Platform  string    `json:"platform"`
	Protocols Protocols `json:"protocols"`
	Restic    string    `json:"restic_version"`
	Archive   Archive   `json:"archive"`
	Files     []File    `json:"files"`
}

func decode(b []byte, dst any) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func readJSON(p string, dst any) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return errors.New("JSON too large")
	}
	return decode(b, dst)
}
func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func validOrigin(o Origin) error {
	u, e := url.Parse(o.Base)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(o.Base, "/") {
		return errors.New("release origin must be an HTTPS base URL without credentials/query/trailing slash")
	}
	if o.Schema != 1 || o.Project != "matrix-admin" || o.Platform != "linux/amd64" {
		return errors.New("unsupported project/platform")
	}
	return nil
}
func allowed(p string) bool {
	if p == "release.json" {
		return true
	}
	if p == "backup/bin/restic" || p == "backup/bin/matrix-release" || p == "control/bin/matrix-control" || p == "control/bin/matrix-mailbox" {
		return true
	}
	if strings.HasPrefix(p, "backup/") && strings.Count(p, "/") == 1 {
		n := path.Base(p)
		if n == "host-state.schema.json" {
			return true
		}
		for _, s := range []string{".sh", ".md", ".txt", ".example", ".service", ".timer"} {
			if strings.HasSuffix(n, s) {
				return true
			}
		}
	}
	switch p {
	case "control/software.sha256", "control/handover-worker.sh", "control/mailbox-install.sh", "control/matrix-control.service", "control/matrix-handover.service", "control/matrix-handover.timer", "control/matrix-mailbox.service", "control/matrix-mailbox.timer", "control/nginx-server.conf", "control/nginx-location.conf":
		return true
	}
	return false
}
func validateManifest(m Manifest) error {
	if m.Schema != 1 || m.Project != "matrix-admin" || !versionRE.MatchString(m.Version) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(m.Commit) || m.Platform != "linux/amd64" || m.Protocols != (Protocols{1, 1, 2}) || !versionRE.MatchString(m.Restic) {
		return errors.New("unsupported release contract")
	}
	if m.Archive.Name != "matrix-admin-linux-amd64.tar.gz" || !hashRE.MatchString(m.Archive.SHA) || m.Archive.Size <= 0 || m.Archive.Size > maxArchive || len(m.Files) == 0 || len(m.Files) > 200 {
		return errors.New("invalid archive contract")
	}
	seen := map[string]bool{}
	var total int64
	for _, f := range m.Files {
		if !allowed(f.Path) || path.Clean(f.Path) != f.Path || strings.ContainsAny(f.Path, "\\\x00\n\r") || seen[f.Path] || !hashRE.MatchString(f.SHA) || f.Size < 0 || f.Size > maxExpanded {
			return errors.New("invalid/duplicate release file")
		}
		seen[f.Path] = true
		total += f.Size
	}
	if total > maxExpanded {
		return errors.New("expanded release too large")
	}
	for _, p := range []string{"release.json", "backup/recover.sh", "backup/recover-main.sh", "backup/release-common.sh", "backup/tuwunel-restic.sh", "backup/prepare-standby.sh", "backup/foundation-software.sh", "backup/bin/restic", "backup/bin/matrix-release", "control/bin/matrix-control", "control/bin/matrix-mailbox", "control/handover-worker.sh", "control/matrix-control.service", "control/matrix-handover.service", "control/matrix-handover.timer", "control/matrix-mailbox.service", "control/matrix-mailbox.timer", "control/mailbox-install.sh", "control/nginx-server.conf", "control/nginx-location.conf"} {
		if !seen[p] {
			return fmt.Errorf("missing required release file: %s", p)
		}
	}
	return nil
}
func get(c *http.Client, uri string, w io.Writer, limit int64) error {
	resp, e := c.Get(uri)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("release download HTTP %d", resp.StatusCode)
	}
	n, e := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if e != nil {
		return e
	}
	if n > limit {
		return errors.New("download exceeds limit")
	}
	return nil
}
func verify(root string, m Manifest) error {
	if err := validateManifest(m); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if real != filepath.Clean(root) {
		return errors.New("symlink in release root")
	}
	for _, f := range m.Files {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		cur := root
		for _, part := range strings.Split(f.Path, "/") {
			cur = filepath.Join(cur, part)
			s, e := os.Lstat(cur)
			if e != nil {
				return e
			}
			if s.Mode()&os.ModeSymlink != 0 {
				return errors.New("symlink in release tree")
			}
		}
		s, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if !s.Mode().IsRegular() || s.Size() != f.Size || s.Mode()&0111 != 0 != f.Executable {
			return fmt.Errorf("file type/size/mode mismatch: %s", f.Path)
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(h, in)
		in.Close()
		if e != nil {
			return e
		}
		if hex.EncodeToString(h.Sum(nil)) != f.SHA {
			return fmt.Errorf("file checksum mismatch: %s", f.Path)
		}
	}
	return nil
}
func extract(archive, root string, m Manifest) error {
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
	tr := tar.NewReader(io.LimitReader(gz, maxExpanded+(2<<20)))
	expected := map[string]File{}
	for _, f := range m.Files {
		expected[f.Path] = f
	}
	seen := map[string]bool{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		f, ok := expected[h.Name]
		if !ok || seen[h.Name] || h.Typeflag != tar.TypeReg || h.Size != f.Size {
			return errors.New("unlisted/duplicate/unsafe archive member")
		}
		seen[h.Name] = true
		p := filepath.Join(root, filepath.FromSlash(h.Name))
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		mode := os.FileMode(0600)
		if f.Executable {
			mode = 0700
		}
		out, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, tr)
		if e == nil {
			e = out.Sync()
		}
		closeErr := out.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if len(seen) != len(expected) {
		return errors.New("incomplete release archive")
	}
	return verify(root, m)
}
func fetch(c *http.Client, o Origin, d Descriptor, dest string) error {
	if e := validOrigin(o); e != nil {
		return e
	}
	if d.Project != o.Project || !versionRE.MatchString(d.Version) || !hashRE.MatchString(d.SHA) {
		return errors.New("invalid release descriptor")
	}
	if _, e := os.Lstat(dest); !errors.Is(e, os.ErrNotExist) {
		return errors.New("destination already exists or cannot be inspected")
	}
	parent := filepath.Dir(dest)
	stage, e := os.MkdirTemp(parent, ".matrix-release-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	var buf strings.Builder
	base := o.Base + "/v" + d.Version + "/"
	if e = get(c, base+"matrix-admin-linux-amd64.manifest.json", &buf, 1<<20); e != nil {
		return e
	}
	raw := []byte(buf.String())
	if digest(raw) != d.SHA {
		return errors.New("release manifest checksum mismatch")
	}
	var m Manifest
	if e = decode(raw, &m); e != nil {
		return e
	}
	if e = validateManifest(m); e != nil {
		return e
	}
	if m.Project != d.Project || m.Version != d.Version {
		return errors.New("release identity mismatch")
	}
	archive := filepath.Join(stage, "archive.gz")
	out, e := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	h := sha256.New()
	e = get(c, base+m.Archive.Name, io.MultiWriter(out, h), m.Archive.Size)
	ce := out.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	s, e := os.Stat(archive)
	if e != nil {
		return e
	}
	if s.Size() != m.Archive.Size || hex.EncodeToString(h.Sum(nil)) != m.Archive.SHA {
		return errors.New("archive checksum/size mismatch")
	}
	root := filepath.Join(stage, "tree")
	if e = os.Mkdir(root, 0700); e != nil {
		return e
	}
	if e = extract(archive, root, m); e != nil {
		return e
	}
	var actual Origin
	if e = readJSON(filepath.Join(root, "release.json"), &actual); e != nil {
		return e
	}
	if validOrigin(actual) != nil || actual.Base != o.Base || actual.Project != o.Project || actual.Platform != o.Platform || actual.Protocols != m.Protocols || actual.Restic != m.Restic {
		return errors.New("embedded release origin differs from trusted origin")
	}
	if e = os.WriteFile(filepath.Join(root, "release-manifest.json"), raw, 0600); e != nil {
		return e
	}
	return os.Rename(root, dest)
}
func run(args []string) error {
	if len(args) > 0 && (args[0] == "config-pack" || args[0] == "config-unpack" || args[0] == "data-merge") {
		return catalogCommand(args)
	}
	if len(args) == 0 {
		return errors.New("use fetch|verify|describe")
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	origin := fs.String("origin", "", "trusted local release.json")
	descriptor := fs.String("descriptor", "", "point software descriptor")
	root := fs.String("root", "", "installed deploy directory")
	manifest := fs.String("manifest", "", "release manifest")
	output := fs.String("output", "", "new directory")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	switch cmd {
	case "fetch":
		var o Origin
		var d Descriptor
		if e := readJSON(*origin, &o); e != nil {
			return e
		}
		if e := readJSON(*descriptor, &d); e != nil {
			return e
		}
		if !filepath.IsAbs(*output) {
			return errors.New("output must be absolute")
		}
		c := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Scheme != "https" || len(via) > 5 {
				return errors.New("unsafe release redirect")
			}
			return nil
		}}
		return fetch(c, o, d, *output)
	case "verify", "describe":
		b, e := os.ReadFile(*manifest)
		if e != nil {
			return e
		}
		if len(b) > 1<<20 {
			return errors.New("manifest too large")
		}
		var m Manifest
		if e = decode(b, &m); e != nil {
			return e
		}
		if e = validateManifest(m); e != nil {
			return e
		}
		if cmd == "verify" {
			if !filepath.IsAbs(*root) {
				return errors.New("root must be absolute")
			}
			installed, err := os.ReadFile(filepath.Join(*root, "release-manifest.json"))
			if err != nil || digest(installed) != digest(b) {
				return errors.New("installed release descriptor differs from expected release")
			}
			return verify(*root, m)
		}
		return json.NewEncoder(os.Stdout).Encode(Descriptor{m.Project, m.Version, digest(b)})
	default:
		return errors.New("unknown command")
	}
}
func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "matrix-release:", e)
		os.Exit(1)
	}
}
