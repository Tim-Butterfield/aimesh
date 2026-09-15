package roster

import (
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// Explore's persisted state lives in `.aimesh/explore/`, a component directory under the shared `.aimesh/`
// state root, so one `init`, one VCS exclusion and one home override (localstate.HomeEnvVar) serve every
// domain. Configuration writes and runs resolve the same directory.
const (
	// ComponentName is explore's subdirectory of the shared `.aimesh/` state root.
	ComponentName = "explore"
	// FileName is the default name of a roster file passed with `--roster <path>`. Roster files are never
	// discovered (see profile.Discover).
	FileName = "roster.yaml"
)

// HomeDir returns the base directory that contains the user-scope `.aimesh/`. AIMESH_HOME overrides the OS
// home directory.
func HomeDir() (string, error) { return localstate.UserHomeBase() }

// UserComponentDir returns the user-scope `.aimesh/explore/` directory, which need not exist.
func UserComponentDir() (string, error) { return localstate.UserComponentDir(ComponentName) }

// ProjectComponentDir returns the project-scope `.aimesh/explore/` directory at the VCS root above cwd. It
// returns false when cwd is not inside a repository.
func ProjectComponentDir(cwd string) (string, bool) {
	return localstate.ProjectComponentPath(cwd, ComponentName)
}
