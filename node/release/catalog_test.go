package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func capsuleFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	files := map[string]string{"database/CURRENT": "MANIFEST-1\n", "database/MANIFEST-1": "database bytes", "telegram/data/telegram.db": "sqlite bytes", "telegram/data/config.yaml": "secret configuration", "control/source-local.json": "{\"cluster_id\":\"test\"}"}
	var names []string
	for p := range files {
		names = append(names, p)
	}
	sort.Strings(names)
	var manifest string
	for _, p := range names {
		dest := filepath.Join(bundle, "payload", p)
		os.MkdirAll(filepath.Dir(dest), 0700)
		if e := os.WriteFile(dest, []byte(files[p]), 0600); e != nil {
			t.Fatal(e)
		}
		manifest += fileHash([]byte(files[p])) + "  ./" + p + "\n"
	}
	os.WriteFile(filepath.Join(bundle, "manifest.sha256"), []byte(manifest), 0600)
	os.WriteFile(filepath.Join(bundle, "metadata.json"), []byte(`{"format":"fixture"}`), 0600)
	os.Chmod(filepath.Join(bundle, "payload/control"), 0750)
	os.MkdirAll(filepath.Join(bundle, "payload/config/appservices"), 0755)
	return root, bundle
}
func TestConfigurationAndDatabaseRoundTrip(t *testing.T) {
	root, bundle := capsuleFixture(t)
	capsule := filepath.Join(root, "capsule.json")
	if e := packConfiguration(bundle, capsule); e != nil {
		t.Fatal(e)
	}
	c, e := readConfiguration(capsule)
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Files) != 2 {
		t.Fatal("database bytes included in configuration")
	}
	for _, f := range c.Files {
		if dataPath(f.Path) {
			t.Fatal("data in R2 capsule")
		}
	}
	restored := filepath.Join(root, "restored")
	if e = unpackConfiguration(capsule, restored); e != nil {
		t.Fatal(e)
	}
	st, e := os.Stat(filepath.Join(restored, "payload/control"))
	if e != nil || st.Mode().Perm() != 0750 {
		t.Fatal("directory permissions lost")
	}
	if st, e := os.Stat(filepath.Join(restored, "payload/config/appservices")); e != nil || !st.IsDir() {
		t.Fatal("empty appservices directory lost")
	}
	if e = unpackConfiguration(capsule, restored); e == nil {
		t.Fatal("overwrote existing config")
	}
	download := filepath.Join(root, "download")
	for _, p := range []string{"database/CURRENT", "database/MANIFEST-1", "telegram/data/telegram.db"} {
		src := filepath.Join(bundle, "payload", p)
		b, _ := os.ReadFile(src)
		dst := filepath.Join(download, "bundle/payload", p)
		os.MkdirAll(filepath.Dir(dst), 0700)
		os.WriteFile(dst, b, 0600)
	}
	if e = mergeData(download, restored); e != nil {
		t.Fatal(e)
	}
	a, _ := os.ReadFile(filepath.Join(restored, "payload/telegram/data/config.yaml"))
	if string(a) != "secret configuration" {
		t.Fatal("config not restored")
	}
	b, _ := os.ReadFile(filepath.Join(restored, "payload/database/CURRENT"))
	if string(b) != "MANIFEST-1\n" {
		t.Fatal("data not restored")
	}
}
func TestCapsuleRejectsInvalidContentBeforeCreatingDestination(t *testing.T) {
	mutations := map[string]func(*Configuration){
		"hash":                func(c *Configuration) { c.Files[0].Content = []byte("corrupt") },
		"missing":             func(c *Configuration) { c.Files = c.Files[:1] },
		"duplicate":           func(c *Configuration) { c.Files = append(c.Files, c.Files[0]) },
		"traversal":           func(c *Configuration) { c.Files[0].Path = "../outside" },
		"control credentials": func(c *Configuration) { c.Files[0].Path = "control/s3.env" },
		"database":            func(c *Configuration) { c.Files[0].Path = "database/CURRENT" },
		"mode":                func(c *Configuration) { c.Files[0].Mode = 04777 },
		"schema":              func(c *Configuration) { c.Schema = 2 },
		"manifest duplicate":  func(c *Configuration) { c.Manifest += strings.Split(c.Manifest, "\n")[0] + "\n" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			root, bundle := capsuleFixture(t)
			p := filepath.Join(root, "c.json")
			if e := packConfiguration(bundle, p); e != nil {
				t.Fatal(e)
			}
			c, _ := readConfiguration(p)
			mutate(&c)
			b, _ := json.Marshal(c)
			os.WriteFile(p, b, 0600)
			dest := filepath.Join(root, "dest")
			if e := unpackConfiguration(p, dest); e == nil {
				t.Fatal("accepted invalid capsule")
			}
			if _, e := os.Stat(dest); !os.IsNotExist(e) {
				t.Fatal("destination created on invalid capsule")
			}
		})
	}
}
func TestPackRejectsLinksAndUnknownFiles(t *testing.T) {
	for _, link := range []bool{false, true} {
		root, bundle := capsuleFixture(t)
		p := filepath.Join(bundle, "payload/control/s3.env")
		if link {
			os.Symlink("/etc/passwd", p)
		} else {
			os.WriteFile(p, []byte("unclassified secret"), 0600)
		}
		if e := packConfiguration(bundle, filepath.Join(root, "c.json")); e == nil {
			t.Fatal("accepted unclassified file")
		}
	}
}
func TestDataMergeRejectsConfigInjectionAndMissingFiles(t *testing.T) {
	for _, p := range []string{"bundle/payload/control/source-local.json", "bundle/metadata.json", "bundle/payload/database/CURRENT"} {
		root, bundle := capsuleFixture(t)
		capsule := filepath.Join(root, "c.json")
		packConfiguration(bundle, capsule)
		out := filepath.Join(root, "out")
		unpackConfiguration(capsule, out)
		input := filepath.Join(root, "input")
		dest := filepath.Join(input, p)
		os.MkdirAll(filepath.Dir(dest), 0700)
		os.WriteFile(dest, []byte("corrupt"), 0600)
		if e := mergeData(input, out); e == nil {
			t.Fatal("accepted injected/incomplete database")
		}
		if _, e := os.Stat(filepath.Join(out, "payload/database")); !os.IsNotExist(e) {
			t.Fatal("partial data applied")
		}
	}
}
