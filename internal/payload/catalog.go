// Recovery configuration is a bounded data format, never a shell archive.
package payload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

const maxCapsule = 32 << 20
const maxConfiguration = 16 << 20

var payloadPath = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

type ConfigFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	Mode    uint32 `json:"mode"`
	UID     uint32 `json:"uid"`
	GID     uint32 `json:"gid"`
}
type ConfigDirectory struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`
}

func configurationDirectory(p string) bool {
	switch p {
	case "config", "config/appservices", "config/s3", "telegram", "telegram/config", "telegram/data", "nginx", "control":
		return true
	}
	return false
}

type Configuration struct {
	Directories []ConfigDirectory `json:"directories"`
	Schema      int               `json:"schema"`
	Metadata    json.RawMessage   `json:"metadata"`
	Manifest    string            `json:"manifest"`
	Files       []ConfigFile      `json:"files"`
}

func dataPath(p string) bool {
	return (strings.HasPrefix(p, "database/") && !strings.Contains(strings.TrimPrefix(p, "database/"), "/")) || p == "telegram/data/telegram.db"
}
func allowedPayload(p string) bool {
	if !payloadPath.MatchString(p) || strings.Contains(p, "..") || strings.HasPrefix(p, "/") || filepath.Clean(p) != p {
		return false
	}
	if dataPath(p) {
		return true
	}
	if strings.HasPrefix(p, "config/appservices/") && !strings.Contains(strings.TrimPrefix(p, "config/appservices/"), "/") {
		return true
	}
	switch p {
	case "config/docker-compose.yml", "config/registration-token", "config/.env", "config/s3/media.env", "telegram/config/docker-compose.yml", "telegram/data/config.yaml", "telegram/data/registration.yaml", "telegram/metadata.json", "nginx/matrix.conf", "nginx/00-matrix-common.conf", "control/source-local.json":
		return true
	}
	return false
}
func fileHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func manifestEntries(s string) (map[string]string, error) {
	if len(s) == 0 || len(s) > 4<<20 || !strings.HasSuffix(s, "\n") {
		return nil, errors.New("invalid configuration manifest size/framing")
	}
	entries := map[string]string{}
	previous := ""
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if len(line) < 69 || line[64:68] != "  ./" || !hashRE.MatchString(line[:64]) {
			return nil, errors.New("invalid configuration manifest entry")
		}
		p := line[68:]
		if !allowedPayload(p) || p <= previous {
			return nil, errors.New("unsupported, duplicated or unordered configuration path")
		}
		entries[p] = line[:64]
		previous = p
	}
	return entries, nil
}
func validateConfiguration(c *Configuration) (map[string]string, error) {
	if c.Schema != 1 || len(c.Metadata) == 0 || len(c.Metadata) > 1<<20 || !json.Valid(c.Metadata) || len(c.Files) > 4096 {
		return nil, errors.New("unsupported configuration capsule")
	}
	entries, e := manifestEntries(c.Manifest)
	if e != nil {
		return nil, e
	}
	dirs := map[string]bool{}
	for _, d := range c.Directories {
		if !configurationDirectory(d.Path) || dirs[d.Path] || d.Mode & ^uint32(0777) != 0 || d.UID > 2147483647 || d.GID > 2147483647 {
			return nil, errors.New("invalid configuration directory")
		}
		dirs[d.Path] = true
	}
	for _, f := range c.Files {
		for p := filepath.Dir(f.Path); p != "."; p = filepath.Dir(p) {
			if !dirs[p] {
				return nil, errors.New("configuration directory metadata missing")
			}
		}
	}
	for p := range dirs {
		if parent := filepath.Dir(p); parent != "." && !dirs[parent] {
			return nil, errors.New("configuration parent metadata missing")
		}
	}
	seen := map[string]bool{}
	size := 0
	for _, f := range c.Files {
		size += len(f.Content)
		if !allowedPayload(f.Path) || dataPath(f.Path) || seen[f.Path] || size > maxConfiguration || len(f.Content) > 8<<20 || f.Mode & ^uint32(0777) != 0 || f.UID > 2147483647 || f.GID > 2147483647 || entries[f.Path] != fileHash(f.Content) {
			return nil, errors.New("configuration file inventory, size or hash mismatch")
		}
		seen[f.Path] = true
	}
	for p := range entries {
		if !dataPath(p) && !seen[p] {
			return nil, errors.New("configuration file missing")
		}
	}
	if len(seen) == 0 {
		return nil, errors.New("empty configuration capsule")
	}
	return entries, nil
}
func boundedFile(p string, limit int64) ([]byte, error) {
	st, e := os.Lstat(p)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("expected bounded regular file")
	}
	return os.ReadFile(p)
}
func packConfiguration(bundle, output string) error {
	m, e := boundedFile(filepath.Join(bundle, "metadata.json"), 1<<20)
	if e != nil {
		return e
	}
	manifest, e := boundedFile(filepath.Join(bundle, "manifest.sha256"), 4<<20)
	if e != nil {
		return e
	}
	entries, e := manifestEntries(string(manifest))
	if e != nil {
		return e
	}
	c := Configuration{Schema: 1, Metadata: m, Manifest: string(manifest)}
	root := filepath.Join(bundle, "payload")
	seen := map[string]bool{}
	total := 0
	e = filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		if d.IsDir() {
			if rel == "." || rel == "database" {
				return nil
			}
			if !configurationDirectory(rel) {
				return errors.New("unclassified payload directory")
			}
			st, e := d.Info()
			if e != nil {
				return e
			}
			sys := st.Sys().(*syscall.Stat_t)
			c.Directories = append(c.Directories, ConfigDirectory{rel, uint32(st.Mode().Perm()), sys.Uid, sys.Gid})
			return nil
		}
		if !d.Type().IsRegular() || !allowedPayload(rel) || entries[rel] == "" {
			return errors.New("unclassified payload file")
		}
		seen[rel] = true
		if dataPath(rel) {
			return nil
		}
		b, e := boundedFile(p, 8<<20)
		if e != nil {
			return e
		}
		total += len(b)
		if total > maxConfiguration {
			return errors.New("configuration size limit exceeded")
		}
		st, e := os.Stat(p)
		if e != nil {
			return e
		}
		sys := st.Sys().(*syscall.Stat_t)
		c.Files = append(c.Files, ConfigFile{rel, b, uint32(st.Mode().Perm()), sys.Uid, sys.Gid})
		return nil
	})
	if e != nil {
		return e
	}
	for p := range entries {
		if !seen[p] {
			return errors.New("payload manifest file missing")
		}
	}
	if _, e = validateConfiguration(&c); e != nil {
		return e
	}
	b, e := json.Marshal(c)
	if e != nil {
		return e
	}
	b = append(b, '\n')
	if len(b) > maxCapsule {
		return errors.New("capsule exceeds size limit")
	}
	return exclusiveWrite(output, b, 0600)
}
func exclusiveWrite(p string, b []byte, mode fs.FileMode) error {
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func readConfiguration(input string) (Configuration, error) {
	var c Configuration
	b, e := boundedFile(input, maxCapsule)
	if e != nil {
		return c, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, errors.New("invalid configuration JSON")
	}
	// The JSON decoder must consume exactly one object.
	if strings.TrimSpace(string(b[d.InputOffset():])) != "" {
		return c, errors.New("trailing configuration data")
	}
	_, e = validateConfiguration(&c)
	return c, e
}
func unpackConfiguration(input, bundle string) error {
	c, e := readConfiguration(input)
	if e != nil {
		return e
	}
	parent := filepath.Dir(bundle)
	real, e := filepath.EvalSymlinks(parent)
	if e != nil {
		return e
	}
	abs, e := filepath.Abs(parent)
	if e != nil || real != abs {
		return errors.New("configuration destination has symlink parent")
	}
	if _, e = os.Lstat(bundle); !errors.Is(e, os.ErrNotExist) {
		return errors.New("configuration destination must be new")
	}
	stage, e := os.MkdirTemp(parent, ".configuration-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	for _, d := range c.Directories {
		if e = os.MkdirAll(filepath.Join(stage, "payload", d.Path), 0700); e != nil {
			return e
		}
	}
	for _, f := range c.Files {
		p := filepath.Join(stage, "payload", f.Path)
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		if e = exclusiveWrite(p, f.Content, 0600); e != nil {
			return e
		}
		if e = os.Chown(p, int(f.UID), int(f.GID)); e != nil {
			return e
		}
		if e = os.Chmod(p, fs.FileMode(f.Mode)); e != nil {
			return e
		}
	}
	// Apply directory permissions after all content has been materialized.
	sort.Slice(c.Directories, func(i, j int) bool { return c.Directories[i].Path > c.Directories[j].Path })
	for _, d := range c.Directories {
		p := filepath.Join(stage, "payload", d.Path)
		if e = os.Chown(p, int(d.UID), int(d.GID)); e != nil {
			return e
		}
		if e = os.Chmod(p, fs.FileMode(d.Mode)); e != nil {
			return e
		}
	}
	if e = exclusiveWrite(filepath.Join(stage, "metadata.json"), append(c.Metadata, '\n'), 0600); e != nil {
		return e
	}
	if e = exclusiveWrite(filepath.Join(stage, "manifest.sha256"), []byte(c.Manifest), 0600); e != nil {
		return e
	}
	return os.Rename(stage, bundle)
}

// Restic contributes only the two database trees. Validate all names and hashes
// before moving them into the separately validated R2 configuration bundle.
func mergeData(input, bundle string) error {
	raw, e := boundedFile(filepath.Join(bundle, "manifest.sha256"), 4<<20)
	if e != nil {
		return e
	}
	entries, e := manifestEntries(string(raw))
	if e != nil {
		return e
	}
	seen := map[string]bool{}
	paths := []string{}
	e = filepath.WalkDir(input, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(input, p)
		if e != nil {
			return e
		}
		if d.IsDir() {
			switch rel {
			case ".", "bundle", "bundle/payload", "bundle/payload/database", "bundle/payload/telegram", "bundle/payload/telegram/data":
				return nil
			}
			return errors.New("unexpected database restore directory")
		}
		key := strings.TrimPrefix(rel, "bundle/payload/")
		if key == rel || !d.Type().IsRegular() || !allowedPayload(key) || !dataPath(key) || entries[key] == "" {
			return errors.New("unexpected file in database snapshot")
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return e
		}
		if hex.EncodeToString(h.Sum(nil)) != entries[key] {
			return errors.New("database content hash mismatch")
		}
		seen[key] = true
		paths = append(paths, key)
		return nil
	})
	if e != nil {
		return e
	}
	for key := range entries {
		if dataPath(key) && !seen[key] {
			return errors.New("database snapshot missing a file")
		}
	}
	if !seen["database/CURRENT"] || !seen["telegram/data/telegram.db"] {
		return errors.New("both databases are required")
	}
	// Destination is a new, private work tree; still reject symlink ancestors.
	for _, key := range paths {
		dest := filepath.Join(bundle, "payload", key)
		if _, e = os.Lstat(dest); !errors.Is(e, os.ErrNotExist) {
			return errors.New("database destination already exists")
		}
		for p := filepath.Dir(dest); p != "." && p != "/"; p = filepath.Dir(p) {
			st, e := os.Lstat(p)
			if e == nil && !st.IsDir() {
				return errors.New("unsafe database destination parent")
			}
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
	}
	sort.Strings(paths)
	for _, key := range paths {
		src := filepath.Join(input, "bundle/payload", key)
		dst := filepath.Join(bundle, "payload", key)
		if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
			return e
		}
		st, e := os.Stat(filepath.Dir(src))
		if e != nil {
			return e
		}
		sys := st.Sys().(*syscall.Stat_t)
		if e = os.Chown(filepath.Dir(dst), int(sys.Uid), int(sys.Gid)); e != nil {
			return e
		}
		if e = os.Chmod(filepath.Dir(dst), st.Mode().Perm()); e != nil {
			return e
		}
		if e = os.Rename(src, dst); e != nil {
			return e
		}
	}
	return nil
}
