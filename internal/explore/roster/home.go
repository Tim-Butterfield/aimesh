package roster

import (
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// Explore's persisted state lives in its own component directory under the SHARED `.aimesh/` state
// root: `.aimesh/explore/`. It used to sit in an app-private `~/.exploremesh/` with its own
// EXPLOREMESH_HOME override, from when explore was a separate application; both are gone. One state
// root with a component subdirectory per domain means one `init`, one VCS exclusion, and one home
// override (localstate.HomeEnvVar) instead of three that had to agree.
//
// The UI writes here and a no---roster `explore`/`acp` run reads the SAME resolution, so the workbench
// and a run bind to one file (design §F10).
const (
	// ComponentName is explore's subdirectory of the shared `.aimesh/` state root.
	ComponentName = "explore"
	// FileName is the roster file's name. Retained for an EXPLICIT `--roster <path>`, which is still a
	// supported per-invocation input; it is no longer a discovered location (see profile.Discover).
	FileName = "roster.yaml"
)

// HomeDir is the base directory containing the user-scope `.aimesh/`. AIMESH_HOME overrides the OS home
// directory (tests set it so they never touch the real home).
func HomeDir() (string, error) { return localstate.UserHomeBase() }

// UserComponentDir is the user-scope `.aimesh/explore/` directory. It is a write TARGET — the directory
// need not exist yet.
func UserComponentDir() (string, error) { return localstate.UserComponentDir(ComponentName) }

// ProjectComponentDir is the project-scope `.aimesh/explore/` directory, ROOT-ANCHORED: it walks up from
// cwd to the VCS root so a run from a subdirectory sees the repo-wide state. ok is false when cwd is not
// inside a repo (no project scope applies).
func ProjectComponentDir(cwd string) (string, bool) {
	return localstate.ProjectComponentPath(cwd, ComponentName)
}

// DefaultComponentDir resolves the directory the workbench (and a no-flag run) binds to: project scope
// when cwd is inside a repo, else user scope. This single shared default is what makes a UI save and a
// subsequent no-flag run target the SAME files (§F10).
func DefaultComponentDir(cwd string) (string, error) {
	if p, ok := ProjectComponentDir(cwd); ok {
		return p, nil
	}
	return UserComponentDir()
}
