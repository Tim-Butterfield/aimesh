// Package workspace is the WorkspaceAccess layer: it provides isolated copies of
// the files under review (containment), applies edits to a copy, diffs a copy
// against the live tree, commits a copy back (apply mode), and discards a copy
// with unexpected-mutation (M5) detection.
//
// # What Commit's rollback guarantees, and what it does not
//
// Commit is COMPENSATING, not transactional. It is honest about the difference because a
// caller that believes otherwise will build an audit claim on it.
//
// It DOES guarantee, for a commit that fails partway:
//   - every destination it replaced is written back with the bytes and the permission bits
//     it read immediately beforehand;
//   - every destination it created is removed again;
//   - every parent directory it created is removed again (best effort: a directory that
//     has since acquired somebody else's file is left alone rather than destroyed);
//   - no destination is replaced unless it is still, by identity (device+inode, not name),
//     the object that was inspected and backed up — and no destination is created unless it
//     is still absent. A concurrent change is a REFUSAL, so the commit fails instead of
//     clobbering something it never looked at.
//   - for CommitExpecting, additionally: no destination is replaced unless its CONTENT still
//     digests to the pin the caller recorded when it decided what to write. Identity alone
//     cannot see an in-place save (an editor rewriting a file keeps its inode), so a caller
//     with a decision window — read the file, think, write it back — needs the content pin
//     to turn "your concurrent edit was silently discarded" into a refusal.
//
// It does NOT guarantee:
//   - cross-process atomicity. There is no filesystem lock. The re-verification above
//     narrows the window between "check" and "replace" to a few syscalls, but another
//     process that writes inside that window is neither prevented nor detected. This is the
//     irreducible residue: closing it needs an exclusive lock (or a filesystem that offers
//     compare-and-swap), and this package deliberately does not pretend to have one. What
//     the pins remove is the MINUTES-wide window between a caller's decision and its write;
//     what remains is a few syscalls.
//   - restoration of ownership (uid/gid), timestamps, extended attributes, ACLs, or any
//     other metadata beyond the permission bits. A rolled-back file has its original
//     content and mode; its mtime is the rollback's.
//   - anything at all once the ROLLBACK itself fails. That case is reported as an error
//     naming both failures; the live tree is then in a state only a human can judge.
//   - protection against a concurrent writer between the rollback and the caller's next
//     read.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// WriteGuard authorizes a LIVE-tree write target. Access consults it (when set) before
// applying an edit to a copy and before committing a copy back, passing the path in the
// LIVE tree the write is ultimately destined for — not the temp-copy path, which is
// outside every caller-consented root by construction.
//
// *scope.Resolver satisfies this. Keeping it an interface holds the containment layer
// domain-free and lets a caller compose additional policy.
type WriteGuard interface {
	ResolveWrite(path string) (string, error)
}

// Access creates and manages isolated copies under a temp base directory.
type Access struct {
	base string
	// Guard, when set, is the authoritative check on every write target. It is the
	// LAST line of the write path: a caller is expected to check earlier (so a refusal
	// can halt with a full audit record), and this guard exists so no future code path
	// can reach a live write without passing the same rule.
	Guard WriteGuard
	// AllowProtectedRoots waives the protected-ANCESTOR refusal when admitting a root,
	// matching scope.Options.AllowProtectedWrites on the Guard. The two travel together:
	// admitting the root without the write waiver would copy a tree nothing may edit, and
	// the write waiver without root admission would never reach a file to edit.
	//
	// It does NOT waive rule 1 of rootRefusal — a root that IS or sits inside a SECRET
	// path stays refused, because that rule protects a read and a read reaches a vendor.
	AllowProtectedRoots bool
}

// New returns a WorkspaceAccess rooted at baseDir (a temp area), with every root rule in
// force. Set AllowProtectedRoots afterwards for the operator-granted relaxation.
func New(baseDir string) *Access { return &Access{base: baseDir} }

// authorizeWrite runs the configured guard against the LIVE path a workspace-relative
// target maps to. No guard = no additional restriction (the historical behavior).
func (a *Access) authorizeWrite(h *Handle, rel string) error {
	if a == nil || a.Guard == nil || h == nil {
		return nil
	}
	_, err := a.Guard.ResolveWrite(filepath.Join(h.Live, rel))
	return err
}

// authorizeCommit is authorizeWrite plus the check the previous code skipped: the guard
// returns the CANONICAL path it approved, and a commit must write to exactly that path.
// Discarding it and writing the original string is how an intermediate symlinked
// directory gets written through — the guard resolved `live/sub/f` to somewhere else
// entirely and the writer never noticed. liveCanon is the canonicalized live root.
func (a *Access) authorizeCommit(h *Handle, liveCanon, rel string) error {
	if a == nil || a.Guard == nil || h == nil {
		return nil
	}
	got, err := a.Guard.ResolveWrite(filepath.Join(h.Live, rel))
	if err != nil {
		return err
	}
	if got == "" || liveCanon == "" {
		return nil // a guard that returns no path (a custom policy) keeps the old contract
	}
	want := filepath.Join(liveCanon, rel)
	if !samePath(got, want) {
		return &Refusal{
			Op: "commit", Path: rel, Reason: ReasonPathMismatch,
			Rule:   "the authorized canonical path must be the destination under the live root",
			Detail: fmt.Sprintf("authorized %q, destination %q", got, want),
		}
	}
	return nil
}

