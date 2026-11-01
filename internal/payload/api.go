package payload

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"io"
	"os"
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
// Explicit drills use Verify; normal restores rely on restic for data integrity.
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
	return requiredFiles(entries)
}

// VerifyInventory checks staging identity and bounded configuration contents.
// Database hashes were recorded when staging froze the files; reading them again
// here would duplicate the backup producer's work on every upload or retry.
func VerifyInventory(bundle string) error {
	b, err := local.ReadFile(filepath.Join(bundle, "manifest.sha256"), 4<<20)
	if err != nil {
		return err
	}
	entries, err := manifestEntries(string(b))
	if err != nil {
		return err
	}
	if err = requiredFiles(entries); err != nil {
		return err
	}
	seen := map[string]bool{}
	root := filepath.Join(bundle, "payload")
	if err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name == "." || name == "database" || name == "database/archive" || configurationDirectory(name) {
				return nil
			}
			return fmt.Errorf("unclassified payload directory: %q", name)
		}
		if !d.Type().IsRegular() || entries[name] == "" {
			return fmt.Errorf("unexpected staged payload file: %q", name)
		}
		if !dataPath(name) {
			content, err := boundedFile(path, 8<<20)
			if err != nil {
				return err
			}
			if fileHash(content) != entries[name] {
				return errors.New("staged configuration content mismatch")
			}
		}
		seen[name] = true
		return nil
	}); err != nil {
		return err
	}
	if len(seen) != len(entries) {
		return errors.New("staged payload file missing")
	}
	return nil
}

func requiredFiles(entries map[string]string) error {
	for _, name := range []string{"database/CURRENT", "telegram/data/telegram.db", "telegram/metadata.json", "config/docker-compose.yml", "config/s3/media.env", "control/source-local.json", "nginx/matrix.conf", "nginx/00-matrix-common.conf"} {
		if entries[name] == "" {
			return errors.New("required recovery file missing: " + name)
		}
	}
	return nil
}
