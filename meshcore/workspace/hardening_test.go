package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// hardlink creates a hardlink from an out-of-workspace secret to an innocuous in-workspace
// name, or skips the test where the platform/filesystem cannot.
func hardlink(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hardlink probing is unix-only here; Windows is covered by TestWindows_FilesystemSafety")
	}
	if err := os.Link(target, link); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}
}

// --- a denied root is a refusal ---

// TestCopy_RefusesDeniedRoot pins that a secret directory given as the copy target is refused, since
// judging only its children by their own names would expose its contents.
func TestCopy_RefusesDeniedRoot(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, ".env")
	writeFile(t, filepath.Join(live, "production"), "SECRET=prod\n")
	writeFile(t, filepath.Join(live, "staging"), "SECRET=stage\n")

	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "t")
	if err == nil {
		ws.Cleanup(h)
		t.Fatal("Copy of a `.env` directory must be refused")
	}
	if got := ReasonOf(err); got != ReasonRootDenied {
		t.Errorf("reason = %q, want %q (err: %v)", got, ReasonRootDenied, err)
	}
	if h != nil {
		t.Error("a refused Copy must not return a handle")
	}
}

// TestCollectSnippets_RefusesDeniedRoot pins the same rule at the prompt-assembly layer:
// with the root itself denied, no content may come back.
func TestCollectSnippets_RefusesDeniedRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".env")
	writeFile(t, filepath.Join(root, "production"), "SECRET=prod\n")

	got, caveats, err := CollectSnippetsWithCaveats(root)
	if err == nil {
		t.Fatal("collection from a denied root must refuse")
	}
	if len(got) != 0 || len(caveats) != 0 {
		t.Errorf("a refusal must return no content: %+v / %+v", got, caveats)
	}
	if r := ReasonOf(err); r != ReasonRootDenied {
		t.Errorf("reason = %q, want %q", r, ReasonRootDenied)
	}
}

// --- a protected ancestor still protects a root chosen beneath it ---

// protectedRoots are protected directories a root could be chosen inside. `.vscode/mcp.json` can carry
// API keys and is readable, so the read denylist never catches it. `.aimesh` is not listed; see
// TestCopy_AimeshRunsAreReadable.
var protectedRoots = []string{".vscode", ".git", ".claude", ".cursor"}

// TestCopy_RefusesRootInsideProtectedAncestor pins that a root inside a protected directory is refused.
// Exclusion is judged relative to the root, so otherwise its contents would become ordinary files.
func TestCopy_RefusesRootInsideProtectedAncestor(t *testing.T) {
	for _, dir := range protectedRoots {
		t.Run(dir, func(t *testing.T) {
			base := t.TempDir()
			// The root sits inside the protected directory under an innocuous name.
			root := filepath.Join(base, dir, "workspace")
			writeFile(t, filepath.Join(root, "mcp.json"), `{"servers":{"x":{"env":{"API_KEY":"sk-SECRET"}}}}`)

			ws := New(t.TempDir())
			h, err := ws.Copy(root, true, "t")
			if err == nil {
				ws.Cleanup(h)
				t.Fatalf("Copy of a root inside %s must be refused", dir)
			}
			if got := ReasonOf(err); got != ReasonExcludedAncestor {
				t.Errorf("reason = %q, want %q (err: %v)", got, ReasonExcludedAncestor, err)
			}
			if h != nil {
				t.Error("a refused Copy must not return a handle")
			}
			// The protected directory itself is refused with the same typed reason.
			if got := ReasonOf(mustFailCopy(t, ws, filepath.Join(base, dir))); got != ReasonExcludedAncestor {
				t.Errorf("reason for the protected dir itself = %q, want %q", got, ReasonExcludedAncestor)
			}
		})
	}
}

// TestCopy_AimeshRunsAreReadable pins that a run directory under `.aimesh` can be a copy target, while
// `.aimesh` stays write-denied and excluded from walks of the project above it.
func TestCopy_AimeshRunsAreReadable(t *testing.T) {
	base := t.TempDir()
	runDir := filepath.Join(base, ".aimesh", "explore", "runs", "20260101-120000-abc")
	writeFile(t, filepath.Join(runDir, "result.json"), `{"findings":[]}`)

	ws := New(t.TempDir())
	h, err := ws.Copy(runDir, true, "t")
	if err != nil {
		t.Fatalf("a run directory must be readable without any flag: %v", err)
	}
	ws.Cleanup(h)

	// Still write-denied.
	if rule := scope.DeniedWrite(filepath.Join(runDir, "result.json")); rule == "" {
		t.Error(".aimesh became WRITABLE — only the root refusal was meant to go")
	}
	// Still excluded from a walk of the project above it.
	writeFile(t, filepath.Join(base, "main.go"), "package main\n")
	snips, _, cerr := CollectSnippetsWithCaveats(base)
	if cerr != nil {
		t.Fatalf("collect: %v", cerr)
	}
	for _, s := range snips {
		if strings.Contains(s.Path, ".aimesh") {
			t.Errorf("reviewing the PROJECT collected a run artifact (%s) — .aimesh must stay excluded from a walk", s.Path)
		}
	}
}

