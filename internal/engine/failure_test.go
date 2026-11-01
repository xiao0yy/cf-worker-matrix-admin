package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
)

func TestCancellationDoesNotEraseRootFailure(t *testing.T) {
	e := testEngine(t)
	base := t.TempDir()
	e.recordError(base, "move-test", "stage-backup", errors.New("unsupported configuration path: database/archive/001.log"))
	e.recordError(base, "move-test", "final-backup", context.Canceled)
	var r failureRecord
	must(t, local.ReadJSON(filepath.Join(base, "error.json"), &r))
	if r.Kind != "interrupted" || r.First == nil || r.First.Step != "stage-backup" || !strings.Contains(r.First.Error, "001.log") {
		t.Fatal(r)
	}
	e.recordError(base, "move-test", "complete", nil)
	if _, err := os.Stat(filepath.Join(base, "error.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful retry retained active failure", err)
	}
}

func TestCommandFailurePublishesSafeCauseAndExactPrivateLog(t *testing.T) {
	dir := t.TempDir()
	runner := ExecRunner{LogDir: dir}
	_, err := runner.Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "echo 'no space left on device; token=PRIVATE_FIXTURE' >&2; exit 23"}})
	var detail *commandFailure
	if !errors.As(err, &detail) || !strings.Contains(err.Error(), "no space left on device") || strings.Contains(err.Error(), "PRIVATE_FIXTURE") || detail.Log == "" {
		t.Fatal(err)
	}
	b, er := os.ReadFile(filepath.Join(dir, detail.Log))
	must(t, er)
	if !strings.Contains(string(b), "PRIVATE_FIXTURE") {
		t.Fatal("private diagnostic lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runner.Run(ctx, Command{Name: "sh", Args: []string{"-c", "exit 0"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation cause lost", err)
	}
}
