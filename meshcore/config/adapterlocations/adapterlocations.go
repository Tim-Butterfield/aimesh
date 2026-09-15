// Package adapterlocations reads and writes the shared adapter configuration, `adapters.yaml`, in the
// user and project `.aimesh` directories, written atomically via meshcore/config. It holds:
//
//   - `adapters.<name>.path`, a machine-local binary path override for a built-in adapter recipe;
//   - `acpAdapters.<name>`, user-defined ACP adapter instances (title, binary path, and the arguments
//     that start the CLI's ACP server), driven by meshcore/model/acpagent.
package adapterlocations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/Tim-Butterfield/aimesh/meshcore/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

const (
	// SchemaVersion is the adapters.yaml schema version.
	SchemaVersion = 1
	// FileName is the location file's name under a `.aimesh` home directory.
	FileName = "adapters.yaml"
)

// Entry is one adapter's configured location. Path is presence-aware: nil inherits a lower layer, and a
// non-nil value, including the empty string (use PATH), overrides it.
type Entry struct {
	Path *string `yaml:"path,omitempty"`
}

// ACPInstance is one user-defined ACP adapter: a Title, the binary Path (empty looks up the instance
// name on PATH), the Args that start the CLI's ACP server over stdio (such as ["--acp"] or ["acp"]),
// and the Model the session reported when validated, used as the expected model for identity
// verification. Unlike Entry it has no presence-aware clear.
type ACPInstance struct {
	Title string   `yaml:"title,omitempty"`
	Path  string   `yaml:"path,omitempty"`
	Args  []string `yaml:"args,omitempty"`
	Model string   `yaml:"model,omitempty"`
}

// Locations is the typed adapter-location file: path overrides for code-owned recipes plus user-defined
// ACP adapter instances.
type Locations struct {
	SchemaVersion int                    `yaml:"schemaVersion"`
	Adapters      map[string]Entry       `yaml:"adapters"`
	ACPAdapters   map[string]ACPInstance `yaml:"acpAdapters,omitempty"`
}

// FilePath returns the adapters.yaml path under a given `.aimesh` home directory.
func FilePath(aimeshHome string) string { return filepath.Join(aimeshHome, FileName) }

// UserHomeBase returns the base directory containing the user-scope `.aimesh/`, delegating to
// localstate, which owns the AIMESH_HOME override.
func UserHomeBase() (string, error) { return localstate.UserHomeBase() }

// UserLocationsPath is the user-scope adapters.yaml path: <UserHomeBase>/.aimesh/adapters.yaml.
func UserLocationsPath() (string, error) {
	base, err := UserHomeBase()
	if err != nil {
		return "", err
	}
	return FilePath(filepath.Join(base, localstate.HomeDirName)), nil
}

// ProjectLocationsPath is the project-scope adapters.yaml path, found by walking up from cwd to the VCS
// root (localstate.FindRoot). ok is false when cwd is not inside a repository.
func ProjectLocationsPath(cwd string) (string, bool) {
	root, ok := localstate.FindRoot(cwd)
	if !ok {
		return "", false
	}
	return FilePath(filepath.Join(root, localstate.HomeDirName)), true
}

// ResolvePaths loads the effective adapter binary-path overrides: the user layer overlaid by the project
// layer. A missing file is an absent layer; a malformed file is an error. The result is the name→path map
// passed to shell.Registry and acpagent.Registry.
func ResolvePaths(cwd string) (map[string]string, error) {
	var layers []Locations
	if up, err := UserLocationsPath(); err == nil {
		u, err := Load(up)
		if err != nil {
			return nil, err
		}
		layers = append(layers, u)
	}
	if pp, ok := ProjectLocationsPath(cwd); ok {
		p, err := Load(pp)
		if err != nil {
			return nil, err
		}
		layers = append(layers, p) // project overlays user
	}
	return Paths(layers...), nil
}