// TestCollectSnippets_RefusesRootInsideProtectedAncestor pins the same rule at the
// prompt-assembly layer, where the secret would actually reach the model.
func TestCollectSnippets_RefusesRootInsideProtectedAncestor(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".vscode", "workspace")
	writeFile(t, filepath.Join(root, "mcp.json"), `{"servers":{"x":{"env":{"API_KEY":"sk-SECRET"}}}}`)

	got, caveats, err := CollectSnippetsWithCaveats(root)
	if err == nil {
		t.Fatal("collection from a root inside .vscode must be refused")
	}
	if r := ReasonOf(err); r != ReasonExcludedAncestor {
		t.Errorf("reason = %q, want %q", r, ReasonExcludedAncestor)
	}
	if len(got) != 0 || len(caveats) != 0 {
		t.Errorf("a refusal must return no content: %+v / %+v", got, caveats)
	}
}

// TestCopy_RefusesRootAliasedBySymlinkIntoProtectedDir pins that the ancestor rule is not a
// spelling rule: a symlink whose own path has no protected component still lands inside one.
func TestCopy_RefusesRootAliasedBySymlinkIntoProtectedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, ".vscode", "workspace")
	writeFile(t, filepath.Join(real, "mcp.json"), `{"env":{"API_KEY":"sk-SECRET"}}`)
	alias := filepath.Join(base, "innocent")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	ws := New(t.TempDir())
	// A symlinked target is refused as a reparse point before the ancestor rule is reached, so
	// point at a path through the alias; the target itself is then an ordinary directory.
	writeFile(t, filepath.Join(real, "sub", "x.json"), "{}\n")
	if err := ReasonOf(mustFailCopy(t, ws, filepath.Join(alias, "sub"))); err != ReasonExcludedAncestor {
		t.Errorf("reason = %q, want %q", err, ReasonExcludedAncestor)
	}
}

// TestCopy_AllowsRootUnderGenericExcludedAncestor guards against false refusals: generic build and
// artifact names are not protected ancestors, so a repository under `tmp` or `build` still works.
func TestCopy_AllowsRootUnderGenericExcludedAncestor(t *testing.T) {
	for _, dir := range []string{"tmp", "build", "dist", "node_modules", "vendor", "coverage", ".cache"} {
		t.Run(dir, func(t *testing.T) {
			live := filepath.Join(t.TempDir(), dir, "myrepo")
			writeFile(t, filepath.Join(live, "main.go"), "package main\n")
			ws := New(t.TempDir())
			h, err := ws.Copy(live, true, "t")
			if err != nil {
				t.Fatalf("a repo under a %q ancestor must still be reviewable: %v", dir, err)
			}
			defer ws.Cleanup(h)
			if _, serr := os.Stat(h.Abs("main.go")); serr != nil {
				t.Errorf("source under a %q ancestor was not copied: %v", dir, serr)
			}
		})
	}
}

func mustFailCopy(t *testing.T, ws *Access, target string) error {
	t.Helper()
	h, err := ws.Copy(target, true, "t")
	if err == nil {
		ws.Cleanup(h)
		t.Fatalf("Copy(%q) must be refused", target)
	}
	return err
}

// --- os.Root hardens interior traversal, not boundary selection ---

// TestCopy_RefusesRootSwappedAfterCheck pins the identity binding: a root swapped for a symlink between
// the checks and the open is refused. The window is entered through the test seam rather than raced.
func TestCopy_RefusesRootSwappedAfterCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	base := t.TempDir()
	live := filepath.Join(base, "approved")
	writeFile(t, filepath.Join(live, "main.go"), "package main\n")
	attacker := filepath.Join(base, "attacker")
	writeFile(t, filepath.Join(attacker, "loot.txt"), "ATTACKER CONTENT\n")

	swapped := false
	testHookBeforeOpenRoot = func(dir string) {
		if swapped || dir != live {
			return
		}
		swapped = true
		if err := os.Rename(live, filepath.Join(base, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(attacker, live); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testHookBeforeOpenRoot = nil })

	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "t")
	if err == nil {
		ws.Cleanup(h)
		t.Fatal("Copy must refuse when the root was replaced between the check and the open")
	}
	if !swapped {
		t.Fatal("the test seam never fired; the swap window was not exercised")
	}
	if got := ReasonOf(err); got != ReasonRootChanged {
		t.Errorf("reason = %q, want %q (err: %v)", got, ReasonRootChanged, err)
	}
	if h != nil {
		t.Error("a refused Copy must not return a handle")
	}
}

