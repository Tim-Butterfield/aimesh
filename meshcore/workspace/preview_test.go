package workspace

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// previewTree builds a live workspace exercising every decision the collector makes: ordinary
// files, an excluded directory, a read-denied file, a binary file, and a file over the per-file
// budget.
func previewTree(t *testing.T) string {
	t.Helper()
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.go"), "package a\n")
	writeFile(t, filepath.Join(live, "pkg", "b.go"), "package b\n")
	writeFile(t, filepath.Join(live, "README.md"), "# readme\n")
	writeFile(t, filepath.Join(live, ".git", "config"), "[core]\n")               // excluded family
	writeFile(t, filepath.Join(live, "node_modules", "dep.js"), "module.exports") // excluded family
	writeFile(t, filepath.Join(live, ".env"), "SECRET=1\n")                       // read-denied
	writeFile(t, filepath.Join(live, "blob.bin"), "head\x00tail")                 // binary
	writeFile(t, filepath.Join(live, "big.txt"), strings.Repeat("x", 20<<10))     // large: carried WHOLE
	return live
}

// TestPreviewPayload_MatchesWhatTheRunWouldCarry is the claim the dry-run disclosure rests on: the
// preview describes the ACTUAL prompt payload, not an approximation of it.
//
// It asserts against a real Copy + CollectSnippetsWithCaveats rather than against a hand-written
// expectation, because the risk being guarded is drift — the preview and the collector agreeing
// today and diverging the first time an exclusion rule changes. Comparing to an expectation would
// pass happily while both drifted, and a disclosure that quietly stops describing the run is worse
// than no disclosure at all.
func TestPreviewPayload_MatchesWhatTheRunWouldCarry(t *testing.T) {
	live := previewTree(t)

	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "t")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	defer ws.Cleanup(h)
	snips, cav, err := CollectSnippetsWithCaveats(h.Root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	p, err := PreviewPayload(live)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(snips) == 0 {
		t.Fatal("the fixture collected nothing — the comparison below would be vacuous")
	}
	want := make([]string, 0, len(snips))
	for _, s := range snips {
		want = append(want, fmt.Sprintf("%s %d", s.Path, len(s.Content)))
	}
	got := make([]string, 0, len(p.Files))
	for _, f := range p.SortedFiles() {
		got = append(got, fmt.Sprintf("%s %d", f.Path, f.Bytes))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("preview describes a different payload than the run would send:\n got:\n%s\nwant:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	total := 0
	for _, s := range snips {
		total += len(s.Content)
	}
	if p.TotalBytes != total {
		t.Errorf("preview TotalBytes = %d, collected = %d", p.TotalBytes, total)
	}
	// The real run's withheld list is the union of two stages (the copy withholds, then the
	// collector withholds); the preview is one walk, so it must equal that union.
	if len(h.Caveats)+len(cav) != len(p.Withheld) {
		t.Errorf("withheld: preview %d, run %d (copy %d + collect %d)",
			len(p.Withheld), len(h.Caveats)+len(cav), len(h.Caveats), len(cav))
	}
	// Spot-check the exclusions the fixture exists for, so a change that makes BOTH sides carry a
	// secret still fails here rather than agreeing quietly.
	for _, f := range p.Files {
		switch {
		case strings.HasPrefix(f.Path, ".git/"), strings.HasPrefix(f.Path, "node_modules/"):
			t.Errorf("preview carries an excluded path: %s", f.Path)
		case f.Path == ".env":
			t.Errorf("preview carries a read-denied path: %s", f.Path)
		case f.Path == "blob.bin":
			t.Errorf("preview carries a binary file: %s", f.Path)
		}
	}
}

// TestCollect_CarriesEveryFileWhole is the regression test for the coverage cap that used to live
// here: 50 files, 8 KiB per file, 64 KiB total, none of them justified anywhere. Pointed at a real
// repository those constants meant a review saw ten files in path order and never reached any
// source at all — findings it could not have made were indistinguishable from findings it did not
// find. aimesh cannot know a model's context limit and must not approximate one.
//
// The fixture's big.txt is 20 KiB, well past the old per-file clip, and must arrive whole.
func TestCollect_CarriesEveryFileWhole(t *testing.T) {
	live := previewTree(t)
	const bigSize = 20 << 10

	p, err := PreviewPayload(live)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	byPath := map[string]int{}
	for _, f := range p.Files {
		byPath[f.Path] = f.Bytes
	}
	if got := byPath["big.txt"]; got != bigSize {
		t.Errorf("big.txt carried %d bytes, want the whole %d — no file is clipped", got, bigSize)
	}

	// And the collector agrees, over an actual copy: same file, same length, content intact.
	ws := New(t.TempDir())
	h, cerr := ws.Copy(live, true, "t")
	if cerr != nil {
		t.Fatalf("copy: %v", cerr)
	}
	defer ws.Cleanup(h)
	snips, _, serr := CollectSnippetsWithCaveats(h.Root)
	if serr != nil {
		t.Fatalf("collect: %v", serr)
	}
	found := false
	for _, s := range snips {
		if s.Path == "big.txt" {
			found = true
			if len(s.Content) != bigSize {
				t.Errorf("collected big.txt is %d bytes, want %d", len(s.Content), bigSize)
			}
		}
	}
	if !found {
		t.Error("big.txt reached no prompt at all")
	}
}

// TestCollect_ManyFilesAllArrive: the old walk stopped at 50 files wherever in the tree that fell.
// A workspace of 120 files must yield 120 — "we ran out of budget" is not a reason a reviewer
// could be told about a defect it therefore never saw.
func TestCollect_ManyFilesAllArrive(t *testing.T) {
	live := t.TempDir()
	const n = 120
	for i := range n {
		writeFile(t, filepath.Join(live, fmt.Sprintf("f%03d.go", i)), fmt.Sprintf("package p // %d\n", i))
	}
	p, err := PreviewPayload(live)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Files) != n {
		t.Errorf("files = %d, want %d — the walk must not stop early", len(p.Files), n)
	}
}

// TestPreviewPayload_SingleFileWorkspaceIsThatFileAlone: Copy treats a file target as the whole
// subject of the run, so previewing its DIRECTORY would report files no reviewer would ever see —
// an over-statement in exactly the direction this feature exists to remove.
func TestPreviewPayload_SingleFileWorkspaceIsThatFileAlone(t *testing.T) {
	live := previewTree(t)

	p, err := PreviewPayload(filepath.Join(live, "a.go"))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Files) != 1 || p.Files[0].Path != "a.go" {
		t.Fatalf("files = %v, want exactly a.go", p.Files)
	}
	if p.TotalBytes != len("package a\n") {
		t.Errorf("bytes = %d, want %d", p.TotalBytes, len("package a\n"))
	}
}

