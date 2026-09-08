package workspace

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A file dropped from the payload used to vanish without a word, while a hardlinked file got a
// caveat for the same class of reason. docs/security.md argues against exactly that — "a reviewer
// cannot object to a file it was never shown" — and the argument has grown teeth: `.claude/` and
// `.cursor/` now hold rules and prompts that ARE source, so a user can reasonably ask for a review
// of them and never learn it did not happen.

func TestCollect_AnExcludedFileIsRecordedNotSilent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, ".claude", "rules.md"), "# my rules\n")

	snips, caveats, err := CollectSnippetsWithCaveats(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// Still excluded — the point is disclosure, not inclusion.
	for _, s := range snips {
		if strings.Contains(s.Path, ".claude") {
			t.Fatalf("%s reached the payload; exclusion must still hold", s.Path)
		}
	}
	idx := slices.IndexFunc(caveats, func(c Caveat) bool { return strings.Contains(c.Path, ".claude") })
	if idx < 0 {
		t.Fatalf("the excluded path was dropped silently; caveats = %+v", caveats)
	}
	c := caveats[idx]
	if c.Reason != ReasonExcluded {
		t.Errorf("reason = %q, want %q", c.Reason, ReasonExcluded)
	}
	// The caveat must name the RULE that fired, or a reader cannot tell why it went.
	if !strings.Contains(c.Rule, ".claude") {
		t.Errorf("caveat does not name the rule: %+v", c)
	}
	if !strings.Contains(c.Detail, "not shown to any reviewer") {
		t.Errorf("caveat does not say what it means: %+v", c)
	}
}

// TestCollect_AnExcludedDirectoryIsRecordedONCE. A caveat per file inside `.claude` would bury
// every other caveat in the list; the useful fact is that the subtree went.
func TestCollect_AnExcludedDirectoryIsRecordedOnce(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	for _, f := range []string{"rules.md", "settings.json", "commands/a.md", "commands/deep/b.md"} {
		writeFile(t, filepath.Join(root, ".claude", filepath.FromSlash(f)), "x\n")
	}

	_, caveats, err := CollectSnippetsWithCaveats(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var n int
	for _, c := range caveats {
		if strings.Contains(c.Path, ".claude") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf(".claude produced %d caveats, want exactly 1 for the directory: %+v", n, caveats)
	}
	for _, c := range caveats {
		if c.Path == ".claude" && !strings.Contains(c.Detail, "everything under it") {
			t.Errorf("a directory caveat must say it stands for the subtree: %+v", c)
		}
	}
}

// TestCollect_RoutineBuildExclusionsStaySilent is the noise half, and it protects the signal.
//
// Every repository has `node_modules`/`dist`/`.git`. Reporting them on every run would put five
// lines nobody reads above the one line that matters, and a disclosure people skim is worth about
// as much as no disclosure. These are universal convention and documented; nobody is surprised.
func TestCollect_RoutineBuildExclusionsStaySilent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	// `.aimesh` is in this list deliberately: it is THIS TOOL's own run artifacts, and the run
	// reporting on a workspace is frequently the run that just created the directory there. A line
	// on every single run is the noise this distinction exists to prevent.
	for _, dir := range []string{"node_modules", "dist", "build", "vendor", "coverage", ".cache", "tmp", ".aimesh"} {
		writeFile(t, filepath.Join(root, dir, "x.js"), "x\n")
	}

	_, caveats, err := CollectSnippetsWithCaveats(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(caveats) != 0 {
		t.Errorf("routine build/artifact exclusions produced %d caveat(s); they must not crowd out the ones a user would be surprised by: %+v", len(caveats), caveats)
	}
}

// TestCollect_ASecretIsSTILLSilent is the deliberate asymmetry, and it is the more important half.
//
// A caveat is reported to the caller, and over MCP that caller is a MODEL. A list of the paths
// where this machine keeps its credentials is precisely the inventory the secret rule exists to
// withhold — so naming them "for transparency" would hand over the map while withholding the
// treasure. The exclusion family has no such problem: nobody is attacked by learning `dist/` was
// skipped, and the secret families are fixed and documented, so what is never shown is knowable
// without being told where it lives.
func TestCollect_ASecretIsStillSilent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, ".env"), "API_KEY=sk-SECRET\n")
	writeFile(t, filepath.Join(root, ".ssh", "id_rsa"), "-----BEGIN KEY-----\n")

	snips, caveats, err := CollectSnippetsWithCaveats(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, s := range snips {
		if strings.Contains(s.Path, ".env") || strings.Contains(s.Path, ".ssh") {
			t.Fatalf("a secret reached the payload: %s", s.Path)
		}
	}
	for _, c := range caveats {
		if strings.Contains(c.Path, ".env") || strings.Contains(c.Path, ".ssh") {
			t.Errorf("a secret's PATH was disclosed in a caveat, handing a reader the inventory the rule withholds: %+v", c)
		}
	}
}

// TestPreview_ReportsTheSameExclusions. The dry run must describe the run that would happen; if it
// reported a clean payload where the run drops files, the disclosure would be wrong in the one
// place a user consults BEFORE spending.
func TestPreview_ReportsTheSameExclusions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, ".vscode", "mcp.json"), "{}\n")

	p, err := PreviewPayload(root)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !slices.ContainsFunc(p.Withheld, func(c Caveat) bool { return strings.Contains(c.Path, ".vscode") }) {
		t.Errorf("the dry run does not report the exclusion the real run would make: %+v", p.Withheld)
	}
}
