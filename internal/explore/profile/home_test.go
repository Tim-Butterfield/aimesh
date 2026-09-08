package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// componentDir is where explore's config lives under a given base: <base>/.aimesh/explore.
func componentDir(base string) string {
	return filepath.Join(base, localstate.HomeDirName, roster.ComponentName)
}

// isolate points AIMESH_HOME at a temp dir so no test reads or writes the developer's real state, and
// returns that home plus a NON-repo temp cwd (project-scope resolution walks up to a VCS root, which
// would otherwise anchor at this repo).
func isolate(t *testing.T) (home, cwd string) {
	t.Helper()
	home, cwd = t.TempDir(), t.TempDir()
	t.Setenv(localstate.HomeEnvVar, home)
	return home, cwd
}

// repoAt makes dir look like a repo root so project-scope resolution anchors there.
func repoAt(t *testing.T, dir string) string {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUserProfilesPath_UnderSharedStateRoot(t *testing.T) {
	home, _ := isolate(t)
	got, err := UserProfilesPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(componentDir(home), FileName); got != want {
		t.Errorf("UserProfilesPath = %q, want %q", got, want)
	}
}

func TestProjectProfilesPath_RootAnchored(t *testing.T) {
	root := repoAt(t, t.TempDir())
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := ProjectProfilesPath(sub)
	if !ok {
		t.Fatal("a cwd inside a repo must have a project scope")
	}
	if want := filepath.Join(componentDir(root), FileName); got != want {
		t.Errorf("ProjectProfilesPath(subdir) = %q, want the repo-root path %q", got, want)
	}
	// Outside a repo there is no project scope.
	if _, ok := ProjectProfilesPath(t.TempDir()); ok {
		t.Error("a non-repo cwd must have no project scope")
	}
}

// DefaultProfilesPath (the write target the workbench + a no-flag run share) prefers the project scope
// inside a repo and the user scope outside one — the SAME resolution the roster uses (§F10).
func TestDefaultProfilesPath_ProjectInsideRepo_UserOutside(t *testing.T) {
	home, nonRepo := isolate(t)
	repo := repoAt(t, t.TempDir())

	got, err := DefaultProfilesPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(componentDir(repo), FileName); got != want {
		t.Errorf("DefaultProfilesPath inside a repo = %q, want the project path %q", got, want)
	}
	got, err = DefaultProfilesPath(nonRepo)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(componentDir(home), FileName); got != want {
		t.Errorf("DefaultProfilesPath outside a repo = %q, want the user path %q", got, want)
	}
}

// TestDiscover_Precedence walks the resolution order: nothing persisted → not found; a user
// profiles.yaml is found; a PROJECT profiles.yaml outranks the user one.
func TestDiscover_Precedence(t *testing.T) {
	home, _ := isolate(t)
	repo := repoAt(t, t.TempDir())

	// (a) nothing persisted anywhere → callers fall back to the built-in DefaultSet.
	if p, ok := Discover(repo); ok {
		t.Fatalf("expected no persisted config, got %q", p)
	}

	// (b) a user profiles.yaml is discovered.
	userProfiles := filepath.Join(componentDir(home), FileName)
	if err := Save(userProfiles, oneProfileSet()); err != nil {
		t.Fatal(err)
	}
	p, ok := Discover(repo)
	if !ok || p != userProfiles {
		t.Fatalf("Discover = (%q, ok=%v), want the user profiles.yaml", p, ok)
	}

	// (c) a PROJECT profiles.yaml outranks the user one (project scope wins).
	projProfiles := filepath.Join(componentDir(repo), FileName)
	if err := Save(projProfiles, oneProfileSet()); err != nil {
		t.Fatal(err)
	}
	p, ok = Discover(repo)
	if !ok || p != projProfiles {
		t.Fatalf("Discover = (%q, ok=%v), want the project profiles.yaml", p, ok)
	}
}

// A roster.yaml sitting beside profiles.yaml is NOT a discovered config. Discovery answers exactly one
// question — "which profiles.yaml is this run bound to?" — and a second answer-bearing file in the same
// directory would make that unanswerable without reading the code. An explicit --roster still works;
// that is a per-invocation input the caller resolves, not a location we search.
func TestDiscover_IgnoresARosterFileBesideIt(t *testing.T) {
	home, cwd := isolate(t)
	if err := roster.Save(filepath.Join(componentDir(home), roster.FileName), twoValid()); err != nil {
		t.Fatal(err)
	}
	if p, ok := Discover(cwd); ok {
		t.Fatalf("a roster.yaml must not be discovered as config, got %q", p)
	}
	target := filepath.Join(componentDir(home), FileName)
	if err := Save(target, oneProfileSet()); err != nil {
		t.Fatal(err)
	}
	p, ok := Discover(cwd)
	if !ok || p != target {
		t.Fatalf("Discover = (%q, %v), want the saved profiles.yaml", p, ok)
	}
}

// TestResolve_FallsBackToDefaultSet: with nothing persisted, resolution binds to the shipped
// UNCONFIGURED default — an empty profile that exists (so the workbench can open on it and configure
// it) but cannot run. Nothing, not even the deterministic fake adapter, ships pre-selected.
func TestResolve_FallsBackToDefaultSet(t *testing.T) {
	_, cwd := isolate(t)
	got, err := Resolve(cwd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.DefaultProfile != DefaultProfileName || len(got.Profiles) != 1 {
		t.Fatalf("Resolve with nothing persisted = %+v, want the built-in DefaultSet", got)
	}
	p, _ := got.Get(DefaultProfileName)
	if p.IsConfigured() {
		t.Errorf("the shipped default must be UNCONFIGURED (no explorers, no collator), got %+v", p)
	}
}

// TestResolve_PersistedSet: a persisted profiles.yaml resolves to the saved multi-profile set — the
// SAME resolution a no-flag run binds to.
func TestResolve_PersistedSet(t *testing.T) {
	home, cwd := isolate(t)

	set := oneProfileSet()
	set.Profiles["wide"] = Profile{Explorers: twoValid().Explorers, Collator: twoValid().Collator, DefaultMode: "catalog"}
	if err := Save(filepath.Join(componentDir(home), FileName), set); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(cwd)
	if err != nil {
		t.Fatalf("Resolve profiles: %v", err)
	}
	if len(got.Profiles) != 2 {
		t.Fatalf("Resolve should see both profiles, got %v", got.Names())
	}
	if w, _ := got.Get("wide"); w.DefaultMode != "catalog" {
		t.Errorf("resolved profile lost its defaultMode: %+v", w)
	}
}

// A profiles.yaml at the ROOT of the state directory is REFUSED by name. Discovery does not look
// there, so without this the file sits ignored while the run binds to the shipped unconfigured
// default — and the resulting "profile is unconfigured" error says nothing about the file the user
// just wrote. The refusal must name the offending path AND both valid destinations.
func TestCheckMisplaced_RefusesAProfilesFileAtTheStateRoot(t *testing.T) {
	home, _ := isolate(t)
	repo := repoAt(t, t.TempDir())

	// Nothing misplaced → no error, at either scope.
	if err := CheckMisplaced(repo); err != nil {
		t.Fatalf("a clean tree must not error: %v", err)
	}

	for _, tc := range []struct{ name, base string }{
		{"project scope", repo},
		{"user scope", home},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := filepath.Join(tc.base, localstate.HomeDirName, FileName)
			if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bad, []byte("schemaVersion: 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(bad) })

			err := CheckMisplaced(repo)
			if err == nil {
				t.Fatal("a profiles.yaml at the state root must be refused, not ignored")
			}
			msg := err.Error()
			if !strings.Contains(msg, bad) {
				t.Errorf("the refusal must name the offending file; got %q", msg)
			}
			if !strings.Contains(msg, filepath.Join(componentDir(repo), FileName)) {
				t.Errorf("the refusal must name the project-scope destination; got %q", msg)
			}
			if !strings.Contains(msg, filepath.Join(componentDir(home), FileName)) {
				t.Errorf("the refusal must name the user-scope destination; got %q", msg)
			}
			// Resolve refuses too — the run path must never bind past a misplaced config.
			if _, rerr := Resolve(repo); rerr == nil {
				t.Error("Resolve must surface the same refusal")
			}
		})
	}
}

// TestResolve_SurfacesAMalformedFile: a present-but-broken profiles.yaml is an ERROR, never a silent
// fallback to the demo set (that would run a panel the user did not configure).
func TestResolve_SurfacesAMalformedFile(t *testing.T) {
	home, cwd := isolate(t)
	target := filepath.Join(componentDir(home), FileName)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("defaultProfile: nope\nprofiles: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(cwd); err == nil {
		t.Fatal("a persisted but invalid profiles.yaml must surface as an error")
	}
}
