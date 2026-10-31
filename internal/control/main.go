package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func Run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: matrix-admin control <adopt-existing|daemon|status|fence-local|resume-local|emergency-stop> --config local.json")
	}
	cmd := args[0]
	if cmd == "capabilities" {
		fmt.Println(`{"schema":1,"planned_handover":true,"force_takeover":false,"recovery_protocol":2}`)
		return nil
	}
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	path := flags.String("config", "/opt/matrix/deploy/control/local.json", "local host configuration")
	expected := flags.String("expected-instance", "", "required expected instance identity")
	epoch := flags.Uint64("expected-epoch", 0, "required expected data epoch")
	operation := flags.String("operation-id", "", "idempotent final backup operation ID")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	if cmd == "status" || cmd == "fence-local" || cmd == "resume-local" || strings.HasPrefix(cmd, "final-") || strings.HasPrefix(cmd, "handover-") {
		client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(cfg.RuntimeDir, "admin.sock"))
		}}}
		method, endpoint := "GET", "status"
		var body []byte
		if cmd != "status" {
			if *expected != cfg.Instance {
				return errors.New("supply the exact --expected-instance")
			}
			method = "POST"
			endpoint = "fence"
			if cmd == "resume-local" {
				endpoint = "resume"
				if *epoch == 0 {
					return errors.New("supply --expected-epoch")
				}
			}
			if strings.HasPrefix(cmd, "final-") || strings.HasPrefix(cmd, "handover-") {
				if *epoch == 0 || !safeID(*operation) {
					return errors.New("final actions require --operation-id and --expected-epoch")
				}
				endpoint = cmd
			}
			body, _ = json.Marshal(map[string]any{"expected_instance_id": *expected, "expected_epoch": *epoch, "operation_id": *operation})
		}
		req, _ := http.NewRequest(method, "http://controller/"+endpoint, bytes.NewReader(body))
		resp, err := client.Do(req)
		if err != nil {
			return errors.New("local controller unavailable; no action taken")
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		if resp.StatusCode >= 300 {
			return fmt.Errorf("controller HTTP %d: %s", resp.StatusCode, string(b))
		}
		if len(b) > 0 {
			fmt.Print(string(b))
		} else {
			fmt.Println("Accepted; query status to check completion")
		}
		return nil
	}
	docker := newDocker(cfg.DockerSocket)
	if cmd == "emergency-stop" {
		// Independent of R2 availability and local journal parsing. This is also the
		// systemd ExecStopPost safety path after a crash/watchdog termination.
		c := &Controller{cfg: cfg, docker: docker}
		return c.stopServices()
	}
	store, err := newStore(cfg.S3Config, cfg.Cluster)
	if err != nil {
		return err
	}
	if cmd == "prepare-standby" {
		if *expected != cfg.Instance {
			return errors.New("supply the exact --expected-instance")
		}
		if _, err = os.Stat(filepath.Join(cfg.StateDir, "journal.json")); !os.IsNotExist(err) {
			return errors.New("existing local journal; refusing standby initialization")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, s := range cfg.Services {
			if _, err = docker.Inspect(ctx, s); !errors.Is(err, errContainerMissing) {
				return errors.New("standby preparation requires absent application containers")
			}
		}
		state, _, err := store.Read(ctx)
		if err != nil {
			return err
		}
		if state.Owner == cfg.Instance {
			return errors.New("existing owner cannot be prepared as an empty standby")
		}
		j := Journal{Schema: 1, Cluster: cfg.Cluster, Instance: cfg.Instance, HighEpoch: state.Epoch, HighRevision: state.Revision, Blocked: true, Reason: "standby: no restored data or run authorization"}
		return atomicJSON(filepath.Join(cfg.StateDir, "journal.json"), j)
	}
	if cmd == "adopt-existing" {
		if *expected != cfg.Instance {
			return errors.New("supply the exact --expected-instance")
		}
		if _, err = os.Stat(filepath.Join(cfg.StateDir, "journal.json")); !os.IsNotExist(err) {
			return errors.New("local journal already exists or cannot be inspected; initialization refused")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, s := range cfg.Services {
			running, e := docker.Inspect(ctx, s)
			if e != nil {
				return e
			}
			if !running {
				return errors.New("adopt-existing requires both verified existing services running")
			}
			if e = docker.Healthy(ctx, s); e != nil {
				return e
			}
		}
		state, _, err := store.Read(ctx)
		if errors.Is(err, errMissing) {
			state = State{Schema: 1, ClusterID: cfg.Cluster, Revision: 1, Epoch: 1, Owner: cfg.Instance, Phase: "ONLINE", Operation: json.RawMessage("null")}
			if err = store.Write(ctx, state, "*"); err != nil {
				return err
			}
			state, _, err = store.Read(ctx)
		}
		if err != nil {
			return err
		}
		if state.Owner != cfg.Instance || state.Phase != "ONLINE" || state.Epoch != 1 {
			return errors.New("existing control state belongs elsewhere; refusing adoption")
		}
		journal := Journal{Schema: 1, Cluster: cfg.Cluster, Instance: cfg.Instance, BoundEpoch: state.Epoch, HighRevision: state.Revision, HighEpoch: state.Epoch}
		if err = atomicJSON(filepath.Join(cfg.StateDir, "journal.json"), journal); err != nil {
			return err
		}
		fmt.Printf("Adopted existing instance %s at epoch %d\n", cfg.Instance, state.Epoch)
		return nil
	}
	if cmd != "daemon" {
		return errors.New("unknown command")
	}
	journal, err := readJournal(cfg)
	if err != nil {
		return err
	}
	c := &Controller{cfg: cfg, store: store, docker: docker, journal: journal, wake: make(chan struct{}, 1), now: time.Now}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	return daemon(ctx, c)
}
