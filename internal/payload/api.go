package payload

import (
	"encoding/json"
	"errors"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

var hashRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

func decode(b []byte, v any) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func readJSON(p string, v any) error    { return local.ReadJSON(p, v) }
func Pack(bundle, out string) error     { return packConfiguration(bundle, out) }
func Unpack(input, bundle string) error { return unpackConfiguration(input, bundle) }
func Merge(input, bundle string) error  { return mergeData(input, bundle) }
func Verify(bundle string) error {
	actual, err := local.Manifest(filepath.Join(bundle, "payload"))
	if err != nil {
		return err
	}
	return VerifyManifest(bundle, actual)
}

// VerifyManifest validates a manifest computed from this private frozen payload.
// Source callers may reuse immutable SST hashes; untrusted restores use Verify.
func VerifyManifest(bundle, actual string) error {
	b, e := local.ReadFile(filepath.Join(bundle, "manifest.sha256"), 4<<20)
	if e != nil {
		return e
	}
	entries, e := manifestEntries(string(b))
	if e != nil {
		return e
	}
	if actual != string(b) {
		return errors.New("payload inventory or content mismatch")
	}
	for _, name := range []string{"database/CURRENT", "telegram/data/telegram.db", "telegram/metadata.json", "config/docker-compose.yml", "config/s3/media.env", "control/source-local.json", "nginx/matrix.conf", "nginx/00-matrix-common.conf"} {
		if entries[name] == "" {
			return errors.New("required recovery file missing: " + name)
		}
	}
	return nil
}