// TestCollect_RefusesRootSwappedAfterCheck pins the same binding at the prompt layer: the
// swap decides which tree is read into a model prompt.
func TestCollect_RefusesRootSwappedAfterCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "approved")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	attacker := filepath.Join(base, "attacker")
	writeFile(t, filepath.Join(attacker, "loot.txt"), "ATTACKER CONTENT\n")

	testHookBeforeOpenRoot = func(dir string) {
		if dir != root {
			return
		}
		testHookBeforeOpenRoot = nil
		if err := os.Rename(root, filepath.Join(base, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(attacker, root); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testHookBeforeOpenRoot = nil })

	got, _, err := CollectSnippetsWithCaveats(root)
	if err == nil {
		t.Fatal("collection must refuse when the root was replaced between the check and the open")
	}
	if r := ReasonOf(err); r != ReasonRootChanged {
		t.Errorf("reason = %q, want %q (err: %v)", r, ReasonRootChanged, err)
	}
	for _, s := range got {
		t.Errorf("attacker content reached a snippet: %q: %q", s.Path, s.Content)
	}
}

// --- client and IDE configuration is excluded from the copy, not only write-denied ---

// TestCopy_ExcludesAgentAndIDEConfig pins that agent/IDE configuration never enters the
// isolated copy. A file that is copied is a file that can be shown to a model, proposed
// as an edit and accepted by a human reading a diff — and `.vscode/mcp.json` is
// executable configuration, not source.
func TestCopy_ExcludesAgentAndIDEConfig(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "main.go"), "package main\n")
	protected := []string{
		".vscode/mcp.json",
		".vscode/settings.json",
		".idea/workspace.xml",
		".windsurf/rules.md",
		".claude/settings.json",
		".cursor/rules.md",
		".mcp.json",
		"claude_desktop_config.json",
	}
	for _, rel := range protected {
		writeFile(t, filepath.Join(live, filepath.FromSlash(rel)), "{\"servers\":{}}\n")
	}

	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Cleanup(h)
	for _, rel := range protected {
		if _, err := os.Stat(h.Abs(filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s was copied into the isolated copy; client config must never be", rel)
		}
	}
	if _, err := os.Stat(h.Abs("main.go")); err != nil {
		t.Errorf("ordinary source must still be copied: %v", err)
	}
}

// --- a hardlink defeats name-based exclusion: refuse the file, not the run ---

// TestCopy_SkipsHardlinkedFileWithCaveat pins that a hardlinked file, whose other name may be a secret,
// is withheld with a recorded caveat rather than copied or failing the whole copy.
func TestCopy_SkipsHardlinkedFileWithCaveat(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "main.go"), "package main\n")
	secret := filepath.Join(t.TempDir(), "id_rsa")
	writeFile(t, secret, "PRIVATE KEY MATERIAL\n")
	hardlink(t, secret, filepath.Join(live, "notes.txt"))

	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "t")
	if err != nil {
		t.Fatalf("one hardlinked file must not fail the whole copy: %v", err)
	}
	defer ws.Cleanup(h)
	if _, serr := os.Stat(h.Abs("notes.txt")); serr == nil {
		t.Error("the hardlinked file was copied; its bytes may be a protected file's")
	}
	if _, serr := os.Stat(h.Abs("main.go")); serr != nil {
		t.Errorf("ordinary source must still be copied: %v", serr)
	}
	if len(h.Caveats) != 1 {
		t.Fatalf("caveats = %+v, want exactly one (the withheld file)", h.Caveats)
	}
	if h.Caveats[0].Path != "notes.txt" || h.Caveats[0].Reason != ReasonHardlink {
		t.Errorf("caveat = %+v, want notes.txt / %s", h.Caveats[0], ReasonHardlink)
	}
}

// TestCopy_RefusesHardlinkedSingleFileTarget pins that a hardlinked file given as the target is refused
// outright, since withholding it would leave nothing to copy.
func TestCopy_RefusesHardlinkedSingleFileTarget(t *testing.T) {
	live := t.TempDir()
	secret := filepath.Join(t.TempDir(), "id_rsa")
	writeFile(t, secret, "PRIVATE KEY MATERIAL\n")
	target := filepath.Join(live, "notes.txt")
	hardlink(t, secret, target)

	ws := New(t.TempDir())
	h, err := ws.Copy(target, true, "t")
	if err == nil {
		ws.Cleanup(h)
		t.Fatal("Copy of a hardlinked file AS THE TARGET must be refused")
	}
	if got := ReasonOf(err); got != ReasonHardlink {
		t.Errorf("reason = %q, want %q (err: %v)", got, ReasonHardlink, err)
	}
}

