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

// --- F2: a denied ROOT is a refusal, not a self-exception ---

// TestCopy_RefusesDeniedRoot pins that a protected directory handed in as the review
// target is refused outright. Judging only its CHILDREN by their own names ("production",
// "staging") is exactly how the contents of a `.env` directory reach a model.
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
// with the root itself denied, NO content may come back — not the root, not its children.
func TestCollectSnippets_RefusesDeniedRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".env")
	writeFile(t, filepath.Join(root, "production"), "SECRET=prod\n")

	if snips := CollectSnippets(root); len(snips) != 0 {
		t.Errorf("CollectSnippets on a denied root returned %d snippets: %+v", len(snips), snips)
	}
	got, err := CollectSnippetsChecked(root)
	if err == nil {
		t.Fatal("CollectSnippetsChecked on a denied root must refuse")
	}
	if len(got) != 0 {
		t.Errorf("a refusal must return no content, got %+v", got)
	}
	if r := ReasonOf(err); r != ReasonRootDenied {
		t.Errorf("reason = %q, want %q", r, ReasonRootDenied)
	}
}

// --- H1: an excluded/protected ANCESTOR is lost when the root is chosen beneath it ---

// protectedRoots are the shapes an operator can name to strip the copy-exclusion rule by
// choosing a root INSIDE a protected directory. `.vscode/mcp.json` is the lead case: it
// routinely carries MCP server env vars (API keys) and is read-ALLOWED, so the read
// denylist (which catches `.env`/`.ssh`) never fires for it.
//
// `.aimesh` is deliberately NOT here — see TestCopy_AimeshRunsAreReadable.
var protectedRoots = []string{".vscode", ".git", ".claude", ".cursor"}

// TestCopy_RefusesRootInsideProtectedAncestor is the H1 blocker proof. It needs no race at
// all: exclusion is judged on components RELATIVE to the workspace root, so naming a
// DESCENDANT of an excluded directory as the root makes its contents ordinary relative
// files and walks them into a model prompt.
func TestCopy_RefusesRootInsideProtectedAncestor(t *testing.T) {
	for _, dir := range protectedRoots {
		t.Run(dir, func(t *testing.T) {
			base := t.TempDir()
			// The root is a DESCENDANT of the protected directory — its own basename
			// ("workspace") is entirely innocent, which is the whole point.
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
			// The protected directory ITSELF is refused with the same typed reason.
			if got := ReasonOf(mustFailCopy(t, ws, filepath.Join(base, dir))); got != ReasonExcludedAncestor {
				t.Errorf("reason for the protected dir itself = %q, want %q", got, ReasonExcludedAncestor)
			}
		})
	}
}

// TestCopy_AimeshRunsAreReadable is the deliberate exception, and it is about USABILITY.
//
// `.aimesh/{review,explore}/runs/**` is where every run artifact lands. Reading a previous run is
// ordinary work — some review and exploration modes exist to do exactly that — so refusing any
// root beneath `.aimesh` made this tool's own output unreachable, and required a flag to do a
// thing that should never have needed one.
//
// Two properties keep that safe, and both are asserted here: `.aimesh` is still WRITE-denied, so
// nothing model-authored edits the config that lives beside the runs; and it is still EXCLUDED
// from a walk, so reviewing a PROJECT does not drag its run artifacts into the payload. Naming a
// run directory and walking past one are different acts, and only the second needed refusing.
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

	// STILL WRITE-DENIED. The runs are readable; the state is not writable.
	if rule := scope.DeniedWrite(filepath.Join(runDir, "result.json")); rule == "" {
		t.Error(".aimesh became WRITABLE — only the root refusal was meant to go")
	}
	// STILL EXCLUDED FROM A WALK. Reviewing the project above it must not collect the artifacts.
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
	for _, s := range CollectSnippets(root) {
		t.Errorf("the legacy entry point must fail closed; got %q: %q", s.Path, s.Content)
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
	// A symlinked TARGET is refused as a reparse point before the ancestor rule is reached;
	// what must not happen is that the alias makes the protected ancestor invisible. Point
	// at a path THROUGH the alias so the target itself is an ordinary directory.
	writeFile(t, filepath.Join(real, "sub", "x.json"), "{}\n")
	if err := ReasonOf(mustFailCopy(t, ws, filepath.Join(alias, "sub"))); err != ReasonExcludedAncestor {
		t.Errorf("reason = %q, want %q", err, ReasonExcludedAncestor)
	}
}

