package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCopyEditDiffCommit(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "hello\n")
	ws := New(t.TempDir())

	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.ApplyEdit(h, core.Edit{File: "a.txt", Anchor: "", Replacement: "X\n"}); err != nil {
		t.Fatal(err)
	}
	diff, changes, err := ws.Diff(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].File != "a.txt" {
		t.Errorf("changes = %+v", changes)
	}
	if !strings.Contains(diff, "+X") {
		t.Errorf("diff missing addition:\n%s", diff)
	}

	committed, err := ws.Commit(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != 1 {
		t.Errorf("committed = %v", committed)
	}
	got, _ := os.ReadFile(filepath.Join(live, "a.txt"))
	if !strings.HasPrefix(string(got), "X\n") {
		t.Errorf("live file not updated: %q", got)
	}
}

// A substring edit preserves the file's existing line endings in the UNCHANGED regions
// (ApplyEdit is a byte-substring replace, never a whole-file re-join). The replacement's
// own newlines are adapted to the file's convention separately (see the adaptation tests).
func TestApplyEdit_PreservesExistingCRLFInUnchangedRegions(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "c.txt"), "alpha\r\nbeta\r\ngamma\r\n")
	ws := New(t.TempDir())
	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.ApplyEdit(h, core.Edit{File: "c.txt", Anchor: "beta", Replacement: "BETA"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(h.Root, "c.txt"))
	if want := "alpha\r\nBETA\r\ngamma\r\n"; string(got) != want {
		t.Errorf("CRLF not preserved in unchanged regions:\n got %q\n want %q", got, want)
	}
}

func TestDetectLineEnding(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"empty":         {"", "\n"},
		"lf":            {"a\nb\n", "\n"},
		"crlf":          {"a\r\nb\r\n", "\r\n"},
		"crlf-dominant": {"a\r\nb\r\nc\n", "\r\n"},
		"lf-dominant":   {"a\nb\nc\r\n", "\n"},
		"tie":           {"a\r\nb\n", "\n"},
		"no-newline":    {"abc", "\n"},
	}
	for name, c := range cases {
		if got := DetectLineEnding(c.in); got != c.want {
			t.Errorf("%s: DetectLineEnding(%q) = %q, want %q", name, c.in, got, c.want)
		}
	}
}

// An LF-terminated replacement (e.g. a remediation marker) prepended into a CRLF file is
// adapted to CRLF — the file does not become mixed-ending.
func TestApplyEdit_AdaptsReplacementToCRLFFile(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "c.txt"), "alpha\r\nbeta\r\n")
	ws := New(t.TempDir())
	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.ApplyEdit(h, core.Edit{File: "c.txt", Anchor: "", Replacement: "// marker\n"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(h.Root, "c.txt"))
	if want := "// marker\r\nalpha\r\nbeta\r\n"; string(got) != want {
		t.Errorf("CRLF adaptation:\n got %q\n want %q", got, want)
	}
	if strings.Contains(strings.ReplaceAll(string(got), "\r\n", ""), "\n") {
		t.Error("result contains a bare LF → mixed endings")
	}
}

// A CRLF replacement into an LF file is normalized to LF — the file stays pure LF.
func TestApplyEdit_AdaptsReplacementToLFFile(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "l.txt"), "alpha\nbeta\n")
	ws := New(t.TempDir())
	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.ApplyEdit(h, core.Edit{File: "l.txt", Anchor: "", Replacement: "// marker\r\n"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(h.Root, "l.txt"))
	if want := "// marker\nalpha\nbeta\n"; string(got) != want {
		t.Errorf("LF adaptation:\n got %q\n want %q", got, want)
	}
	if strings.Contains(string(got), "\r") {
		t.Error("result contains CR → not pure LF")
	}
}