// foldPaths is true only on Windows, where the filesystem itself is case-insensitive. It is a
// VAR for the reason streamAliasing and reparseAttr are: this repository does not gate on
// Windows, so an inline `runtime.GOOS ==` would leave the containment branch compiled
// everywhere and executed nowhere. Substituting it exercises the branch on any platform.
var foldPaths = runtime.GOOS == "windows"

// samePath compares two canonical paths, folding case only on Windows.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if foldPaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Caveat records a path that was deliberately left OUT of a copy or a snippet set for a
// containment reason. It exists so an exclusion is never SILENT: the caller can surface
// the list in the run record, so "this file was withheld" is a stated fact rather than an
// absence nobody can distinguish from "there was no such file".
type Caveat struct {
	Path   string // workspace-relative
	Reason Reason
	Rule   string
	Detail string
}

func (c Caveat) String() string {
	s := fmt.Sprintf("%s [%s]", c.Path, c.Reason)
	if c.Rule != "" {
		s += ": rule " + c.Rule
	}
	if c.Detail != "" {
		s += ": " + c.Detail
	}
	return s
}

// caveatFor turns a per-file containment refusal into a Caveat.
func caveatFor(rel string, err error) Caveat {
	if r, ok := AsRefusal(err); ok {
		return Caveat{Path: filepath.ToSlash(rel), Reason: r.Reason, Rule: r.Rule, Detail: r.Detail}
	}
	return Caveat{Path: filepath.ToSlash(rel), Detail: err.Error()}
}

// Handle identifies an isolated copy and its live source.
type Handle struct {
	Root string // the copy root (temp)
	Live string // the live source root
	// Caveats lists files that were withheld from this copy for a containment reason
	// (currently: hardlinked regular files). It is the caller's to surface in the run
	// record — see Caveat.
	Caveats  []Caveat
	readOnly bool              // reviewer copies are read-only (mutation → M5)
	snapshot map[string]string // rel path → sha at copy time
}

// Copy makes an isolated copy of live (a dir or a single file). readOnly marks a
// reviewer copy whose later mutation is an M5 breach.
func (a *Access) Copy(live string, readOnly bool, label string) (*Handle, error) {
	info, err := os.Lstat(live)
	if err != nil {
		return nil, fmt.Errorf("stat workspace %q: %w", live, err)
	}
	if isReparse(info) {
		return nil, fmt.Errorf("refusing to review a symlink/reparse-point target %q", live)
	}
	// The ROOT is judged by the denylist and by the protected-ancestor rule, on its
	// ABSOLUTE path. See rootRefusal: a root self-exception, or a root chosen INSIDE a
	// protected directory, is how a protected subtree reaches a model. This runs BEFORE
	// the basename rule below so a protected target is refused with the typed reason an
	// audit record can classify, rather than with the generic message.
	if ref := rootRefusal("copy", live, a.AllowProtectedRoots); ref != nil {
		return nil, ref
	}
	// Refuse a target that is itself an excluded/internal path (or a file directly inside
	// one) by NAME — this is the remaining, generic half (`tmp`, `node_modules`, `dist`).
	if excludedTarget(live, info.IsDir()) {
		return nil, fmt.Errorf("refusing to review excluded/internal path %q", live)
	}
	root, err := os.MkdirTemp(a.base, "rmcopy-"+label+"-")
	if err != nil {
		return nil, fmt.Errorf("create isolated copy: %w", err)
	}
	// liveRoot is always a directory; for a single-file target it is the parent,
	// so Diff/Commit can join workspace-relative paths against it correctly.
	// wantRoot is the identity the os.Root boundary must have: for a directory target it
	// is the very object every check above ran against, so the checks and the opened
	// boundary cannot come apart (see openRootBound).
	liveRoot, wantRoot := live, info
	if !info.IsDir() {
		liveRoot = filepath.Dir(live)
		// The parent was not itself checked, and may legitimately BE a symlink (a
		// single-file target reached through a linked directory), so its identity is
		// captured with a following stat rather than an Lstat.
		wantRoot, err = os.Stat(liveRoot)
		if err != nil {
			_ = os.RemoveAll(root)
			return nil, fmt.Errorf("stat workspace root %q: %w", liveRoot, err)
		}
	}
	caveats, err := a.fillCopy(live, liveRoot, root, info, wantRoot)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	h := &Handle{Root: root, Live: liveRoot, readOnly: readOnly, Caveats: caveats}
	snap, err := hashTree(root)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("snapshot isolated copy: %w", err)
	}
	h.snapshot = snap
	return h, nil
}

// AdmitRoot judges whether a workspace ROOT would be accepted for a copy, and does NOTHING else:
// no directory is created, nothing is read, nothing is copied. It returns the same *Refusal the
// copy itself would return, or nil.
//
// It exists for a caller that must answer "could this run even start?" before committing to the
// run — a review dry run. Without it, such a caller reports a tidy plan for a workspace that the
// very next step refuses: measured 2026-08-11, `--dry-run` on a directory under a protected path
// printed a full panel and a model-call range for a root that `ProvideCopy` then rejected outright.
//
// It calls the SAME rootRefusal the copy path calls, deliberately. The rule it enforces is subtle
// — exclusion judged on components relative to the root, so that naming a protected directory AS
// the root cannot strip its protection — and a second implementation of it is exactly the kind of
// thing that would come to disagree with the first.
func AdmitRoot(root string) *Refusal { return rootRefusal("copy", root, false) }

