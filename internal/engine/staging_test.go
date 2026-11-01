package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/payload"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
)

// Exercise the real cold staging path, starting from an application data
// directory rather than a prebuilt recovery bundle. Only Docker is simulated.
func TestFinalStageSeparatesDatabaseFromExternalMedia(t *testing.T) {
	for _, scenario := range []string{"empty-media", "cached-media", "local-provider", "mixed-providers", "unknown-directory"} {
		t.Run(scenario, func(t *testing.T) {
			e := testEngine(t)
			e.Host.SQLite = sqlite(t)
			e.Host.NginxDirectory = filepath.Join(e.Host.Root, "nginx")
			version := release.Version
			release.Version = "0.4.1"
			t.Cleanup(func() { release.Version = version })
			e.ReleaseDir = runtimeFixture(t, release.Version)
			root := e.Host.Root
			data := filepath.Join(root, "deploy/tuwunel/data")
			bridge := filepath.Join(root, "deploy/telegram")
			for p, content := range map[string]string{
				"deploy/tuwunel/data/CURRENT":            "MANIFEST-1\n",
				"deploy/tuwunel/data/MANIFEST-1":         "RocksDB metadata",
				"deploy/tuwunel/data/000002.sst":         "immutable table",
				"deploy/tuwunel/data/archive/000003.log": "archived WAL",
				"deploy/tuwunel/docker-compose.yml":      "compose fixture",
				"deploy/tuwunel/registration-token":      "test-registration",
				"deploy/tuwunel/s3/media.env":            "TEST_MEDIA=external",
				"deploy/telegram/docker-compose.yml":     "bridge compose",
				"deploy/telegram/data/config.yaml":       "bridge config",
				"deploy/telegram/data/registration.yaml": "bridge registration",
				"nginx/matrix.conf":                      "nginx fixture",
				"nginx/00-matrix-common.conf":            "common fixture",
			} {
				write(t, filepath.Join(root, p), content)
			}
			must(t, os.MkdirAll(filepath.Join(root, "deploy/tuwunel/appservices"), 0700))
			must(t, os.Mkdir(filepath.Join(data, "media"), 0700))
			if scenario == "cached-media" {
				write(t, filepath.Join(data, "media/cache/file"), "media must remain outside the database backup")
			}
			if scenario == "unknown-directory" {
				must(t, os.Mkdir(filepath.Join(data, "unexpected"), 0700))
			}
			database(t, filepath.Join(bridge, "data/telegram.db"))
			s := Status{Cluster: e.Cluster, Instance: "node-a", Epoch: 1, Blocked: true,
				Control: &control.State{Owner: "node-a", Epoch: 1, Phase: "SNAPSHOTTING"},
				Final:   &control.FinalOperation{ID: "move-stage", Source: "node-a", Epoch: 1, Phase: "SNAPSHOTTING", Cutoff: e.stamp()}}
			fakeController(t, e, &s, nil)
			providers := `["media_s3"]`
			if scenario == "local-provider" {
				providers = `["filesystem"]`
			}
			if scenario == "mixed-providers" {
				providers = `["media_s3","filesystem"]`
			}
			var tuwunel, telegram Container
			for _, c := range []*Container{&tuwunel, &telegram} {
				c.ID = "stopped-fixture"
				c.Image = "sha256:" + strings.Repeat("b", 64)
				c.Config.Image = "registry.example/image@sha256:" + strings.Repeat("a", 64)
				c.State.StartedAt = "fixed"
			}
			tuwunel.Config.Env = []string{"TUWUNEL_SERVER_NAME=matrix.example.com", "TUWUNEL_MEDIA_STORAGE_PROVIDERS=" + providers, "TUWUNEL_STORAGE_PROVIDER__MEDIA_S3__S3__BASE_PATH=media"}
			tuwunel.Mounts = append(tuwunel.Mounts, struct{ Source, Destination string }{data, "/var/lib/tuwunel"})
			telegram.Mounts = append(telegram.Mounts, struct{ Source, Destination string }{filepath.Join(bridge, "data"), "/data"})
			must(t, json.Unmarshal([]byte(`{"Networks":{"test-net":{"IPAddress":"172.30.0.3"}}}`), &telegram.NetworkSettings))
			real := e.Runner
			e.Runner = runFunc(func(ctx context.Context, c Command) ([]byte, error) {
				if c.Name != e.Host.Docker {
					if c.Name == e.Host.SQLite && strings.Contains(strings.ToLower(strings.Join(c.Args, " ")), "check;") {
						t.Fatal("cold staging performed a deep database scan")
					}
					return real.Run(ctx, c)
				}
				switch c.Args[0] {
				case "inspect":
					if c.Args[1] == "tuwunel" {
						return json.Marshal([]Container{tuwunel})
					}
					if c.Args[1] == "telegram-bridge" {
						return json.Marshal([]Container{telegram})
					}
				case "image":
					return json.Marshal([]map[string]string{{"Id": tuwunel.Image, "OS": "linux", "Architecture": "amd64"}})
				case "network":
					return []byte(`[{"Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.30.0.0/24","Gateway":"172.30.0.1"}]}}]`), nil
				}
				return nil, fmt.Errorf("unexpected Docker mutation: %v", c.Args)
			})
			base := filepath.Join(root, "backup-state/finals", s.Final.ID)
			err := e.stage(context.Background(), root, base, s.Final.ID, s)
			if strings.Contains(scenario, "provider") {
				if err == nil || !strings.Contains(err.Error(), "TUWUNEL_MEDIA_STORAGE_PROVIDERS") {
					t.Fatalf("unsupported media backend accepted: %v", err)
				}
				if _, err = os.Stat(filepath.Join(base, "preparing/bundle/payload/database")); !os.IsNotExist(err) {
					t.Fatal("database staged before verifying media provider")
				}
				return
			}
			if scenario == "unknown-directory" {
				if err == nil || !strings.Contains(err.Error(), `"database/unexpected"`) {
					t.Fatalf("missing offending directory: %v", err)
				}
				return
			}
			must(t, err)
			pending := filepath.Join(base, "pending")
			bundle := filepath.Join(pending, "bundle")
			must(t, payload.VerifyInventory(bundle))
			if _, err = os.Stat(filepath.Join(bundle, "payload/database/media")); !os.IsNotExist(err) {
				t.Fatal("media entered the database payload")
			}
			for _, name := range []string{"CURRENT", "MANIFEST-1", "000002.sst", "archive/000003.log"} {
				a, err := os.Stat(filepath.Join(data, name))
				must(t, err)
				b, err := os.Stat(filepath.Join(bundle, "payload/database", name))
				must(t, err)
				if !os.SameFile(a, b) {
					t.Fatal("cold database was copied", name)
				}
			}
			out := filepath.Join(t.TempDir(), "unpacked")
			must(t, payload.Unpack(filepath.Join(pending, "configuration.json"), out))
			var metadata Metadata
			must(t, local.ReadJSON(filepath.Join(out, "metadata.json"), &metadata))
			if metadata.Media.Restore != "reuse-existing" || metadata.Consistency != "globally-quiesced" {
				t.Fatal(metadata.Media, metadata.Consistency)
			}
			row, err := exec.Command(e.Host.SQLite, "-readonly", filepath.Join(bundle, "payload/telegram/data/telegram.db"), "SELECT body FROM message WHERE id=1;").Output()
			must(t, err)
			if strings.TrimSpace(string(row)) != "state" {
				t.Fatal("Telegram state lost")
			}
			if scenario == "cached-media" {
				b, err := os.ReadFile(filepath.Join(data, "media/cache/file"))
				must(t, err)
				if string(b) != "media must remain outside the database backup" {
					t.Fatal("source media modified")
				}
			}
		})
	}
}
