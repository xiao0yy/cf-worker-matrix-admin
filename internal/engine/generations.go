package engine

import (
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"os"
	"path/filepath"
)

// The registry is the ownership inventory, not a directory-name convention.
// A record is created before writing data and retained after explicit cleanup.
// Epoch is set before binding, so an interrupted admission is conservative.
type generation struct {
	Schema   int    `json:"schema"`
	Cluster  string `json:"cluster_id"`
	Instance string `json:"instance_id"`
	Root     string `json:"deployment_root"`
	Work     string `json:"work_dir,omitempty"`
	Snapshot string `json:"snapshot_id,omitempty"`
	Hash     string `json:"point_sha256,omitempty"`
	Epoch    uint64 `json:"bound_epoch"`
}

func (e *Engine) generationPath(root string) string {
	return filepath.Join(e.Host.Root, "agent/generations", local.Digest([]byte(root))+".json")
}
func (e *Engine) validateGeneration(g generation) error {
	if g.Schema != 1 || g.Cluster != e.Cluster || !local.SafeID(g.Instance) {
		return errors.New("generation registry identity differs")
	}
	if g.Work == "" {
		if g.Root != e.Host.Root || g.Epoch == 0 {
			return errors.New("invalid foundation generation")
		}
	} else if filepath.Dir(g.Work) != filepath.Join(e.Host.Root, "generations") || g.Root != filepath.Join(g.Work, "rebuilt") || !local.SHA.MatchString(g.Snapshot) || !local.SHA.MatchString(g.Hash) {
		return errors.New("generation registry path or point differs")
	}
	return local.SafePath(g.Root)
}
func (e *Engine) registerGeneration(g generation) error {
	if err := e.validateGeneration(g); err != nil {
		return err
	}
	path := e.generationPath(g.Root)
	var old generation
	err := local.ReadJSON(path, &old)
	if err == nil {
		want := g
		want.Epoch = old.Epoch
		if old != want || (old.Epoch != 0 && old.Epoch != g.Epoch && g.Epoch != 0) {
			return errors.New("generation registry is immutable")
		}
		if g.Epoch == 0 || old.Epoch == g.Epoch {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return local.JSON(path, g)
}

func (e *Engine) bindGeneration(r Restored, instance string, epoch uint64) error {
	return e.registerGeneration(generation{1, e.Cluster, instance, r.Root, r.Work, r.Snapshot, r.Hash, epoch})
}

// A returning old node can still have an upload finishing after fencing.
// Cleanup must not unlink a producer's inputs, even though it cannot publish
// under the new owner's epoch. The normal backup path uses these same locks.
func (e *Engine) lockGenerationProducers() (func(), error) {
	var locks []func()
	unlock := func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i]()
		}
	}
	entries, err := os.ReadDir(filepath.Join(e.Host.Root, "agent/generations"))
	if errors.Is(err, os.ErrNotExist) {
		return unlock, nil
	}
	if err != nil {
		return nil, err
	}
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		var g generation
		if err = local.ReadJSON(filepath.Join(e.Host.Root, "agent/generations", ent.Name()), &g); err == nil {
			err = e.validateGeneration(g)
		}
		if err != nil {
			unlock()
			return nil, err
		}
		if _, err = os.Stat(g.Root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			unlock()
			return nil, err
		}
		lock, er := local.Lock(filepath.Join(g.Root, "backup-state/tuwunel-restic/operation.lock"))
		if er != nil {
			unlock()
			return nil, er
		}
		locks = append(locks, lock)
	}
	return unlock, nil
}

// Existing installations are adopted only from the controller's pinned active
// deployment, never by scanning unclassified directories or copying databases.
func (e *Engine) registerDeployment(s Status) error {
	if s.Epoch == 0 {
		return nil
	}
	root := s.Deployment
	if root == "" {
		root = e.Host.Root
	}
	var known generation
	if err := local.ReadJSON(e.generationPath(root), &known); err == nil {
		if err = e.validateGeneration(known); err != nil {
			return err
		}
		if known.Instance != s.Instance || known.Root != root || known.Epoch != s.Epoch {
			return errors.New("bound generation registry differs")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if root == e.Host.Root {
		return e.registerGeneration(generation{Schema: 1, Cluster: e.Cluster, Instance: s.Instance, Root: root, Epoch: s.Epoch})
	}
	var r Restored
	if err := local.ReadJSON(filepath.Join(filepath.Dir(root), "result.json"), &r); err != nil {
		return err
	}
	if !r.Ready || r.Root != root || r.Work != filepath.Dir(root) {
		return errors.New("deployment has no matching generation receipt")
	}
	return e.bindGeneration(r, s.Instance, s.Epoch)
}
