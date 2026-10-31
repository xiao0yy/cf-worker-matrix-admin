package local

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLiteralConfiguration(t *testing.T) {
	for _, input := range []string{"A=$(touch /tmp/not-executed)", "A=one\nA=two", "A=\"${HOME}\"", "A='bad'quote'", "A=one;date", "A=\"line\\nnext\""} {
		if _, e := ParseEnv([]byte(input), false); e == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	v, e := ParseEnv([]byte("# comment\nA='literal$secret'\nB=auto\n"), false)
	if e != nil || v["A"] != "literal$secret" || v["B"] != "auto" {
		t.Fatal(v, e)
	}
}
func TestPrivateAtomicAndSymlinkBoundary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	if e := Atomic(p, []byte("first"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := Atomic(p, []byte("second"), 0600); e != nil {
		t.Fatal(e)
	}
	b, e := Private(p)
	if e != nil || string(b) != "second" {
		t.Fatal(e)
	}
	os.Chmod(p, 0644)
	if _, e = Private(p); e == nil {
		t.Fatal("world-readable secret accepted")
	}
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(dir, "link"))
	if e = Atomic(filepath.Join(dir, "link/new/file"), []byte("bad"), 0600); e == nil {
		t.Fatal("followed symlink")
	}
	if _, e = os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(e) {
		t.Fatal("created outside boundary")
	}
}
func TestLockAndStrictJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	done, e := Lock(p)
	if e != nil {
		t.Fatal(e)
	}
	defer done()
	if close, e := Lock(p); e == nil {
		close()
		t.Fatal("duplicate lock")
	}
	var v struct{ N int }
	for _, b := range []string{`{"N":1} {}`, `{"N":1,"unknown":true}`} {
		if e = Decode([]byte(b), &v); e == nil {
			t.Fatal("accepted invalid JSON")
		}
	}
}