// TestCopy_AllowsRootUnderGenericExcludedAncestor is the false-refusal guard for H1. The
// ancestor rule covers only names that are protected BY CONTENT; the generic build/artifact
// names must keep working, or every root under `/tmp` (and every `.../build/myrepo`
// checkout) becomes unreviewable — an outage, not a confinement layer.
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

// --- H2: os.Root hardens interior traversal, not BOUNDARY selection ---

// TestCopy_RefusesRootSwappedAfterCheck pins the identity binding. Every containment check
// runs against a PATH; the boundary is opened from that same path afterwards. Swap the
// approved directory for a symlink to an attacker tree in between and os.Root then confines
// every interior read faithfully — to the attacker's tree. The window is entered
// deterministically through the package's test seam rather than raced.
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
// swap decides which tree is READ into a model prompt.
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

// --- F3: client/IDE execution config is excluded from the COPY, not only write-denied ---

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

// --- F6/H4: a hardlink defeats basename-based exclusion — refuse the FILE, not the RUN ---

// TestCopy_SkipsHardlinkedFileWithCaveat pins the H4 policy. A regular in-root file with an
// innocuous name and more than one link is never copied — the OTHER name may be
// `~/.ssh/id_rsa`, and every name-based rule in this package is blind to it. But it is
// refused as a FILE, not as a RUN: any ordinary Unix tree with two legitimate names for a
// file (a `cp -al` tree, a dedup store, a hardlinked fixture) would otherwise kill the
// whole review, and a writer inside a trusted tree could force that halt at will.
//
// The exclusion is never SILENT: it is recorded as a Caveat the caller can surface.
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

// TestCopy_RefusesHardlinkedSingleFileTarget pins the OTHER side of the H4 boundary: when
// the hardlinked file is the EXPLICIT target of the operation, the hard refusal stays.
// Withholding it would leave nothing to review, so a caveat would be a success return over
// an empty copy — the silent-omission shape this package refuses everywhere else.
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
	// The legacy entry points keep working and simply do not carry the caveat.
	if snips := CollectSnippets(root); len(snips) != 1 {
		t.Errorf("CollectSnippets = %+v, want just main.go", snips)
	}
}

// --- H7: NTFS alternate-data-stream aliasing ---

