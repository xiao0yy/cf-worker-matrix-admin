package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/payload"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/storage"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const checkpointCommand = "server checkpoint-database --path /var/lib/tuwunel/checkpoint-restic"

type BackupJob struct {
	Schema     int        `json:"schema"`
	Run        string     `json:"run_id"`
	Snapshot   string     `json:"snapshot_id"`
	Verified   bool       `json:"verified"`
	Full       bool       `json:"full_restore"`
	Startup    bool       `json:"database_startup_verified"`
	VerifiedAt string     `json:"verified_at"`
	IndexMatch string     `json:"index_match"`
	Repository Repository `json:"repository"`
}

func (e *Engine) permit(ctx context.Context, op string, binding *Binding) (Status, error) {
	s, err := e.status(ctx)
	if err != nil {
		return s, err
	}
	if s.Control == nil || s.Cluster != e.Cluster || s.Control.Owner != s.Instance || s.Control.Epoch != s.Epoch {
		return s, errors.New("instance does not own current epoch")
	}
	if op == "" {
		if !s.Serving || s.Blocked || s.Control.Phase != "ONLINE" {
			return s, errors.New("daily backup requires online owner")
		}
	} else {
		if !s.Blocked || s.Serving || s.Final == nil || s.Final.ID != op || s.Final.Phase != "SNAPSHOTTING" || s.Control.Phase != "SNAPSHOTTING" {
			return s, errors.New("final backup requires frozen ownership")
		}
		if err = e.action(ctx, "final-verify", op, s.Instance, s.Epoch); err != nil {
			return s, err
		}
	}
	if binding != nil && (binding.Cluster != s.Cluster || binding.Instance != s.Instance || binding.Epoch != s.Epoch) {
		return s, errors.New("staged backup belongs to another epoch")
	}
	return s, nil
}
func (e *Engine) Backup(ctx context.Context, full bool) error {
	if err := e.connect(); err != nil {
		return err
	}
	root, _, err := e.active(ctx)
	if err != nil {
		return err
	}
	state := filepath.Join(root, "backup-state/tuwunel-restic")
	unlock, err := local.Lock(filepath.Join(state, "operation.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return e.backup(ctx, root, "", full)
}
func (e *Engine) backup(ctx context.Context, root, op string, full bool) error {
	s, err := e.permit(ctx, op, nil)
	if err != nil {
		return err
	}
	state := filepath.Join(root, "backup-state/tuwunel-restic")
	base := state
	if op != "" {
		base = filepath.Join(state, "finals", op)
		full = true
	}
	if err = os.MkdirAll(base, 0700); err != nil {
		return err
	}
	pending := filepath.Join(base, "pending")
	if _, err = os.Stat(pending); errors.Is(err, os.ErrNotExist) {
		if err = e.stage(ctx, root, base, op, s); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	bundle := filepath.Join(pending, "bundle")
	var meta Metadata
	if err = local.ReadJSON(filepath.Join(bundle, "metadata.json"), &meta); err != nil {
		return err
	}
	if _, err = e.permit(ctx, op, &meta.Binding); err != nil {
		return err
	}
	if err = payload.Verify(bundle); err != nil {
		return err
	}
	jobfile := filepath.Join(pending, "job.json")
	var job BackupJob
	if _, err = os.Stat(jobfile); errors.Is(err, os.ErrNotExist) {
		point, _, err := e.readPoint(ctx, "", "")
		if err != nil {
			return err
		}
		repo, err := e.repository(ctx, point)
		if err != nil {
			return err
		}
		_, etag, err := e.Store.Get(ctx, e.prefix()+"/latest.json")
		if err != nil {
			return err
		}
		if etag == "" {
			return errors.New("missing recovery index ETag")
		}
		job = BackupJob{Schema: 1, Run: "backup-" + local.RandomID(), IndexMatch: etag, Repository: repo, Full: full}
		var previous struct {
			Verified string `json:"verified_at"`
		}
		previousBytes, readErr := local.ReadFile(filepath.Join(state, "last-full-verify.json"), 1<<20)
		if readErr != nil || json.Unmarshal(previousBytes, &previous) != nil || !strings.HasPrefix(previous.Verified, e.Now().UTC().Format("2006-01-02")) {
			job.Full = true
		}
		if err = local.JSON(jobfile, job); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err = local.ReadJSON(jobfile, &job); err != nil {
		return err
	}
	if job.Schema != 1 || !local.SafeID(job.Run) {
		return errors.New("invalid backup journal")
	}
	if err = e.verifyRepository(ctx, job.Repository); err != nil {
		return err
	}
	if job.Snapshot == "" {
		b, err := e.rest(ctx, job.Repository, "", "snapshots", "--json", "--tag", job.Run)
		if err != nil {
			return err
		}
		var snapshots []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(b, &snapshots) != nil || len(snapshots) > 1 {
			return errors.New("ambiguous snapshot history")
		}
		if len(snapshots) == 0 {
			if _, err = e.rest(ctx, job.Repository, pending, "backup", "--read-concurrency", "1", "--json", "--host", meta.Server, "--tag", "matrix-admin", "--tag", "recovery-v2", "--tag", job.Run, "bundle/payload/database", "bundle/payload/telegram/data/telegram.db"); err != nil {
				return err
			}
			b, err = e.rest(ctx, job.Repository, "", "snapshots", "--json", "--tag", job.Run)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(b, &snapshots); err != nil {
				return err
			}
		}
		if len(snapshots) != 1 || !local.SHA.MatchString(snapshots[0].ID) {
			return errors.New("snapshot full ID unavailable")
		}
		job.Snapshot = snapshots[0].ID
		if err = local.JSON(jobfile, job); err != nil {
			return err
		}
	}
	if !job.Verified {
		if job.Full {
			size, err := treeSize(bundle)
			if err != nil {
				return err
			}
			if err = requireSpace(base, size*3+(512<<20)); err != nil {
				return err
			}
			verify, err := os.MkdirTemp(base, "verify-")
			if err != nil {
				return err
			}
			if err = payload.Unpack(filepath.Join(pending, "configuration.json"), filepath.Join(verify, "bundle")); err != nil {
				return err
			}
			if err = e.downloadData(ctx, job.Repository, job.Snapshot, filepath.Join(verify, "bundle"), verify); err != nil {
				return err
			}
			if err = e.verifyTelegram(ctx, filepath.Join(verify, "bundle/payload/telegram/data/telegram.db"), meta.Telegram); err != nil {
				return err
			}
			if op != "" {
				if err = e.probe(ctx, filepath.Join(verify, "bundle"), meta, filepath.Join(verify, "probe")); err != nil {
					return err
				}
				job.Startup = true
			}
			if err = os.RemoveAll(verify); err != nil {
				return err
			}
		} else {
			if err = e.verifyTree(ctx, job.Repository, job.Snapshot, bundle); err != nil {
				return err
			}
		}
		job.Verified = true
		job.VerifiedAt = e.stamp()
		if err = local.JSON(jobfile, job); err != nil {
			return err
		}
	}
	if _, err = e.permit(ctx, op, &meta.Binding); err != nil {
		return err
	}
	point, receipt, err := e.publish(ctx, meta, pending, job)
	if err != nil {
		return err
	}
	success := map[string]any{"snapshot_id": job.Snapshot, "verified_at": job.VerifiedAt, "verification": point.Verification, "data_time": meta.Time, "consistency": meta.Consistency, "instance_control": meta.Binding, "telegram_included": true, "media_included": false, "database_startup_verified": job.Startup, "repository_id": job.Repository.ID}
	if err = local.JSON(filepath.Join(base, "last-success.json"), success); err != nil {
		return err
	}
	if job.Full && op == "" {
		if err = local.JSON(filepath.Join(state, "last-full-verify.json"), success); err != nil {
			return err
		}
	}
	if op != "" {
		if err = local.JSON(filepath.Join(base, "published.json"), receipt); err != nil {
			return err
		}
	}
	return os.RemoveAll(pending)
}
func (e *Engine) stage(ctx context.Context, root, base, op string, s Status) error {
	preparing := filepath.Join(base, "preparing")
	bundle := filepath.Join(preparing, "bundle")
	project := filepath.Join(root, "deploy/tuwunel")
	data := filepath.Join(project, "data")
	if err := os.MkdirAll(filepath.Join(bundle, "payload"), 0700); err != nil {
		return err
	}
	c, err := e.inspect(ctx, "tuwunel")
	if err != nil {
		return err
	}
	if c.mount("/var/lib/tuwunel") != data {
		return errors.New("Tuwunel data mount differs")
	}
	if err = requireSpace(base, 1<<30); err != nil {
		return err
	}
	stamp := e.stamp()
	database := filepath.Join(bundle, "payload/database")
	if op != "" {
		if !c.cleanStop() {
			return errors.New("final database copy requires clean stop")
		}
		stamp = s.Final.Cutoff
		if _, err = os.Stat(database); err == nil {
			// The writer is still fenced; preserve the partial copy and restart staging.
			if err = os.Rename(database, filepath.Join(base, "interrupted-database-"+local.RandomID())); err != nil {
				return err
			}
		}
		before, err := local.Manifest(data)
		if err != nil {
			return err
		}
		if err = local.CopyTree(data, database); err != nil {
			return err
		}
		after, err := local.Manifest(data)
		if err != nil {
			return err
		}
		copied, err := local.Manifest(database)
		if err != nil || before != after || before != copied {
			return errors.New("frozen database changed")
		}
	} else {
		request := filepath.Join(preparing, "request.json")
		var req struct {
			At        string    `json:"at"`
			Container Container `json:"container"`
		}
		if _, err = os.Stat(request); errors.Is(err, os.ErrNotExist) {
			if !c.State.Running || c.State.Restarting {
				return errors.New("Tuwunel not running")
			}
			if _, err = e.docker(ctx, "exec", c.ID, "tuwunel", "--health-check"); err != nil {
				return err
			}
			signal, err := c.env("TUWUNEL_ADMIN_SIGNAL_EXECUTE")
			var commands []string
			if err != nil || json.Unmarshal([]byte(signal), &commands) != nil || len(commands) != 1 || commands[0] != checkpointCommand {
				return errors.New("checkpoint signal contract differs")
			}
			if _, err = os.Lstat(filepath.Join(data, "checkpoint-restic")); !errors.Is(err, os.ErrNotExist) {
				return errors.New("unclassified checkpoint already exists")
			}
			req.At = stamp
			req.Container = c
			if err = local.JSON(request, req); err != nil {
				return err
			}
			if _, err = e.docker(ctx, "kill", "--signal", "USR2", c.ID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if err = local.ReadJSON(request, &req); err != nil {
			return err
		}
		stamp = req.At
		if _, err = os.Stat(database); errors.Is(err, os.ErrNotExist) {
			if err = e.wait(ctx, 5*time.Minute, func() error {
				after, err := e.inspect(ctx, "tuwunel")
				if err != nil {
					return err
				}
				if !sameContainer(req.Container, after) {
					return errors.New("Tuwunel restarted during checkpoint")
				}
				logs, err := e.docker(ctx, "logs", "--since", req.At, "--tail", "2000", req.Container.ID)
				if err != nil {
					return err
				}
				text := regexp.MustCompile(`\x1b\[[0-9;]*[[:alpha:]]`).ReplaceAllString(string(logs), "")
				text = strings.NewReplacer("\n", " ", "\r", " ").Replace(text)
				if !regexp.MustCompile("Created checkpoint at[[:space:]`]+/var/lib/tuwunel/checkpoint-restic[`]?\\.").MatchString(text) {
					return errors.New("checkpoint not complete")
				}
				return nil
			}); err != nil {
				return err
			}
			if err = os.Rename(filepath.Join(data, "checkpoint-restic"), database); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	current, err := os.ReadFile(filepath.Join(database, "CURRENT"))
	if err != nil || !regexp.MustCompile(`^MANIFEST-[0-9]+$`).MatchString(strings.TrimSpace(string(current))) {
		return errors.New("RocksDB CURRENT invalid")
	}
	if st, err := os.Stat(filepath.Join(database, strings.TrimSpace(string(current)))); err != nil || st.Size() == 0 {
		return errors.New("RocksDB manifest missing")
	}
	// Only configuration subtrees created by this operation may be replaced on retry.
	for _, n := range []string{"config", "telegram", "nginx", "control"} {
		if err = os.RemoveAll(filepath.Join(bundle, "payload", n)); err != nil {
			return err
		}
	}
	config := filepath.Join(bundle, "payload/config")
	if err = os.MkdirAll(config, 0700); err != nil {
		return err
	}
	for _, n := range []string{"docker-compose.yml", "registration-token"} {
		if err = local.Copy(filepath.Join(project, n), filepath.Join(config, n)); err != nil {
			return err
		}
	}
	if err = copyApplicationConfiguration(project, config); err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(project, ".env")); err == nil {
		if err = local.Copy(filepath.Join(project, ".env"), filepath.Join(config, ".env")); err != nil {
			return err
		}
	}
	for _, n := range []string{"matrix.conf", "00-matrix-common.conf"} {
		if err = local.Copy(filepath.Join(e.Host.NginxDirectory, n), filepath.Join(bundle, "payload/nginx", n)); err != nil {
			return err
		}
	}
	if err = local.Copy(e.controlConfig(), filepath.Join(bundle, "payload/control/source-local.json")); err != nil {
		return err
	}
	tg, err := e.telegramSnapshot(ctx, filepath.Join(root, "deploy/telegram"), filepath.Join(bundle, "payload/telegram"), op != "")
	if err != nil {
		return err
	}
	tgc, err := e.inspect(ctx, "telegram-bridge")
	if err != nil {
		return err
	}
	ti, err := e.image(ctx, c)
	if err != nil {
		return err
	}
	gi, err := e.image(ctx, tgc)
	if err != nil {
		return err
	}
	domain, err := c.env("TUWUNEL_SERVER_NAME")
	if err != nil || !regexp.MustCompile(`^[a-zA-Z0-9.-]+$`).MatchString(domain) {
		return errors.New("invalid Matrix server identity")
	}
	mediaPrefix, err := c.env("TUWUNEL_STORAGE_PROVIDER__MEDIA_S3__S3__BASE_PATH")
	if err != nil {
		return err
	}
	software, err := release.Current(e.ReleaseDir)
	if err != nil {
		return err
	}
	meta := Metadata{Format: "tuwunel-checkpoint-v2", Recovery: 2, Layout: "database-only-v1", Time: stamp, Image: c.Image, Server: domain, Checkpoint: checkpointCommand, Software: software, Images: map[string]Image{"tuwunel": ti, "telegram": gi}, Media: Media{"media_s3", mediaPrefix, "external-s3", "reuse-existing"}, TelegramIncluded: true, Consistency: "independent-online-snapshots", Telegram: tg, Binding: Binding{1, s.Cluster, s.Instance, s.Epoch, "controller-admission"}}
	if op != "" {
		meta.Operation = op
		meta.Consistency = "globally-quiesced"
		meta.Stopped = s.Final.Proof
	}
	if _, err = e.permit(ctx, op, &meta.Binding); err != nil {
		return err
	}
	if err = local.JSON(filepath.Join(bundle, "metadata.json"), meta); err != nil {
		return err
	}
	manifest, err := local.Manifest(filepath.Join(bundle, "payload"))
	if err != nil {
		return err
	}
	if err = local.Atomic(filepath.Join(bundle, "manifest.sha256"), []byte(manifest), 0600); err != nil {
		return err
	}
	if err = payload.Verify(bundle); err != nil {
		return err
	}
	capsule := filepath.Join(preparing, "configuration.json")
	if _, err = os.Lstat(capsule); err == nil {
		if err = os.Rename(capsule, filepath.Join(preparing, "interrupted-configuration-"+local.RandomID())); err != nil {
			return err
		}
	}
	if err = payload.Pack(bundle, capsule); err != nil {
		return err
	}
	return os.Rename(preparing, filepath.Join(base, "pending"))
}

// S3 configuration lives beside documentation and examples on existing hosts.
// Only the mounted runtime file belongs to the recovery configuration capsule.
func copyApplicationConfiguration(project, config string) error {
	if err := local.CopyTree(filepath.Join(project, "appservices"), filepath.Join(config, "appservices")); err != nil {
		return err
	}
	return local.Copy(filepath.Join(project, "s3/media.env"), filepath.Join(config, "s3/media.env"))
}
func (e *Engine) verifyTree(ctx context.Context, r Repository, snapshot, bundle string) error {
	b, err := e.rest(ctx, r, "", "ls", "--json", "--recursive", snapshot, "/bundle")
	if err != nil {
		return err
	}
	remote := map[string]int64{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		var n struct {
			Struct string `json:"struct_type"`
			Type   string `json:"type"`
			Path   string `json:"path"`
			Size   int64  `json:"size"`
		}
		if err = json.Unmarshal([]byte(line), &n); err != nil {
			return err
		}
		if n.Struct == "node" && n.Type == "file" {
			if _, ok := remote[n.Path]; ok {
				return errors.New("duplicate remote file")
			}
			remote[n.Path] = n.Size
		}
	}
	source := map[string]int64{}
	for _, rel := range []string{"database", "telegram/data/telegram.db"} {
		err = filepath.WalkDir(filepath.Join(bundle, "payload", rel), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			st, err := d.Info()
			if err != nil {
				return err
			}
			r, err := filepath.Rel(filepath.Join(bundle, "payload"), p)
			if err != nil {
				return err
			}
			source["/bundle/payload/"+filepath.ToSlash(r)] = st.Size()
			return nil
		})
		if err != nil {
			return err
		}
	}
	if !sameJSON(source, remote) {
		return errors.New("remote database inventory differs")
	}
	return nil
}
func (e *Engine) publish(ctx context.Context, m Metadata, pending string, j BackupJob) (Point, any, error) {
	p := Point{Protocol: 2, Schema: 2, Catalog: 1, Layout: m.Layout, Format: m.Format, Recovery: 2, Platform: "linux/amd64", Snapshot: j.Snapshot, RepositoryID: j.Repository.ID, Software: m.Software, Server: m.Server, Time: m.Time, Startup: j.Startup, Image: m.Image, Images: m.Images, Consistency: m.Consistency, Telegram: m.Telegram, Operation: m.Operation, Stopped: m.Stopped, Media: m.Media, Binding: m.Binding, Verification: "database-tree"}
	p.Scope.Telegram = true
	if j.Full {
		p.Verification = "full-restore"
	}
	repo := release.JSON(j.Repository)
	capsule, err := local.ReadFile(filepath.Join(pending, "configuration.json"), 32<<20)
	if err != nil {
		return p, nil, err
	}
	p.RepositoryHash = local.Digest(repo)
	p.ConfigurationHash = local.Digest(capsule)
	p.RepositoryObject = e.prefix() + "/repositories/" + p.RepositoryID + "-" + p.RepositoryHash + ".json"
	p.ConfigurationObject = e.prefix() + "/configurations/" + p.ConfigurationHash + ".json"
	if err = p.validate(e.Cluster, e.prefix()); err != nil {
		return p, nil, err
	}
	cache := filepath.Join(e.Host.Root, "agent/cache/releases", m.Software.SHA)
	if err = os.MkdirAll(filepath.Dir(cache), 0700); err != nil {
		return p, nil, err
	}
	if _, err = os.Stat(cache); errors.Is(err, os.ErrNotExist) {
		if err = release.Fetch(ctx, m.Software.Version, m.Software.SHA, cache); err != nil {
			return p, nil, fmt.Errorf("public release is not retrievable: %w", err)
		}
	}
	desc, err := release.Describe(cache)
	if err != nil || desc != m.Software {
		return p, nil, errors.New("public release cache differs")
	}
	if err = immutable(ctx, e.Store, p.RepositoryObject, repo); err != nil {
		return p, nil, err
	}
	if err = immutable(ctx, e.Store, p.ConfigurationObject, capsule); err != nil {
		return p, nil, err
	}
	key := e.prefix() + "/points/" + p.Snapshot + ".json"
	point := release.JSON(p)
	if err = immutable(ctx, e.Store, key, point); err != nil {
		return p, nil, err
	}
	if _, err = e.permit(ctx, m.Operation, &m.Binding); err != nil {
		return p, nil, err
	}
	if m.Operation != "" {
		if !j.Full || !j.Startup || m.Consistency != "globally-quiesced" {
			return p, nil, errors.New("final backup not fully verified")
		}
		receipt := map[string]any{"schema": 1, "operation_id": m.Operation, "cluster_id": e.Cluster, "source_instance_id": m.Binding.Instance, "source_epoch": m.Binding.Epoch, "cutoff_time": m.Time, "snapshot_id": j.Snapshot, "point_key": key, "point_sha256": local.Digest(point), "full_restore_verified": true, "database_startup_verified": true, "verified_at": j.VerifiedAt}
		return p, receipt, nil
	}
	index := release.JSON(Index{2, 2, key, local.Digest(point)})
	err = e.Store.Put(ctx, e.prefix()+"/latest.json", index, j.IndexMatch)
	if err != nil && !errors.Is(err, storage.Conflict) {
		return p, nil, err
	}
	got, _, readErr := e.Store.Get(ctx, e.prefix()+"/latest.json")
	if readErr != nil {
		return p, nil, readErr
	}
	if string(got) != string(index) {
		return p, nil, errors.New("latest changed; previous success preserved")
	}
	return p, nil, nil
}