// TestCollectSnippets_SkipsHardlinkedFileWithCaveat pins the same policy at the prompt
// layer: the hardlinked file never reaches a snippet, the rest of the set still does, and
// the omission is recorded rather than invisible.
func TestCollectSnippets_SkipsHardlinkedFileWithCaveat(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	secret := filepath.Join(t.TempDir(), "id_rsa")
	writeFile(t, secret, "PRIVATE KEY MATERIAL\n")
	hardlink(t, secret, filepath.Join(root, "notes.txt"))

	got, caveats, err := CollectSnippetsWithCaveats(root)
	if err != nil {
		t.Fatalf("one hardlinked file must not fail the whole collection: %v", err)
	}
	for _, s := range got {
		if s.Path == "notes.txt" || strings.Contains(s.Content, "PRIVATE KEY MATERIAL") {
			t.Errorf("a hardlinked file reached a snippet: %q: %q", s.Path, s.Content)
		}
	}
	if len(got) != 1 || got[0].Path != "main.go" {
		t.Errorf("snippets = %+v, want just main.go", got)
	}
	if len(caveats) != 1 || caveats[0].Path != "notes.txt" || caveats[0].Reason != ReasonHardlink {
		t.Errorf("caveats = %+v, want notes.txt / %s", caveats, ReasonHardlink)
	}
}

// --- NTFS alternate-data-stream aliasing ---

// TestExclusion_NTFSStreamAliasing pins that on NTFS a stream suffix cannot alias a component past the
// exclusion set (`.git::$DATA` resolves to `.git`), and that the trim does not apply elsewhere, where
// `:` is an ordinary filename character.
func TestExclusion_NTFSStreamAliasing(t *testing.T) {
	aliased := []string{`.git::$DATA/config`, `.vscode:x:$DATA/mcp.json`, `a/.claude::$DATA/settings.json`}

	// Both platforms' expectations are exercised on every platform by toggling the gate.
	t.Run("windows semantics", func(t *testing.T) {
		defer restoreStreamAliasing(streamAliasing)
		streamAliasing = true
		for _, rel := range aliased {
			if !IsExcluded(filepath.FromSlash(rel)) {
				t.Errorf("IsExcluded(%q) = false, want true (NTFS stream aliasing)", rel)
			}
		}
		if rule := protectedAncestorRule(filepath.FromSlash(`/base/.vscode::$DATA/workspace`)); rule == "" {
			t.Error("an aliased protected ANCESTOR must still refuse a root beneath it")
		}
	})

	t.Run("unix semantics", func(t *testing.T) {
		defer restoreStreamAliasing(streamAliasing)
		streamAliasing = false
		// `:` is an ordinary filename character here: the literal name is not the excluded
		// name, and trimming it would only widen what this package lets through.
		for _, rel := range aliased {
			if IsExcluded(filepath.FromSlash(rel)) {
				t.Errorf("IsExcluded(%q) = true, want false (a literal colon name)", rel)
			}
		}
	})

	// Unrelated to the gate: an ordinary name that merely contains a colon is never an
	// exclusion on either platform.
	if IsExcluded("notes:draft.md") {
		t.Error(`IsExcluded("notes:draft.md") must be false: a colon is not an exclusion`)
	}
	// The unaliased ancestor matches everywhere, gate or no gate.
	if rule := protectedAncestorRule(filepath.FromSlash("/base/.vscode/workspace")); rule == "" {
		t.Error("the unaliased protected ancestor must match on every platform")
	}
}

func restoreStreamAliasing(v bool) { streamAliasing = v }

// --- root-relative traversal (os.Root), not path-string re-opening ---

// TestCommit_RefusesSymlinkedLiveDirectory pins that a commit cannot write through a pre-planted symlink
// `sub` pointing outside the live tree; a root-relative write refuses to leave the root.
func TestCommit_RefusesSymlinkedLiveDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows; the junction equivalent is in TestWindows_FilesystemSafety")
	}
	live := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(live, "sub")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "sub", "x.txt"), "PWNED\n")

	ws := New(t.TempDir())
	h := &Handle{Root: copyDir, Live: live}
	committed, err := ws.Commit(h)
	if err == nil { // t.Error, not t.Fatal: the escape assertion below is the real proof
		t.Error("Commit through a symlinked live directory must be refused")
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing", committed)
	}
	if _, serr := os.Stat(filepath.Join(outside, "x.txt")); serr == nil {
		t.Error("the commit escaped the workspace: it wrote through the symlinked directory")
	}
	// Nothing was written, so the rollback has nothing to undo: the refusal must come
	// back clean rather than dressed up as a rollback failure.
	if err != nil && strings.Contains(err.Error(), "rollback failed") {
		t.Errorf("rollback should be a no-op when nothing was written: %v", err)
	}
}

