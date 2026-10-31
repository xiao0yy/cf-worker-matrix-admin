package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

var tables = []string{"user_login", "crypto_account", "crypto_olm_session", "crypto_megolm_inbound_session", "crypto_megolm_outbound_session", "portal", "message"}

func (e *Engine) counts(ctx context.Context, db string) (map[string]int64, error) {
	var parts []string
	for _, t := range tables {
		parts = append(parts, "SELECT '"+t+"' AS name,count(*) AS count FROM "+t)
	}
	b, err := e.run(ctx, e.Host.SQLite, "-readonly", "-batch", "-bail", "-json", db, strings.Join(parts, " UNION ALL ")+";")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name  string
		Count int64
	}
	if err = json.Unmarshal(b, &rows); err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for _, r := range rows {
		m[r.Name] = r.Count
	}
	if len(m) != len(tables) {
		return nil, errors.New("missing Telegram state tables")
	}
	return m, nil
}
func (e *Engine) verifyTelegram(ctx context.Context, db string, m Telegram) error {
	if st, err := os.Lstat(db); err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return errors.New("missing regular Telegram database")
	}
	for _, q := range []string{"PRAGMA integrity_check;", "PRAGMA foreign_key_check;"} {
		b, err := e.run(ctx, e.Host.SQLite, "-readonly", "-batch", "-bail", db, q)
		if err != nil {
			return err
		}
		want := ""
		if strings.Contains(q, "integrity") {
			want = "ok"
		}
		if strings.TrimSpace(string(b)) != want {
			return errors.New("Telegram SQLite integrity check failed")
		}
	}
	counts, err := e.counts(ctx, db)
	if err != nil {
		return err
	}
	if m.Schema != 1 || (m.Method != "sqlite-online-backup" && m.Method != "sqlite-cold-backup") || !reflect.DeepEqual(counts, m.Counts) {
		return errors.New("Telegram account/crypto state counts differ")
	}
	return nil
}
func (e *Engine) telegramSnapshot(ctx context.Context, project, dest string, cold bool) (Telegram, error) {
	var m Telegram
	source := filepath.Join(project, "data")
	if strings.HasPrefix(dest, source+"/") || strings.HasPrefix(source, dest+"/") {
		return m, errors.New("snapshot overlaps source")
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return m, err
	}
	for _, entry := range entries {
		st, err := os.Lstat(filepath.Join(source, entry.Name()))
		if err != nil {
			return m, err
		}
		switch entry.Name() {
		case "config.yaml", "registration.yaml", "telegram.db", "telegram.db-wal", "telegram.db-shm":
			if !st.Mode().IsRegular() {
				return m, errors.New("unexpected bridge file type")
			}
		case "tmp":
			if !st.IsDir() {
				return m, errors.New("unexpected temporary data")
			}
		default:
			return m, errors.New("unclassified Telegram persistent file")
		}
	}
	before, err := e.inspect(ctx, "telegram-bridge")
	if err != nil {
		return m, err
	}
	if before.mount("/data") != source || (!cold && (!before.State.Running || before.State.Restarting)) || (cold && !before.cleanStop()) {
		return m, errors.New("bridge mount/state differs")
	}
	started := e.stamp()
	if err = os.MkdirAll(filepath.Join(dest, "data"), 0700); err != nil {
		return m, err
	}
	if err = os.MkdirAll(filepath.Join(dest, "config"), 0700); err != nil {
		return m, err
	}
	for _, n := range []string{"config.yaml", "registration.yaml"} {
		if err = local.Copy(filepath.Join(source, n), filepath.Join(dest, "data", n)); err != nil {
			return m, err
		}
	}
	if err = local.Copy(filepath.Join(project, "docker-compose.yml"), filepath.Join(dest, "config/docker-compose.yml")); err != nil {
		return m, err
	}
	db := filepath.Join(dest, "data/telegram.db")
	if strings.ContainsAny(db, "'\n\r") {
		return m, errors.New("unsafe SQLite destination")
	}
	_, err = e.Runner.Run(ctx, Command{Name: e.Host.SQLite, Args: []string{"-readonly", "-batch", "-bail", "-cmd", ".timeout 15000", filepath.Join(source, "telegram.db"), ".backup '" + db + "'"}, Timeout: 120 * time.Second})
	if err != nil {
		return m, err
	}
	b, err := e.run(ctx, e.Host.SQLite, "-batch", "-bail", db, "PRAGMA journal_mode=DELETE;")
	if err != nil || strings.TrimSpace(string(b)) != "delete" {
		return m, errors.New("cannot finalize SQLite snapshot")
	}
	if err = os.Chmod(db, 0600); err != nil {
		return m, err
	}
	if err = copyOwner(filepath.Join(source, "telegram.db"), db); err != nil {
		return m, err
	}
	if err = copyOwner(source, filepath.Join(dest, "data")); err != nil {
		return m, err
	}
	for _, n := range []string{"config.yaml", "registration.yaml"} {
		a, err := local.FileHash(filepath.Join(source, n))
		if err != nil {
			return m, err
		}
		b, err := local.FileHash(filepath.Join(dest, "data", n))
		if err != nil || a != b {
			return m, errors.New("Telegram configuration changed during snapshot")
		}
	}
	after, err := e.inspect(ctx, "telegram-bridge")
	if err != nil || !sameContainer(before, after) {
		return m, errors.New("Telegram restarted during snapshot")
	}
	if len(before.NetworkSettings.Networks) != 1 {
		return m, errors.New("expected one bridge network")
	}
	var network Network
	for name, n := range before.NetworkSettings.Networks {
		network.Name = name
		network.Address = n.IPAddress
		if n.IPAMConfig != nil && n.IPAMConfig.IPv4Address != "" {
			network.Address = n.IPAMConfig.IPv4Address
		}
	}
	b, err = e.docker(ctx, "network", "inspect", network.Name)
	if err != nil {
		return m, err
	}
	var nets []struct {
		Driver string
		IPAM   struct {
			Config []struct{ Subnet, Gateway string }
		}
	}
	if json.Unmarshal(b, &nets) != nil || len(nets) != 1 || nets[0].Driver != "bridge" || len(nets[0].IPAM.Config) != 1 {
		return m, errors.New("unsupported Docker network")
	}
	network.Subnet = nets[0].IPAM.Config[0].Subnet
	network.Gateway = nets[0].IPAM.Config[0].Gateway
	_, subnet, netErr := net.ParseCIDR(network.Subnet)
	if netErr != nil || !local.SafeID(network.Name) || !subnet.Contains(net.ParseIP(network.Address)) || !subnet.Contains(net.ParseIP(network.Gateway)) {
		return m, errors.New("invalid bridge network addressing")
	}
	counts, err := e.counts(ctx, db)
	if err != nil {
		return m, err
	}
	method := "sqlite-online-backup"
	if cold {
		method = "sqlite-cold-backup"
	}
	m = Telegram{Schema: 1, Method: method, Started: started, Finished: e.stamp(), Image: before.Image, Network: network, Counts: counts, Excluded: []string{"data/tmp", "data/telegram.db-wal", "data/telegram.db-shm"}}
	if err = e.verifyTelegram(ctx, db, m); err != nil {
		return m, err
	}
	return m, local.JSON(filepath.Join(dest, "metadata.json"), m)
}