// AdmitRootWith is AdmitRoot with the operator's protected-ancestor waiver applied. The
// SECRET rule still refuses regardless — see rootRefusal.
func AdmitRootWith(root string, allowProtected bool) *Refusal {
	return rootRefusal("copy", root, allowProtected)
}

// rootRefusal judges a workspace ROOT (the path a caller consented to) by its own absolute
// path, and returns the typed refusal or nil.
//
// Two rules, in order of severity:
//
//  1. The READ denylist, on every component. When the root IS a protected path (a `.env`
//     directory, a `.ssh` directory), its children are not innocent just because their own
//     names are — the whole subtree is the secret.
//  2. The protected-ANCESTOR rule. The copy-exclusion families (`.git`, `.vscode`,
//     `.claude`, `.cursor`, `.aimesh`, …) are read-ALLOWED, so rule 1 never fires for them;
//     without this rule, naming a DESCENDANT of one as the root strips the protection
//     entirely — root `/trusted/.vscode` makes `mcp.json` an ordinary relative file, and
//     `.vscode/mcp.json` routinely holds API keys.
//
// Both rules are applied to every form of the root (as given, absolute, symlink-resolved),
// so neither a relative spelling nor a symlink alias can walk past a component rule.
//
// allowProtected waives rule 2 ONLY. Rule 1 is unconditional: it is the secret family, and
// admitting a `.ssh` root would put key material into a prompt, which no operator intent
// can undo once the vendor has it. Rule 2 is protection against a write, and an operator
// who names such a root is asking to edit a tree they own — see scope's package comment.
func rootRefusal(op, root string, allowProtected bool) *Refusal {
	forms := rootForms(root)
	for _, f := range forms {
		if rule := scope.DeniedRead(f); rule != "" {
			return &Refusal{Op: op, Path: root, Reason: ReasonRootDenied, Rule: rule}
		}
	}
	if allowProtected {
		return nil
	}
	for _, f := range forms {
		if rule := protectedAncestorRule(f); rule != "" {
			return &Refusal{
				Op: op, Path: root, Reason: ReasonExcludedAncestor, Rule: rule,
				// NAME THE WAY PAST IT. Every other containment refusal names its remedy flag and
				// this one did not, so the person most likely to hit it — someone whose own dotfiles
				// or agent config IS the tree they want reviewed, which is exactly who the flag was
				// built for — was told the rule and left to guess that an override exists at all.
				Detail: "the workspace root is, or sits inside, a protected path; exclusion is judged on components RELATIVE to the root, so such a root would strip the protection. If this tree is yours and reviewing it is the point (your own hooks, your own agent config), pass --allow-protected-paths",
			}
		}
	}
	return nil
}

// fillCopy populates the freshly created copy root from the live target. Both sides are
// opened as os.Root handles ONCE and every subsequent operation is root-relative, so no
// component of either tree is re-resolved by string after it was checked. The source root
// is bound to wantRoot: the boundary that gets opened must be the object the containment
// checks approved.
func (a *Access) fillCopy(live, liveRoot, dst string, info, wantRoot os.FileInfo) ([]Caveat, error) {
	srcRoot, err := openRootBound("copy", liveRoot, wantRoot)
	if err != nil {
		return nil, fmt.Errorf("open workspace root %q: %w", liveRoot, err)
	}
	defer srcRoot.Close()
	dstRoot, err := openRootDir(dst)
	if err != nil {
		return nil, fmt.Errorf("open isolated copy root: %w", err)
	}
	defer dstRoot.Close()
	if info.IsDir() {
		return copyTree(srcRoot, dstRoot)
	}
	// A single-file target is the EXPLICIT object of the operation: a containment refusal
	// on it is a refusal of the whole call, never a caveat, because there is nothing left
	// to review once it is withheld.
	return nil, copyRegular(srcRoot, dstRoot, "copy", info.Name())
}

// Abs resolves a workspace-relative path within the copy.
func (h *Handle) Abs(rel string) string { return filepath.Join(h.Root, rel) }

// ApplyEdit applies one Edit to the copy. An empty Anchor prepends Replacement;
// otherwise it replaces the anchor text. A missing anchor is a non-fatal error
// the caller records against the finding.
func (a *Access) ApplyEdit(h *Handle, e core.Edit) error {
	if !safeRel(e.File) || IsExcluded(e.File) {
		return fmt.Errorf("apply edit: refusing excluded/escaping path %q", e.File)
	}
	// Protected-path / root confinement, evaluated against the LIVE destination. This is
	// checked even in patch mode: a patch artifact naming a protected file is already the
	// dangerous thing (it is what a human or a later apply would replay).
	if err := a.authorizeWrite(h, e.File); err != nil {
		return fmt.Errorf("apply edit: %w", err)
	}
	// The edit is applied through a root handle on the COPY: the file name comes from a
	// model, and a copy is a place a contained reviewer can plant things. os.Root refuses
	// any component that leaves the copy, and the no-follow open refuses a planted symlink
	// at the target itself.
	copyRoot, err := openRootDir(h.Root)
	if err != nil {
		return fmt.Errorf("apply edit: open copy root: %w", err)
	}
	defer copyRoot.Close()
	b, err := readRegular(copyRoot, "edit", e.File)
	if err != nil {
		return fmt.Errorf("apply edit: read %q: %w", e.File, err)
	}
	content := string(b)
	// Adapt the replacement's line endings to the target file's dominant convention, so a
	// hardcoded-LF insertion (e.g. the remediation marker) into a CRLF source does not
	// produce a mixed-ending file. Only the replacement's newlines are rewritten; the
	// anchor is matched against the original bytes EXACTLY (never normalized), and all
	// unchanged bytes are preserved verbatim.
	repl := adaptLineEnding(e.Replacement, DetectLineEnding(content))
	var out string
	switch {
	case e.Anchor == "":
		out = repl + content
	case !strings.Contains(content, e.Anchor):
		return fmt.Errorf("apply edit: anchor not found in %q", e.File)
	case e.Occurrence <= 0: // 0 = all/unique
		out = strings.ReplaceAll(content, e.Anchor, repl)
	default: // replace the Nth (1-based) occurrence
		out = replaceNth(content, e.Anchor, repl, e.Occurrence)
		if out == content {
			return fmt.Errorf("apply edit: occurrence %d of anchor not found in %q", e.Occurrence, e.File)
		}
	}
	if err := writeThroughRoot(copyRoot, "edit", e.File, []byte(out), 0o644); err != nil {
		return fmt.Errorf("apply edit: write %q: %w", e.File, err)
	}
	return nil
}