// TestCommit_RefusesSymlinkedDestination pins that a live destination that is a symlink is refused, not
// silently skipped.
func TestCommit_RefusesSymlinkedDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	live := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	writeFile(t, target, "ORIGINAL\n")
	if err := os.Symlink(target, filepath.Join(live, "a.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "a.txt"), "REWRITTEN\n")

	ws := New(t.TempDir())
	committed, err := ws.Commit(&Handle{Root: copyDir, Live: live})
	if err == nil {
		t.Fatal("Commit onto a symlinked destination must be refused, not silently skipped")
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing", committed)
	}
	if got, _ := os.ReadFile(target); string(got) != "ORIGINAL\n" {
		t.Errorf("the symlink target was written through: %q", got)
	}
}

// --- the reparse guard's typed refusals ---
//
// On POSIX, deeper checks (os.Root, no-follow opens, the IsRegular filter) would refuse symlinks anyway,
// so these tests assert the reparse refusal itself. On Windows, hasReparseAttr is the only check that
// recognizes a junction.

// TestCopy_SymlinkTargetIsRefusedAsAReparsePoint pins that a symlink given as the copy target is refused
// by the reparse rule, naming what it is.
func TestCopy_SymlinkTargetIsRefusedAsAReparsePoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "main.go"), "package main\n")
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(live, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	ws := New(t.TempDir())
	h, err := ws.Copy(link, true, "t")
	if err == nil {
		ws.Cleanup(h)
		t.Fatal("a symlink handed in as the review target must be refused")
	}
	if !strings.Contains(err.Error(), "symlink/reparse-point target") {
		t.Errorf("the refusal must name WHAT was refused (a symlink/reparse point), got: %v", err)
	}
}

// TestCommit_SymlinkedDestinationCarriesTheTypedReparseReason pins the machine reason on the write
// path, since callers classify commit refusals by it.
func TestCommit_SymlinkedDestinationCarriesTheTypedReparseReason(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	live := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	writeFile(t, target, "ORIGINAL\n")
	if err := os.Symlink(target, filepath.Join(live, "a.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "a.txt"), "REWRITTEN\n")

	ws := New(t.TempDir())
	_, err := ws.Commit(&Handle{Root: copyDir, Live: live})
	if err == nil {
		t.Fatal("Commit onto a symlinked destination must be refused")
	}
	ref, ok := AsRefusal(err)
	if !ok {
		t.Fatalf("the refusal must be a typed *Refusal a caller can classify, got %T: %v", err, err)
	}
	if ref.Reason != ReasonReparse {
		t.Errorf("reason = %q, want %q — a symlinked destination is a REPARSE refusal, not an incidental I/O failure", ref.Reason, ReasonReparse)
	}
	if got, _ := os.ReadFile(target); string(got) != "ORIGINAL\n" {
		t.Errorf("the symlink target was written through: %q", got)
	}
}

// TestCopy_SkipsAReparseAttributeEntryThatLooksLikeAnOrdinaryFile simulates a Windows junction on any
// platform: an ordinary file that the reparse-attribute check flags. Without the copy walk's isReparse
// guard, this test copies it.
func TestCopy_SkipsAReparseAttributeEntryThatLooksLikeAnOrdinaryFile(t *testing.T) {
	prev := reparseAttr
	t.Cleanup(func() { reparseAttr = prev })
	// The simulated junction: an ordinary regular file that the attribute probe flags.
	reparseAttr = func(fi os.FileInfo) bool { return fi != nil && fi.Name() == "junction.txt" }

	live := t.TempDir()
	writeFile(t, filepath.Join(live, "main.go"), "package main\n")
	writeFile(t, filepath.Join(live, "junction.txt"), "CONTENT FROM OUTSIDE THE WORKSPACE\n")

	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "t")
	if err != nil {
		t.Fatalf("a reparse entry must be SKIPPED, not fail the whole copy: %v", err)
	}
	defer ws.Cleanup(h)

	if _, serr := os.Stat(h.Abs("junction.txt")); serr == nil {
		t.Error("an entry carrying the reparse attribute was copied into the isolated copy; a junction is exactly how content from outside the workspace reaches a model")
	}
	if _, serr := os.Stat(h.Abs("main.go")); serr != nil {
		t.Errorf("ordinary files must still be copied: %v", serr)
	}
}

// TestApplyEdit_RefusesSymlinkInCopy pins that an edit goes through a root handle on the copy, so a
// symlink planted in the copy cannot redirect the edit to a file outside it.
func TestApplyEdit_RefusesSymlinkInCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "hello\n")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "ORIGINAL\n")

	ws := New(t.TempDir())
	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Cleanup(h)
	if err := os.Symlink(outside, h.Abs("notes.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := ws.ApplyEdit(h, core.Edit{File: "notes.txt", Replacement: "INJECTED\n"}); err == nil {
		t.Fatal("ApplyEdit through a planted symlink must be refused")
	}
	if got, _ := os.ReadFile(outside); string(got) != "ORIGINAL\n" {
		t.Errorf("the edit followed the symlink out of the copy: %q", got)
	}
}

