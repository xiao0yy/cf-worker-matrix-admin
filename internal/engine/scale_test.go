package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
)

func TestHandoverErrorPausesInsteadOfRepeatingSideEffects(t *testing.T) {
	e := testEngine(t)
	op := control.FinalOperation{ID: "move-one", Mode: "planned", Source: "node-a", Target: "node-b", Epoch: 1}
	s := Status{Instance: "node-a", Cluster: e.Cluster, Epoch: 1, Control: &control.State{Owner: "node-a", Phase: "REQUESTED", Epoch: 1, Operation: release.JSON(op)}}
	actions := 0
	fakeController(t, e, &s, func(string) error { actions++; return errors.New("should not mutate") })
	e.Store = &memoryObjects{data: map[string][]byte{}}
	e.Runner = runFunc(func(context.Context, Command) ([]byte, error) {
		t.Fatal("ran host command with unsupported target")
		return nil, nil
	})
	if err := e.Handover(context.Background()); err == nil {
		t.Fatal("missing capability accepted")
	}
	pause := filepath.Join(e.Host.Root, "agent/handover-paused.json")
	var record struct {
		Operation string `json:"operation_id"`
		At        string `json:"at"`
		Reason    string `json:"reason"`
		Step      string `json:"step"`
	}
	must(t, local.ReadJSON(pause, &record))
	if record.Operation != op.ID || record.Step != "check-target-reader" {
		t.Fatal(record)
	}
	must(t, e.Handover(context.Background()))
	if actions != 0 {
		t.Fatal("paused worker mutated source")
	}
}
func TestFinalReaderRequiresFreshMatchingIdentity(t *testing.T) {
	e := testEngine(t)
	objects := &memoryObjects{data: map[string][]byte{}}
	e.Store = objects
	key := "cluster/" + e.Cluster + "/agents/node-b.json"
	for _, field := range []string{"valid", "old-schema", "stale", "future", "other-cluster", "other-instance"} {
		t.Run(field, func(t *testing.T) {
			r := map[string]any{"instance_id": "node-b", "cluster_id": e.Cluster, "handover_receipt_schema": 2, "reported_at": e.stamp()}
			switch field {
			case "old-schema":
				delete(r, "handover_receipt_schema")
			case "stale":
				r["reported_at"] = e.Now().Add(-2 * time.Minute).Format(time.RFC3339)
			case "future":
				r["reported_at"] = e.Now().Add(time.Minute).Format(time.RFC3339)
			case "other-cluster":
				r["cluster_id"] = "other"
			case "other-instance":
				r["instance_id"] = "other"
			}
			objects.data[key] = release.JSON(r)
			err := e.requireFinalReader(context.Background(), "node-b")
			if (err == nil) != (field == "valid") {
				t.Fatal(err)
			}
		})
	}
}
func TestTerabyteSnapshotRequiresOneDatabasePlusMutableProbeSpace(t *testing.T) {
	e := testEngine(t)
	const table int64 = 1 << 40
	const mutable int64 = 32 << 20
	e.Runner = runFunc(func(_ context.Context, c Command) ([]byte, error) {
		joined := " " + strings.Join(c.Args, " ") + " "
		if strings.Contains(joined, " stats ") {
			return json.Marshal(map[string]int64{"total_size": table + mutable})
		}
		if strings.Contains(joined, " ls ") {
			return []byte(fmt.Sprintf("{\"struct_type\":\"node\",\"type\":\"file\",\"path\":\"/bundle/payload/database/000001.sst\",\"size\":%d}\n{\"struct_type\":\"node\",\"type\":\"file\",\"path\":\"/bundle/payload/telegram/data/telegram.db\",\"size\":%d}\n", table, mutable)), nil
		}
		return nil, errors.New("unexpected command")
	})
	need, _ := e.snapshotSpace(context.Background(), Repository{}, strings.Repeat("a", 64))
	if need != uint64(table+2*mutable)+(1<<30) {
		t.Fatal("amplified space estimate", need)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	must(t, os.Mkdir(cache, 0700))
	f, err := os.Create(filepath.Join(cache, "sparse"))
	must(t, err)
	must(t, f.Truncate(table))
	must(t, f.Close())
	allocated, err := allocatedSize(cache)
	must(t, err)
	if allocated >= uint64(table) {
		t.Fatal("counted sparse logical length as reusable downloaded bytes")
	}
}

// Docker startup is an isolated fixture here; restic transfer, SQLite checks,
// workspace replay and file moves use the real implementation and local disk.
func testRestoreResume(t *testing.T, e *Engine, s *Status, repoDir, bin string, receipt control.FinalReceipt) {
	t.Helper()
	version := release.Version
	release.Version = "0.2.0"
	defer func() { release.Version = version }()
	real := ExecRunner{filepath.Join(e.Host.Root, "test-restore-logs")}
	transfers := 0
	e.Runner = runFunc(func(ctx context.Context, c Command) ([]byte, error) {
		if filepath.Base(c.Name) == "restic" {
			c.Name = bin
			c.Env = append(c.Env, "RESTIC_REPOSITORY="+repoDir)
			b, err := real.Run(ctx, c)
			for i, a := range c.Args {
				if a == "--target" {
					transfers++
					if transfers == 1 && err == nil {
						// Model a transfer interrupted after creating files, with one incomplete
						// file. The next attempt must repair it in the same download directory.
						must(t, os.Truncate(filepath.Join(c.Args[i+1], "bundle/payload/database/CURRENT"), 1))
						return nil, errors.New("injected interrupted restore")
					}
				}
			}
			return b, err
		}
		if c.Name != e.Host.Docker {
			return real.Run(ctx, c)
		}
		switch c.Args[0] {
		case "image":
			return []byte(`[{"Id":"sha256:` + strings.Repeat("b", 64) + `","OS":"linux","Architecture":"amd64"}]`), nil
		case "run":
			if !strings.Contains(strings.Join(c.Args, " "), "--network none") {
				t.Fatal("probe had network")
			}
			for i, a := range c.Args {
				if a == "-v" && strings.HasSuffix(c.Args[i+1], ":/var/lib/tuwunel") {
					probe := strings.TrimSuffix(c.Args[i+1], ":/var/lib/tuwunel")
					must(t, os.WriteFile(filepath.Join(probe, "CURRENT"), []byte("probe writes mutable metadata"), 0600))
				}
			}
			return nil, nil
		case "logs":
			return []byte("{\"message\":\"Starting Matrix connector\"}\n{\"message\":\"Starting HTTP listener\"}\n{\"message\":\"Database is up to date\",\"db_section\":\"matrix_state\"}\n"), nil
		case "stop", "rm":
			return nil, nil
		case "inspect":
			return []byte(`[ {"State":{"Running":false,"ExitCode":0}} ]`), nil
		case "compose":
			if c.Args[len(c.Args)-1] == "--quiet" {
				return nil, nil
			}
			name, target := "tuwunel", "/var/lib/tuwunel"
			if filepath.Base(c.Args[2]) == "telegram" {
				name, target = "telegram", "/data"
			}
			return json.Marshal(map[string]any{"services": map[string]any{name: map[string]any{"volumes": []any{map[string]string{"type": "bind", "source": "/old/data", "target": target}}, "environment": map[string]string{}, "networks": map[string]any{"default": map[string]any{}}}}, "networks": map[string]any{"default": map[string]any{}}})
		}
		return nil, fmt.Errorf("unexpected Docker command: %v", c.Args)
	})
	ctx := context.Background()
	work := filepath.Join(e.Host.Root, "generations/handover-resume-test")
	if _, err := e.restoreAt(ctx, receipt.Snapshot, receipt.PointHash, work); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatal("missing interrupted transfer", err)
	}
	cached := filepath.Join(work, "download/bundle/payload/database/MANIFEST-1")
	before, err := os.Stat(cached)
	must(t, err)
	result, err := e.restoreAt(ctx, receipt.Snapshot, receipt.PointHash, work)
	must(t, err)
	after, err := os.Stat(filepath.Join(result.Root, "deploy/tuwunel/data/MANIFEST-1"))
	must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("unchanged downloaded data was copied or replaced")
	}
	current, err := os.ReadFile(filepath.Join(result.Root, "deploy/tuwunel/data/CURRENT"))
	must(t, err)
	if string(current) != "MANIFEST-1\n" {
		t.Fatal("probe changed restored original", string(current))
	}
	for _, name := range []string{"download", "probe"} {
		if _, err = os.Stat(filepath.Join(work, name)); !os.IsNotExist(err) {
			t.Fatal("temporary copies survived completion", name)
		}
	}
	again, err := e.restoreAt(ctx, receipt.Snapshot, receipt.PointHash, work)
	must(t, err)
	if again != result || transfers != 2 {
		t.Fatal("completed restore replayed", transfers)
	}
	// Simulate death after removing temporary links but before publishing either
	// ready receipt. The prepared generation must survive without any transfer.
	must(t, os.Remove(filepath.Join(work, "result.json")))
	must(t, os.Remove(filepath.Join(result.Root, "recovery-state/result.json")))
	again, err = e.restoreAt(ctx, receipt.Snapshot, receipt.PointHash, work)
	must(t, err)
	afterPublish, err := os.Stat(filepath.Join(result.Root, "deploy/tuwunel/data/MANIFEST-1"))
	must(t, err)
	if again != result || transfers != 2 || !os.SameFile(after, afterPublish) {
		t.Fatal("prepared generation was rebuilt or downloaded")
	}
	s.Handover = &control.TargetOperation{Root: result.Root}
	if _, err = e.restoreAt(ctx, receipt.Snapshot, receipt.PointHash, work); err == nil {
		t.Fatal("replayed bound generation")
	}
	preserved, err := os.Stat(filepath.Join(result.Root, "deploy/tuwunel/data/MANIFEST-1"))
	must(t, err)
	if !os.SameFile(after, preserved) || transfers != 2 {
		t.Fatal("bound generation touched")
	}
}