// Load reads a locations file. A missing file yields an empty result without error; a malformed file or
// an unsupported schemaVersion is rejected.
func Load(path string) (Locations, error) {
	loc, _, err := loadWithHash(path)
	return loc, err
}

// loadWithHash is Load plus the file's content hash ("" for a missing file) — the compare token for the
// Update CAS. A missing file yields an empty (schema-stamped) Locations.
func loadWithHash(path string) (Locations, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Locations{SchemaVersion: SchemaVersion, Adapters: map[string]Entry{}, ACPAdapters: map[string]ACPInstance{}}, "", nil
		}
		return Locations{}, "", err
	}
	var loc Locations
	if err := yaml.Unmarshal(b, &loc); err != nil {
		return Locations{}, "", fmt.Errorf("%s: %w", path, err)
	}
	if loc.SchemaVersion != 0 && loc.SchemaVersion != SchemaVersion {
		return Locations{}, "", fmt.Errorf("%s: unsupported schemaVersion %d (want %d)", path, loc.SchemaVersion, SchemaVersion)
	}
	if loc.Adapters == nil {
		loc.Adapters = map[string]Entry{}
	}
	if loc.ACPAdapters == nil {
		loc.ACPAdapters = map[string]ACPInstance{}
	}
	sum := sha256.Sum256(b)
	return loc, hex.EncodeToString(sum[:]), nil
}

// Update is a transactional read-modify-write of a locations file: it loads (missing is empty), applies
// mutate, and writes atomically. A content-hash compare-and-swap detects a concurrent writer and retries
// a bounded number of times, so processes sharing the file do not overwrite each other's edits.
func Update(path string, mutate func(*Locations)) error {
	for range 5 {
		loc, hash, err := loadWithHash(path)
		if err != nil {
			return err
		}
		mutate(&loc)
		if _, cur, err := loadWithHash(path); err != nil {
			return err
		} else if cur != hash {
			continue // another writer landed between our load and now — retry from fresh state
		}
		return Write(path, loc)
	}
	return fmt.Errorf("%s: concurrent-writer CAS retries exhausted", path)
}

// Write serializes locations to path atomically, stamping the current schemaVersion.
func Write(path string, loc Locations) error {
	loc.SchemaVersion = SchemaVersion
	if loc.Adapters == nil {
		loc.Adapters = map[string]Entry{}
	}
	b, err := yaml.Marshal(loc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b)
}

// Paths flattens layered locations (lowest precedence first) into a name→path map: a later non-nil Path
// overrides and a nil Path inherits. A final empty Path means use PATH and is omitted.
func Paths(layers ...Locations) map[string]string {
	merged := map[string]string{} // final resolved override
	present := map[string]bool{}  // whether any layer set an explicit override (incl. clear)
	for _, loc := range layers {
		for name, e := range loc.Adapters {
			if e.Path != nil {
				merged[name] = *e.Path
				present[name] = true
			}
		}
	}
	out := map[string]string{}
	for name := range present {
		if p := merged[name]; p != "" {
			out[name] = p
		}
	}
	return out
}

// ACPInstances flattens layered locations (lowest precedence first) into the effective ACP adapter
// instances by name; a later layer's entry replaces an earlier one.
func ACPInstances(layers ...Locations) map[string]ACPInstance {
	out := map[string]ACPInstance{}
	for _, loc := range layers {
		maps.Copy(out, loc.ACPAdapters)
	}
	return out
}

// ResolveACPInstances loads the effective ACP adapter instances: the user layer overlaid by the project
// layer. A missing file is an absent layer; a malformed file is an error.
func ResolveACPInstances(cwd string) (map[string]ACPInstance, error) {
	var layers []Locations
	if up, err := UserLocationsPath(); err == nil {
		u, err := Load(up)
		if err != nil {
			return nil, err
		}
		layers = append(layers, u)
	}
	if pp, ok := ProjectLocationsPath(cwd); ok {
		p, err := Load(pp)
		if err != nil {
			return nil, err
		}
		layers = append(layers, p)
	}
	return ACPInstances(layers...), nil
}