// --- commit rollback ---

// TestCommit_RollsBackEarlierWritesOnFailure pins that a mid-sequence failure leaves the live tree as it
// was: the second destination cannot be written (it is a non-empty directory), so the first is restored.
func TestCommit_RollsBackEarlierWritesOnFailure(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "ORIGINAL A\n")
	writeFile(t, filepath.Join(live, "b.txt", "occupied"), "in the way\n") // b.txt is a dir

	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "a.txt"), "NEW A\n")
	writeFile(t, filepath.Join(copyDir, "b.txt"), "NEW B\n")

	ws := New(t.TempDir())
	committed, err := ws.Commit(&Handle{Root: copyDir, Live: live})
	if err == nil {
		t.Fatal("Commit must fail when a destination cannot be written")
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing after a rollback", committed)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "a.txt")); string(got) != "ORIGINAL A\n" {
		t.Errorf("a.txt was left half-applied: %q, want %q", got, "ORIGINAL A\n")
	}
	if got, _ := os.ReadFile(filepath.Join(live, "b.txt", "occupied")); string(got) != "in the way\n" {
		t.Errorf("the blocking destination was damaged: %q", got)
	}
}

// TestCommit_RollsBackNewFileOnFailure pins the other rollback direction: a destination
// that did not exist before the commit must not survive it.
func TestCommit_RollsBackNewFileOnFailure(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "b.txt", "occupied"), "in the way\n")

	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "a.txt"), "BRAND NEW\n") // no live counterpart
	writeFile(t, filepath.Join(copyDir, "b.txt"), "NEW B\n")

	ws := New(t.TempDir())
	if _, err := ws.Commit(&Handle{Root: copyDir, Live: live}); err == nil {
		t.Fatal("Commit must fail when a destination cannot be written")
	}
	if _, err := os.Stat(filepath.Join(live, "a.txt")); !os.IsNotExist(err) {
		t.Error("a file created by a rolled-back commit must be removed again")
	}
}

// TestCommit_UnreadableStagedFileIsError pins that a staged file that cannot be read, such as a symlink
// planted in the copy, fails the commit instead of being followed or skipped.
func TestCommit_UnreadableStagedFileIsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "keep.txt"), "keep\n")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, outside, "OUTSIDE SECRET\n")

	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "keep.txt"), "keep\n")
	if err := os.Symlink(outside, filepath.Join(copyDir, "notes.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	ws := New(t.TempDir())
	committed, err := ws.Commit(&Handle{Root: copyDir, Live: live})
	if err == nil { // t.Error, not t.Fatal: the exfiltration assertion below is the real proof
		t.Error("an unreadable/symlinked staged file must fail the commit, never be skipped")
	}
	// The reason is pinned, since callers classify on it and record it.
	if got := ReasonOf(err); got != ReasonStagedUnreadable {
		t.Errorf("reason = %q, want %q (err: %v)", got, ReasonStagedUnreadable, err)
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing", committed)
	}
	if got, rerr := os.ReadFile(filepath.Join(live, "notes.txt")); rerr == nil {
		t.Errorf("out-of-workspace content was committed into the live tree: %q", got)
	}
}

// --- an unreadable staged subtree must not vanish silently ---

// unreadableDir makes dir unlistable, or skips where that cannot be arranged.
func unreadableDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions have no Windows equivalent to arrange here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not restrict enumeration")
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skipf("cannot make a directory unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// TestCommit_UnenumerableStagedDirectoryIsError pins that a staged directory that cannot be listed fails
// the commit instead of silently dropping its files.
func TestCommit_UnenumerableStagedDirectoryIsError(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "keep.txt"), "keep\n")

	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "keep.txt"), "CHANGED\n")
	staged := filepath.Join(copyDir, "sub")
	writeFile(t, filepath.Join(staged, "remediated.go"), "package x // FIXED\n")
	unreadableDir(t, staged)

	ws := New(t.TempDir())
	committed, err := ws.Commit(&Handle{Root: copyDir, Live: live})
	if err == nil {
		t.Fatal("a staged subtree that cannot be listed must fail the commit, never be dropped")
	}
	if got := ReasonOf(err); got != ReasonStagedUnenumerable {
		t.Errorf("reason = %q, want %q (err: %v)", got, ReasonStagedUnenumerable, err)
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing", committed)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "keep.txt")); string(got) != "keep\n" {
		t.Errorf("nothing may be applied when the staged set is incomplete: %q", got)
	}
}

