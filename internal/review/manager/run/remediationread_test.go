package run

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// A3 — the remediation read goes through a handle on the COPY ROOT, not through a joined
// path string.
//
// readBounded's bytes become the file content the remediation model is shown and asked to
// anchor edits into. The old form took `rcopy.Abs(f.File)` and called os.ReadFile on it,
// which follows whatever that name points at NOW — so an entry swapped for a symlink inside
// the copy, after the shown-file checks, is read straight out of the copy. That the copy is
// ours makes the window narrow, not absent.
func TestReadBounded_RefusesSymlinkInsideTheCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs developer mode/elevation on Windows")
	}
	copyRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(copyRoot, "main.go")
	if err := os.Symlink(outside, planted); err != nil {
		t.Fatal(err)
	}
	// Precondition: the OLD path-string form would have returned the outside content.
	if b, err := os.ReadFile(planted); err != nil || string(b) != "SECRET" {
		t.Fatalf("precondition: a path-string read should yield the outside content, got %q / %v", b, err)
	}
	content, _, err := readBounded(copyRoot, "main.go")
	if err == nil {
		t.Fatalf("a symlink inside the copy must be refused, got %q", content)
	}
	if got := workspace.ReasonOf(err); got != workspace.ReasonReparse {
		t.Errorf("reason = %q, want %q (err: %v)", got, workspace.ReasonReparse, err)
	}
	if strings.Contains(content, "SECRET") {
		t.Error("outside content must never reach the remediation prompt")
	}
}

// An escaping relative path is refused before any open, rather than being joined and read.
func TestReadBounded_RefusesEscapingPath(t *testing.T) {
	copyRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(filepath.Dir(copyRoot), "outside.go"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if content, _, err := readBounded(copyRoot, filepath.Join("..", "outside.go")); err == nil {
		t.Fatalf("an escaping path must be refused, got %q", content)
	}
}

// The ordinary path still works, and the truncation contract is unchanged.
func TestReadBounded_ReadsAndTruncates(t *testing.T) {
	copyRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(copyRoot, "small.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, truncated, err := readBounded(copyRoot, "small.go")
	if err != nil || truncated || content != "package main\n" {
		t.Fatalf("readBounded = (%q, %v, %v)", content, truncated, err)
	}
	big := strings.Repeat("x", remediationMaxBytes+10)
	if err := os.WriteFile(filepath.Join(copyRoot, "big.go"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	content, truncated, err = readBounded(copyRoot, "big.go")
	if err != nil || !truncated || len(content) != remediationMaxBytes {
		t.Fatalf("readBounded(big) = (%d bytes, %v, %v), want %d bytes truncated", len(content), truncated, err, remediationMaxBytes)
	}
}

// A7 — a withheld file is deduped but never dropped: the same file is withheld again on every
// retry, every seat and every cycle, and the run record must say it once, not N times.
func TestAppendWithheld_DedupesButKeepsDistinctFacts(t *testing.T) {
	a := review.WithheldFile{Path: "a.go", Reason: "workspace_hardlink_denied", Stage: stageCopy}
	b := review.WithheldFile{Path: "b.go", Reason: "workspace_hardlink_denied", Stage: stageCopy}
	sameFileOtherStage := review.WithheldFile{Path: "a.go", Reason: "workspace_hardlink_denied", Stage: stageSnippets}
	got := appendWithheld(nil, a, a, b, a, sameFileOtherStage)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (a@copy, b@copy, a@snippets): %+v", len(got), got)
	}
	if got[0] != a || got[1] != b || got[2] != sameFileOtherStage {
		t.Errorf("order/content = %+v", got)
	}
}

// withheldFrom carries meshcore's typed caveat through verbatim — the reason code a consumer
// branches on is meshcore's own, never re-derived here.
func TestWithheldFrom_CarriesTheMeshcoreReasonVerbatim(t *testing.T) {
	got := withheldFrom(stageCopy, []workspace.Caveat{{
		Path: "vendor/x.go", Reason: workspace.ReasonHardlink, Rule: "the rule", Detail: "link count 2",
	}})
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	want := review.WithheldFile{
		Path: "vendor/x.go", Reason: string(workspace.ReasonHardlink),
		Rule: "the rule", Detail: "link count 2", Stage: stageCopy,
	}
	if got[0] != want {
		t.Errorf("withheldFrom = %+v, want %+v", got[0], want)
	}
}