// A hardcoded-LF replacement (the shape the remediation marker takes) prepended into a CRLF
// source must not produce a mixed-ending file — ApplyEdit adapts it to the file's CRLF convention.
func TestApplyEdit_LFReplacementIntoCRLFSource_NotMixed(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "x.go"), "package x\r\n\r\nfunc x() {}\r\n")
	ws := New(t.TempDir())
	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	// The same edit the remediation marker produces (empty anchor = prepend), constructed
	// directly so containment does not depend on the review-domain remediation engine.
	edit := core.Edit{File: "x.go", Anchor: "", Replacement: "// reviewmesh[F1]: note\n", Occurrence: 1}
	if err := ws.ApplyEdit(h, edit); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(h.Root, "x.go"))
	if strings.Contains(strings.ReplaceAll(string(got), "\r\n", ""), "\n") {
		t.Errorf("remediation marker created a mixed-ending file:\n%q", got)
	}
	if !strings.HasPrefix(string(got), "// reviewmesh[F1]: note\r\n") {
		t.Errorf("marker not adapted to CRLF:\n%q", got)
	}
}

// A diff of a CRLF source emits LF framing and no spurious trailing CR in body lines.
func TestDiff_CRLFSource_NoSpuriousCR(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "c.txt"), "alpha\r\nbeta\r\n")
	ws := New(t.TempDir())
	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.ApplyEdit(h, core.Edit{File: "c.txt", Anchor: "beta", Replacement: "BETA"}); err != nil {
		t.Fatal(err)
	}
	diff, _, err := ws.Diff(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diff, "\r") {
		t.Errorf("CRLF-source diff must not carry stray CR:\n%q", diff)
	}
	for _, want := range []string{"--- a/c.txt\n", "-beta\n", "+BETA\n"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q in:\n%s", want, diff)
		}
	}
}

// A pure LF↔CRLF reformat (identical logical content) is NOT reported as a change — no
// spurious empty-hunk diff or FileChange.
func TestDiff_PureLFvsCRLF_NotReportedAsChange(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "alpha\r\nbeta\r\n") // CRLF live
	ws := New(t.TempDir())
	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	// rewrite the copy to LF-only, same logical content (no real content edit)
	if err := os.WriteFile(filepath.Join(h.Root, "a.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, changes, err := ws.Diff(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("pure LF↔CRLF reformat must not be a change: %+v", changes)
	}
	if strings.TrimSpace(diff) != "" {
		t.Errorf("expected empty diff for a pure reformat, got:\n%s", diff)
	}
}

// The unified diff is emitted with deterministic LF framing (---/+++/@@ + line prefixes).
func TestDiff_UsesLFFraming(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "hello\n")
	ws := New(t.TempDir())
	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.ApplyEdit(h, core.Edit{File: "a.txt", Anchor: "", Replacement: "X\n"}); err != nil {
		t.Fatal(err)
	}
	diff, _, err := ws.Diff(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diff, "\r\n") {
		t.Errorf("diff framing must be LF, found CRLF:\n%q", diff)
	}
	for _, want := range []string{"--- a/a.txt\n", "+++ b/a.txt\n"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing LF-framed header %q in:\n%s", want, diff)
		}
	}
}

