package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/payload"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/storage"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func (e *Engine) readPoint(ctx context.Context, snapshot, expectedHash string) (Point, []byte, error) {
	var p Point
	if err := e.connect(); err != nil {
		return p, nil, err
	}
	key := ""
	if snapshot == "" {
		b, _, err := e.Store.Get(ctx, e.prefix()+"/latest.json")
		if err != nil {
			return p, nil, err
		}
		var index Index
		if err = local.Decode(b, &index); err != nil {
			return p, nil, err
		}
		if index.Protocol != 2 || index.Schema != 2 || !local.SHA.MatchString(index.Hash) {
			return p, nil, errors.New("invalid recovery index")
		}
		key = index.Key
		expectedHash = index.Hash
		snapshot = strings.TrimSuffix(strings.TrimPrefix(key, e.prefix()+"/points/"), ".json")
	} else {
		if !local.SHA.MatchString(snapshot) || !local.SHA.MatchString(expectedHash) {
			return p, nil, errors.New("explicit point requires full snapshot and SHA256")
		}
		key = e.prefix() + "/points/" + snapshot + ".json"
	}
	if !local.SHA.MatchString(snapshot) || key != e.prefix()+"/points/"+snapshot+".json" {
		return p, nil, errors.New("invalid point namespace")
	}
	if _, _, er := e.Store.Get(ctx, e.prefix()+"/retired/"+snapshot+".json"); !errors.Is(er, storage.Missing) {
		return p, nil, errors.New("recovery point retired or retirement status unavailable")
	}
	b, _, err := e.Store.Get(ctx, key)
	if err != nil {
		return p, nil, err
	}
	if local.Digest(b) != expectedHash {
		return p, nil, errors.New("point checksum mismatch")
	}
	if err = local.Decode(b, &p); err != nil {
		return p, nil, err
	}
	if err = p.validate(e.Cluster, e.prefix()); err != nil {
		return p, nil, err
	}
	if p.Snapshot != snapshot {
		return p, nil, errors.New("point snapshot differs")
	}
	return p, b, nil
}
func (e *Engine) configuration(ctx context.Context, p Point, work string) (string, Metadata, error) {
	var m Metadata
	b, _, err := e.Store.Get(ctx, p.ConfigurationObject)
	if err != nil {
		return "", m, err
	}
	if local.Digest(b) != p.ConfigurationHash {
		return "", m, errors.New("configuration checksum mismatch")
	}
	input := filepath.Join(work, "configuration.json")
	if err = local.Atomic(input, b, 0600); err != nil {
		return "", m, err
	}
	bundle := filepath.Join(work, "bundle")
	if err = payload.Unpack(input, bundle); err != nil {
		return "", m, err
	}
	if err = local.ReadJSON(filepath.Join(bundle, "metadata.json"), &m); err != nil {
		return "", m, err
	}
	if !p.matches(m) {
		return "", m, errors.New("configuration metadata differs from recovery point")
	}
	return bundle, m, nil
}
func (e *Engine) repository(ctx context.Context, p Point) (Repository, error) {
	var r Repository
	b, _, err := e.Store.Get(ctx, p.RepositoryObject)
	if err != nil {
		return r, err
	}
	if local.Digest(b) != p.RepositoryHash {
		return r, errors.New("repository record checksum mismatch")
	}
	if err = local.Decode(b, &r); err != nil {
		return r, err
	}
	u, err := url.Parse(r.Endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Host == "" || r.Schema != 1 || r.ID != p.RepositoryID || !local.SafeID(r.ID) || !local.SafeID(r.Bucket) || !local.SafeID(r.Region) || !regexp.MustCompile(`^[a-zA-Z0-9/_-]+$`).MatchString(r.Path) || r.Path == "" || r.Access == "" || r.Secret == "" || r.Password == "" {
		return r, errors.New("invalid database repository record")
	}
	return r, nil
}
func (e *Engine) rest(ctx context.Context, r Repository, dir string, args ...string) ([]byte, error) {
	env := []string{"RESTIC_REPOSITORY=s3:" + r.Endpoint + "/" + r.Bucket + "/" + r.Path, "RESTIC_PASSWORD=" + r.Password, "AWS_ACCESS_KEY_ID=" + r.Access, "AWS_SECRET_ACCESS_KEY=" + r.Secret, "AWS_DEFAULT_REGION=" + r.Region, "RESTIC_CACHE_DIR=" + filepath.Join(e.Host.Root, "agent/cache/restic"), "GOMAXPROCS=1", "RESTIC_READ_CONCURRENCY=1"}
	timeout := 90 * time.Minute
	if len(args) > 0 && (args[0] == "backup" || args[0] == "restore") {
		timeout = -1
	}
	return e.Runner.Run(ctx, Command{Name: filepath.Join(e.ReleaseDir, "restic"), Args: append([]string{"--quiet", "--compression", "auto", "-o", "s3.connections=2"}, args...), Dir: dir, Env: env, Timeout: timeout})
}
func (e *Engine) verifyRepository(ctx context.Context, r Repository) error {
	b, err := e.rest(ctx, r, "", "cat", "config")
	if err != nil {
		return err
	}
	var v struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &v) != nil || v.ID != r.ID {
		return errors.New("B2 repository identity mismatch")
	}
	return nil
}
func (e *Engine) downloadData(ctx context.Context, r Repository, snapshot, bundle, work string) error {
	download := filepath.Join(work, "download")
	binding := struct {
		Repository string `json:"repository_id"`
		Snapshot   string `json:"snapshot_id"`
	}{r.ID, snapshot}
	marker := filepath.Join(work, "download-binding.json")
	if b, err := os.ReadFile(marker); err == nil {
		if string(b) != string(release.JSON(binding)) {
			return errors.New("restore cache belongs to another snapshot")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if _, er := os.Lstat(download); !errors.Is(er, os.ErrNotExist) {
			return errors.New("unclassified restore destination already exists")
		}
		if err = local.Atomic(marker, release.JSON(binding), 0600); err != nil {
			return err
		}
	} else {
		return err
	}
	if err := filepath.WalkDir(download, func(p string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) && p == download {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return errors.New("unsafe restore cache entry")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := local.SafePath(download); err != nil {
		return err
	}
	// Restic authenticates restored blocks and repairs partial files on retry.
	// --verify would add a separate full read of the restored files. Deep reads
	// belong to an explicit restore drill, never the normal handover path.
	if _, err := e.rest(ctx, r, "", "restore", snapshot, "--target", download, "--overwrite", "always"); err != nil {
		return err
	}
	return payload.Merge(download, bundle)
}
