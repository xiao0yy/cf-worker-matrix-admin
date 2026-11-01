package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
)

type commandFailure struct {
	Program, Reason, Log string
	Cause                error
}

func (e *commandFailure) Error() string {
	message := e.Program + ": " + e.Reason
	if e.Log != "" {
		message += " (private log: " + e.Log + ")"
	}
	return message
}
func (e *commandFailure) Unwrap() error { return e.Cause }

// Raw command output can contain access tokens, DB contents and signed URLs.
// Publish a classified cause, exit status and exact private log reference; never
// put arbitrary stderr into R2. Locally recorded logs retain the full detail.
func commandReason(err error, stderr []byte) string {
	if errors.Is(err, context.Canceled) {
		return "operation interrupted; data retained"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "command timed out; data retained"
	}
	for _, reason := range []string{"no space left on device", "permission denied", "cannot connect to the docker daemon", "manifest unknown", "no matching manifest", "connection refused", "no such host", "i/o timeout", "out of memory"} {
		if strings.Contains(strings.ToLower(string(stderr)), reason) {
			return reason
		}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("command exited with status %d", exit.ExitCode())
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "executable not found"
	}
	return "command could not complete"
}

type failureDetail struct {
	Step  string `json:"step"`
	At    string `json:"at"`
	Error string `json:"error"`
	Kind  string `json:"kind"`
	Log   string `json:"log,omitempty"`
}
type failureRecord struct {
	Operation string `json:"operation_id"`
	failureDetail
	First *failureDetail `json:"first_failure,omitempty"`
}

func (e *Engine) recordError(path, op, step string, err error) {
	file := filepath.Join(path, "error.json")
	if err == nil {
		_ = os.Remove(file)
		return
	}
	d := failureDetail{Step: step, At: e.stamp(), Error: err.Error(), Kind: "failed"}
	if errors.Is(err, context.Canceled) {
		d.Kind = "interrupted"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		d.Kind = "timeout"
	}
	var cmd *commandFailure
	if errors.As(err, &cmd) {
		d.Log = cmd.Log
	}
	var old failureRecord
	var first *failureDetail
	if local.ReadJSON(file, &old) == nil && old.Operation == op {
		first = old.First
		if first == nil && old.Error != "" && old.Kind != "interrupted" && !strings.Contains(old.Error, "context canceled") {
			first = &old.failureDetail
		}
	}
	if first == nil && d.Kind != "interrupted" {
		first = &d
	}
	_ = local.JSON(file, failureRecord{Operation: op, failureDetail: d, First: first})
}
