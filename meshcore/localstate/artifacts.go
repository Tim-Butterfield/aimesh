package localstate

import (
	"os"
	"path/filepath"
)

// RunsSubdir is where a component's per-run output lands inside its own state subdirectory.
const RunsSubdir = "runs"

// ProjectHome reports the EXISTING project-local `.aimesh/` directory for the current working
// directory, if any. It is resolved ROOT-ANCHORED — FindRoot walks up to the VCS root, falling back to
// the cwd itself for a `folder init`ed non-repo tree — so the answer does not change with which
// subdirectory a command happens to run from.
//
// "Existing" is the whole test. An absent directory means the user never ran `init`, and inventing it
// here would be an unannounced write into their tree.
func ProjectHome() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	base := cwd
	if root, ok := FindRoot(cwd); ok {
		base = root
	}
	home := filepath.Join(base, HomeDirName)
	if fi, err := os.Stat(home); err == nil && fi.IsDir() {
		return home, true
	}
	return "", false
}

// ComponentDir is a component's own subdirectory of the EXISTING project state directory — where its
// config and its run output live, keyed by a caller-supplied name so two components sharing one
// `.aimesh/` never interleave. Returns ok=false when no project state directory exists.
//
// Contrast ProjectComponentPath, which answers the same question for a WRITE TARGET.
func ComponentDir(name string) (string, bool) {
	home, ok := ProjectHome()
	if !ok {
		return "", false
	}
	return filepath.Join(home, name), true
}

// HomeEnvVar overrides the base directory containing the user-scope `.aimesh/`. It is the ONE home
// override for the whole tool: each component used to carry its own (REVIEWMESH_HOME, EXPLOREMESH_HOME)
// back when they were separate applications with separate state directories, and three variables that
// had to be set together to get one hermetic run was a trap rather than a feature.
const HomeEnvVar = "AIMESH_HOME"

// UserHomeBase returns the BASE directory that contains the user-scope `.aimesh/` (so the state
// directory is at <base>/.aimesh). AIMESH_HOME overrides it — tests set it so they never touch a real
// home directory; otherwise the OS user home.
func UserHomeBase() (string, error) {
	if h := os.Getenv(HomeEnvVar); h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}

// UserComponentDir is a component's user-scope directory: <UserHomeBase>/.aimesh/<name>. It is a WRITE
// TARGET — it need not exist yet.
func UserComponentDir(name string) (string, error) {
	base, err := UserHomeBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, HomeDirName, name), nil
}

// ProjectComponentPath is a component's project-scope directory, ROOT-ANCHORED: it walks up from cwd to
// the VCS root so a run from a subdirectory sees the repo-wide state, exactly as project-scope adapter
// config resolves. ok is false when cwd is not inside a repo (no project scope applies).
//
// Unlike ComponentDir this does NOT require the directory to exist — it is the write target too.
func ProjectComponentPath(cwd, name string) (string, bool) {
	root, ok := FindRoot(cwd)
	if !ok {
		return "", false
	}
	return filepath.Join(root, HomeDirName, name), true
}

// RunDir resolves the base directory a component's run artifacts are written under.
//
// It is NEVER a relative path resolved against the process cwd. A run started anywhere inside a user's
// repository would create that tree INSIDE the repository, in tools otherwise documented as leaving the
// user's files alone — a surprise write, and one whose contents are not innocuous: run artifacts embed
// verbatim copies of everything the models were shown. The default is chosen so there is no surprise:
//
//  1. `<project .aimesh>/<name>/runs` WHEN the state directory ALREADY EXISTS. That is what `init` /
//     `repo init` creates, and `repo init` has already registered it with the VCS exclude file, so
//     writing there cannot dirty a checkout.
//  2. otherwise `<os temp>/aimesh/<name>/runs`: outside the user's tree entirely, self-cleaning, and
//     never an unexplained new directory in a checkout.
//
// An explicit override (an env value or a flag, resolved by the caller) wins over both.
//
// The directory is deliberately NOT created here — this answers "where"; the caller's own MkdirAll
// answers "make it".
func RunDir(name, override string) string {
	if override != "" {
		return override
	}
	if dir, ok := ComponentDir(name); ok {
		return filepath.Join(dir, RunsSubdir)
	}
	return filepath.Join(os.TempDir(), HomeDirName[1:], name, RunsSubdir)
}
