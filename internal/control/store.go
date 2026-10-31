package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/storage"
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
func (s *Store) request(ctx context.Context, method, key string, body []byte, match string) ([]byte, string, error) {
	c := &storage.Client{Endpoint: s.endpoint, Bucket: s.bucket, Region: s.region, Access: s.access, Secret: s.secret, HTTP: s.client}
	b, tag, err := c.Request(ctx, method, key, body, match)
	if errors.Is(err, storage.Missing) {
		err = errMissing
	}
	if errors.Is(err, storage.Conflict) {
		err = errConflict
	}
	return b, tag, err
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
