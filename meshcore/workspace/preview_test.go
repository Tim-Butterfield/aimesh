package workspace

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// previewTree builds a live workspace exercising every decision the collector makes: ordinary
// files, excluded directories, a read-denied file, a binary file and a large file.
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

// The preview describes the payload a run actually sends. It is compared with a real Copy and
// CollectSnippetsWithCaveats, not a fixed expectation, so the two cannot drift apart unnoticed.
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

	p, err := PreviewPayloadWith(live, false)
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

// Collection never clips a file: the fixture's 20 KiB big.txt arrives whole in both the preview and
// the collected snippets.
func TestCollect_CarriesEveryFileWhole(t *testing.T) {
	live := previewTree(t)
	const bigSize = 20 << 10

	p, err := PreviewPayloadWith(live, false)
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

// Collection has no file-count cap: a workspace of 120 files yields 120.
func TestCollect_ManyFilesAllArrive(t *testing.T) {
	live := t.TempDir()
	const n = 120
	for i := range n {
		writeFile(t, filepath.Join(live, fmt.Sprintf("f%03d.go", i)), fmt.Sprintf("package p // %d\n", i))
	}
	p, err := PreviewPayloadWith(live, false)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Files) != n {
		t.Errorf("files = %d, want %d — the walk must not stop early", len(p.Files), n)
	}
}

// A single-file workspace is previewed as that file alone, as Copy copies only it.
func TestPreviewPayload_SingleFileWorkspaceIsThatFileAlone(t *testing.T) {
	live := previewTree(t)

	p, err := PreviewPayloadWith(filepath.Join(live, "a.go"), false)
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

// The preview reads the live tree, so it refuses the roots Copy refuses, such as a `.env` directory.
func TestPreviewPayload_RefusesTheRootsTheCopyRefuses(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, ".env")
	writeFile(t, filepath.Join(live, "production"), "SECRET=prod\n")

	if _, err := PreviewPayloadWith(live, false); err == nil {
		t.Fatal("preview walked a read-denied root")
	} else if r, ok := AsRefusal(err); !ok || r.Reason != ReasonRootDenied {
		t.Errorf("refusal = %v, want %s", err, ReasonRootDenied)
	}
}

// The preview withholds a hardlinked file and reports it as a caveat, so the omission is visible.
func TestPreviewPayload_WithholdsAHardlinkedFile(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(base, "secret")
	writeFile(t, secret, "SECRET\n")
	live := filepath.Join(base, "ws")
	writeFile(t, filepath.Join(live, "a.go"), "package a\n")
	hardlink(t, secret, filepath.Join(live, "innocuous.txt"))

	p, err := PreviewPayloadWith(live, false)
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
