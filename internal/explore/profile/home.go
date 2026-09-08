package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// The profiles file lives in explore's component directory under the shared state root:
// `.aimesh/explore/profiles.yaml`, resolved project-scope-before-user and root-anchored, with
// AIMESH_HOME overriding the user base.
//
// THERE IS NO LEGACY roster.yaml DISCOVERY. Earlier builds fell back to a single `roster.yaml` and
// migrated it in memory to a `default` profile. Nothing was ever published that wrote one, so the
// fallback could only ever find a file this project's own development had left behind — a migration
// path for a population of zero, and one more location a reader had to check to answer "which config
// is this run using?". An explicit `--roster <path>` is unaffected: that is a per-invocation input the
// caller resolves, not a discovered location.

// FileName is the profiles file's name inside the component directory.
const FileName = "profiles.yaml"

// UserProfilesPath is the user-scope profiles path: <AIMESH_HOME|~>/.aimesh/explore/profiles.yaml. It is
// a write TARGET — the file need not exist yet (Save creates it + the directory).
func UserProfilesPath() (string, error) {
	dir, err := roster.UserComponentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// ProjectProfilesPath is the project-scope profiles path, ROOT-ANCHORED so a run from a subdirectory
// sees the repo-wide file. ok is false when cwd is not inside a repo. The path is returned whether or
// not the file exists (it is also the write target).
func ProjectProfilesPath(cwd string) (string, bool) {
	dir, ok := roster.ProjectComponentDir(cwd)
	if !ok {
		return "", false
	}
	return filepath.Join(dir, FileName), true
}

// DefaultProfilesPath resolves the profiles path the workbench (and a no-flag run) writes to and binds
// to: project scope when cwd is inside a repo, else user scope (§F10). A write TARGET.
func DefaultProfilesPath(cwd string) (string, error) {
	if p, ok := ProjectProfilesPath(cwd); ok {
		return p, nil
	}
	return UserProfilesPath()
}

// Discover returns the EXISTING profiles.yaml to load for a run when no explicit file/profile flag is
// given (project scope preferred over user), and whether one was found. "" + false = nothing persisted;
// callers fall back to the built-in DefaultSet.
func Discover(cwd string) (string, bool) {
	if p, ok := ProjectProfilesPath(cwd); ok && fileExists(p) {
		return p, true
	}
	if p, err := UserProfilesPath(); err == nil && fileExists(p) {
		return p, true
	}
	return "", false
}

// DiscoverProfiles is Discover under its historical name, kept because callers distinguish "the
// workbench's own saved config" from a caller-resolved seed roster at their call sites.
func DiscoverProfiles(cwd string) (string, bool) { return Discover(cwd) }

// CheckMisplaced refuses a profiles.yaml sitting at the ROOT of a `.aimesh/` state directory rather
// than inside explore's component directory.
//
// The root is a plausible-looking wrong place: it is where `adapters.yaml` lives, so writing
// `profiles.yaml` beside it is the obvious guess. Discovery does not look there, so the file would sit
// on disk being silently ignored while the run bound to some other profile set — or to the shipped
// UNCONFIGURED default, whose error message says nothing about the file the user just wrote. Refusing
// by name, and naming both valid locations, is the only outcome that cannot be mistaken for the
// config having been applied.
func CheckMisplaced(cwd string) error {
	var bad []string
	if root, ok := localstate.FindRoot(cwd); ok {
		if p := filepath.Join(root, localstate.HomeDirName, FileName); fileExists(p) {
			bad = append(bad, p)
		}
	}
	if base, err := localstate.UserHomeBase(); err == nil {
		if p := filepath.Join(base, localstate.HomeDirName, FileName); fileExists(p) {
			bad = append(bad, p)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	proj, projOK := ProjectProfilesPath(cwd)
	user, _ := UserProfilesPath()
	where := "the user-scope path " + user
	if projOK {
		where = "the project-scope path " + proj + " (preferred inside a repository), or " + where
	}
	return fault.New(fault.Config, fmt.Sprintf(
		"profiles: %s sits at the root of the .aimesh state directory, where nothing reads it — "+
			"that root holds the SHARED adapters.yaml, not per-domain config. Move it to %s.",
		strings.Join(bad, " and "), where))
}

// Resolve returns the profiles Set a no-flag run binds to for cwd: the discovered profiles.yaml
// (loaded), else the built-in DefaultSet (the shipped UNCONFIGURED empty default). An explicit
// --profile/--roster is handled by the caller. A misplaced profiles.yaml is an error rather than a
// silent miss (CheckMisplaced).
func Resolve(cwd string) (Set, error) {
	if err := CheckMisplaced(cwd); err != nil {
		return Set{}, err
	}
	if p, ok := Discover(cwd); ok {
		return Load(p)
	}
	return DefaultSet(), nil
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
