package mailbox

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportPublishesBackupStatisticsWithItsSnapshot(t *testing.T) {
	p, store, _, _ := setup(t)
	p.foundation = t.TempDir()
	p.status = func(context.Context) (Status, error) {
		return Status{Deployment: p.foundation}, nil
	}
	// Keep this report test offline, including the independent public-IP observer.
	if err := writeJSON(filepath.Join(p.foundation, "agent/public-address.json"), map[string]any{"ip": "8.8.8.8", "at": p.now().Format(time.RFC3339), "source": "fixture"}); err != nil {
		t.Fatal(err)
	}
	stats := map[string]any{"schema": 1, "snapshot_id": strings.Repeat("a", 64), "logical_bytes": 1 << 40, "added_packed_bytes": 0}
	if err := writeJSON(filepath.Join(p.foundation, "backup-state/tuwunel-restic/last-success.json"), map[string]any{"snapshot_id": strings.Repeat("a", 64), "statistics": stats, "private_field": "must not be reported"}); err != nil {
		t.Fatal(err)
	}
	if err := p.report(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Backup map[string]json.RawMessage `json:"backup_observation"`
	}
	if err := json.Unmarshal(store.objects[p.prefix()+"agents/"+p.cfg.Instance+".json"], &report); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(stats)
	if string(report.Backup["statistics"]) != string(want) || report.Backup["private_field"] != nil {
		t.Fatal("statistics missing or private fields leaked")
	}
}

func TestPublicAddressRejectsPrivateSharedAndReservedRanges(t *testing.T) {
	for _, ip := range []string{"10.1.2.3", "127.0.0.1", "100.64.1.2", "192.0.2.3", "198.51.100.2", "203.0.113.5", "169.254.1.1", "198.18.1.1", "240.1.1.1", "0.1.1.1", "::1", "2001:db8::1", "invalid"} {
		if publicV4(ip) {
			t.Fatal("not a public origin", ip)
		}
	}
	if !publicV4("8.8.8.8") {
		t.Fatal("public IPv4 rejected")
	}
}
