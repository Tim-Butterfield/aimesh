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

// DefaultComponentDir (what the UI and a no-flag run both bind to) prefers the project scope inside a
// repo and the user scope outside one (§F10).
func TestDefaultComponentDir_ProjectInsideRepo_UserOutside(t *testing.T) {
	home := t.TempDir()
	t.Setenv(localstate.HomeEnvVar, home)

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := DefaultComponentDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(repo, localstate.HomeDirName, ComponentName); got != want {
		t.Errorf("DefaultComponentDir inside a repo = %q, want the project path %q", got, want)
	}

	nonRepo := t.TempDir() // a bare temp dir is not a repo
	got, err = DefaultComponentDir(nonRepo)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, localstate.HomeDirName, ComponentName); got != want {
		t.Errorf("DefaultComponentDir outside a repo = %q, want the user path %q", got, want)
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
	if _, err := DefaultComponentDir(repo); err != nil {
		t.Fatal(err)
	}

	for _, base := range []string{home, repo} {
		if _, err := os.Stat(filepath.Join(base, localstate.HomeDirName)); !os.IsNotExist(err) {
			t.Errorf("resolving a component dir created %s/%s", base, localstate.HomeDirName)
		}
	}
}

// A Save→Load round-trip through DefaultComponentDir proves the UI-written roster is the file a
// no---roster run picks up.
func TestSaveLoad_RoundTripThroughDefaultComponentDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv(localstate.HomeEnvVar, home)
	nonRepo := t.TempDir()

	r := Roster{
		Explorers: []Explorer{{Adapter: "fake", Model: "a"}, {Adapter: "fake", Model: "b"}},
		Collator:  Collator{Adapter: "fake", Model: "c"},
	}
	dir, err := DefaultComponentDir(nonRepo)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, FileName)
	if err := Save(target, r); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := Load(target)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Explorers) != 2 || loaded.Collator.Model != "c" {
		t.Errorf("round-trip mismatch: %+v", loaded)
	}
}
