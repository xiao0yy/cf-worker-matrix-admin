package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"path/filepath"
	"regexp"
	"strings"
)

type Container struct {
	ID     string `json:"Id"`
	Image  string
	Config struct {
		Image string
		Env   []string
	}
	State struct {
		Running, Restarting, OOMKilled bool
		Pid, ExitCode                  int
		StartedAt, FinishedAt          string
	}
	Mounts          []struct{ Source, Destination string }
	HostConfig      struct{ RestartPolicy struct{ Name string } }
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress  string
			IPAMConfig *struct{ IPv4Address string }
		}
	}
}

func (e *Engine) inspect(ctx context.Context, name string) (Container, error) {
	var list []Container
	b, err := e.docker(ctx, "inspect", name)
	if err == nil {
		err = json.Unmarshal(b, &list)
	}
	if err != nil {
		return Container{}, err
	}
	if len(list) != 1 {
		return Container{}, errors.New("ambiguous container")
	}
	return list[0], nil
}
func (c Container) mount(target string) string {
	for _, m := range c.Mounts {
		if m.Destination == target {
			return m.Source
		}
	}
	return ""
}
func (c Container) env(key string) (string, error) {
	var value string
	n := 0
	for _, v := range c.Config.Env {
		if strings.HasPrefix(v, key+"=") {
			value = strings.TrimPrefix(v, key+"=")
			n++
		}
	}
	if n != 1 {
		return "", fmt.Errorf("missing/duplicate container variable %s", key)
	}
	return value, nil
}
func (c Container) cleanStop() bool {
	return !c.State.Running && !c.State.Restarting && !c.State.OOMKilled && c.State.Pid == 0 && c.State.ExitCode == 0
}
func sameContainer(a, b Container) bool {
	return a.ID == b.ID && a.Image == b.Image && a.State.StartedAt == b.State.StartedAt && a.State.Running == b.State.Running && !b.State.Restarting
}

var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]+@sha256:[a-f0-9]{64}$`)

type Image struct {
	Ref string `json:"ref"`
	ID  string `json:"id"`
}

func validImage(i Image) bool {
	prefix := strings.Split(i.Ref, "/")[0]
	return imagePattern.MatchString(i.Ref) && strings.Contains(i.Ref, "/") && (strings.Contains(prefix, ".") || strings.Contains(prefix, ":")) && strings.HasPrefix(i.ID, "sha256:") && local.SHA.MatchString(strings.TrimPrefix(i.ID, "sha256:"))
}
func (e *Engine) image(ctx context.Context, c Container) (Image, error) {
	i := Image{c.Config.Image, c.Image}
	if !validImage(i) {
		return i, errors.New("container image must use public registry digest")
	}
	return i, e.checkImage(ctx, i, false)
}
func (e *Engine) checkImage(ctx context.Context, i Image, pull bool) error {
	if !validImage(i) {
		return errors.New("invalid immutable image")
	}
	b, err := e.docker(ctx, "image", "inspect", i.Ref)
	if err != nil && pull {
		if _, err = e.docker(ctx, "pull", i.Ref); err != nil {
			return err
		}
		b, err = e.docker(ctx, "image", "inspect", i.Ref)
	}
	if err != nil {
		return err
	}
	var list []struct {
		ID               string `json:"Id"`
		OS, Architecture string
	}
	if json.Unmarshal(b, &list) != nil || len(list) != 1 || list[0].ID != i.ID || list[0].OS != "linux" || list[0].Architecture != "amd64" {
		return errors.New("image identity/platform mismatch")
	}
	return nil
}
func (e *Engine) rewriteCompose(ctx context.Context, project string, i Image, telegram bool, n Network) error {
	b, err := e.compose(ctx, project, "config", "--format", "json")
	if err != nil {
		return err
	}
	var doc map[string]any
	if err = json.Unmarshal(b, &doc); err != nil {
		return err
	}
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) != 1 {
		return errors.New("unexpected compose services")
	}
	name := "tuwunel"
	if telegram {
		name = "telegram"
	}
	service, ok := services[name].(map[string]any)
	if !ok {
		return errors.New("compose service missing")
	}
	// Rebind every supported persistent mount to this generation. A source host's
	// absolute paths must never survive restore, even if Compose accepts them.
	volumes, ok := service["volumes"].([]any)
	if !ok {
		return errors.New("compose persistent mounts missing")
	}
	mounts := map[string]string{"/var/lib/tuwunel": "data", "/etc/tuwunel/appservices": "appservices", "/etc/tuwunel/registration-token": "registration-token"}
	if telegram {
		mounts = map[string]string{"/data": "data"}
	}
	seen := map[string]bool{}
	for _, raw := range volumes {
		mount, ok := raw.(map[string]any)
		if !ok || mount["type"] != "bind" {
			return errors.New("unsupported compose mount")
		}
		target, _ := mount["target"].(string)
		rel, ok := mounts[target]
		if !ok || seen[target] {
			return errors.New("unknown/duplicate persistent mount")
		}
		seen[target] = true
		mount["source"] = filepath.Join(project, rel)
	}
	required := "/var/lib/tuwunel"
	if telegram {
		required = "/data"
	}
	if !seen[required] {
		return errors.New("data mount missing")
	}
	delete(service, "build")
	service["image"] = i.Ref
	service["restart"] = "no"
	if telegram {
		networks, ok := service["networks"].(map[string]any)
		if !ok || len(networks) != 1 {
			return errors.New("unsupported Telegram network")
		}
		nets, ok := doc["networks"].(map[string]any)
		if !ok || len(nets) != 1 {
			return errors.New("missing Telegram network")
		}
		for key := range networks {
			networks[key] = map[string]any{"ipv4_address": n.Address}
			nets[key] = map[string]any{"name": n.Name, "external": true}
		}
	}
	if !telegram {
		env, ok := service["environment"].(map[string]any)
		if !ok {
			return errors.New("tuwunel environment missing")
		}
		for key := range env {
			if strings.HasPrefix(key, "TUWUNEL_STORAGE_PROVIDER__MEDIA_S3__S3__") {
				delete(env, key)
			}
		}
		service["env_file"] = []string{"./s3/media.env"}
		nets, ok := doc["networks"].(map[string]any)
		if !ok {
			return errors.New("compose network missing")
		}
		net, ok := nets["default"].(map[string]any)
		if !ok {
			return errors.New("default network missing")
		}
		net["name"] = n.Name
		net["ipam"] = map[string]any{"config": []any{map[string]string{"subnet": n.Subnet, "gateway": n.Gateway}}}
	}
	if err = local.JSON(filepath.Join(project, "docker-compose.yml"), doc); err != nil {
		return err
	}
	_, err = e.compose(ctx, project, "config", "--quiet")
	return err
}
