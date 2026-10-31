package main

import (
	"context"
	"testing"
)

func TestInvalidCommandsCannotInvokeWork(t *testing.T) {
	for _, args := range [][]string{{"prepare", "--full"}, {"backup", "--apply"}, {"restore", "--apply"}, {"unknown"}, {"version", "--root", "/tmp/fixture"}, {"verify", "--from", "."}} {
		if e := run(context.Background(), args); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
