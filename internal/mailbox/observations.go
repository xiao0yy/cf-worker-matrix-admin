package mailbox

import (
	"context"
	"encoding/json"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/management"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func publicV4(value string) bool {
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"} {
		if netip.MustParsePrefix(cidr).Contains(addr) {
			return false
		}
	}
	return true
}
func (p *Poller) publicAddress(ctx context.Context, host map[string]any) map[string]any {
	path := filepath.Join(p.foundation, "agent/public-address.json")
	var cached struct {
		IP     string `json:"ip"`
		At     string `json:"at"`
		Source string `json:"source"`
		Error  string `json:"error,omitempty"`
	}
	if b, er := os.ReadFile(path); er == nil {
		_ = json.Unmarshal(b, &cached)
		at, _ := time.Parse(time.RFC3339Nano, cached.At)
		if p.now().Sub(at) >= 0 && p.now().Sub(at) < time.Hour {
			return map[string]any{"ip": cached.IP, "at": cached.At, "source": cached.Source, "error": cached.Error}
		}
	}
	cached.IP = ""
	cached.Error = ""
	cached.Source = "interface"
	cached.At = p.now().UTC().Format(time.RFC3339Nano)
	ips := map[string]bool{}
	if addresses, ok := host["addresses"].([]string); ok {
		for _, v := range addresses {
			if publicV4(v) {
				ips[v] = true
			}
		}
	}
	if len(ips) == 1 {
		for v := range ips {
			cached.IP = v
		}
	} else if len(ips) > 1 {
		cached.Error = "多个公网 IPv4，请手动指定回源地址"
	} else {
		cached.Source = "cloudflare-trace"
		client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, er := http.NewRequestWithContext(ctx, "GET", "https://www.cloudflare.com/cdn-cgi/trace", nil)
		if er == nil {
			resp, er := client.Do(req)
			if er == nil {
				b, er := io.ReadAll(io.LimitReader(resp.Body, 32769))
				resp.Body.Close()
				if er == nil && resp.StatusCode == 200 && len(b) <= 32768 {
					for _, line := range strings.Split(string(b), "\n") {
						if value, ok := strings.CutPrefix(line, "ip="); ok && publicV4(value) {
							cached.IP = value
						}
					}
				}
			}
		}
		if cached.IP == "" {
			cached.Error = "未探测到公网 IPv4，可手动填写；出口 IP 不证明入站可达"
		}
	}
	_ = writeJSON(path, cached)
	return map[string]any{"ip": cached.IP, "at": cached.At, "source": cached.Source, "error": cached.Error}
}
func (p *Poller) observations(ctx context.Context, report map[string]any) {
	actions := report["actions"].([]string)
	actions = append(actions, management.Actions...)
	actions = append(actions, "task-cancel", "task-retry", "handover-pause", "handover-continue")
	report["actions"] = actions
	report["management_protocol"] = management.Protocol
	report["force_takeover"] = true
	report["public_address"] = p.publicAddress(ctx, report["host"].(map[string]any))
	for name, path := range map[string]string{"preflight": filepath.Join(p.cfg.StateDir, "preflight.json"), "health": filepath.Join(p.foundation, "agent/health.json"), "handover_paused": filepath.Join(p.foundation, "agent/handover-paused.json"), "update": filepath.Join(p.foundation, "agent/pending-update.json")} {
		b, er := os.ReadFile(path)
		if er == nil && len(b) < 65536 {
			var v any
			if json.Unmarshal(b, &v) == nil {
				report[name] = v
			}
		}
	}
	entries, er := os.ReadDir(management.Directory(p.foundation))
	if er != nil {
		return
	}
	var summaries []map[string]any
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		t, er := management.Read(p.foundation, strings.TrimSuffix(entry.Name(), ".json"))
		if er != nil {
			report["task_report_error"] = "任务状态文件无效"
			continue
		}
		if er = management.Publish(ctx, p.foundation, t, p.store, p.prefix()+"tasks/"+p.cfg.Instance+"/"+t.Request.ID+".json"); er != nil {
			report["task_report_error"] = "任务状态上传失败"
		}
		if !management.Terminal(t.State) {
			summaries = append(summaries, map[string]any{"id": t.Request.ID, "action": t.Request.Action, "state": t.State, "stage": t.Stage, "updated_at": t.Updated})
		}
	}
	report["active_tasks"] = summaries
}