func TestDiscard_ReadOnly_DetectsM5Mutation(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "hello\n")
	ws := New(t.TempDir())

	h, err := ws.Copy(live, true, "ro")
	if err != nil {
		t.Fatal(err)
	}
	// simulate a reviewer mutating its isolated copy
	if err := os.WriteFile(h.Abs("a.txt"), []byte("mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mutDiff, m5, err := ws.Discard(h)
	if err != nil {
		t.Fatal(err)
	}
	if m5 == nil || string(*m5) != "M5" {
		t.Fatalf("expected M5 halt, got %v", m5)
	}
	if !strings.Contains(mutDiff, "modified: a.txt") {
		t.Errorf("mutation diff = %q", mutDiff)
	}
}

func TestIsExcluded(t *testing.T) {
	cases := map[string]bool{
		"src/main.go":    false,
		".git/config":    true,
		"a/.git/config":  true,
		"tmp/run/x":      true,
		".cache/out":     true,
		"node_modules/m": true,
		// A third-party tool's local directory is NOT excluded: only names present on machines
		// this tool has never seen earn a place in the list.
		".aikit/out":    false,
		"vendor/v":      true,
		"dir/.DS_Store": true,
		"main.go":       false,
	}
	for rel, want := range cases {
		if got := IsExcluded(rel); got != want {
			t.Errorf("IsExcluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

// Every exclusion here is a CONTAINMENT rule. Size is not one of them: big.txt is collected
// whole, because the only reasons to keep a file from a reviewer are reasons it must not be seen.
func TestCollectSnippets_ExcludesSkipsBinaryAndCarriesLargeFilesWhole(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, ".git", "config"), "secret\n")        // excluded
	writeFile(t, filepath.Join(root, "big.txt"), strings.Repeat("a", 100)) // carried whole
	writeFile(t, filepath.Join(root, "bin"), "a\x00b")                     // binary → skipped

	snips := CollectSnippets(root)

	byPath := map[string]Snippet{}
	for _, s := range snips {
		if strings.Contains(s.Path, ".git") {
			t.Errorf("excluded path leaked into snippets: %q", s.Path)
		}
		if s.Path == "bin" {
			t.Error("binary file must be skipped")
		}
		byPath[s.Path] = s
	}
	if _, ok := byPath["main.go"]; !ok {
		t.Error("main.go should be collected")
	}
	big, ok := byPath["big.txt"]
	if !ok || len(big.Content) != 100 {
		t.Errorf("big.txt should be collected whole (100 bytes): %+v", big)
	}
}

func TestCollectSnippets_SkipsSymlinkedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dir symlink/junction handling is verify-on-windows")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "SECRET\n")
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	for _, s := range CollectSnippets(root) {
		if strings.Contains(s.Path, "linkdir") || strings.Contains(s.Content, "SECRET") {
			t.Errorf("symlinked dir must not be traversed: %q", s.Path)
		}
	}
}

func TestSafeRel(t *testing.T) {
	for _, p := range []string{"a.go", "dir/b.go", "./c.go", "x/y/z.go", "README.md", "src/main.go", "docs/design.md"} {
		if !safeRel(p) {
			t.Errorf("safeRel(%q) should be allowed", p)
		}
	}
	// Rooted/absolute-like + escape inputs must be refused on EVERY platform (Windows and
	// non-Windows) — the cross-platform invariant. POSIX-rooted "/x" is rooted even where
	// filepath.IsAbs would not flag it; "\x"/UNC + drive-letter are rooted likewise.
	for _, p := range []string{
		"../x", "../../etc/passwd", "/abs/x", "/etc/passwd",
		`\abs\x`, `\\server\share\x`, `C:\abs\x`, `C:relative`, "",
	} {
		if safeRel(p) {
			t.Errorf("safeRel(%q) should be refused (rooted/absolute-like/escape)", p)
		}
	}
}

// SafeRel (exported) shares the same invariant — used by the ACP inline materializer.
func TestSafeRel_ExportedMatchesUnexported(t *testing.T) {
	for _, p := range []string{"a.go", "/abs/x", `\abs\x`, "../x", `C:\x`, "ok/path.go"} {
		if SafeRel(p) != safeRel(p) {
			t.Errorf("SafeRel(%q)=%v != safeRel=%v", p, SafeRel(p), safeRel(p))
		}
	}
}

func TestIsExcluded_OSNativeSeparators(t *testing.T) {
	// filepath.Join uses the OS separator; exclusion must catch it on every OS.
	if !IsExcluded(filepath.Join("a", ".git", "config")) {
		t.Error("OS-native .git path should be excluded")
	}
	if runtime.GOOS == "windows" && !IsExcluded(`pkg\node_modules\m.js`) {
		t.Error("windows backslash node_modules path should be excluded")
	}
}

func TestCopy_ExcludesInternalDirs(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "main.go"), "package main\n")
	writeFile(t, filepath.Join(live, ".git", "config"), "gitcfg\n")
	writeFile(t, filepath.Join(live, ".cache", "x"), "a\n")
	writeFile(t, filepath.Join(live, "tmp", "t"), "t\n")
	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.Root, "main.go")); err != nil {
		t.Error("main.go should be copied")
	}
	for _, ex := range []string{".git", ".aikit", "tmp"} {
		if _, err := os.Stat(filepath.Join(h.Root, ex)); !os.IsNotExist(err) {
			t.Errorf("excluded dir %q must not be copied", ex)
		}
	}
}