// --- grouped edits: staging inside the isolated copy ---
//
// ApplyEdit is one hunk. A caller that applies a GROUP of hunks as a unit (all of them or
// none) needs a way to undo the group's earlier hunks when a later one fails — otherwise a
// group reported "not applied" still leaves its successful hunks in the copy, and the copy
// is what a commit ships. FileSnapshot/RestoreFiles are that undo. They operate ONLY on the
// isolated copy: nothing here touches the live tree.

// FileSnapshot is the recorded content of specific files inside an isolated copy, taken
// before a group of edits so the group can be rolled back as a unit. It is opaque: its
// only use is RestoreFiles.
type FileSnapshot struct {
	files []snapshotFile
}

type snapshotFile struct {
	rel    string
	exists bool
	data   []byte
	perm   fs.FileMode
}

// Len reports how many files the snapshot holds (so a caller can record that it staged).
func (s *FileSnapshot) Len() int {
	if s == nil {
		return 0
	}
	return len(s.files)
}

// SnapshotFiles records the current content of rels inside the copy. A path that does not
// exist is recorded as absent (RestoreFiles then removes whatever was created at it). Every
// path is read through the copy's root handle under the same no-follow/regular-file rules as
// every other read in this package, so a planted symlink cannot redirect the snapshot.
func (a *Access) SnapshotFiles(h *Handle, rels []string) (*FileSnapshot, error) {
	if h == nil {
		return nil, fmt.Errorf("snapshot: no copy handle")
	}
	copyRoot, err := openRootDir(h.Root)
	if err != nil {
		return nil, fmt.Errorf("snapshot: open copy root: %w", err)
	}
	defer copyRoot.Close()
	snap := &FileSnapshot{}
	seen := map[string]bool{}
	for _, rel := range rels {
		if !safeRel(rel) || IsExcluded(rel) {
			return nil, fmt.Errorf("snapshot: refusing excluded/escaping path %q", rel)
		}
		if seen[pinKey(rel)] {
			continue
		}
		seen[pinKey(rel)] = true
		sf := snapshotFile{rel: rel, perm: 0o644}
		switch fi, lerr := copyRoot.Lstat(toRootPath(rel)); {
		case lerr != nil:
			// absent: the undo is a removal
		case isReparse(fi) || !fi.Mode().IsRegular():
			return nil, &Refusal{
				Op: "snapshot", Path: rel, Reason: ReasonNotRegular,
				Rule: "only regular files can be staged for a grouped edit",
			}
		default:
			data, perm, rerr := readBackup(copyRoot, "snapshot", rel)
			if rerr != nil {
				return nil, fmt.Errorf("snapshot: read %q: %w", rel, rerr)
			}
			sf.exists, sf.data, sf.perm = true, data, perm
		}
		snap.files = append(snap.files, sf)
	}
	return snap, nil
}

// RestoreFiles puts a snapshot back, undoing every edit made to those files since it was
// taken. A failure is reported rather than swallowed: a copy that could not be restored is
// in a state the caller never recorded an intent for, and shipping it would be worse than
// failing.
func (a *Access) RestoreFiles(h *Handle, snap *FileSnapshot) error {
	if snap == nil || len(snap.files) == 0 {
		return nil
	}
	if h == nil {
		return fmt.Errorf("restore: no copy handle")
	}
	copyRoot, err := openRootDir(h.Root)
	if err != nil {
		return fmt.Errorf("restore: open copy root: %w", err)
	}
	defer copyRoot.Close()
	var firstErr error
	for i := len(snap.files) - 1; i >= 0; i-- {
		f := snap.files[i]
		var rerr error
		if f.exists {
			rerr = writeThroughRoot(copyRoot, "restore", f.rel, f.data, f.perm)
		} else {
			rerr = rollbackRemove(copyRoot, f.rel)
		}
		if rerr != nil && firstErr == nil {
			firstErr = fmt.Errorf("restore %q: %w", f.rel, rerr)
		}
	}
	return firstErr
}

