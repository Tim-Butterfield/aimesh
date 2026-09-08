package authority

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// A2 — the authority read goes THROUGH A ROOT HANDLE, not through the resolved path string.
//
// The discriminating case is a HARDLINKED document. The scope resolver approves it (its
// component names are innocuous, and canonicalization cannot see a second name for the same
// inode), and a path-string `os.ReadFile` on the resolved path returns the bytes and embeds
// them in every judging phase's prompt. Reading through the handle validates the OPENED
// DESCRIPTOR, sees the link count, and refuses — which is what makes "the read is the check"
// true rather than aspirational. A regular file with two names may have its second name
// under `~/.ssh`, where the component denylist would have refused it outright.
func TestResolve_RefusesHardlinkedAuthorityDocument(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("link count is not knowable from a Win32FileAttributeData")
	}
	ws := t.TempDir()
	doc := writeFile(t, ws, "docs/design.md", "INTENT\n")
	if err := os.Link(doc, filepath.Join(ws, "docs", "second-name.md")); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}
	// Precondition: a path-string read WOULD have succeeded — this is the behavior being
	// replaced, not a file that was already unreadable.
	if b, err := os.ReadFile(doc); err != nil || string(b) != "INTENT\n" {
		t.Fatalf("precondition: %q / %v", b, err)
	}
	_, err := Resolve(Input{
		Workspace: ws, Mode: review.ModeReport,
		Docs: []review.AuthorityDoc{{Name: "design", Path: doc}},
	})
	if err == nil {
		t.Fatal("a hardlinked authority document must be refused, not embedded")
	}
	if got := fault.ReasonOf(err); got != string(workspace.ReasonHardlink) {
		t.Errorf("reasonCode = %q, want %q (err: %v)", got, workspace.ReasonHardlink, err)
	}
	var f *fault.Fault
	if !asFault(err, &f) || f.Halt != ScopeHaltClass {
		t.Errorf("a containment refusal must carry halt class %s, got %+v", ScopeHaltClass, f)
	}
}

// The ordinary path is untouched: a real document under a real root still resolves and is
// embedded verbatim. (Without this, the test above would also pass on a build that simply
// refused every authority document.)
func TestResolve_ReadsOrdinaryDocumentThroughTheHandle(t *testing.T) {
	ws := t.TempDir()
	doc := writeFile(t, ws, "docs/design.md", "INTENT\n")
	set, err := Resolve(Input{
		Workspace: ws, Mode: review.ModeReport,
		Docs: []review.AuthorityDoc{{Name: "design", Path: doc}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(set.All()) != 1 || set.All()[0].Text != "INTENT\n" {
		t.Fatalf("embedded text = %+v, want the document's own bytes", set.All())
	}
}

// An AGENT surface (trusted roots supplied out of band) reads through those roots. The read
// must stay confined to them even when the declared path resolves inside — the handle is
// opened on the trusted root, so a component that later leaves it cannot be followed.
func TestResolve_TrustedRootIsTheReadBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs developer mode/elevation on Windows")
	}
	trusted := t.TempDir()
	doc := writeFile(t, trusted, "docs/design.md", "INTENT\n")
	res, err := scope.New(trusted)
	if err != nil {
		t.Fatal(err)
	}
	set, rerr := Resolve(Input{
		Workspace: trusted, Mode: review.ModeReport, Trust: res,
		Docs: []review.AuthorityDoc{{Name: "design", Path: doc}},
	})
	if rerr != nil {
		t.Fatalf("Resolve: %v", rerr)
	}
	if set.All()[0].Text != "INTENT\n" {
		t.Fatalf("embedded text = %q", set.All()[0].Text)
	}

	// Now the approved path names a symlink pointing outside the trusted root — the state a
	// post-resolution swap produces. It must never yield the outside content.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(doc); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, doc); err != nil {
		t.Fatal(err)
	}
	set, rerr = Resolve(Input{
		Workspace: trusted, Mode: review.ModeReport, Trust: res,
		Docs: []review.AuthorityDoc{{Name: "design", Path: doc}},
	})
	if rerr == nil {
		t.Fatalf("a symlink out of the trusted root must be refused, got %+v", set.All())
	}
	for _, d := range set.All() {
		if strings.Contains(d.Text, "SECRET") {
			t.Fatal("outside content reached the authority set")
		}
	}
}

// asFault is errors.As for *fault.Fault without importing errors into every case above.
func asFault(err error, target **fault.Fault) bool {
	if f, ok := err.(*fault.Fault); ok {
		*target = f
		return true
	}
	return false
}