func TestCopy_RejectsExcludedSingleFileTarget(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, ".git", "config"), "x\n")
	ws := New(t.TempDir())
	if _, err := ws.Copy(filepath.Join(live, ".git", "config"), true, "t"); err == nil {
		t.Error("Copy must refuse an excluded single-file target")
	}
}

func TestApplyEdit_RejectsExcludedAndEscaping(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "x\n")
	ws := New(t.TempDir())
	h, _ := ws.Copy(live, false, "t")
	if err := ws.ApplyEdit(h, core.Edit{File: ".git/config", Replacement: "y"}); err == nil {
		t.Error("ApplyEdit must reject an excluded path")
	}
	if err := ws.ApplyEdit(h, core.Edit{File: "../escape.txt", Replacement: "y"}); err == nil {
		t.Error("ApplyEdit must reject a path escaping the workspace")
	}
}

func TestCopy_RejectsExcludedTargets(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, ".git", "config"), "x\n")
	writeFile(t, filepath.Join(live, "tmp", "y"), "y\n")
	ws := New(t.TempDir())
	for _, target := range []string{
		filepath.Join(live, ".git"),           // excluded dir as target
		filepath.Join(live, "tmp"),            // excluded dir as target
		filepath.Join(live, ".git", "config"), // file directly inside an excluded dir
	} {
		if _, err := ws.Copy(target, true, "t"); err == nil {
			t.Errorf("Copy(%q) must be refused", target)
		}
	}
}

func TestCopy_AllowsRepoUnderExcludedNamedAncestor(t *testing.T) {
	// A legitimate repo whose ABSOLUTE path passes through an excluded-named ancestor
	// (e.g. .../build/myrepo) must NOT be refused — only the target and its immediate
	// parent are checked, never arbitrary absolute ancestors.
	live := filepath.Join(t.TempDir(), "build", "myrepo")
	writeFile(t, filepath.Join(live, "main.go"), "package main\n")
	ws := New(t.TempDir())
	if _, err := ws.Copy(live, true, "t"); err != nil {
		t.Errorf("repo under a 'build' ancestor should be allowed: %v", err)
	}
}

func TestCopy_SkipsAndRefusesSymlinks(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "main.go"), "package main\n")
	outside := filepath.Join(t.TempDir(), "outside.go")
	writeFile(t, outside, "EXTERNAL\n")
	link := filepath.Join(live, "link.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.Root, "link.go")); !os.IsNotExist(err) {
		t.Error("a symlink must not be copied into the isolated copy")
	}
	if _, err := ws.Copy(link, true, "t2"); err == nil {
		t.Error("Copy must refuse a symlink as the direct target")
	}
}

func TestApplyEdit_Occurrence(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "x x x\n")
	ws := New(t.TempDir())
	h, _ := ws.Copy(live, false, "t")
	if err := ws.ApplyEdit(h, core.Edit{File: "a.txt", Anchor: "x", Replacement: "y", Occurrence: 2}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(h.Abs("a.txt")); string(got) != "x y x\n" {
		t.Errorf("occurrence replace = %q, want %q", got, "x y x\n")
	}
}

func TestDiscard_SymlinkAddedToCopy_M5_NoFollow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is POSIX here; reparse handling is verify-on-windows")
	}
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "hello\n")
	ws := New(t.TempDir())
	h, _ := ws.Copy(live, true, "ro")
	// a contained reviewer adds a symlink pointing OUTSIDE its copy
	outside := filepath.Join(t.TempDir(), "secret")
	writeFile(t, outside, "SECRET")
	if err := os.Symlink(outside, filepath.Join(h.Root, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	_, m5, err := ws.Discard(h)
	if err != nil {
		t.Fatal(err)
	}
	if m5 == nil { // the new reparse entry changes the snapshot → M5, recorded without following the link
		t.Error("a reviewer-added symlink in a read-only copy must be detected as M5")
	}
}

func TestDiscard_ReadOnly_CleanNoM5(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "hello\n")
	ws := New(t.TempDir())
	h, _ := ws.Copy(live, true, "ro")
	_, m5, err := ws.Discard(h)
	if err != nil {
		t.Fatal(err)
	}
	if m5 != nil {
		t.Errorf("unexpected M5 on a clean read-only copy")
	}
}
