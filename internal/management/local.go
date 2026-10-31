package management

import (
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"os"
	"path/filepath"
	"time"
)

func Directory(root string) string { return filepath.Join(root, "deploy/control/state/tasks") }
func Path(root, id string) string  { return filepath.Join(Directory(root), id+".json") }
func stamp() string                { return time.Now().UTC().Format(time.RFC3339Nano) }
func Enqueue(root string, r Request) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if !IsTask(r.Action) {
		return errors.New("not a queued task")
	}
	unlock, err := local.Lock(filepath.Join(Directory(root), "queue.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	path := Path(root, r.ID)
	var old Task
	if err := local.ReadJSON(path, &old); err == nil {
		if string(release.JSON(old.Request)) != string(release.JSON(r)) {
			return errors.New("task ID already used for different content")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(Directory(root))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var t Task
		if err := local.ReadJSON(filepath.Join(Directory(root), entry.Name()), &t); err != nil {
			return err
		}
		if !Terminal(t.State) {
			return errors.New("another maintenance task is unfinished; inspect or cancel it first")
		}
	}
	return local.JSON(path, Task{Schema: 1, Request: r, State: "QUEUED", Stage: "queued", Updated: stamp()})
}
func Read(root, id string) (Task, error) {
	var t Task
	if !local.SafeID(id) {
		return t, errors.New("invalid task ID")
	}
	err := local.ReadJSON(Path(root, id), &t)
	if err == nil && (t.Schema != 1 || t.Request.ID != id || t.Request.Validate() != nil) {
		err = errors.New("invalid persisted task")
	}
	return t, err
}
func Cancel(root, id string) error {
	t, err := Read(root, id)
	if err != nil {
		return err
	}
	if Terminal(t.State) {
		return nil
	}
	return local.JSON(filepath.Join(Directory(root), id+".cancel"), map[string]any{"id": id, "at": stamp()})
}
func Retry(root, id string) error {
	queue, err := local.Lock(filepath.Join(Directory(root), "queue.lock"))
	if err != nil {
		return err
	}
	defer queue()
	unlock, err := local.Lock(filepath.Join(Directory(root), "worker.lock"))
	if err != nil {
		return err
	}
	defer unlock()

	entries, err := os.ReadDir(Directory(root))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var other Task
		if err = local.ReadJSON(filepath.Join(Directory(root), entry.Name()), &other); err != nil {
			return err
		}
		if other.Request.ID != id && !Terminal(other.State) {
			return errors.New("another maintenance task is unfinished")
		}
	}
	t, err := Read(root, id)
	if err != nil {
		return err
	}
	if t.State != "FAILED" && t.State != "INTERRUPTED" && t.State != "CANCELLED" {
		return errors.New("only failed or interrupted tasks can retry")
	}
	if err = os.Remove(filepath.Join(Directory(root), id+".cancel")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	t.State, t.Stage, t.Error, t.Updated = "QUEUED", "retry-requested", "", stamp()
	return local.JSON(Path(root, id), t)
}