// TestDiscard_UnenumerableCopyIsBreach pins the same rule on the mutation-detection path:
// a read-only copy whose subtree cannot be listed cannot be verified, so it fails closed rather
// than reporting a clean discard.
func TestDiscard_UnenumerableCopyIsBreach(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "hello\n")
	writeFile(t, filepath.Join(live, "sub", "b.txt"), "world\n")
	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "ro")
	if err != nil {
		t.Fatal(err)
	}
	unreadableDir(t, h.Abs("sub"))

	mutDiff, m5, derr := ws.Discard(h)
	if m5 == nil {
		t.Error("an unverifiable read-only copy must not be reported as clean")
	}
	if derr == nil {
		t.Error("the enumeration failure must be returned, not swallowed")
	}
	if !strings.Contains(mutDiff, "unverifiable") {
		t.Errorf("mutation diff = %q, want it to say the copy could not be verified", mutDiff)
	}
}

// --- concurrent changes to commit destinations ---

// TestCommit_RefusesConcurrentlyCreatedDestination pins that a destination created after inspection is
// refused, rather than treated as absent and removed by the rollback.
func TestCommit_RefusesConcurrentlyCreatedDestination(t *testing.T) {
	live := t.TempDir()
	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "new.txt"), "REMEDIATED\n")

	testHookBeforeDestinationVerify = func(rel string) {
		if rel != "new.txt" {
			return
		}
		testHookBeforeDestinationVerify = nil
		writeFile(t, filepath.Join(live, "new.txt"), "SOMEONE ELSE'S FILE\n")
	}
	t.Cleanup(func() { testHookBeforeDestinationVerify = nil })

	ws := New(t.TempDir())
	committed, err := ws.Commit(&Handle{Root: copyDir, Live: live})
	if err == nil {
		t.Fatal("a destination created inside the check→write window must be refused")
	}
	if got := ReasonOf(err); got != ReasonDestinationChanged {
		t.Errorf("reason = %q, want %q (err: %v)", got, ReasonDestinationChanged, err)
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing", committed)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "new.txt")); string(got) != "SOMEONE ELSE'S FILE\n" {
		t.Errorf("the concurrently created file was clobbered or unlinked: %q", got)
	}
}

// TestCommit_RefusesConcurrentlyReplacedDestination pins the other direction: a destination
// replaced after its backup was read would otherwise be restored to stale bytes on a later
// rollback. Identity (device and inode), not name, decides it is still the same file.
func TestCommit_RefusesConcurrentlyReplacedDestination(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "ORIGINAL\n")
	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "a.txt"), "REMEDIATED\n")

	testHookBeforeDestinationVerify = func(rel string) {
		if rel != "a.txt" {
			return
		}
		testHookBeforeDestinationVerify = nil
		// remove + recreate: same name, different inode — exactly what an editor's
		// atomic save does, and what a name comparison cannot see.
		if err := os.Remove(filepath.Join(live, "a.txt")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(live, "a.txt"), "SOMEONE ELSE'S EDIT\n")
	}
	t.Cleanup(func() { testHookBeforeDestinationVerify = nil })

	ws := New(t.TempDir())
	committed, err := ws.Commit(&Handle{Root: copyDir, Live: live})
	if err == nil {
		t.Fatal("a destination replaced inside the check→write window must be refused")
	}
	if got := ReasonOf(err); got != ReasonDestinationChanged {
		t.Errorf("reason = %q, want %q (err: %v)", got, ReasonDestinationChanged, err)
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing", committed)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "a.txt")); string(got) != "SOMEONE ELSE'S EDIT\n" {
		t.Errorf("the concurrent edit was clobbered or reverted to stale bytes: %q", got)
	}
}

// TestCommit_RefusesConcurrentlyRewrittenDestination covers a destination rewritten in place, which
// identity checks cannot see. A delete-and-recreate looks the same on filesystems that reuse inode
// numbers (such as ext4), so this test reproduces that case on every filesystem.
func TestCommit_RefusesConcurrentlyRewrittenDestination(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "ORIGINAL\n")
	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "a.txt"), "REMEDIATED\n")

	testHookBeforeDestinationVerify = func(rel string) {
		if rel != "a.txt" {
			return
		}
		testHookBeforeDestinationVerify = nil
		writeFile(t, filepath.Join(live, "a.txt"), "SOMEONE ELSE'S EDIT\n") // same inode, new bytes
	}
	t.Cleanup(func() { testHookBeforeDestinationVerify = nil })

	ws := New(t.TempDir())
	committed, err := ws.Commit(&Handle{Root: copyDir, Live: live})
	if err == nil {
		t.Fatal("a destination rewritten in place inside the check→write window must be refused")
	}
	if got := ReasonOf(err); got != ReasonDestinationChanged {
		t.Errorf("reason = %q, want %q (err: %v)", got, ReasonDestinationChanged, err)
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing", committed)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "a.txt")); string(got) != "SOMEONE ELSE'S EDIT\n" {
		t.Errorf("the concurrent edit was clobbered or reverted to stale bytes: %q", got)
	}
}

