package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type githubFixture struct {
	IsDraft bool   `json:"isDraft"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	Assets  []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

func TestMain(m *testing.M) {
	if os.Getenv("MATRIX_PUBLISH_HELPER") == "1" {
		os.Exit(fakeGitHub(os.Args[1:]))
	}
	os.Exit(m.Run())
}
func fakeGitHub(args []string) int {
	root := os.Getenv("MATRIX_PUBLISH_FIXTURE")
	statefile := filepath.Join(root, "release.json")
	var state githubFixture
	option := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	save := func() int {
		if local.JSON(statefile, state) != nil {
			return 1
		}
		return 0
	}
	if len(args) < 2 {
		return 1
	}
	if args[0] == "git" {
		fmt.Println(strings.Repeat("a", 40))
		return 0
	}
	args = args[1:]
	if args[0] == "api" {
		fmt.Println(strings.Repeat("a", 40))
		return 0
	}
	if args[0] != "release" || len(args) < 3 {
		return 1
	}
	action := args[1]
	loaded := local.ReadJSON(statefile, &state) == nil
	switch action {
	case "view":
		if !loaded {
			return 1
		}
		json.NewEncoder(os.Stdout).Encode(state)
		return 0
	case "create":
		if loaded {
			return 1
		}
		state.IsDraft = true
		state.Assets = make([]struct {
			Name string `json:"name"`
		}, 0)
		return save()
	case "upload":
		if !loaded {
			return 1
		}
		src := args[3]
		name := filepath.Base(src)
		if os.Getenv("GH_FAIL_ASSET") == name {
			return 1
		}
		if local.Copy(src, filepath.Join(root, name)) != nil {
			return 1
		}
		state.Assets = append(state.Assets, struct {
			Name string `json:"name"`
		}{name})
		return save()
	case "download":
		if local.Copy(filepath.Join(root, option("--pattern")), filepath.Join(option("--dir"), option("--pattern"))) != nil {
			return 1
		}
		return 0
	case "edit":
		for _, arg := range args {
			if arg == "--draft=false" {
				state.IsDraft = false
			} else if strings.HasPrefix(arg, "--draft") {
				return 1
			}
		}
		state.Name = option("--title")
		notes, err := os.ReadFile(option("--notes-file"))
		if err != nil {
			return 1
		}
		state.Body = string(notes)
		return save()
	}
	return 1
}

type publisherFixture struct {
	assets, remote string
	run            func(string) error
}

func newPublisherFixture(t *testing.T) publisherFixture {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	assets := filepath.Join(root, "assets")
	remote := filepath.Join(root, "remote")
	for _, p := range []string{bin, assets, remote} {
		if e := os.Mkdir(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	self, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"gh", "git"} {
		text := "#!/bin/sh\nMATRIX_PUBLISH_HELPER=1 exec '" + strings.ReplaceAll(self, "'", "'\\''") + "' " + name + " \"$@\"\n"
		if e = os.WriteFile(filepath.Join(bin, name), []byte(text), 0700); e != nil {
			t.Fatal(e)
		}
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	verifier := "#!/bin/sh\nprintf '%s\\n' '{\"commit\":\"" + strings.Repeat("a", 40) + "\",\"version\":\"0.2.0\"}'\n"
	if e = tw.WriteHeader(&tar.Header{Name: "matrix-admin", Mode: 0700, Size: int64(len(verifier)), Typeflag: tar.TypeReg}); e != nil {
		t.Fatal(e)
	}
	tw.Write([]byte(verifier))
	tw.Close()
	gz.Close()
	archive := compressed.Bytes()
	agent := []byte("instructions\n")
	for name, b := range map[string][]byte{"matrix-admin-linux-amd64.tar.gz": archive, "AGENT.md": agent, "notes.md": []byte("notes"), "SHA256SUMS": []byte(local.Digest(agent) + "  AGENT.md\n" + local.Digest(archive) + "  matrix-admin-linux-amd64.tar.gz\n")} {
		if e = os.WriteFile(filepath.Join(assets, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	script, e := filepath.Abs("../../scripts/publish-release.sh")
	if e != nil {
		t.Fatal(e)
	}
	run := func(fail string) error {
		cmd := exec.Command("bash", script, assets)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GH_TOKEN=test-only", "GH_REPO=fixture/project", "RELEASE_TAG=v0.2.0", "MATRIX_PUBLISH_FIXTURE="+remote, "GH_FAIL_ASSET="+fail)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Log(string(out))
		}
		return err
	}
	return publisherFixture{assets: assets, remote: remote, run: run}
}

func TestPublisherInterruptedUploadRetriesAndNeverHidesPublishedRelease(t *testing.T) {
	f := newPublisherFixture(t)
	if e := f.run("SHA256SUMS"); e == nil {
		t.Fatal("upload failure ignored")
	}
	var state githubFixture
	if e := local.ReadJSON(filepath.Join(f.remote, "release.json"), &state); e != nil {
		t.Fatal(e)
	}
	if !state.IsDraft || len(state.Assets) != 1 {
		t.Fatal("incomplete release published")
	}
	if e := f.run(""); e != nil {
		t.Fatal(e)
	}
	if e := local.ReadJSON(filepath.Join(f.remote, "release.json"), &state); e != nil {
		t.Fatal(e)
	}
	if state.IsDraft || len(state.Assets) != 3 || state.Name != "Matrix Admin v0.2.0" || state.Body != "notes" {
		t.Fatal("not published complete")
	}
	if e := f.run(""); e != nil {
		t.Fatal("completed retry failed", e)
	}
	if e := os.WriteFile(filepath.Join(f.remote, "AGENT.md"), []byte("different"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := f.run(""); e == nil {
		t.Fatal("conflicting published release accepted")
	}
	if e := local.ReadJSON(filepath.Join(f.remote, "release.json"), &state); e != nil {
		t.Fatal(e)
	}
	if state.IsDraft {
		t.Fatal("published release hidden")
	}
}

func TestPublisherCompletesPublicReleaseAndRetriesWithoutHidingIt(t *testing.T) {
	f := newPublisherFixture(t)
	state := githubFixture{IsDraft: false, Assets: make([]struct {
		Name string `json:"name"`
	}, 0)}
	statefile := filepath.Join(f.remote, "release.json")
	if e := local.JSON(statefile, state); e != nil {
		t.Fatal(e)
	}
	if e := f.run("SHA256SUMS"); e == nil {
		t.Fatal("upload failure ignored")
	}
	if e := local.ReadJSON(statefile, &state); e != nil {
		t.Fatal(e)
	}
	if state.IsDraft || len(state.Assets) != 1 || state.Body != "" {
		t.Fatal("public release must stay public and defer notes until all uploads succeed")
	}
	if e := f.run(""); e != nil {
		t.Fatal(e)
	}
	if e := local.ReadJSON(statefile, &state); e != nil {
		t.Fatal(e)
	}
	if state.IsDraft || len(state.Assets) != 3 || state.Name != "Matrix Admin v0.2.0" || state.Body != "notes" {
		t.Fatal("public release was not completed")
	}
}

func TestPublisherChecksAllExistingAssetsBeforeUploadingMissingOnes(t *testing.T) {
	f := newPublisherFixture(t)
	state := githubFixture{IsDraft: false}
	state.Assets = append(state.Assets, struct {
		Name string `json:"name"`
	}{Name: "matrix-admin-linux-amd64.tar.gz"})
	statefile := filepath.Join(f.remote, "release.json")
	if e := local.JSON(statefile, state); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(f.remote, "matrix-admin-linux-amd64.tar.gz"), []byte("different build"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := f.run(""); e == nil {
		t.Fatal("conflicting existing build accepted")
	}
	if e := local.ReadJSON(statefile, &state); e != nil {
		t.Fatal(e)
	}
	if state.IsDraft || len(state.Assets) != 1 {
		t.Fatal("release changed before existing assets were validated")
	}
}
