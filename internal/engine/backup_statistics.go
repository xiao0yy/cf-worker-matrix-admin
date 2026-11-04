package engine

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
)

// BackupStatistics is an optional observation, never a recovery requirement.
// Only aggregate values enter the instance report: no paths or credentials.
type BackupStatistics struct {
	Schema           int     `json:"schema"`
	Snapshot         string  `json:"snapshot_id"`
	LogicalBytes     uint64  `json:"logical_bytes"`
	AddedPackedBytes uint64  `json:"added_packed_bytes"`
	FilesTotal       uint64  `json:"files_total"`
	FilesNew         uint64  `json:"files_new"`
	FilesChanged     uint64  `json:"files_changed"`
	FilesUnmodified  uint64  `json:"files_unmodified"`
	StartedAt        string  `json:"started_at"`
	FinishedAt       string  `json:"finished_at"`
	DurationSeconds  float64 `json:"duration_seconds"`
}

// restic persists these counters in the snapshot summary, so retries can use
// the existing snapshots response even after losing the backup command output.
// Decode separately: malformed/missing optional statistics must not fail backup.
func statisticsFromSnapshot(id string, raw json.RawMessage) *BackupStatistics {
	var s struct {
		Start      time.Time `json:"backup_start"`
		End        time.Time `json:"backup_end"`
		Bytes      *uint64   `json:"total_bytes_processed"`
		Added      *uint64   `json:"data_added_packed"`
		Files      *uint64   `json:"total_files_processed"`
		New        *uint64   `json:"files_new"`
		Changed    *uint64   `json:"files_changed"`
		Unmodified *uint64   `json:"files_unmodified"`
	}
	if json.Unmarshal(raw, &s) != nil || s.Bytes == nil || s.Added == nil || s.Files == nil || s.New == nil || s.Changed == nil || s.Unmodified == nil || s.Start.IsZero() || s.End.IsZero() || s.End.Before(s.Start) {
		return nil
	}
	stats := &BackupStatistics{
		Schema: 1, Snapshot: id,
		LogicalBytes: *s.Bytes, AddedPackedBytes: *s.Added,
		FilesTotal: *s.Files, FilesNew: *s.New, FilesChanged: *s.Changed, FilesUnmodified: *s.Unmodified,
		StartedAt: s.Start.UTC().Format(time.RFC3339Nano), FinishedAt: s.End.UTC().Format(time.RFC3339Nano),
		DurationSeconds: s.End.Sub(s.Start).Seconds(),
	}
	if !stats.valid(id) {
		return nil
	}
	return stats
}

func (s *BackupStatistics) valid(snapshot string) bool {
	if s == nil || s.Schema != 1 || s.Snapshot != snapshot || !local.SHA.MatchString(snapshot) {
		return false
	}
	// Reports are consumed by JavaScript; retain exact integer byte/file counts.
	for _, value := range []uint64{s.LogicalBytes, s.AddedPackedBytes, s.FilesTotal, s.FilesNew, s.FilesChanged, s.FilesUnmodified} {
		if value > 1<<53-1 {
			return false
		}
	}
	start, startErr := time.Parse(time.RFC3339Nano, s.StartedAt)
	end, endErr := time.Parse(time.RFC3339Nano, s.FinishedAt)
	return startErr == nil && endErr == nil && !start.IsZero() && !end.IsZero() && !end.Before(start) &&
		s.DurationSeconds == end.Sub(start).Seconds() &&
		s.FilesNew+s.FilesChanged+s.FilesUnmodified == s.FilesTotal
}

func saveBackupStatistics(pending, snapshot string, summary json.RawMessage) {
	if stats := statisticsFromSnapshot(snapshot, summary); stats != nil {
		// Separate from job.json and immutable recovery points. A failed write
		// loses only this observation, not the backup or its readiness receipt.
		_ = local.JSON(filepath.Join(pending, "statistics.json"), stats)
	}
}

func readBackupStatistics(pending, snapshot string) *BackupStatistics {
	var stats BackupStatistics
	if local.ReadJSON(filepath.Join(pending, "statistics.json"), &stats) != nil || !stats.valid(snapshot) {
		return nil
	}
	return &stats
}
