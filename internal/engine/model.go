package engine

import (
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/control"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/release"
)

type Network struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
}
type Telegram struct {
	Schema    int              `json:"schema"`
	Method    string           `json:"method"`
	Started   string           `json:"started_at"`
	Finished  string           `json:"finished_at"`
	Image     string           `json:"image_id"`
	Network   Network          `json:"network"`
	Counts    map[string]int64 `json:"state_counts"`
	Restarted bool             `json:"source_service_restarted"`
	Excluded  []string         `json:"excluded_paths"`
}
type Binding struct {
	Schema     int    `json:"schema"`
	Cluster    string `json:"cluster_id"`
	Instance   string `json:"source_instance_id"`
	Epoch      uint64 `json:"source_epoch"`
	Activation string `json:"activation"`
}
type Media struct {
	Provider string `json:"provider"`
	Prefix   string `json:"prefix"`
	Backup   string `json:"backup"`
	Restore  string `json:"restore"`
}
type Metadata struct {
	Format           string              `json:"format"`
	Recovery         int                 `json:"recovery_version"`
	Layout           string              `json:"data_layout"`
	Time             string              `json:"data_time"`
	Image            string              `json:"image_id"`
	Server           string              `json:"server_name"`
	Checkpoint       string              `json:"checkpoint_command"`
	Software         release.Descriptor  `json:"software"`
	Images           map[string]Image    `json:"images"`
	MediaIncluded    bool                `json:"media_included"`
	Media            Media               `json:"media"`
	TelegramIncluded bool                `json:"telegram_included"`
	Consistency      string              `json:"consistency"`
	Telegram         Telegram            `json:"telegram"`
	Binding          Binding             `json:"instance_control"`
	Operation        string              `json:"operation_id,omitempty"`
	Stopped          []control.StopProof `json:"stopped_proof,omitempty"`
}
type Point struct {
	Protocol            int                 `json:"protocol"`
	Schema              int                 `json:"point_schema"`
	Catalog             int                 `json:"catalog_schema"`
	Layout              string              `json:"data_layout"`
	Format              string              `json:"backup_format"`
	Recovery            int                 `json:"recovery_version"`
	Platform            string              `json:"platform"`
	Snapshot            string              `json:"snapshot_id"`
	RepositoryID        string              `json:"repository_id"`
	RepositoryObject    string              `json:"repository_object"`
	RepositoryHash      string              `json:"repository_sha256"`
	ConfigurationObject string              `json:"configuration_object"`
	ConfigurationHash   string              `json:"configuration_sha256"`
	Software            release.Descriptor  `json:"software"`
	Server              string              `json:"server_name"`
	Time                string              `json:"data_time"`
	Verification        string              `json:"verification"`
	Startup             bool                `json:"database_startup_verified"`
	Image               string              `json:"image_id"`
	Images              map[string]Image    `json:"images"`
	Consistency         string              `json:"consistency"`
	Telegram            Telegram            `json:"telegram"`
	Operation           string              `json:"operation_id"`
	Stopped             []control.StopProof `json:"stopped_proof"`
	Media               Media               `json:"media"`
	Binding             Binding             `json:"instance_control"`
	Scope               struct {
		Media    bool `json:"media"`
		Telegram bool `json:"telegram"`
	} `json:"scope"`
}
type Index struct {
	Protocol int    `json:"protocol"`
	Schema   int    `json:"point_schema"`
	Key      string `json:"point_key"`
	Hash     string `json:"point_sha256"`
}
type Repository struct {
	Schema   int    `json:"schema"`
	ID       string `json:"repository_id"`
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	Path     string `json:"path"`
	Access   string `json:"access_key_id"`
	Secret   string `json:"secret_access_key"`
	Password string `json:"password"`
}

func (p Point) validate(cluster, prefix string) error {
	if p.Protocol != 2 || p.Schema != 2 || p.Catalog != 1 || p.Layout != "database-only-v1" || p.Format != "tuwunel-checkpoint-v2" || p.Recovery != 2 || p.Platform != "linux/amd64" || !local.SHA.MatchString(p.Snapshot) || !local.SafeID(p.RepositoryID) || !local.SHA.MatchString(p.RepositoryHash) || !local.SHA.MatchString(p.ConfigurationHash) || p.Binding.Schema != 1 || p.Binding.Cluster != cluster || !local.SafeID(p.Binding.Instance) || p.Binding.Epoch == 0 || p.Binding.Activation != "controller-admission" || p.Scope.Media || !p.Scope.Telegram || p.Media.Backup != "external-s3" || p.Media.Provider != "media_s3" || p.Media.Restore != "reuse-existing" {
		return errors.New("unsupported recovery point")
	}
	if p.RepositoryObject != prefix+"/repositories/"+p.RepositoryID+"-"+p.RepositoryHash+".json" || p.ConfigurationObject != prefix+"/configurations/"+p.ConfigurationHash+".json" {
		return errors.New("recovery reference outside namespace")
	}
	if !validImage(p.Images["tuwunel"]) || !validImage(p.Images["telegram"]) || p.Image != p.Images["tuwunel"].ID || p.Telegram.Image != p.Images["telegram"].ID {
		return errors.New("recovery image identity differs")
	}
	if p.Software.Project != "matrix-admin" || !release.ValidVersion(p.Software.Version) || !local.SHA.MatchString(p.Software.SHA) {
		return errors.New("invalid producer software descriptor")
	}
	return nil
}
func (p Point) matches(m Metadata) bool {
	return m.Format == p.Format && m.Recovery == p.Recovery && m.Layout == p.Layout && m.Server == p.Server && m.Time == p.Time && m.Software == p.Software && m.Binding == p.Binding && m.Image == p.Image && m.Operation == p.Operation && m.Consistency == p.Consistency && sameJSON(m.Stopped, p.Stopped) && sameJSON(m.Images, p.Images) && sameJSON(m.Telegram, p.Telegram) && m.Media == p.Media && !m.MediaIncluded && m.TelegramIncluded
}
func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