// TestPreviewPayload_RefusesTheRootsTheCopyRefuses: the preview reads the live tree, so it answers
// the root question with the same rule the copy does. A preview that happily walked a `.env`
// directory would be a second, weaker path to the contents of one.
func TestPreviewPayload_RefusesTheRootsTheCopyRefuses(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, ".env")
	writeFile(t, filepath.Join(live, "production"), "SECRET=prod\n")

	if _, err := PreviewPayload(live); err == nil {
		t.Fatal("preview walked a read-denied root")
	} else if r, ok := AsRefusal(err); !ok || r.Reason != ReasonRootDenied {
		t.Errorf("refusal = %v, want %s", err, ReasonRootDenied)
	}
}

// TestPreviewPayload_WithholdsAHardlinkedFile pins that the preview reports the containment
// caveat rather than the file: a withheld file must not look like a file that never existed, and
// the disclosure is the only place a reader learns it was left out.
func TestPreviewPayload_WithholdsAHardlinkedFile(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(base, "secret")
	writeFile(t, secret, "SECRET\n")
	live := filepath.Join(base, "ws")
	writeFile(t, filepath.Join(live, "a.go"), "package a\n")
	hardlink(t, secret, filepath.Join(live, "innocuous.txt"))

	p, err := PreviewPayload(live)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	for _, f := range p.Files {
		if f.Path == "innocuous.txt" {
			t.Fatal("the preview carries a hardlinked file")
		}
	}
	if len(p.Withheld) != 1 || p.Withheld[0].Path != "innocuous.txt" {
		t.Fatalf("withheld = %v, want innocuous.txt", p.Withheld)
	}
	if p.Withheld[0].Reason != ReasonHardlink {
		t.Errorf("reason = %s, want %s", p.Withheld[0].Reason, ReasonHardlink)
	}
}
