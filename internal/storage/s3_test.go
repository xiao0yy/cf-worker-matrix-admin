package storage

import (
	"context"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSignedConditionalRequests(t *testing.T) {
	var seen []string
	c := &Client{Endpoint: "https://example.r2.cloudflarestorage.com", Bucket: "fixture", Region: "auto", Access: "test-access", Secret: "test-secret"}
	c.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Amz-Content-Sha256") != local.Digest(b) {
			t.Fatal("missing payload hash, including empty GET")
		}
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, "/auto/s3/aws4_request") || !strings.Contains(auth, "x-amz-content-sha256") {
			t.Fatal("unsigned required header")
		}
		condition := r.Header.Get("If-Match")
		if r.Header.Get("If-None-Match") == "*" {
			condition = "*"
			if !strings.Contains(auth, "if-none-match") {
				t.Fatal("unsigned create condition")
			}
		} else if condition != "" && !strings.Contains(auth, "if-match") {
			t.Fatal("unsigned CAS")
		}
		seen = append(seen, r.Method+condition)
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": {`"revision"`}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	if _, tag, e := c.Get(context.Background(), "cluster/test/control.json"); e != nil || tag != `"revision"` {
		t.Fatal(e, tag)
	}
	for _, condition := range []string{"*", `"old"`} {
		if e := c.Put(context.Background(), "cluster/test/control.json", []byte(`{}`), condition); e != nil {
			t.Fatal(e)
		}
	}
	if strings.Join(seen, ",") != "GET,PUT*,PUT\"old\"" {
		t.Fatal(seen)
	}
	for _, k := range []string{"../other", "/root", "x?secret", "x%2fother"} {
		if _, _, e := c.Get(context.Background(), k); e == nil {
			t.Fatal("accepted key", k)
		}
	}
}
func TestTransportFailuresDoNotExposeCredentials(t *testing.T) {
	c := &Client{Endpoint: "https://example.r2.cloudflarestorage.com", Bucket: "fixture", Region: "auto", Access: "secret-access", Secret: "secret-key"}
	for code, want := range map[int]error{404: Missing, 409: Conflict, 412: Conflict, 500: nil} {
		c.HTTP = &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("secret-server-error"))}, nil
		})}
		_, _, e := c.Get(context.Background(), "control.json")
		if e == nil || want != nil && !errors.Is(e, want) || strings.Contains(e.Error(), "secret") {
			t.Fatal(code, e)
		}
	}
}
