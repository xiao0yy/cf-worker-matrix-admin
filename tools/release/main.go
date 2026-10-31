// This development-only tool builds release assets. It is not shipped to VPSes.
package main

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
)

func command(name string, args ...string) (string, error) {
	b, e := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(b)), e
}
func identity(tag string, dev bool) (string, string, error) {
	commit, e := command("git", "rev-parse", "HEAD")
	if e != nil {
		return "", "", e
	}
	version := strings.TrimPrefix(tag, "v")
	if !strings.HasPrefix(tag, "v") || !release.ValidVersion(version) {
		return "", "", errors.New("--tag must be a fixed vX.Y.Z tag")
	}
	if !dev {
		tagged, e := command("git", "rev-parse", tag+"^{commit}")
		if e != nil || tagged != commit {
			return "", "", errors.New("tag must resolve to tested HEAD")
		}
		dirty, e := command("git", "status", "--porcelain")
		if e != nil || dirty != "" {
			return "", "", errors.New("release requires clean source")
		}
	}
	return version, commit, nil
}
func restic(ctx context.Context, target, provided string) error {
	var lock struct {
		Schema int `json:"schema"`
		Restic struct {
			Version, Platform, URL string
			SHA                    string `json:"binary_sha256"`
		}
	}
	if e := local.ReadJSON("dependencies.lock.json", &lock); e != nil {
		return e
	}
	if lock.Schema != 1 || lock.Restic.Version != release.ResticVersion || lock.Restic.Platform != "linux/amd64" || !local.SHA.MatchString(lock.Restic.SHA) {
		return errors.New("invalid dependency lock")
	}
	if provided != "" {
		if e := local.Copy(provided, target); e != nil {
			return e
		}
	} else {
		expected := "https://github.com/restic/restic/releases/download/v" + release.ResticVersion + "/restic_" + release.ResticVersion + "_linux_amd64.bz2"
		if lock.Restic.URL != expected {
			return errors.New("unexpected dependency origin")
		}
		client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Scheme != "https" || len(via) > 5 {
				return errors.New("unsafe dependency redirect")
			}
			return nil
		}}
		req, e := http.NewRequestWithContext(ctx, "GET", expected, nil)
		if e != nil {
			return e
		}
		res, e := client.Do(req)
		if e != nil {
			return e
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("dependency HTTP %d", res.StatusCode)
		}
		out, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if e != nil {
			return e
		}
		n, e := io.Copy(out, io.LimitReader(bzip2.NewReader(io.LimitReader(res.Body, 64<<20)), (128<<20)+1))
		closeErr := out.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if n > 128<<20 {
			return errors.New("dependency size limit")
		}
	}
	got, e := local.FileHash(target)
	if e != nil || got != lock.Restic.SHA {
		return errors.New("restic checksum mismatch")
	}
	return os.Chmod(target, 0700)
}
func build(ctx context.Context, tag, out, dependency string, dev bool) error {
	version, commit, e := identity(tag, dev)
	if e != nil {
		return e
	}
	out, e = filepath.Abs(out)
	if e != nil {
		return e
	}
	if e = local.SafePath(out); e != nil {
		return e
	}
	if _, e = os.Stat(out); !errors.Is(e, os.ErrNotExist) {
		return errors.New("output directory must be new")
	}
	if e = os.MkdirAll(filepath.Dir(out), 0700); e != nil {
		return e
	}
	stage, e := os.MkdirTemp(filepath.Dir(out), ".build-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	tree := filepath.Join(stage, "runtime")
	if e = os.Mkdir(tree, 0700); e != nil {
		return e
	}
	flags := "-s -w -buildid= -X github.com/xiao0yy/cf-worker-matrix-admin/internal/release.Version=" + version + " -X github.com/xiao0yy/cf-worker-matrix-admin/internal/release.Commit=" + commit
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags", flags, "-o", filepath.Join(tree, "matrix-admin"), "./cmd/matrix-admin")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	cmd.Stderr = os.Stderr
	if e = cmd.Run(); e != nil {
		return e
	}
	if e = os.Chmod(filepath.Join(tree, "matrix-admin"), 0700); e != nil {
		return e
	}
	if e = restic(ctx, filepath.Join(tree, "restic"), dependency); e != nil {
		return e
	}
	for src, dst := range map[string]string{"docs/AGENT.md": "AGENT.md", "LICENSE": "LICENSE", "docs/THIRD-PARTY.md": "THIRD-PARTY.md"} {
		if e = local.Copy(src, filepath.Join(tree, dst)); e != nil {
			return e
		}
		if e = os.Chmod(filepath.Join(tree, dst), 0600); e != nil {
			return e
		}
	}
	m := release.Manifest{Schema: 1, Project: "matrix-admin", Version: version, Commit: commit, Platform: "linux/amd64", Runtime: "native-go-v1", RecoveryReaders: []int{2}, Control: 1, Mailbox: 1, Restic: release.ResticVersion}
	names := []string{"AGENT.md", "LICENSE", "THIRD-PARTY.md", "matrix-admin", "restic"}
	for _, n := range names {
		p := filepath.Join(tree, n)
		s, e := os.Stat(p)
		if e != nil {
			return e
		}
		hash, e := local.FileHash(p)
		if e != nil {
			return e
		}
		m.Files = append(m.Files, release.File{Path: n, SHA: hash, Size: s.Size(), Executable: s.Mode()&0111 != 0})
	}
	if e = local.JSON(filepath.Join(tree, "manifest.json"), m); e != nil {
		return e
	}
	if _, e = release.Verify(tree); e != nil {
		return e
	}
	assets := filepath.Join(stage, "assets")
	if e = os.Mkdir(assets, 0700); e != nil {
		return e
	}
	archive := filepath.Join(assets, "matrix-admin-linux-amd64.tar.gz")
	f, e := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	names = append(names, "manifest.json")
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(tree, n)
		s, e := os.Stat(p)
		if e != nil {
			return e
		}
		mode := int64(0600)
		if s.Mode()&0111 != 0 {
			mode = 0700
		}
		if e = tw.WriteHeader(&tar.Header{Name: n, Size: s.Size(), Mode: mode, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); e != nil {
			return e
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		_, e = io.Copy(tw, in)
		in.Close()
		if e != nil {
			return e
		}
	}
	if e = tw.Close(); e != nil {
		return e
	}
	if e = gz.Close(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = local.Copy(filepath.Join(tree, "AGENT.md"), filepath.Join(assets, "AGENT.md")); e != nil {
		return e
	}
	var sums string
	for _, n := range []string{"AGENT.md", "matrix-admin-linux-amd64.tar.gz"} {
		hash, e := local.FileHash(filepath.Join(assets, n))
		if e != nil {
			return e
		}
		sums += hash + "  " + n + "\n"
	}
	if e = local.Atomic(filepath.Join(assets, "SHA256SUMS"), []byte(sums), 0600); e != nil {
		return e
	}
	return os.Rename(assets, out)
}
func notes(tag string) error {
	version, commit, e := identity(tag, false)
	if e != nil {
		return e
	}
	tags, e := command("git", "tag", "--merged", "HEAD", "--sort=-version:refname")
	if e != nil {
		return e
	}
	previous := ""
	for _, t := range strings.Split(tags, "\n") {
		if t != tag && strings.HasPrefix(t, "v") && release.ValidVersion(t[1:]) {
			previous = t
			break
		}
	}
	revision := commit
	if previous != "" {
		revision = previous + ".." + commit
	}
	log, e := command("git", "log", "--reverse", "--format=- %s (%h)", revision)
	if e != nil {
		return e
	}
	fmt.Printf("Matrix Admin %s\n\n%s\n\n下载 `matrix-admin-linux-amd64.tar.gz`，用 `SHA256SUMS` 校验，按同版本 `AGENT.md` 准备实例。运行包内包含统一 Go 程序与固定版本 restic，无需 clone 仓库。\n\n源码提交：`%s`。\n", version, log, commit)
	return nil
}
func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "release build|validate|notes --tag vX.Y.Z")
		os.Exit(1)
	}
	f := flag.NewFlagSet("release", flag.ExitOnError)
	tag := f.String("tag", os.Getenv("RELEASE_TAG"), "fixed version tag")
	out := f.String("out", "dist/release", "new output directory")
	dependency := f.String("restic", "", "verified local restic binary (optional)")
	dev := f.Bool("dev", false, "local untagged validation only; never publish")
	f.Parse(os.Args[2:])
	var e error
	switch os.Args[1] {
	case "build":
		e = build(context.Background(), *tag, *out, *dependency, *dev)
	case "validate":
		var v, c string
		v, c, e = identity(*tag, false)
		if e == nil {
			json.NewEncoder(os.Stdout).Encode(map[string]string{"version": v, "commit": c})
		}
	case "notes":
		e = notes(*tag)
	default:
		e = errors.New("unknown release command")
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
