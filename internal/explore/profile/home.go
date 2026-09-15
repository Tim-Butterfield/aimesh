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

// The profiles file is `.aimesh/explore/profiles.yaml`, found at project scope before user scope. The
// project path is anchored at the repository root, and AIMESH_HOME overrides the user base. An explicit
// `--roster <path>` is resolved by the caller and is not discovered here.

// FileName is the profiles file's name inside the component directory.
const FileName = "profiles.yaml"

// UserProfilesPath returns the user-scope profiles path, <AIMESH_HOME|~>/.aimesh/explore/profiles.yaml.
// The file need not exist.
func UserProfilesPath() (string, error) {
	dir, err := roster.UserComponentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// ProjectProfilesPath returns the project-scope profiles path, anchored at the repository root. It
// returns false when cwd is not inside a repository. The file need not exist.
func ProjectProfilesPath(cwd string) (string, bool) {
	dir, ok := roster.ProjectComponentDir(cwd)
	if !ok {
		return "", false
	}
	return filepath.Join(dir, FileName), true
}

// DefaultProfilesPath returns the profiles path to write for cwd: project scope inside a repository,
// otherwise user scope.
func DefaultProfilesPath(cwd string) (string, error) {
	if p, ok := ProjectProfilesPath(cwd); ok {
		return p, nil
	}
	return UserProfilesPath()
}

// Discover returns the existing profiles file for cwd, preferring project scope, and whether one exists.
func Discover(cwd string) (string, bool) {
	if p, ok := ProjectProfilesPath(cwd); ok && fileExists(p) {
		return p, true
	}
	if p, err := UserProfilesPath(); err == nil && fileExists(p) {
		return p, true
	}
	return "", false
}

// DiscoverProfiles is an alias for Discover.
func DiscoverProfiles(cwd string) (string, bool) { return Discover(cwd) }

// CheckMisplaced returns an error if a profiles.yaml sits at the root of a `.aimesh/` directory, beside
// adapters.yaml, instead of in the explore component directory. Discovery never reads that location, so
// the file would otherwise be silently ignored.
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

// Resolve returns the profile set for a run in cwd with no explicit profile flags: the discovered file,
// or DefaultSet if there is none. A misplaced file is an error (see CheckMisplaced).
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
