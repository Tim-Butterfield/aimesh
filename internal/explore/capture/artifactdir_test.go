package capture

// Tests for where a captured run LANDS. The default is never a relative path resolved against the
// process cwd (such as `tmp/exploremesh`): running `explore --dump-run` anywhere inside a user's
// repository would create that tree inside the repository. These pin the rule: `<project
// .aimesh>/explore/runs` when the user has actually made a state directory, a namespaced subdirectory
// of the OS temp directory otherwise, and $EXPLOREMESH_ARTIFACT_DIR over both.
//
// The rule itself lives in meshcore/localstate.RunDir — reviewmesh resolves its runs the same way. These
// tests cover exploremesh's binding to it; localstate has its own.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// TestArtifactDir_DefaultsToTheProjectStateDirWhenOneExists: `init` / `repo init` created `.aimesh/` at the
// VCS root and registered it with the exclude file, so it is the one project-local place a captured run
// can go without dirtying a checkout. Resolution is ROOT-ANCHORED — running from a subdirectory finds the
// same directory, exactly as project-scope adapter config does.
func TestArtifactDir_DefaultsToTheProjectStateDirWhenOneExists(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aimesh"), 0o755); err != nil {
		t.Fatalf("mkdir .aimesh: %v", err)
	}
	sub := filepath.Join(root, "services", "ingest")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", "")
	t.Chdir(sub)

	got := ArtifactDir()
	want := filepath.Join(root, ".aimesh", ArtifactSubdir, localstate.RunsSubdir)
	if !samePath(t, got, want) {
		t.Errorf("ArtifactDir() = %q, want the project state dir %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ArtifactDir() must be absolute so it cannot drift with the cwd, got %q", got)
	}
}

// TestArtifactDir_FallsBackToTheOSTempDir: with no `.aimesh/` the user has not asked for anything
// project-local, so a captured run goes OUTSIDE their tree entirely rather than creating a directory in it.
// The old default wrote `tmp/exploremesh` into the checkout here.
func TestArtifactDir_FallsBackToTheOSTempDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", "")
	t.Chdir(root)

	got := ArtifactDir()
	if !filepath.IsAbs(got) {
		t.Fatalf("ArtifactDir() must be absolute, got %q", got)
	}
	if within(t, got, root) {
		t.Errorf("ArtifactDir() = %q — the default must NOT write inside the user's tree %q", got, root)
	}
	if !within(t, got, os.TempDir()) {
		t.Errorf("ArtifactDir() = %q, want a subdirectory of the OS temp dir %q", got, os.TempDir())
	}
	// Namespaced by app under the OS temp dir, so two aimesh components never interleave their runs.
	if want := filepath.Join("aimesh", ArtifactSubdir, localstate.RunsSubdir); !strings.HasSuffix(got, want) {
		t.Errorf("ArtifactDir() = %q, want it to end in %q", got, want)
	}
	// Answering "where" must not CREATE anything: a user who never captures a run gets no directories.
	if _, err := os.Stat(filepath.Join(root, "tmp")); err == nil {
		t.Error("resolving the artifact dir created a tmp/ directory in the project")
	}
}

// TestArtifactDir_FolderInitOutsideARepo: `folder init` creates `.aimesh/` in a plain directory where
// there is no VCS root to anchor to, and that directory is still the right answer.
func TestArtifactDir_FolderInitOutsideARepo(t *testing.T) {
	// A directory with no .git anywhere above it. t.TempDir() is under the OS temp dir, which is not a
	// repository, so FindRoot legitimately finds nothing.
	root := t.TempDir()
	if _, ok := localstate.FindRoot(root); ok {
		t.Skip("the OS temp dir is inside a repository on this machine, so the non-repo case cannot be exercised here")
	}
	if err := os.MkdirAll(filepath.Join(root, ".aimesh"), 0o755); err != nil {
		t.Fatalf("mkdir .aimesh: %v", err)
	}
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", "")
	t.Chdir(root)

	got := ArtifactDir()
	want := filepath.Join(root, ".aimesh", ArtifactSubdir, localstate.RunsSubdir)
	if !samePath(t, got, want) {
		t.Errorf("ArtifactDir() = %q, want %q", got, want)
	}
}

// TestArtifactDir_EnvStillWins: $EXPLOREMESH_ARTIFACT_DIR overrides both branches, unchanged. Every
// surface (CLI --dump-run, ACP dumpRun, the MCP job registry) goes through this one function, so the
// override has to keep beating the default or the surfaces would disagree about where a run lives.
func TestArtifactDir_EnvStillWins(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".aimesh"), 0o755); err != nil {
		t.Fatalf("mkdir .aimesh: %v", err)
	}
	pinned := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", pinned)
	t.Chdir(root)

	if got := ArtifactDir(); got != pinned {
		t.Errorf("ArtifactDir() = %q, want the env override %q", got, pinned)
	}
}

// samePath compares two paths after resolving symlinks on the parts that exist (macOS /var → /private/var).
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	return resolve(a) == resolve(b)
}

func within(t *testing.T, path, root string) bool {
	t.Helper()
	r := resolve(root)
	p := resolve(path)
	return p == r || strings.HasPrefix(p, r+string(filepath.Separator))
}

// resolve resolves symlinks on the longest existing prefix of path (the tail may not exist yet).
func resolve(path string) string {
	dir, rest := path, ""
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(path)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}
