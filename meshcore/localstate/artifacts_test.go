package localstate

// Where a component's run artifacts land. This is the rule BOTH apps resolve through, and the reason it
// is one function is a defect it exists to prevent: reviewmesh defaulted to `tmp/reviewmesh` resolved
// against the process cwd, so a run started anywhere inside a repository created that tree inside the
// repository — and run artifacts embed verbatim copies of everything the models were shown.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A cwd-relative default is the thing this must never produce, from any branch.
func TestRunDir_IsNeverCwdRelative(t *testing.T) {
	for _, tc := range []struct{ name, override string }{
		{"no override", ""},
		{"absolute override", filepath.Join(t.TempDir(), "pinned")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if got := RunDir("comp", tc.override); !filepath.IsAbs(got) {
				t.Errorf("RunDir = %q, want an absolute path that cannot drift with the cwd", got)
			}
		})
	}
}

// With a project state directory, runs land in the component's own subdirectory of it — ROOT-ANCHORED,
// so running from a subdirectory finds the same place, and under `runs/` so run output never mixes with
// the component's config in the same directory.
func TestRunDir_ProjectStateDirIsRootAnchored(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	mkdirAll(t, filepath.Join(root, HomeDirName))
	sub := filepath.Join(root, "services", "ingest")
	mkdirAll(t, sub)
	t.Chdir(sub)

	got := RunDir("comp", "")
	want := filepath.Join(root, HomeDirName, "comp", RunsSubdir)
	if resolve(got) != resolve(want) {
		t.Errorf("RunDir = %q, want %q", got, want)
	}
}

// With NO state directory the user never asked for anything project-local, so runs go outside their tree
// entirely — and resolving "where" must not CREATE anything.
func TestRunDir_FallsOutsideTheTreeAndCreatesNothing(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	t.Chdir(root)

	got := RunDir("comp", "")
	if within(got, root) {
		t.Errorf("RunDir = %q — the default must not write inside the user's tree %q", got, root)
	}
	if !within(got, os.TempDir()) {
		t.Errorf("RunDir = %q, want a subdirectory of the OS temp dir", got)
	}
	// Namespaced, so two components sharing one temp dir never interleave.
	if want := filepath.Join("aimesh", "comp", RunsSubdir); !strings.HasSuffix(got, want) {
		t.Errorf("RunDir = %q, want it to end in %q", got, want)
	}
	for _, unwanted := range []string{"tmp", HomeDirName, "comp"} {
		if _, err := os.Stat(filepath.Join(root, unwanted)); err == nil {
			t.Errorf("resolving the run dir created %q in the project; it answers where, not make", unwanted)
		}
	}
}

// An explicit override beats both branches — every surface resolves through here, so an override that
// did not win would leave runs somewhere the other surfaces cannot find.
func TestRunDir_OverrideWins(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, HomeDirName))
	t.Chdir(root)

	pinned := t.TempDir()
	if got := RunDir("comp", pinned); got != pinned {
		t.Errorf("RunDir = %q, want the override %q", got, pinned)
	}
}

// ComponentDir reports absence rather than inventing the directory: an absent `.aimesh/` means the user
// never ran `init`, and creating it here would be the unannounced write the whole rule avoids.
func TestComponentDir_AbsentStateDirIsNotOk(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	t.Chdir(root)

	if dir, ok := ComponentDir("comp"); ok {
		t.Errorf("ComponentDir = %q, ok=true; want ok=false with no state directory", dir)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func within(path, root string) bool {
	r, p := resolve(root), resolve(path)
	return p == r || strings.HasPrefix(p, r+string(filepath.Separator))
}

// resolve resolves symlinks on the longest existing prefix (macOS /var → /private/var); the tail of a
// run dir does not exist yet by design.
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
