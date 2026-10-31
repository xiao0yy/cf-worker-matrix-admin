package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/engine"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/mailbox"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func output(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Println(`matrix-admin <command> [options]
  version                 Show build identity and supported data protocols
  doctor                  Inspect local dependencies and instance state
  prepare [--preflight]   Prepare a standby from the single R2 configuration
  status                  Read the local controller's status
  backup [--full]         Back up the authorized active databases
  restore [--point ID --sha256 HASH]  Isolated restore; never activates services
  fence --instance ID     Block ingress and stop local writers
  resume --instance ID --epoch N     Explicitly resume the current owner
  final --operation ID --instance ID --epoch N
  source-resume --operation ID --instance ID --epoch N
  handover                Advance an explicitly requested planned or forced handover
  preflight [--point ID --sha256 HASH]  Verify standby readiness
  handover-pause / handover-continue --instance ID
  upgrade --from DIR [--apply]       Verify or install a complete native runtime
  download --version X.Y.Z --output DIR   Fetch a verified immutable runtime
  verify --from DIR       Verify a runtime's complete file inventory
  config-example          Print the user's only bootstrap input template
Host commands accept --root /opt/matrix. System prerequisites are prepared by the Agent.
The control, mailbox and tasks subcommands are systemd entrypoints of this same program.`)
		return nil
	}
	cmd := args[0]
	if cmd == "version" {
		if len(args) != 1 {
			return errors.New("unexpected version arguments")
		}
		return output(release.Info())
	}
	if cmd == "control" {
		return control.Run(args[1:])
	}
	if cmd == "mailbox" {
		return mailbox.Run(args[1:])
	}
	if cmd == "config-example" {
		fmt.Print("S3_ENDPOINT='https://ACCOUNT.r2.cloudflarestorage.com'\nS3_REGION='auto'\nS3_BUCKET='YOUR_BUCKET'\nCLUSTER_ID='YOUR_CLUSTER'\nAWS_ACCESS_KEY_ID='YOUR_ACCESS_KEY'\nAWS_SECRET_ACCESS_KEY='YOUR_SECRET_KEY'\n")
		return nil
	}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	root := f.String("root", "/opt/matrix", "instance foundation directory")
	from := f.String("from", "", "verified release directory")
	apply := f.Bool("apply", false, "apply a validated code update")
	preflight := f.Bool("preflight", false, "check host without preparing services")
	full := f.Bool("full", false, "verify full restic restore")
	point := f.String("point", "", "full snapshot ID")
	hash := f.String("sha256", "", "expected point or runtime manifest SHA256")
	op := f.String("operation", "", "explicit idempotent operation ID")
	id := f.String("instance", "", "expected instance ID")
	epoch := f.Uint64("epoch", 0, "expected data epoch")
	version := f.String("version", "", "fixed release version")
	dest := f.String("output", "", "new download directory")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	allowed := map[string]string{
		"download": "version output sha256", "verify": "from", "doctor": "root", "status": "root",
		"tasks": "root", "preflight": "root point sha256", "handover-pause": "root instance", "handover-continue": "root instance",
		"prepare": "root preflight", "backup": "root full", "restore": "root point sha256",
		"fence": "root instance", "resume": "root instance epoch", "final": "root operation instance epoch",
		"source-resume": "root operation instance epoch", "handover": "root", "upgrade": "root from apply",
	}
	permitted, known := allowed[cmd]
	if !known {
		return errors.New("unknown command; use matrix-admin help")
	}
	var flagErr error
	f.Visit(func(v *flag.Flag) {
		if !strings.Contains(" "+permitted+" ", " "+v.Name+" ") {
			flagErr = fmt.Errorf("--%s does not apply to %s", v.Name, cmd)
		}
	})
	if flagErr != nil {
		return flagErr
	}
	switch cmd {
	case "download":
		return release.Fetch(ctx, *version, *hash, *dest)
	case "verify":
		v, err := release.Verify(*from)
		if err != nil {
			return err
		}
		return output(v)
	}
	e, err := engine.New(*root)
	if err != nil {
		return err
	}
	cfg := filepath.Join(*root, "deploy/control/local.json")
	switch cmd {
	case "status":
		return control.Run([]string{"status", "--config", cfg})
	case "doctor":
		v, err := e.Doctor(ctx)
		if err != nil {
			return err
		}
		return output(v)
	}
	if os.Geteuid() != 0 {
		return errors.New("host mutation requires root")
	}
	switch cmd {
	case "tasks":
		return e.Tasks(ctx)
	case "preflight":
		v, err := e.Preflight(ctx, *point, *hash)
		if err != nil {
			return err
		}
		return output(v)
	case "handover-pause", "handover-continue":
		return e.PauseHandover(ctx, *id, cmd == "handover-pause")
	case "prepare":
		return e.Prepare(ctx, *preflight)
	case "backup":
		return e.Backup(ctx, *full)
	case "restore":
		v, err := e.Restore(ctx, *point, *hash)
		if err != nil {
			return err
		}
		return output(v)
	case "handover":
		return e.Handover(ctx)
	case "final":
		return e.Final(ctx, *op, *id, *epoch)
	case "source-resume":
		return e.SourceResume(ctx, *op, *id, *epoch)
	case "upgrade":
		v, err := e.Upgrade(ctx, *from, *apply)
		if err != nil {
			return err
		}
		return output(v)
	case "fence":
		return control.Run([]string{"fence-local", "--config", cfg, "--expected-instance", *id})
	case "resume":
		return e.Resume(ctx, *id, *epoch)
	}
	return errors.New("unknown command; use matrix-admin help")
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "matrix-admin:", err)
		os.Exit(1)
	}
}
