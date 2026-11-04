package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupStatisticsAreOptionalAndBoundToSnapshot(t *testing.T) {
	id := strings.Repeat("a", 64)
	summary := `{"backup_start":"2026-01-01T00:00:00Z","backup_end":"2026-01-01T00:01:12.5Z","total_bytes_processed":1099511627776,"data_added_packed":0,"total_files_processed":10,"files_new":0,"files_changed":0,"files_unmodified":10,"future_field":true}`
	stats := statisticsFromSnapshot(id, json.RawMessage(summary))
	if stats == nil || stats.LogicalBytes != 1<<40 || stats.AddedPackedBytes != 0 || stats.DurationSeconds != 72.5 {
		t.Fatalf("large snapshot or zero increment lost: %+v", stats)
	}
	for _, raw := range []string{"", "null", `{}`, `{"files_new":"invalid"}`,
		strings.Replace(summary, `"data_added_packed":0,`, "", 1),
		strings.Replace(summary, `"data_added_packed":0`, `"data_added_packed":-1`, 1),
		strings.Replace(summary, `"files_unmodified":10`, `"files_unmodified":9`, 1),
		strings.Replace(summary, "1099511627776", "18446744073709551615", 1),
		strings.Replace(summary, "2026-01-01T00:01:12.5Z", "2025-01-01T00:00:00Z", 1),
	} {
		if statisticsFromSnapshot(id, json.RawMessage(raw)) != nil {
			t.Fatalf("unavailable/invalid statistics fabricated: %s", raw)
		}
	}
	pending := t.TempDir()
	saveBackupStatistics(pending, id, json.RawMessage(summary))
	if got := readBackupStatistics(pending, id); got == nil || *got != *stats {
		t.Fatalf("retry lost its statistics: %+v", got)
	}
	if readBackupStatistics(pending, strings.Repeat("b", 64)) != nil {
		t.Fatal("statistics from another snapshot were accepted")
	}
	write(t, filepath.Join(pending, "statistics.json"), "broken optional observation")
	if readBackupStatistics(pending, id) != nil {
		t.Fatal("invalid sidecar accepted")
	}
	must(t, os.Remove(filepath.Join(pending, "statistics.json")))
	must(t, os.Mkdir(filepath.Join(pending, "statistics.json"), 0700))
	// The optional sidecar cannot be written; no error can block backup readiness.
	saveBackupStatistics(pending, id, json.RawMessage(summary))
	if readBackupStatistics(pending, id) != nil {
		t.Fatal("missing observation became a zero-size backup")
	}
}
