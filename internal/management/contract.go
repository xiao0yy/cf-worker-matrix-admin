// Package management defines bounded, declarative maintenance tasks. It cannot
// execute commands or change ownership. Both the mailbox and engine use it.
package management

import (
	"errors"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
	"strings"
	"time"
)

const Protocol = 1

type Expected struct {
	Revision uint64 `json:"revision"`
	Epoch    uint64 `json:"epoch"`
	Owner    string `json:"owner"`
	Phase    string `json:"phase"`
}
type Parameters struct {
	Snapshot     string   `json:"snapshot_id,omitempty"`
	PointHash    string   `json:"point_sha256,omitempty"`
	Version      string   `json:"version,omitempty"`
	ManifestHash string   `json:"manifest_sha256,omitempty"`
	Plan         string   `json:"plan_id,omitempty"`
	PlanHash     string   `json:"plan_sha256,omitempty"`
	KeepLast     int      `json:"keep_last,omitempty"`
	KeepDays     int      `json:"keep_days,omitempty"`
	Full         bool     `json:"full,omitempty"`
	Isolated     []string `json:"isolated_instances,omitempty"`
	Isolation    string   `json:"isolation_method,omitempty"`
	Note         string   `json:"note,omitempty"`
	AcceptLoss   bool     `json:"accept_data_loss,omitempty"`
	Task         string   `json:"task_id,omitempty"`
}
type Request struct {
	ID         string     `json:"id"`
	Cluster    string     `json:"cluster_id"`
	Target     string     `json:"target"`
	Action     string     `json:"action"`
	Expected   Expected   `json:"expected"`
	Parameters Parameters `json:"parameters"`
	Created    string     `json:"created_at"`
}
type Task struct {
	Schema   int     `json:"schema"`
	Request  Request `json:"request"`
	State    string  `json:"state"`
	Stage    string  `json:"stage"`
	Updated  string  `json:"updated_at"`
	Started  string  `json:"started_at,omitempty"`
	Finished string  `json:"finished_at,omitempty"`
	Attempt  int     `json:"attempt"`
	Error    string  `json:"error,omitempty"`
	Result   any     `json:"result,omitempty"`
}

var Actions = []string{"preflight", "backup-now", "restore-drill", "upgrade-runtime", "refresh-config", "storage-plan", "storage-apply", "cleanup-plan", "cleanup-apply", "health-check", "confirm-delivery", "maintenance-recover", "force-takeover"}

func IsTask(s string) bool {
	for _, a := range Actions {
		if a == s {
			return true
		}
	}
	return false
}
func IsAction(s string) bool {
	return IsTask(s) || s == "task-cancel" || s == "task-retry" || s == "handover-pause" || s == "handover-continue"
}
func Terminal(s string) bool {
	return s == "SUCCEEDED" || s == "FAILED" || s == "CANCELLED" || s == "INTERRUPTED"
}
func (r Request) Validate() error {
	if !local.SafeID(r.ID) || strings.Contains(r.ID, ".") || !local.SafeID(r.Cluster) || !local.SafeID(r.Target) || !IsAction(r.Action) || r.Expected.Epoch == 0 || r.Expected.Revision == 0 {
		return errors.New("invalid management task identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Created); err != nil {
		return errors.New("invalid task timestamp")
	}
	p := r.Parameters
	if (p.Snapshot == "") != (p.PointHash == "") || p.Snapshot != "" && (!local.SHA.MatchString(p.Snapshot) || !local.SHA.MatchString(p.PointHash)) {
		return errors.New("explicit snapshot and point checksum must be paired")
	}
	allowed := Parameters{}
	switch r.Action {
	case "preflight", "restore-drill", "refresh-config":
		allowed.Snapshot, allowed.PointHash = p.Snapshot, p.PointHash
	case "confirm-delivery":
		if p.Note != "both-directions-received" {
			return errors.New("explicit observation of both directions required")
		}
		allowed.Note = p.Note
	case "backup-now":
		allowed.Full = p.Full
	case "upgrade-runtime":
		if !release.ValidVersion(p.Version) || !local.SHA.MatchString(p.ManifestHash) {
			return errors.New("upgrade requires a fixed version and manifest checksum")
		}
		allowed.Version, allowed.ManifestHash = p.Version, p.ManifestHash
	case "storage-plan":
		if p.KeepLast < 2 || p.KeepLast > 10000 || p.KeepDays < 1 || p.KeepDays > 3650 {
			return errors.New("keep at least two snapshots and 1–3650 days")
		}
		allowed.KeepLast, allowed.KeepDays = p.KeepLast, p.KeepDays
	case "storage-apply", "cleanup-apply":
		if !local.SafeID(p.Plan) || !local.SHA.MatchString(p.PlanHash) {
			return errors.New("apply requires the exact reviewed plan and checksum")
		}
		allowed.Plan, allowed.PlanHash = p.Plan, p.PlanHash
	case "force-takeover":
		if p.Snapshot == "" || !p.AcceptLoss || len(p.Isolated) == 0 || len(p.Isolated) > 16 || (p.Isolation != "powered-off" && p.Isolation != "fully-isolated") || len(strings.TrimSpace(p.Note)) < 3 || len(p.Note) > 500 {
			return errors.New("takeover requires a selected recovery point, external isolation evidence and acknowledged data loss")
		}
		seen := map[string]bool{}
		for _, id := range p.Isolated {
			if !local.SafeID(id) || id == r.Target || seen[id] {
				return errors.New("invalid isolated instance list")
			}
			seen[id] = true
		}
		allowed.Snapshot, allowed.PointHash, allowed.AcceptLoss = p.Snapshot, p.PointHash, p.AcceptLoss
		allowed.Isolated, allowed.Isolation, allowed.Note = p.Isolated, p.Isolation, p.Note
	case "task-cancel", "task-retry":
		if !local.SafeID(p.Task) {
			return errors.New("exact task ID required")
		}
		allowed.Task = p.Task
	case "handover-pause", "handover-continue", "cleanup-plan", "health-check", "maintenance-recover":
	}
	if string(release.JSON(allowed)) != string(release.JSON(p)) {
		return fmt.Errorf("parameters do not apply to %s", r.Action)
	}
	return nil
}