// TestCommit_RollsBackCreatedParentDirs pins that a rolled-back commit removes the directories it
// created.
func TestCommit_RollsBackCreatedParentDirs(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "zz.txt", "occupied"), "in the way\n") // zz.txt is a dir

	copyDir := t.TempDir()
	// "newdir/a.txt" sorts BEFORE "zz.txt", so its parent is created first and the
	// unwritable destination fails afterwards.
	writeFile(t, filepath.Join(copyDir, "newdir", "deeper", "a.txt"), "BRAND NEW\n")
	writeFile(t, filepath.Join(copyDir, "zz.txt"), "NEW ZZ\n")

	ws := New(t.TempDir())
	if _, err := ws.Commit(&Handle{Root: copyDir, Live: live}); err == nil {
		t.Fatal("Commit must fail when a destination cannot be written")
	}
	if _, err := os.Stat(filepath.Join(live, "newdir")); !os.IsNotExist(err) {
		t.Errorf("a directory created by a rolled-back commit must be removed again (stat err = %v)", err)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "zz.txt", "occupied")); string(got) != "in the way\n" {
		t.Errorf("the blocking destination was damaged: %q", got)
	}
}

// TestCommit_ReplacesHardlinkedDestinationWithFreshFile pins that committing over a name hardlinked to a
// file outside the workspace replaces the name and never truncates the other file.
func TestCommit_ReplacesHardlinkedDestinationWithFreshFile(t *testing.T) {
	live := t.TempDir()
	protected := filepath.Join(t.TempDir(), "id_rsa")
	writeFile(t, protected, "PRIVATE KEY MATERIAL\n")
	hardlink(t, protected, filepath.Join(live, "a.txt"))

	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "a.txt"), "REMEDIATED\n")

	ws := New(t.TempDir())
	committed, err := ws.Commit(&Handle{Root: copyDir, Live: live})
	if err != nil {
		t.Fatalf("commit over a hardlinked destination should replace the name: %v", err)
	}
	if len(committed) != 1 {
		t.Errorf("committed = %v, want [a.txt]", committed)
	}
	if got, _ := os.ReadFile(protected); string(got) != "PRIVATE KEY MATERIAL\n" {
		t.Errorf("the hardlink's other name was truncated/overwritten: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "a.txt")); string(got) != "REMEDIATED\n" {
		t.Errorf("live file = %q, want the remediated content", got)
	}
}

// TestCommit_RelativeLiveRootStillCommits guards against a false refusal: the guard canonicalizes
// against the working directory, so a relative live root such as "." must still compare equal.
func TestCommit_RelativeLiveRootStillCommits(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "ORIGINAL\n")
	res, err := scope.New(live)
	if err != nil {
		t.Fatal(err)
	}
	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, "a.txt"), "REMEDIATED\n")

	ws := New(t.TempDir())
	ws.Guard = res
	t.Chdir(live)

	committed, err := ws.Commit(&Handle{Root: copyDir, Live: "."})
	if err != nil {
		t.Fatalf("commit with a relative live root = %v, want success", err)
	}
	if len(committed) != 1 {
		t.Fatalf("committed = %v, want [a.txt]", committed)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "a.txt")); string(got) != "REMEDIATED\n" {
		t.Errorf("live file = %q, want the remediated content", got)
	}
}

// TestCommit_NestedSecretPathRefused pins end to end that a staged file with an innocuous name inside a
// `.env` directory is refused by the write guard.
func TestCommit_NestedSecretPathRefused(t *testing.T) {
	live := t.TempDir()
	res, err := scope.New(live)
	if err != nil {
		t.Fatal(err)
	}
	ws := New(t.TempDir())
	ws.Guard = res

	copyDir := t.TempDir()
	writeFile(t, filepath.Join(copyDir, ".env", "secret.txt"), "SECRET=1\n")

	committed, cerr := ws.Commit(&Handle{Root: copyDir, Live: live})
	if cerr == nil {
		t.Fatal("committing x/.env/secret.txt must be refused")
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing", committed)
	}
	if _, serr := os.Stat(filepath.Join(live, ".env", "secret.txt")); !os.IsNotExist(serr) {
		t.Error("the nested secret path was written to the live tree")
	}
	if !strings.Contains(cerr.Error(), "commit") {
		t.Errorf("error should name the operation: %v", cerr)
	}
}