// DetectLineEnding returns the dominant line ending of s: "\r\n" when CRLF strictly
// outnumbers bare LF, else "\n" (LF-dominant, tie, empty, or no newline).
func DetectLineEnding(s string) string {
	crlf := strings.Count(s, "\r\n")
	lf := strings.Count(s, "\n") - crlf // LF not part of a CRLF
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

// adaptLineEnding rewrites the newlines in replacement text to `ending`, first normalizing
// any CRLF/CR in the replacement to LF. It only touches newline characters — text with no
// newline is returned unchanged (so a no-newline replacement is never modified).
func adaptLineEnding(replacement, ending string) string {
	norm := strings.ReplaceAll(replacement, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	if ending == "\r\n" {
		return strings.ReplaceAll(norm, "\n", "\r\n")
	}
	return norm
}

// FileChange summarizes a changed file (for the patch summary).
type FileChange struct {
	File      string
	EditCount int
}

// Diff returns a unified diff of the copy vs the live tree, plus the changed files.
func (a *Access) Diff(h *Handle) (string, []FileChange, error) {
	var diffs []string
	var changes []FileChange
	copyRoot, err := openRootDir(h.Root)
	if err != nil {
		return "", nil, fmt.Errorf("diff: open copy root: %w", err)
	}
	defer copyRoot.Close()
	liveRoot, err := openRootDir(h.Live)
	if err != nil {
		return "", nil, fmt.Errorf("diff: open live root: %w", err)
	}
	defer liveRoot.Close()
	cur, err := hashTree(h.Root)
	if err != nil {
		return "", nil, fmt.Errorf("diff: enumerate copy: %w", err)
	}
	for _, rel := range sortedKeys(cur) {
		if IsExcluded(rel) {
			continue
		}
		// Both sides are read root-relative and no-follow. An unreadable/absent side is an
		// empty side here (a diff is informational); Commit is where unreadable is a halt.
		liveContent, _ := readRegular(liveRoot, "diff", rel)
		copyContent, _ := readRegular(copyRoot, "diff", rel)
		if string(liveContent) == string(copyContent) {
			continue
		}
		oldL, newL := splitLines(string(liveContent)), splitLines(string(copyContent))
		// CRLF-aware: a pure LF↔CRLF reformat (identical logical lines) is not a content
		// change — skip it so the patch artifact/summary doesn't report an empty-hunk
		// spurious change.
		if slices.Equal(oldL, newL) {
			continue
		}
		diffs = append(diffs, unifiedDiff(rel, oldL, newL))
		changes = append(changes, FileChange{File: rel, EditCount: 1})
	}
	return strings.Join(diffs, ""), changes, nil
}

// Commit copies changed files from the copy back to the live tree (apply mode).
//
// Every changed path is authorized against the write guard BEFORE anything is written:
// a refusal aborts the whole commit with nothing applied, rather than writing part of the
// set and then refusing (a half-applied remediation is worse than none).
//
// Authorization being all-before-write is not enough on its own, because the WRITES are
// still sequential and a mid-sequence failure (a destination that is a non-empty
// directory, a full disk, a revoked permission) would leave the earlier files applied.
// Commit therefore keeps a rollback copy of every destination it is about to replace and
// restores all of them on ANY failure, so the live tree only ever moves from
// "nothing applied" to "everything applied".
//
// A staged file that cannot be read — or a staged DIRECTORY that cannot be listed — is an
// ERROR, never a silent skip: dropping part of the reviewed set while returning success is
// exactly the half-applied outcome the all-or-nothing rule exists to prevent.
//
// What the rollback does and does not restore is stated precisely in the package doc.
//
// Commit performs NO content pinning. A caller whose staged bytes were derived from a
// snapshot of the live tree — anything with a decision window between "read" and "write" —
// must use CommitExpecting instead; see its doc for why identity alone is not enough.
func (a *Access) Commit(h *Handle) ([]string, error) { return a.CommitExpecting(h, nil) }

// CommitExpecting is Commit with CONTENT PINS: pins maps a workspace-relative path to the
// digest the caller recorded for that live destination when it decided what to write
// (ContentPin of the bytes, or ContentAbsent when the file did not exist). Immediately
// before each destination is replaced — after the identity re-verification, inside the same
// few syscalls — the destination's actual content is digested and compared to the pin, and a
// mismatch is a REFUSAL that rolls the whole commit back.
//
// Identity and content answer different questions and both are needed. Identity
// (verifyDestination, os.SameFile) catches a destination that was SWAPPED — replaced or
// redirected to a different object. It cannot catch the common case: an editor that saves
// in place keeps the device+inode, so a concurrent human edit passes the identity check
// while the staged bytes still derive from the pre-edit content. Pinning the content is
// what turns that silent overwrite into a refusal.
//
// Independently of pins, every existing destination is RE-READ after the identity check and
// compared byte-for-byte with the backup taken moments earlier. That is the check the
// rollback actually depends on — a rollback restores those bytes, so they must still be what
// the destination holds — and it is the only one that survives inode reuse: ext4 hands a
// recreated file the inode it just freed, so "delete and recreate" is invisible to
// os.SameFile there even though it is exactly the case the identity check was written for.
//
// A nil pin map means "no pins recorded" and skips the content check entirely (Commit's
// historical contract). A non-nil map is EXHAUSTIVE: a destination with no pin is refused
// (ReasonDestinationUnpinned) rather than written unchecked, because a caller that pins some
// of its writes and not others has no record of what the unpinned ones were replacing.
//
// What this does NOT buy, stated plainly: there is still no filesystem lock. The pin is
// verified from the bytes read immediately before the write, so the unprotected window is
// the few syscalls between that read and the create — not the minutes-long decision window
// it replaces. A writer inside that syscall window is still neither prevented nor detected.
func (a *Access) CommitExpecting(h *Handle, pins map[string]string) ([]string, error) {
	copyRoot, err := openRootDir(h.Root)
	if err != nil {
		return nil, fmt.Errorf("commit: open copy root: %w", err)
	}
	defer copyRoot.Close()
	liveRoot, err := openRootDir(h.Live)
	if err != nil {
		return nil, fmt.Errorf("commit: open live root: %w", err)
	}
	defer liveRoot.Close()
	// The canonical live root, so each destination can be compared against the path the
	// write guard actually authorized. It is made ABSOLUTE first: a guard canonicalizes
	// against the working directory, so comparing its answer to a relative live root
	// ("." — the shape `review .` produces) would refuse every legitimate write.
	liveAbs, err := filepath.Abs(h.Live)
	if err != nil {
		return nil, fmt.Errorf("commit: resolve live root %q: %w", h.Live, err)
	}
	liveCanon, err := filepath.EvalSymlinks(liveAbs)
	if err != nil {
		return nil, fmt.Errorf("commit: canonicalize live root %q: %w", h.Live, err)
	}

	type staged struct {
		rel  string
		data []byte
	}
	cur, herr := hashTree(h.Root)
	if herr != nil {
		return nil, &Refusal{
			Op: "commit", Path: h.Root, Reason: ReasonStagedUnenumerable,
			Rule:   "every staged subtree must be listable; a commit never drops files it could not see",
			Detail: herr.Error(),
		}
	}
	changed := make([]staged, 0, len(cur))
	for _, rel := range sortedKeys(cur) {
		if IsExcluded(rel) {
			continue // never write excluded/internal paths back to the live workspace
		}
		data, rerr := readRegular(copyRoot, "commit", rel)
		if rerr != nil {
			return nil, &Refusal{
				Op: "commit", Path: rel, Reason: ReasonStagedUnreadable,
				Rule:   "every staged file must be readable; a commit never skips one silently",
				Detail: rerr.Error(),
			}
		}
		if live, lerr := readRegular(liveRoot, "commit", rel); lerr == nil && bytes.Equal(live, data) {
			continue
		}
		changed = append(changed, staged{rel: rel, data: data})
	}
	for _, s := range changed {
		if err := a.authorizeCommit(h, liveCanon, s.rel); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
	}

	// Rollback state, captured immediately before each destination is replaced. The bytes
	// are held in memory rather than in a sibling file: they go back to the very path they
	// came from, so no cross-filesystem rename is involved and no partially-written backup
	// file can be left behind on the live tree.
	type undo struct {
		rel         string
		existed     bool
		data        []byte
		perm        fs.FileMode
		createdDirs []string // parent directories this commit invented, outermost first
	}
	var undos []undo
	rollback := func() error {
		var firstErr error
		for i := len(undos) - 1; i >= 0; i-- {
			u := undos[i]
			var rerr error
			if u.existed {
				rerr = writeThroughRoot(liveRoot, "rollback", u.rel, u.data, u.perm)
			} else {
				// The destination did not exist before this commit, so undoing it means
				// "make sure nothing is there" — which is already true when the write that
				// failed never created anything (see rollbackRemove).
				rerr = rollbackRemove(liveRoot, u.rel)
				// A rolled-back commit must also leave no DIRECTORY it invented. Only the
				// undo of a file this commit created can own directories, and they are
				// removed after that file, deepest first.
				removeCreatedDirs(liveRoot, u.createdDirs)
			}
			if rerr != nil && firstErr == nil {
				firstErr = rerr
			}
		}
		return firstErr
	}

	committed := make([]string, 0, len(changed))
	for _, s := range changed {
		u := undo{rel: s.rel, perm: 0o644}
		// want is the destination's identity as inspected here; it is re-checked
		// immediately before the replacement so a concurrent change is refused rather
		// than clobbered (nil = "absent at check time").
		var want fs.FileInfo
		switch fi, lerr := liveRoot.Lstat(toRootPath(s.rel)); {
		case lerr == nil && isReparse(fi):
			// Never write through a symlink/reparse point in the live workspace — and
			// never skip it silently either: it is a planted redirect, not a no-op.
			err = &Refusal{Op: "commit", Path: s.rel, Reason: ReasonReparse,
				Rule: "a live destination that is a symlink/reparse point is never written through"}
		case lerr == nil && !fi.Mode().IsRegular():
			err = &Refusal{Op: "commit", Path: s.rel, Reason: ReasonNotRegular,
				Rule: "a live destination that is not a regular file is never replaced"}
		case lerr == nil:
			u.existed, want = true, fi
			u.data, u.perm, err = readBackup(liveRoot, "commit", s.rel)
		default:
			err = nil // does not exist yet: the rollback is an unlink
		}
		if err == nil {
			if testHookBeforeDestinationVerify != nil {
				testHookBeforeDestinationVerify(s.rel)
			}
			err = verifyDestination(liveRoot, "commit", s.rel, want)
		}
		// The backup must still be true. Identity says the name still resolves to the same
		// object; only the bytes say the object still holds what the rollback would restore.
		// Re-read and compare — a mismatch means a concurrent writer, and clobbering it or
		// later "restoring" stale bytes over it are both worse than refusing. The re-read goes
		// through readBackup, the same path the backup took: a hardlinked destination is
		// backed up and then replaced by name with a fresh file, so the stricter readRegular
		// (which refuses a multi-link file) would turn that supported case into a refusal.
		if err == nil && u.existed {
			switch cur, _, rerr := readBackup(liveRoot, "commit", s.rel); {
			case rerr != nil:
				err = &Refusal{Op: "commit", Path: s.rel, Reason: ReasonDestinationChanged,
					Rule:   "the destination must still hold the bytes that were backed up",
					Detail: "the destination could not be re-read before the write: " + rerr.Error()}
			case !bytes.Equal(cur, u.data):
				err = &Refusal{Op: "commit", Path: s.rel, Reason: ReasonDestinationChanged,
					Rule:   "the destination must still hold the bytes that were backed up",
					Detail: "the destination's content changed between the backup read and the write"}
			}
		}
		// CONTENT, after identity and immediately before the replacement. u.data is what the
		// backup read just took from this very destination, so the digest is of the bytes
		// about to be overwritten — not of anything read minutes earlier.
		if err == nil && pins != nil {
			err = verifyPinned("commit", s.rel, u.existed, u.data, pins)
		}
		if err == nil {
			undos = append(undos, u)
			var created []string
			created, err = writeThroughRootTracked(liveRoot, "commit", s.rel, s.data, u.perm)
			undos[len(undos)-1].createdDirs = created
		}
		if err != nil {
			// The cause is wrapped (not replaced), so a caller still classifies on the
			// scope denial / typed refusal underneath it.
			if rbErr := rollback(); rbErr != nil {
				return nil, fmt.Errorf("commit of %q failed AND rollback failed (%v): %w", s.rel, rbErr, err)
			}
			return nil, fmt.Errorf("commit of %q rolled back (no file applied): %w", s.rel, err)
		}
		committed = append(committed, s.rel)
	}
	return committed, nil
}

// Discard removes the copy. For a read-only (reviewer) copy it first checks for
// unexpected mutation; on a breach it returns the mutation diff and a non-nil
// M5 halt class (the caller owns audit).
func (a *Access) Discard(h *Handle) (mutationDiff string, m5 *core.HaltClass, err error) {
	if h.readOnly {
		cur, herr := hashTree(h.Root)
		switch {
		case herr != nil:
			// The copy could not be enumerated, so mutation cannot be RULED OUT — and a
			// subtree that became unlistable is itself a change to a copy nobody was
			// supposed to touch. Fail closed: report it as a breach rather than as a clean
			// discard whose error a caller may drop.
			mutationDiff = "unverifiable: " + herr.Error()
			cls := core.HaltClass("M5")
			m5 = &cls
			err = herr
		case !sameHashes(h.snapshot, cur):
			mutationDiff = describeMutation(h.snapshot, cur)
			cls := core.HaltClass("M5")
			m5 = &cls
		}
	}
	rmErr := os.RemoveAll(h.Root)
	if err == nil {
		err = rmErr
	}
	return mutationDiff, m5, err
}

// Cleanup unconditionally removes a copy's working directory. It is a panic-safe
// net (defer it right after Copy); the normal path still uses Discard, which also
// removes the directory, so a later Cleanup is a harmless no-op. It performs NO
// mutation check, so it never spuriously reports M5.
func (a *Access) Cleanup(h *Handle) {
	if h != nil {
		_ = os.RemoveAll(h.Root)
	}
}

// --- helpers ---

// copyTree walks the source ROOT HANDLE (never a path string) and mirrors it into the
// destination root handle. Every entry is re-examined through the same handle it was
// listed from, so a component swapped to a symlink mid-walk cannot redirect a read out
// of the workspace: os.Root refuses it.
func copyTree(srcRoot, dstRoot *os.Root) ([]Caveat, error) {
	var caveats []Caveat
	err := fs.WalkDir(srcRoot.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil // the root itself was judged before this walk started
		}
		rel := filepath.FromSlash(p)
		if IsExcluded(rel) {
			// RECORDED HERE, because this is where a real run drops them. The collector runs on
			// the COPY, where an excluded path no longer exists, so it has nothing left to notice
			// — which is exactly why the omission used to be invisible end to end. Recording it
			// at the copy is also what keeps PreviewPayload honest: the dry run walks the LIVE
			// tree and would otherwise report exclusions the run never mentioned.
			//
			// Only the exclusions a user would be SURPRISED by; see noteworthyExclusion.
			if noteworthyExclusion(rel) {
				caveats = append(caveats, Caveat{
					Path: filepath.ToSlash(rel), Reason: ReasonExcluded,
					Rule: excludedRule(rel), Detail: excludedDetail(d.IsDir()),
				})
			}
			if d.IsDir() {
				return fs.SkipDir // never copy excluded/internal directories
			}
			return nil
		}
		// Read-denied paths (`.env*`, key material) are never copied at all: the isolated
		// copy is what a model is pointed at, so excluding by construction is stronger than
		// filtering them out of a prompt afterwards. The rule is per COMPONENT, so a file
		// under a denied directory is denied with it.
		if scope.DeniedRead(rel) != "" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		fi, lerr := srcRoot.Lstat(toRootPath(rel))
		if lerr != nil {
			return nil // vanished between listing and stat: nothing to copy
		}
		if isReparse(fi) {
			// never follow/copy symlinks or reparse points (junctions/mount points) —
			// they can redirect outside the workspace
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if fi.IsDir() {
			return dstRoot.MkdirAll(toRootPath(rel), 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil // devices/sockets/FIFOs are not review material
		}
		cerr := copyRegular(srcRoot, dstRoot, "copy", rel)
		// A hardlinked file is refused as a FILE, not as a run. Its innocuous in-root name
		// may be a second name for `~/.ssh/id_rsa`, so it must not be copied — but a
		// `cp -al` tree, a dedup store or a legitimately hardlinked fixture is ordinary,
		// and killing the whole review over one such file is an availability hole a writer
		// inside a trusted tree could trigger at will. It is withheld and RECORDED, so the
		// omission is never silent.
		if ReasonOf(cerr) == ReasonHardlink {
			caveats = append(caveats, caveatFor(rel, cerr))
			return nil
		}
		return cerr
	})
	if err != nil {
		return nil, err
	}
	return caveats, nil
}

// copyRegular streams one regular file from srcRoot to dstRoot, both root-relative. The
// source is validated on the OPENED descriptor (regular, not a symlink, not hardlinked),
// so what is copied is what was checked.
func copyRegular(srcRoot, dstRoot *os.Root, op, rel string) error {
	in, _, err := openRegular(srcRoot, op, rel, true)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := mkdirParents(dstRoot, rel); err != nil {
		return err
	}
	if err := removeExisting(dstRoot, op, rel); err != nil {
		return err
	}
	out, err := createExclusive(dstRoot, rel, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// hashTree fingerprints every file in a tree. Enumeration errors are PROPAGATED, never
// swallowed: a staged directory that cannot be listed hides every file beneath it, and a
// caller that treats the resulting set as complete would commit a subset of the reviewed
// files while reporting success (and would see no mutation in a read-only copy whose
// mutated subtree merely became unreadable).
//
// The digest is SHA-256 because the snapshot is a SECURITY control, not a change detector
// of convenience: Discard compares it to catch a model that wrote into its read-only copy
// (M5), and a hash with a practical collision attack would let a mutation hide behind an
// unchanged digest. The digests live only in memory on the Handle — never written, never
// reported — so the algorithm carries no compatibility weight and can be the strong one.
func hashTree(root string) (map[string]string, error) {
	out := map[string]string{}
	r, err := openRootDir(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	werr := fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("enumerate %q: %w", filepath.FromSlash(p), err)
		}
		if p == "." {
			return nil
		}
		rel := filepath.FromSlash(p)
		// Never open/follow a symlink or reparse point: record a sentinel so its
		// appearance still changes the snapshot (→ M5 detection in a read-only copy)
		// without reading whatever it points at.
		fi, lerr := r.Lstat(toRootPath(rel))
		if lerr != nil {
			return nil
		}
		if isReparse(fi) {
			out[rel] = "reparse:" + fi.Mode().String()
			return nil
		}
		if fi.IsDir() {
			return nil
		}
		f, _, oerr := openRegular(r, "hash", rel, false)
		if oerr != nil {
			out[rel] = "unreadable:" + fi.Mode().String()
			return nil
		}
		h := sha256.New()
		_, _ = io.Copy(h, f)
		f.Close()
		out[rel] = fmt.Sprintf("%x", h.Sum(nil))
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	return out, nil
}

// replaceNth replaces the n-th (1-based) occurrence of old with repl; returns s
// unchanged if there are fewer than n occurrences.
func replaceNth(s, old, repl string, n int) string {
	idx := 0
	for i := 1; ; i++ {
		j := strings.Index(s[idx:], old)
		if j < 0 {
			return s
		}
		j += idx
		if i == n {
			return s[:j] + repl + s[j+len(old):]
		}
		idx = j + len(old)
	}
}

func sameHashes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func describeMutation(before, after map[string]string) string {
	var sb strings.Builder
	for _, k := range sortedKeys(after) {
		if before[k] == "" {
			fmt.Fprintf(&sb, "added: %s\n", k)
		} else if before[k] != after[k] {
			fmt.Fprintf(&sb, "modified: %s\n", k)
		}
	}
	for _, k := range sortedKeys(before) {
		if after[k] == "" {
			fmt.Fprintf(&sb, "removed: %s\n", k)
		}
	}
	return sb.String()
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	// CRLF-aware logical lines: drop a trailing CR so a CRLF line and the same LF line
	// compare equal (a pure LF↔CRLF change is not spurious content) and diff body lines
	// never carry a stray "\r". Diff framing stays LF (via ensureNL); the patch artifact
	// is deterministic LF.
	for i, l := range lines {
		switch {
		case strings.HasSuffix(l, "\r\n"):
			lines[i] = l[:len(l)-2] + "\n"
		case strings.HasSuffix(l, "\r"): // final line, no trailing LF
			lines[i] = l[:len(l)-1]
		}
	}
	return lines
}

// unifiedDiff emits a minimal unified diff by trimming common prefix/suffix lines.
func unifiedDiff(path string, oldL, newL []string) string {
	p := 0
	for p < len(oldL) && p < len(newL) && oldL[p] == newL[p] {
		p++
	}
	s := 0
	for s < len(oldL)-p && s < len(newL)-p && oldL[len(oldL)-1-s] == newL[len(newL)-1-s] {
		s++
	}
	oldMid := oldL[p : len(oldL)-s]
	newMid := newL[p : len(newL)-s]
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", p+1, len(oldMid), p+1, len(newMid))
	for _, l := range oldMid {
		sb.WriteString("-" + ensureNL(l))
	}
	for _, l := range newMid {
		sb.WriteString("+" + ensureNL(l))
	}
	return sb.String()
}

func ensureNL(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
