package roster

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

func TestHomeDir_HonorsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(localstate.HomeEnvVar, dir)
	got, err := HomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("HomeDir = %q, want the %s override %q", got, localstate.HomeEnvVar, dir)
	}
}

// Explore's state is a COMPONENT of the shared `.aimesh/` root, not an app-private home of its own —
// the invariant that lets one `init` and one VCS exclusion cover every domain.
func TestUserComponentDir_UnderSharedStateRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(localstate.HomeEnvVar, dir)
	got, err := UserComponentDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, localstate.HomeDirName, ComponentName); got != want {
		t.Errorf("UserComponentDir = %q, want %q", got, want)
	}
}

func TestProjectComponentDir_RootAnchored(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := ProjectComponentDir(sub)
	if !ok {
		t.Fatal("a cwd inside a repo must have a project scope")
	}
	if want := filepath.Join(root, localstate.HomeDirName, ComponentName); got != want {
		t.Errorf("ProjectComponentDir(subdir) = %q, want the repo-root path %q", got, want)
	}
	// Outside a repo there is no project scope.
	if _, ok := ProjectComponentDir(t.TempDir()); ok {
		t.Error("a non-repo cwd must have no project scope")
	}
}

// Resolving a directory must never CREATE one: these helpers answer "where", and a caller's MkdirAll
// answers "make it". A resolver that materialized directories would litter a tree on every read.
func TestComponentDirResolution_CreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv(localstate.HomeEnvVar, home)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := UserComponentDir(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ProjectComponentDir(repo); !ok {
		t.Fatal("expected a project scope")
	}

	for _, base := range []string{home, repo} {
		if _, err := os.Stat(filepath.Join(base, localstate.HomeDirName)); !os.IsNotExist(err) {
			t.Errorf("resolving a component dir created %s/%s", base, localstate.HomeDirName)
		}
	}
}
