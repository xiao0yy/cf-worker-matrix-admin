// Package local owns durable local files. It never evaluates configuration as code.
package local

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

var ID = regexp.MustCompile(`^[a-zA-Z0-9_-][a-zA-Z0-9_.-]{0,99}$`)
var SHA = regexp.MustCompile(`^[a-f0-9]{64}$`)

func SafeID(s string) bool   { return ID.MatchString(s) && !strings.Contains(s, "..") }
func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func FileHash(p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}
func RandomID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func Decode(b []byte, v any) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func ReadJSON(p string, v any) error {
	b, e := ReadFile(p, 32<<20)
	if e != nil {
		return e
	}
	return Decode(b, v)
}
func ReadFile(p string, limit int64) ([]byte, error) {
	s, e := os.Lstat(p)
	if e != nil {
		return nil, e
	}
	if !s.Mode().IsRegular() || s.Size() > limit {
		return nil, errors.New("expected bounded regular file")
	}
	return os.ReadFile(p)
}
func Private(p string) ([]byte, error) {
	s, e := os.Lstat(p)
	if e != nil {
		return nil, e
	}
	if !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private regular file required (0600)")
	}
	return ReadFile(p, 32<<20)
}
func Atomic(p string, b []byte, mode fs.FileMode) error {
	if e := SafePath(p); e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".pending-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	if e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), p); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(p))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func JSON(p string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return Atomic(p, append(b, '\n'), 0600)
}
func SafePath(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
		return errors.New("clean absolute path required")
	}
	cur := "/"
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		cur = filepath.Join(cur, part)
		s, e := os.Lstat(cur)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if s.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in managed path")
		}
	}
	return nil
}
func Lock(p string) (func(), error) {
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return nil, e
	}
	if e := SafePath(p); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("operation already running: %s", p)
	}
	return func() { f.Close() }, nil
}
func Copy(src, dst string) error {
	st, e := os.Lstat(src)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return errors.New("copy requires regular source")
	}
	if e = SafePath(dst); e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
		return e
	}
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, st.Mode().Perm())
	if e != nil {
		return e
	}
	defer out.Close()
	if _, e = io.Copy(out, in); e != nil {
		return e
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		if e = out.Chown(int(stat.Uid), int(stat.Gid)); e != nil {
			return e
		}
	}
	if e = out.Chmod(st.Mode().Perm()); e != nil {
		return e
	}
	return out.Sync()
}
func CopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		target := filepath.Join(dst, rel)
		st, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if d.IsDir() {
			if e = SafePath(target); e != nil {
				return e
			}
			if e = os.MkdirAll(target, 0700); e != nil {
				return e
			}
			if s, ok := st.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
				if e = os.Chown(target, int(s.Uid), int(s.Gid)); e != nil {
					return e
				}
			}
			return os.Chmod(target, st.Mode().Perm())
		}
		return Copy(p, target)
	})
}
func Manifest(root string) (string, error) {
	var lines []string
	e := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		st, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() {
			return errors.New("non-regular payload file")
		}
		r, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		if strings.ContainsAny(r, "\r\n\\") {
			return errors.New("invalid payload path")
		}
		h, e := FileHash(p)
		if e == nil {
			lines = append(lines, h+"  ./"+filepath.ToSlash(r))
		}
		return e
	})
	sort.Slice(lines, func(i, j int) bool { return lines[i][68:] < lines[j][68:] })
	return strings.Join(lines, "\n") + "\n", e
}

// ParseEnv reads literal assignments. Shell expansions, commands and duplicate keys
// are rejected. legacy permits repeated assignments from old generated host files.
func ParseEnv(b []byte, legacy bool) (map[string]string, error) {
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		l = strings.TrimPrefix(l, "export ")
		k, v, ok := strings.Cut(l, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !ok || !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(k) {
			return nil, errors.New("invalid configuration assignment")
		}
		if _, ok = m[k]; ok && !legacy {
			return nil, errors.New("duplicate configuration key")
		}
		if strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") && len(v) >= 2 {
			v = v[1 : len(v)-1]
			if strings.Contains(v, "'") {
				return nil, errors.New("invalid quoted literal")
			}
		} else if strings.HasPrefix(v, "\"") {
			if strings.ContainsAny(v, "$`") {
				return nil, errors.New("configuration expansion forbidden")
			}
			var e error
			v, e = strconv.Unquote(v)
			if e != nil {
				return nil, e
			}
		} else if strings.ContainsAny(v, " \t;$`\\\"'()<>|&") {
			return nil, errors.New("configuration must contain literal values")
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, errors.New("multiline configuration forbidden")
		}
		m[k] = v
	}
	return m, nil
}
