package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var errMissing = errors.New("control object missing; explicit initialization required")
var errConflict = errors.New("conditional write conflict")

type State struct {
	Schema    int             `json:"schema"`
	ClusterID string          `json:"cluster_id"`
	Revision  uint64          `json:"revision"`
	Epoch     uint64          `json:"epoch"`
	Owner     string          `json:"owner"`
	Phase     string          `json:"phase"`
	Operation json.RawMessage `json:"operation"`
	Completed *FinalOperation `json:"last_completed,omitempty"`
}

func (s State) validate(cluster string) error {
	if s.Schema != 1 || s.ClusterID != cluster || s.Revision == 0 || s.Epoch == 0 {
		return errors.New("unsupported or invalid control state")
	}
	switch s.Phase {
	case "ONLINE", "REQUESTED", "QUIESCING", "QUIESCED", "SNAPSHOTTING", "RELEASED", "RESTORING", "VALIDATING", "STARTING":
	default:
		return errors.New("unknown control phase")
	}
	if s.Phase == "ONLINE" && (s.Owner == "" || string(s.Operation) != "null") {
		return errors.New("invalid ONLINE state")
	}
	return nil
}

type Authority interface {
	Read(context.Context) (State, string, error)
	Write(context.Context, State, string) error
	Report(context.Context, string, []byte) error
}
type Store struct {
	endpoint, bucket, region, access, secret, cluster string
	client                                            *http.Client
}

// Parse only assignments, never execute the administrator's credentials file.
func newStore(path, cluster string) (*Store, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("cannot stat private S3 config")
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("S3 config must be a private regular file (0600)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read S3 config")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, errors.New("invalid S3 assignment")
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '\'' && v[len(v)-1] == '\'') || (v[0] == '"' && v[len(v)-1] == '"')) {
			v = v[1 : len(v)-1]
		}
		switch k {
		case "S3_ENDPOINT", "S3_REGION", "S3_BUCKET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "CLUSTER_ID":
		default:
			return nil, errors.New("unknown S3 config field")
		}
		if _, exists := values[k]; exists || v == "" || strings.Contains(v, "REPLACE") || strings.ContainsAny(v, "\r\n\x00") {
			return nil, errors.New("invalid or duplicate S3 config field")
		}
		values[k] = v
	}
	for _, k := range []string{"S3_ENDPOINT", "S3_REGION", "S3_BUCKET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if values[k] == "" {
			return nil, errors.New("incomplete S3 config")
		}
	}
	if v := values["CLUSTER_ID"]; v != "" && v != cluster {
		return nil, errors.New("R2 input belongs to a different cluster")
	}
	u, err := url.Parse(values["S3_ENDPOINT"])
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || !strings.HasSuffix(u.Hostname(), ".r2.cloudflarestorage.com") {
		return nil, errors.New("control storage must use the authenticated R2 S3 endpoint")
	}
	if !safeID(values["S3_BUCKET"]) {
		return nil, errors.New("invalid bucket name")
	}
	return &Store{endpoint: strings.TrimRight(values["S3_ENDPOINT"], "/"), bucket: values["S3_BUCKET"], region: values["S3_REGION"], access: values["AWS_ACCESS_KEY_ID"], secret: values["AWS_SECRET_ACCESS_KEY"], cluster: cluster,
		client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func mac(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}
func (s *Store) request(ctx context.Context, method, key string, body []byte, match string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+"/"+s.bucket+"/"+key, bytes.NewReader(body))
	if err != nil {
		return nil, "", errors.New("invalid S3 request")
	}
	now := time.Now().UTC()
	timestamp, day := now.Format("20060102T150405Z"), now.Format("20060102")
	hash := digest(body)
	req.Header.Set("X-Amz-Date", timestamp)
	req.Header.Set("X-Amz-Content-Sha256", hash)
	req.Header.Set("Cache-Control", "no-store")
	signed, canonical := "host;x-amz-content-sha256;x-amz-date", "host:"+req.URL.Host+"\nx-amz-content-sha256:"+hash+"\nx-amz-date:"+timestamp+"\n"
	if match != "" {
		name := "If-Match"
		if match == "*" {
			name = "If-None-Match"
		}
		req.Header.Set(name, match)
		signed = "host;" + strings.ToLower(name) + ";x-amz-content-sha256;x-amz-date"
		canonical = "host:" + req.URL.Host + "\n" + strings.ToLower(name) + ":" + match + "\nx-amz-content-sha256:" + hash + "\nx-amz-date:" + timestamp + "\n"
	}
	canon := method + "\n" + req.URL.EscapedPath() + "\n\n" + canonical + "\n" + signed + "\n" + hash
	scope := day + "/" + s.region + "/s3/aws4_request"
	k := mac(mac(mac(mac([]byte("AWS4"+s.secret), day), s.region), "s3"), "aws4_request")
	sig := hex.EncodeToString(mac(k, "AWS4-HMAC-SHA256\n"+timestamp+"\n"+scope+"\n"+digest([]byte(canon))))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.access+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", errors.New("R2 request failed or timed out")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(b) > 1024*1024 {
		return nil, "", errors.New("invalid R2 response size")
	}
	switch resp.StatusCode {
	case 200, 204:
		return b, resp.Header.Get("ETag"), nil
	case 404:
		return nil, "", errMissing
	case 409, 412:
		return nil, "", errConflict
	default:
		return nil, "", fmt.Errorf("R2 HTTP %d", resp.StatusCode)
	}
}
func (s *Store) key() string { return "cluster/" + s.cluster + "/control.json" }
func (s *Store) Read(ctx context.Context) (State, string, error) {
	b, etag, err := s.request(ctx, "GET", s.key(), nil, "")
	if err != nil {
		return State{}, "", err
	}
	var state State
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&state); err != nil {
		return State{}, "", errors.New("malformed control state")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return State{}, "", errors.New("trailing control state content")
	}
	if err = state.validate(s.cluster); err != nil {
		return State{}, "", err
	}
	if !strings.HasPrefix(etag, "\"") || !strings.HasSuffix(etag, "\"") {
		return State{}, "", errors.New("missing control ETag")
	}
	return state, etag, nil
}
func (s *Store) Write(ctx context.Context, state State, etag string) error {
	if err := state.validate(s.cluster); err != nil {
		return err
	}
	if etag == "" {
		return errors.New("conditional write required")
	}
	b, _ := json.Marshal(state)
	_, _, err := s.request(ctx, "PUT", s.key(), b, etag)
	return err
}
func (s *Store) Report(ctx context.Context, id string, b []byte) error {
	if !safeID(id) {
		return errors.New("invalid report instance")
	}
	_, _, err := s.request(ctx, "PUT", "cluster/"+s.cluster+"/instances/"+id+".json", b, "")
	return err
}