// TestExclusion_NTFSStreamAliasing pins that on NTFS a stream suffix cannot alias a
// component past the exclusion set: `.git::$DATA` and `.vscode:x:$DATA` resolve to the real
// `.git` / `.vscode`, while an exact lookup of the untrimmed component matches no rule.
//
// The trim is Windows-only and this test states BOTH platforms' expectations, because
// applying it on unix would be a LOOSENING there: `:` is an ordinary filename character, so
// a real file named `report:.pem` would stop matching the key-material rule.
func TestExclusion_NTFSStreamAliasing(t *testing.T) {
	aliased := []string{`.git::$DATA/config`, `.vscode:x:$DATA/mcp.json`, `a/.claude::$DATA/settings.json`}

	// Both platforms' expectations are exercised on EVERY platform by toggling the gate;
	// asserting only the host's semantics would make this test vacuous off Windows, where
	// it happens to be developed.
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

// --- F5: root-relative traversal (os.Root), not path-string re-opening ---

// TestCommit_RefusesSymlinkedLiveDirectory is the deterministic escape proof. The live
// tree contains a PRE-PLANTED symlink `sub` pointing outside it — the same shape a
// component swapped between check and use produces, with the window held open so the
// outcome is observable rather than timing-dependent. Re-opening `live/sub/x.txt` by
// STRING creates the parent through the link and writes outside the workspace; a
// root-relative write refuses to leave the root.
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

// TestCommit_RefusesSymlinkedDestination pins that a live destination that is itself a
// symlink is a REFUSAL, not a silent skip. The previous code recognized the reparse point
// and `continue`d, returning success while quietly dropping the file — the caller then
// recorded a clean apply for a remediation that never landed.
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

// --- the reparse guard, made OBSERVABLE (test-strength sweep) ---
//
// A test-strength sweep found that `isReparse` could be neutered wholesale — every guard in
// this package turned off — and the suite stayed green. Not because symlinks then escaped:
// each site has a deeper POSIX gate (an `os.Root` boundary, a no-follow open, an IsRegular
// filter) that refuses anyway. But "refused somewhere, for some reason" is not the contract.
// The reparse guard's job is to refuse with a DIAGNOSABLE, typed answer — and on Windows,
// where a junction/mount point carries the reparse ATTRIBUTE without the symlink mode bit,
// `hasReparseAttr` is the ONLY thing that recognizes it at all.
//
// The two tests below therefore assert the IDENTITY of the refusal, not merely its existence.
// Both fail when `isReparse` is neutered.

// TestCopy_SymlinkTargetIsRefusedAsAReparsePoint pins that a symlink handed in as the review
// TARGET is refused by the reparse rule itself, naming what it is. Without that guard the
// copy proceeds until a deeper no-follow open fails, and the operator is told "copy failed"
// about a path that is simply a link.
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

// TestCommit_SymlinkedDestinationCarriesTheTypedReparseReason pins the machine code on the
// write path. A commit refusal is classified by a caller (reviewmesh maps ReasonReparse to
// "the live destination is no longer what the review judged"), so a refusal that arrives
// with a different reason — or with none — is a different outcome even though both fail.
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

// TestCopy_SkipsAReparseAttributeEntryThatLooksLikeAnOrdinaryFile is the Windows JUNCTION
// case, simulated on any platform.
//
// It exists because the sweep showed the copy walk's `isReparse` guard could be deleted with
// the suite still green: a POSIX symlink is caught one line later by the `IsRegular` filter,
// so on unix the guard is redundant and no test could tell. A Windows junction is the case
// where it is NOT redundant — it carries the reparse ATTRIBUTE without the symlink mode bit,
// so it presents to every other check in this package as an ordinary entry, and following it
// copies content from outside the workspace into the tree a model is pointed at.
//
// Substituting `reparseAttr` produces exactly that entry: a plain regular file the attribute
// probe reports as a reparse point. Deleting the guard makes this test copy it.
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

// TestApplyEdit_RefusesSymlinkInCopy pins that an edit is applied through a root handle on
// the COPY. A contained reviewer can plant a symlink in its own copy; reading and writing
// that path by STRING follows it straight out of the temp area and edits the real file.
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

// --- F7: Commit is transactional ---

// TestCommit_RollsBackEarlierWritesOnFailure pins that a mid-sequence failure leaves the
// live tree exactly as it was. Authorization being all-before-write does not help here:
// the WRITES are sequential, and the second destination cannot be written (it is a
// non-empty directory), so the first must be put back.
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
// that did NOT exist before the commit must not survive it.
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

// TestCommit_UnreadableStagedFileIsError pins that a staged file which cannot be read is
// an ERROR. The previous code read staged files by STRING and silently `continue`d on
// failure: a symlink planted in the copy was therefore FOLLOWED — the content of an
// out-of-workspace file was read and committed into the live tree under the link's name.
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
	// The REASON is pinned, not merely "an error": a caller classifies on the machine code
	// and persists it in the audit record, so "some error happened" is not the contract.
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

// --- H3: an unreadable staged SUBTREE must not vanish silently ---

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

// TestCommit_UnenumerableStagedDirectoryIsError pins H3. Round 1 made an unreadable staged
// FILE an error, but ENUMERATION errors were still swallowed: every file beneath an
// unreadable staged directory simply never entered the set, so Commit reported success
// without ever having considered them — the half-applied outcome wearing a success return.
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
// a read-only copy whose subtree cannot be listed is a copy whose integrity cannot be
// PROVEN, so it fails closed rather than reporting a clean discard.
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

// --- H5: rollback is compensating — make its limits explicit and safer ---

// TestCommit_RefusesConcurrentlyCreatedDestination pins the case the old rollback got
// exactly backwards: a destination that appears AFTER the pre-write inspection was recorded
// as "did not exist" and then UNLINKED by the rollback — a commit destroying a file it
// never inspected. It must refuse instead, and leave the other file's bytes alone.
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
// REPLACED after its backup was read would otherwise be restored to stale bytes on a later
// rollback. Identity (device+inode), not name, is what decides it is still the same file.
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

// TestCommit_RollsBackCreatedParentDirs pins that a rolled-back commit leaves no DIRECTORY
// it invented either. The old rollback restored/removed files only, so a failed commit left
// a fresh directory tree behind — visible, committable, and not what the caller was told.
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

// TestCommit_ReplacesHardlinkedDestinationWithFreshFile pins the write side of F6: the
// unlink before a write is REQUIRED, so committing over a name that is hardlinked to a
// file outside the workspace replaces the NAME and never truncates the other name's file.
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

// TestCommit_RelativeLiveRootStillCommits guards the destination/authorization comparison
// against a false refusal: a guard canonicalizes against the working directory, so a
// RELATIVE live root (the shape `review .` produces) must still compare equal.
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

// TestCommit_NestedSecretPathRefused ties F1 to the write path end to end: a staged file
// whose innocuous basename sits inside a `.env` directory is refused by the guard.
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
