package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func copyOwner(source, dest string) error {
	st, err := os.Stat(source)
	if err != nil {
		return err
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		return os.Chown(dest, int(s.Uid), int(s.Gid))
	}
	return nil
}
func (e *Engine) probe(ctx context.Context, bundle string, m Metadata, work string) error {
	if err := os.MkdirAll(work, 0700); err != nil {
		return err
	}
	matrix := filepath.Join(work, "database")
	if err := local.CopyTree(filepath.Join(bundle, "payload/database"), matrix); err != nil {
		return err
	}
	name := "matrix-probe-" + local.RandomID()
	defer e.docker(context.Background(), "rm", "-f", name)
	args := []string{"run", "--rm", "--name", name, "--network", "none", "--memory", "192m", "--memory-swap", "384m", "--pids-limit", "128", "--cpus", "0.5", "--read-only", "--tmpfs", "/tmp:size=16m", "-v", matrix + ":/var/lib/tuwunel", "-e", "TUWUNEL_SERVER_NAME=" + m.Server, "-e", "TUWUNEL_DATABASE_PATH=/var/lib/tuwunel", "-e", "TUWUNEL_ADDRESS=0.0.0.0", "-e", "TUWUNEL_DB_CACHE_CAPACITY_MB=16", "-e", "TUWUNEL_DB_WRITE_BUFFER_CAPACITY_MB=8", "-e", "TUWUNEL_CACHE_CAPACITY_MODIFIER=0.1", "-e", "TUWUNEL_DB_POOL_MAX_WORKERS=2", "-e", "TUWUNEL_ALLOW_FEDERATION=false", m.Images["tuwunel"].Ref, "--maintenance", "--execute", "server shutdown"}
	if _, err := e.Runner.Run(ctx, Command{Name: e.Host.Docker, Args: args, Timeout: 180 * time.Second}); err != nil {
		return err
	}
	tg := filepath.Join(work, "telegram")
	if err := local.CopyTree(filepath.Join(bundle, "payload/telegram/data"), tg); err != nil {
		return err
	}
	if err := e.verifyTelegram(ctx, filepath.Join(tg, "telegram.db"), m.Telegram); err != nil {
		return err
	}
	tgName := "telegram-probe-" + local.RandomID()
	defer e.docker(context.Background(), "rm", "-f", tgName)
	if _, err := e.docker(ctx, "run", "-d", "--name", tgName, "--network", "none", "--read-only", "--memory", "128m", "--memory-swap", "192m", "--pids-limit", "96", "--cpus", "0.5", "--tmpfs", "/tmp:size=16m", "-v", tg+":/data", m.Images["telegram"].Ref); err != nil {
		return err
	}
	if err := e.wait(ctx, 45*time.Second, func() error {
		b, err := e.docker(ctx, "logs", tgName)
		if err != nil {
			return err
		}
		found := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			var v struct {
				Message string `json:"message"`
				Section string `json:"db_section"`
			}
			if json.Unmarshal([]byte(line), &v) != nil {
				continue
			}
			if v.Message == "Starting Matrix connector" || v.Message == "Starting HTTP listener" {
				found[v.Message] = true
			}
			if v.Message == "Database is up to date" && v.Section == "matrix_state" {
				found["state"] = true
			}
		}
		if len(found) != 3 {
			return errors.New("waiting for bridge database startup")
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := e.docker(ctx, "stop", "--time", "10", tgName); err != nil {
		return err
	}
	c, err := e.inspect(ctx, tgName)
	if err != nil {
		return err
	}
	if c.State.Running || (c.State.ExitCode != 0 && c.State.ExitCode != 137 && c.State.ExitCode != 143) {
		return errors.New("bridge probe failed")
	}
	if err = e.verifyTelegram(ctx, filepath.Join(tg, "telegram.db"), m.Telegram); err != nil {
		return err
	}
	return local.JSON(filepath.Join(work, "result.json"), map[string]any{"database_startup_verified": true, "telegram_database_startup_verified": true, "production_started": false, "network": "none", "consistency": m.Consistency, "telegram_authentication_tested": false})
}
