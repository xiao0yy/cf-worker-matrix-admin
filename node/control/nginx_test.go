package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveNginxGate(t *testing.T) {
	if os.Getenv("MATRIX_CONTROL_LIVE_S3") == "" {
		t.Skip("opt-in nginx integration test")
	}
	c, a, _, _ := fixture(t)
	c.observe(a.state, nil)
	c.ready = true
	gate := httptest.NewServer(http.HandlerFunc(c.gate))
	defer gate.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/forbidden" {
			w.WriteHeader(403)
			return
		}
		w.WriteHeader(200)
	}))
	defer backend.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	root := t.TempDir()
	server, err := os.ReadFile("nginx-server.conf")
	if err != nil {
		t.Fatal(err)
	}
	location, err := os.ReadFile("nginx-location.conf")
	if err != nil {
		t.Fatal(err)
	}
	server = []byte(strings.ReplaceAll(string(server), "http://127.0.0.1:29318", gate.URL))
	config := fmt.Sprintf("pid %s/nginx.pid; error_log %s/error.log; events {} http { access_log off; client_body_temp_path %s/body; server { listen %s; %s location /_matrix/ { %s proxy_pass %s; } } }", root, root, root, address, server, location, backend.URL)
	path := filepath.Join(root, "nginx.conf")
	if err = os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if b, e := exec.Command("nginx", "-p", root, "-c", path).CombinedOutput(); e != nil {
		t.Fatalf("nginx start: %s %s", e, b)
	}
	defer exec.Command("nginx", "-p", root, "-c", path, "-s", "quit").Run()
	client := &http.Client{Timeout: 3 * time.Second}
	check := func(method, path string, want int) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+address+path, nil)
		r, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("%s %s: got %d want %d", method, path, r.StatusCode, want)
		}
	}
	check("GET", "/_matrix/test", 200)
	check("POST", "/_matrix/test", 200)
	check("GET", "/_matrix/forbidden", 403)
	c.fence("test")
	check("GET", "/_matrix/test", 503)
	check("POST", "/_matrix/test", 503)
	gate.Close()
	check("GET", "/_matrix/test", 503)
	t.Log("PASS: nginx gates GET/POST, preserves backend 403, and returns 503 when controller is absent")
}
