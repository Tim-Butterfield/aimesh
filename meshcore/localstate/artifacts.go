package localstate

import (
	"os"
	"path/filepath"
)

// RunsSubdir is where a component's per-run output lands inside its own state subdirectory.
const RunsSubdir = "runs"

// ProjectHome reports the existing project `.aimesh/` directory for the working directory, if any. It is
// resolved from the VCS root (FindRoot), falling back to the directory itself for a folder-initialized
// tree, so the answer does not depend on the subdirectory. A missing directory is never created.
func ProjectHome() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	return ProjectHomeFor(cwd)
}

// ProjectHomeFor is ProjectHome for an explicit directory rather than the process working directory.
// A server that serves many workspaces resolves each call's state from that call's own workspace,
// never from wherever the process happened to start.
func ProjectHomeFor(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	base := dir
	if root, ok := FindRoot(dir); ok {
		base = root
	}
	home := filepath.Join(base, HomeDirName)
	if fi, err := os.Stat(home); err == nil && fi.IsDir() {
		return home, true
	}
	return "", false
}

// ComponentDir is a component's subdirectory of the existing project state directory, where its
// configuration and run output live. ok is false when no project state directory exists. See
// ProjectComponentPath for the write target.
func ComponentDir(name string) (string, bool) {
	home, ok := ProjectHome()
	if !ok {
		return "", false
	}
	return filepath.Join(home, name), true
}

// ComponentDirFor is ComponentDir for an explicit directory (see ProjectHomeFor).
func ComponentDirFor(dir, name string) (string, bool) {
	home, ok := ProjectHomeFor(dir)
	if !ok {
		return "", false
	}
	return filepath.Join(home, name), true
}

// HomeEnvVar overrides the base directory containing the user-scope `.aimesh/`. It is the one home
// override for the whole tool, so a single variable makes a run hermetic.
const HomeEnvVar = "AIMESH_HOME"

// UserHomeBase returns the base directory containing the user-scope `.aimesh/`: AIMESH_HOME when set,
// otherwise the OS user home.
func UserHomeBase() (string, error) {
	if h := os.Getenv(HomeEnvVar); h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}

// UserComponentDir is a component's user-scope directory, <UserHomeBase>/.aimesh/<name>. It need not
// exist yet.
func UserComponentDir(name string) (string, error) {
	base, err := UserHomeBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, HomeDirName, name), nil
}

// ProjectComponentPath is a component's project-scope directory, found by walking up from cwd to the VCS
// root. ok is false when cwd is not inside a repository. Unlike ComponentDir, the directory need not
// exist.
func ProjectComponentPath(cwd, name string) (string, bool) {
	root, ok := FindRoot(cwd)
	if !ok {
		return "", false
	}
	return filepath.Join(root, HomeDirName, name), true
}

// RunDir resolves the base directory a component's run artifacts are written under. It is never
// relative to the process cwd, since run artifacts embed copies of everything the models were shown:
//
//  1. `<project .aimesh>/<name>/runs` when that state directory already exists (created by `init`,
//     which also excludes it from version control);
//  2. otherwise `<os temp>/aimesh/<name>/runs`.
//
// An explicit override wins over both. The directory is not created here.
func RunDir(name, override string) string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	return RunDirFor(cwd, name, override)
}

// RunDirFor is RunDir for an explicit directory: `<dir's project .aimesh>/<name>/runs` when that state
// directory already exists, else the OS temp location. It never creates `.aimesh/`.
func RunDirFor(dir, name, override string) string {
	if override != "" {
		return override
	}
	if cdir, ok := ComponentDirFor(dir, name); ok {
		return filepath.Join(cdir, RunsSubdir)
	}
	return TempRunDir(name)
}

// RunDirIn is RunDirFor without the walk to a repository root: `<dir>/.aimesh/<name>/runs` only when
// `<dir>/.aimesh` itself exists, else the temp run directory (override wins). A server whose caller
// declares dir as its scope uses it, so a run record never lands in a parent directory's state
// directory outside that scope. It creates nothing.
func RunDirIn(dir, name, override string) string {
	if override != "" {
		return override
	}
	if dir != "" {
		home := filepath.Join(dir, HomeDirName)
		if fi, err := os.Stat(home); err == nil && fi.IsDir() {
			return filepath.Join(home, name, RunsSubdir)
		}
	}
	return TempRunDir(name)
}

// TempRunDir is the run directory used when no project state directory applies.
func TempRunDir(name string) string {
	return filepath.Join(os.TempDir(), HomeDirName[1:], name, RunsSubdir)
}
