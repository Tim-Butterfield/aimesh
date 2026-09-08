// Package adapterlocations is the shared adapter configuration both apps read. It carries two things,
// under a `.aimesh` home (user + project scopes), written atomically via meshcore/config:
//
//   - `adapters.<name>.path` — the machine-local "where is the binary" override for a CODE-OWNED
//     adapter recipe (shell + the finite unique CLIs). Detect/identity/evidence stay code-owned.
//   - `acpAdapters.<name>` — USER-DEFINED generic ACP adapter INSTANCES (title + binary path + the args
//     that start that CLI's ACP server). ACP is an open protocol with no fixed CLI list, so these are
//     config-defined, not a code catalog; meshcore/model/acpagent drives any of them with one generic
//     adapter.
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

// Entry is one adapter's configured location. Path is PRESENCE-AWARE: a nil pointer means "not set at
// this layer" (inherit a lower layer); a non-nil pointer — INCLUDING the empty string — is an explicit
// override at this scope, so an explicit CLEAR ("use PATH") is distinguishable from omission and can
// override an inherited path.
type Entry struct {
	Path *string `yaml:"path,omitempty"`
}

// ACPInstance is one USER-DEFINED generic ACP adapter: a friendly Title, the binary Path (empty → look
// up the instance name on PATH), the Args that put that CLI into ACP-server mode over stdio
// (e.g. ["--acp"], ["acp"], ["--acp","--stdio"]), and the Model the ACP session reports as active
// (captured at validation) — used as the expected model for identity verification, so a review halts
// only if the agent later answers as a DIFFERENT model. Args/Model are auto-detected at add-time but
// editable. Unlike the path-only Entry there is no presence-aware clear: an instance either exists at a
// scope or it doesn't.
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

// UserHomeBase returns the BASE directory that contains the user-scope `.aimesh/` (i.e. `.aimesh` lives
// at <base>/.aimesh). It delegates to localstate, which owns the one AIMESH_HOME override for every
// component's state — this used to hold its own copy of that lookup, back when the two apps each had a
// competing home variable of their own.
func UserHomeBase() (string, error) { return localstate.UserHomeBase() }

// UserLocationsPath is the user-scope adapters.yaml path: <UserHomeBase>/.aimesh/adapters.yaml.
func UserLocationsPath() (string, error) {
	base, err := UserHomeBase()
	if err != nil {
		return "", err
	}
	return FilePath(filepath.Join(base, localstate.HomeDirName)), nil
}

// ProjectLocationsPath is the project-scope adapters.yaml path, ROOT-ANCHORED: it walks up from cwd to
// the VCS root (via localstate.FindRoot) so a run from a subdirectory sees the repo-wide file. ok is
// false when cwd is not inside a repo (no project scope applies).
func ProjectLocationsPath(cwd string) (string, bool) {
	root, ok := localstate.FindRoot(cwd)
	if !ok {
		return "", false
	}
	return FilePath(filepath.Join(root, localstate.HomeDirName)), true
}

// ResolvePaths loads the effective adapter binary-path overrides for a run: the user-scope layer
// overlaid by the project-scope layer (root-anchored via localstate.FindRoot from cwd). Missing files
// are absent layers (no error); a present-but-malformed file IS an error. The result is the immutable
// name→path map a caller passes to shell.Registry / acpagent.Registry.
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

// Load reads a locations file. A MISSING file is an absent optional layer → empty result, no error. A
// present-but-malformed file, or an unsupported schemaVersion, is rejected.
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

// Update is the transactional read-modify-write for a locations file: it loads (missing → empty),
// applies mutate, and writes atomically — guarding against a concurrent writer with a CONTENT-HASH
// compare-and-swap. It captures the file's content hash at load, re-reads it immediately before the
// atomic write, and RETRIES (re-load → re-mutate) a bounded number of times if another process wrote in
// between, so a conflict is invisible to callers. (App-level generation/409 staleness is a separate UI
// concern.) This is the ONLY correct way for two processes — the two apps' UIs, or a UI + the CLI — to
// share `~/.aimesh/adapters.yaml` without clobbering each other's unrelated edits.
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

// Paths flattens layered locations (lowest precedence FIRST) into a name→path map for shell.Registry /
// acpagent.Registry, applying inheritance + explicit clears: a later layer's non-nil Path overrides; a
// nil Path inherits. A final Path that is a non-nil empty string is an explicit "use PATH" and is
// OMITTED (no override) so binary resolution falls back to PATH lookup.
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

// ACPInstances flattens layered locations (lowest precedence FIRST) into the effective set of
// user-defined ACP adapter instances, keyed by name — a later layer's entry replaces an earlier one
// wholesale (project overrides user). The result is what a caller turns into acpagent adapters.
func ACPInstances(layers ...Locations) map[string]ACPInstance {
	out := map[string]ACPInstance{}
	for _, loc := range layers {
		maps.Copy(out, loc.ACPAdapters)
	}
	return out
}

// ResolveACPInstances loads the effective ACP adapter instances for a run (user layer overlaid by the
// root-anchored project layer). Missing files are absent layers; a malformed file IS an error.
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
