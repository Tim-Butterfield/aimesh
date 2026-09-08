package rootfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// mkRoot builds a root directory containing docs/design.md and returns (root, absPath).
func mkRoot(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(root, "docs", "design.md")
	if err := os.WriteFile(abs, []byte("INTENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, abs
}

// The ordinary path still works: a real file under a real root reads its own bytes.
func TestRead_ReadsThroughRoot(t *testing.T) {
	root, abs := mkRoot(t)
	b, err := Read([]string{root}, abs)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(b) != "INTENT\n" {
		t.Errorf("content = %q, want %q", b, "INTENT\n")
	}
}

// THE A2/A3 THREAT. The confinement layer resolved a path and approved it; by the time the
// bytes are fetched, that path names a SYMLINK to a file outside the root. A path-string read
// (os.ReadFile on the resolved string) follows it and hands the outside content to the
// caller, which puts it in a model prompt. Reading through the root handle refuses it, on the
// object that was actually opened.
func TestRead_RefusesFinalComponentSwappedToSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs developer mode/elevation on Windows")
	}
	root, abs := mkRoot(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The swap: the approved path now names a symlink pointing outside the root.
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, abs); err != nil {
		t.Fatal(err)
	}
	// Sanity: this is exactly what the OLD path-string read would have returned.
	if b, err := os.ReadFile(abs); err != nil || string(b) != "SECRET" {
		t.Fatalf("precondition: a path-string read should yield the outside content, got %q / %v", b, err)
	}
	b, err := Read([]string{root}, abs)
	if err == nil {
		t.Fatalf("a swapped-in symlink must be refused, got content %q", b)
	}
	if got := workspace.ReasonOf(err); got != workspace.ReasonReparse {
		t.Errorf("reason = %q, want %q (err: %v)", got, workspace.ReasonReparse, err)
	}
	if strings.Contains(string(b), "SECRET") {
		t.Error("outside content must never be returned")
	}
}

// An INTERMEDIATE component swapped to a symlink that leaves the root is refused too: os.Root
// resolves every component against the open boundary and will not follow one out.
func TestRead_RefusesIntermediateComponentEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs developer mode/elevation on Windows")
	}
	root, abs := mkRoot(t)
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "design.md"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The swap: `docs` is now a symlink to a directory outside the root.
	if err := os.RemoveAll(filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(abs); err != nil || string(b) != "SECRET" {
		t.Fatalf("precondition: a path-string read should yield the outside content, got %q / %v", b, err)
	}
	if b, err := Read([]string{root}, abs); err == nil {
		t.Fatalf("an intermediate symlink out of the root must be refused, got %q", b)
	}
}

// The BOUNDARY itself can be swapped between the identity capture and the open. os.Root
// confines traversal inside whatever boundary it opened; it does not decide which one that
// is. The identity binding (os.SameFile on the opened handle) is what closes that window —
// and the hook is what lets the window be entered deterministically instead of raced.
func TestReadUnder_RefusesSwappedBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs developer mode/elevation on Windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	evil := filepath.Join(base, "evil")
	for _, d := range []string{real, evil} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(real, "f.md"), []byte("INTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, "f.md"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	boundary := filepath.Join(base, "root")
	if err := os.Symlink(real, boundary); err != nil {
		t.Fatal(err)
	}

	swapped := false
	testHookBeforeOpen = func(string) {
		if swapped {
			return
		}
		swapped = true
		if err := os.Remove(boundary); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(evil, boundary); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookBeforeOpen = nil })

	b, err := ReadUnder(boundary, "f.md")
	if err == nil {
		t.Fatalf("a boundary replaced between the check and the open must be refused, got %q", b)
	}
	if got := workspace.ReasonOf(err); got != workspace.ReasonRootChanged {
		t.Errorf("reason = %q, want %q (err: %v)", got, workspace.ReasonRootChanged, err)
	}
}

// A hardlinked regular file has more than one NAME. The denylist matches path COMPONENTS, so
// it cannot see the other name — an innocuous `docs/design.md` may be a second name for
// `~/.ssh/id_rsa`, and these bytes are headed for a prompt. Refused, with the link count.
func TestRead_RefusesHardlinkedFile(t *testing.T) {
	if _, ok := linkCount(mustStat(t, mkFile(t))); !ok {
		t.Skip("link count is not knowable on this platform")
	}
	root, abs := mkRoot(t)
	if err := os.Link(abs, filepath.Join(root, "docs", "other-name.md")); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}
	if _, err := Read([]string{root}, abs); err == nil {
		t.Fatal("a hardlinked file must not be read into a prompt")
	} else if got := workspace.ReasonOf(err); got != workspace.ReasonHardlink {
		t.Errorf("reason = %q, want %q (err: %v)", got, workspace.ReasonHardlink, err)
	}
}

// A directory (or any non-regular entry) where a document was expected is refused rather than
// producing a confusing platform error.
func TestRead_RefusesNonRegular(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "notafile")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Read([]string{root}, dir); err == nil {
		t.Fatal("a directory must not be read as a document")
	}
}

// The longest containing root wins: when a caller consented to both a tree and a subtree
// inside it, the NARROWER consent is the boundary the read is confined to.
func TestBoundary_PrefersLongestContainingRoot(t *testing.T) {
	root, abs := mkRoot(t)
	narrow := filepath.Join(root, "docs")
	dir, rel, err := boundary([]string{root, narrow}, abs)
	if err != nil {
		t.Fatal(err)
	}
	if dir != narrow || rel != "design.md" {
		t.Errorf("boundary = (%q, %q), want (%q, %q)", dir, rel, narrow, "design.md")
	}
}

// SINGLE-FILE CONSENT. A human may name exactly one path, and the scope resolver then records
// that FILE as a root — there is no consented directory above it. The boundary falls back to
// the file's own canonical parent, so the read is still handle-based rather than degrading to
// a path-string read.
func TestBoundary_FallsBackToParentForSingleFileConsent(t *testing.T) {
	_, abs := mkRoot(t)
	dir, rel, err := boundary([]string{abs}, abs)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Dir(abs) || rel != "design.md" {
		t.Errorf("boundary = (%q, %q), want (%q, %q)", dir, rel, filepath.Dir(abs), "design.md")
	}
	b, err := Read([]string{abs}, abs)
	if err != nil || string(b) != "INTENT\n" {
		t.Errorf("single-file consent must still read: %q / %v", b, err)
	}
}

// A relative path that climbs out of its root is refused before any open — the fail-closed
// answer, never a fallback to reading it by string.
func TestReadUnder_RefusesEscapingRelativePath(t *testing.T) {
	root, _ := mkRoot(t)
	for _, rel := range []string{"../outside.md", "/etc/passwd", ""} {
		if _, err := ReadUnder(root, rel); err == nil {
			t.Errorf("rel %q must be refused", rel)
		}
	}
}

func mkFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}
