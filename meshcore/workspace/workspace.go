// Package workspace provides isolated copies of a workspace (containment). It applies edits to a
// copy, diffs a copy against the live tree, commits a copy back, and discards a copy with
// unexpected-mutation (M5) detection.
//
// # Commit rollback
//
// Commit is compensating, not transactional. For a commit that fails partway it guarantees:
//   - every replaced destination is written back with the bytes and permission bits read just
//     before the replacement;
//   - every created destination, and every parent directory the commit created, is removed (a
//     directory that has since gained another file is left alone);
//   - no destination is replaced unless it is still the object (device and inode) that was
//     inspected and backed up, and no destination is created unless it is still absent;
//   - with CommitExpecting, no destination is replaced unless its content still matches the
//     caller's pin, so an in-place save that keeps the inode is refused rather than overwritten.
//
// It does not guarantee cross-process atomicity (there is no filesystem lock, so a writer in the
// few syscalls between check and replace is not detected); restoration of ownership, timestamps
// or other metadata beyond permission bits; anything once the rollback itself fails, which is
// reported as an error naming both failures; or protection against a writer after the rollback.
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

// WriteGuard authorizes a write to a live-tree path. Access consults it, when set, before applying
// an edit to a copy and before committing a copy back, passing the live destination rather than the
// temp-copy path. *scope.Resolver satisfies it.
type WriteGuard interface {
	ResolveWrite(path string) (string, error)
}

// Access creates and manages isolated copies under a temp base directory.
type Access struct {
	base string
	// Guard, when set, is checked for every write target. It is the last check on the write
	// path: callers check earlier so a refusal can be audited, and the guard ensures no code
	// path reaches a live write without the same rule.
	Guard WriteGuard
	// AllowProtectedRoots waives the protected-ancestor refusal when admitting a root. Set it
	// together with scope.Options.AllowProtectedWrites on the Guard. A root that is or sits
	// inside a secret path stays refused.
	AllowProtectedRoots bool
}

// New returns an Access rooted at baseDir (a temp area) with every root rule in force.
func New(baseDir string) *Access { return &Access{base: baseDir} }

// authorizeWrite runs the guard against the live path a workspace-relative target maps to. With
// no guard it allows the write.
func (a *Access) authorizeWrite(h *Handle, rel string) error {
	if a == nil || a.Guard == nil || h == nil {
		return nil
	}
	_, err := a.Guard.ResolveWrite(filepath.Join(h.Live, rel))
	return err
}

// authorizeCommit is authorizeWrite that also requires the guard's canonical path to be the
// destination under liveCanon, the canonical live root. Writing to the original path instead could
// write through an intermediate symlinked directory the guard resolved elsewhere.
func (a *Access) authorizeCommit(h *Handle, liveCanon, rel string) error {
	if a == nil || a.Guard == nil || h == nil {
		return nil
	}
	got, err := a.Guard.ResolveWrite(filepath.Join(h.Live, rel))
	if err != nil {
		return err
	}
	if got == "" || liveCanon == "" {
		return nil // a guard that returns no path (a custom policy) is not compared
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

// foldPaths is true only on Windows, where the filesystem is case-insensitive. It is a variable so
// tests can exercise the Windows branch on any platform.
var foldPaths = runtime.GOOS == "windows"

// samePath compares two canonical paths, folding case only on Windows.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if foldPaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Caveat records a path deliberately left out of a copy or a snippet set for a containment reason,
// so the caller can report the omission instead of leaving it indistinguishable from a missing file.
type Caveat struct {
	Path   string // workspace-relative
	Reason Reason
	Rule   string
	Detail string
}

// String renders the caveat as its path, reason, rule and detail.
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
	// Caveats lists files withheld from this copy for a containment reason (currently
	// hardlinked regular files), for the caller to record.
	Caveats  []Caveat
	readOnly bool              // a read-only copy reports mutation as M5
	snapshot map[string]string // rel path → sha at copy time
}

// Copy makes an isolated copy of live (a directory or a single file). A later mutation of a
// readOnly copy is an M5 breach.
func (a *Access) Copy(live string, readOnly bool, label string) (*Handle, error) {
	info, err := os.Lstat(live)
	if err != nil {
		return nil, fmt.Errorf("stat workspace %q: %w", live, err)
	}
	if isReparse(info) {
		return nil, fmt.Errorf("refusing to review a symlink/reparse-point target %q", live)
	}
	// The root is judged on its absolute path (see rootRefusal) before the basename rule below,
	// so a protected target is refused with a typed reason.
	if ref := rootRefusal("copy", live, a.AllowProtectedRoots); ref != nil {
		return nil, ref
	}
	// Refuse a target that is itself an excluded path, or a file directly inside one, by name.
	if excludedTarget(live, info.IsDir()) {
		return nil, fmt.Errorf("refusing to review excluded/internal path %q", live)
	}
	root, err := os.MkdirTemp(a.base, "rmcopy-"+label+"-")
	if err != nil {
		return nil, fmt.Errorf("create isolated copy: %w", err)
	}
	// liveRoot is always a directory (the parent for a single-file target), so Diff and Commit
	// can join workspace-relative paths against it. wantRoot is the identity the opened root
	// must have (see openRootBound).
	liveRoot, wantRoot := live, info
	if !info.IsDir() {
		liveRoot = filepath.Dir(live)
		// The parent may be a linked directory, so its identity comes from a following stat.
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

// AdmitRootWith reports whether root would be accepted for a copy, returning the *Refusal Copy would
// return, or nil. It creates, reads and copies nothing, so a dry run refuses exactly the workspaces a
// run refuses. allowProtected waives only the protected-ancestor rule; see rootRefusal.
func AdmitRootWith(root string, allowProtected bool) *Refusal {
	return rootRefusal("copy", root, allowProtected)
}

// rootRefusal judges a workspace root by its own path and returns the typed refusal or nil. Two
// rules apply, to every form of the root (as given, absolute and symlink-resolved):
//
//  1. The read denylist, on every component: when the root is a secret path such as `.ssh`, its
//     whole subtree is secret.
//  2. The protected-ancestor rule: families such as `.git` and `.vscode` are readable, so without
//     this rule a root inside one (`/trusted/.vscode`) would make a protected file such as
//     `mcp.json` an ordinary relative file.
//
// allowProtected waives rule 2 only. Rule 1 is unconditional, because a secret sent in a prompt
// cannot be recalled.
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
				// The detail names the operator flag that waives this rule.
				Detail: "the workspace root is, or sits inside, a protected path; exclusion is judged on components RELATIVE to the root, so such a root would strip the protection. If this tree is yours and reviewing it is the point (your own hooks, your own agent config), pass --allow-protected-paths",
			}
		}
	}
	return nil
}

// fillCopy populates the new copy root from the live target. Both sides are opened once as os.Root
// handles and every later operation is root-relative, so no component is re-resolved by string
// after it was checked. The source root is bound to wantRoot, the object the checks approved.
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
	// A single-file target is the whole subject of the operation, so a containment refusal on
	// it refuses the call rather than becoming a caveat.
	return nil, copyRegular(srcRoot, dstRoot, "copy", info.Name())
}

// Abs resolves a workspace-relative path within the copy.
func (h *Handle) Abs(rel string) string { return filepath.Join(h.Root, rel) }

// ApplyEdit applies one Edit to the copy. An empty Anchor prepends Replacement; otherwise the
// anchor text is replaced. A missing anchor is returned as an error for the caller to record.
func (a *Access) ApplyEdit(h *Handle, e core.Edit) error {
	if !safeRel(e.File) || IsExcluded(e.File) {
		return fmt.Errorf("apply edit: refusing excluded/escaping path %q", e.File)
	}
	// Confinement is checked against the live destination even in patch mode: a patch naming a
	// protected file is what a later apply would replay.
	if err := a.authorizeWrite(h, e.File); err != nil {
		return fmt.Errorf("apply edit: %w", err)
	}
	// The file name comes from a model and the copy may hold planted links, so the edit goes
	// through a root handle on the copy (os.Root refuses components that leave it) and a
	// no-follow open (which refuses a symlink at the target).
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
	// Match the replacement's line endings to the file's dominant convention so an LF insertion
	// into a CRLF file does not produce mixed endings. The anchor is matched against the
	// original bytes exactly, and unchanged bytes are preserved.
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

// --- grouped edits ---
//
// A caller applying a group of edits as a unit uses SnapshotFiles and RestoreFiles to undo the
// group's earlier edits when a later one fails. Both operate only on the isolated copy.

// FileSnapshot is the recorded content of files inside an isolated copy, taken before a group of
// edits so the group can be rolled back. Its only use is RestoreFiles.
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

// RestoreFiles restores a snapshot, undoing every edit made to its files since it was taken. A
// failure is returned, because a copy that could not be restored must not be committed.
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
	for _, f := range slices.Backward(snap.files) {
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

// Commit copies changed files from the copy back to the live tree (apply mode) and returns the
// committed paths.
//
// Every changed path is authorized before anything is written, and every replaced destination is
// backed up and restored on any failure, so the live tree moves from nothing applied to everything
// applied. A staged file or directory that cannot be read is an error, never a silent skip. The
// package doc states what the rollback does and does not restore.
//
// Commit performs no content pinning; a caller that decided what to write from an earlier read of
// the live tree must use CommitExpecting.
func (a *Access) Commit(h *Handle) ([]string, error) { return a.CommitExpecting(h, nil) }

// CommitExpecting is Commit with content pins. pins maps a workspace-relative path to the digest
// the caller recorded for that live destination (ContentPin of its bytes, or ContentAbsent).
// Immediately before each replacement the destination's content is digested and compared, and a
// mismatch refuses and rolls back the whole commit. Identity checks alone miss an in-place save,
// which keeps the inode.
//
// Independently of pins, each existing destination is re-read after the identity check and
// compared with its backup, because the rollback restores those bytes and inode reuse (on ext4,
// for example) can hide a delete-and-recreate from os.SameFile.
//
// A nil map skips the content check. A non-nil map is exhaustive: a destination without a pin is
// refused (ReasonDestinationUnpinned). There is still no filesystem lock, so a writer in the few
// syscalls between the final read and the write is not detected.
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
	// The canonical live root lets each destination be compared with the path the guard
	// authorized. It is made absolute first because the guard canonicalizes against the
	// working directory, and a relative live root such as "." would refuse every write.
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
		for _, u := range slices.Backward(undos) {
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
		// The destination must still hold the backed-up bytes; a mismatch means a concurrent
		// writer. The re-read uses readBackup, like the backup, because a hardlinked destination
		// is supported here and readRegular would refuse it.
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
		// Content pins are checked after identity, against the bytes the backup just read.
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

// Discard removes the copy. For a read-only copy it first checks for unexpected mutation and, on a
// breach, returns the mutation diff and an M5 halt class for the caller to audit.
func (a *Access) Discard(h *Handle) (mutationDiff string, m5 *core.HaltClass, err error) {
	if h.readOnly {
		cur, herr := hashTree(h.Root)
		switch {
		case herr != nil:
			// Mutation cannot be ruled out when the copy cannot be enumerated, so fail closed
			// and report a breach.
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

// Cleanup removes a copy's working directory without a mutation check. Defer it after Copy as a
// panic-safe net; after Discard it is a no-op.
func (a *Access) Cleanup(h *Handle) {
	if h != nil {
		_ = os.RemoveAll(h.Root)
	}
}

// --- helpers ---

// copyTree mirrors srcRoot into dstRoot. Every entry is examined through the source root handle,
// so a component swapped to a symlink mid-walk cannot redirect a read out of the workspace.
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
			// Exclusions are recorded here because the collector later runs on the copy, where
			// excluded paths no longer exist; PreviewPayloadWith, which walks the live tree, reports
			// the same set. Only noteworthy exclusions are recorded.
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
		// Read-denied paths (`.env*`, key material) are never copied, since the copy is what a
		// model is shown. The rule applies per component, so a denied directory's files are
		// denied too.
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
			// Symlinks and reparse points are never followed; they can redirect outside the
			// workspace.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if fi.IsDir() {
			return dstRoot.MkdirAll(toRootPath(rel), 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil // devices, sockets and FIFOs are not copied
		}
		cerr := copyRegular(srcRoot, dstRoot, "copy", rel)
		// A hardlinked file may be a second name for a secret, so it is withheld and recorded
		// rather than copied. Hardlinked trees are common, so failing the whole copy would be
		// an availability hole.
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

// copyRegular streams one regular file from srcRoot to dstRoot, both root-relative. The source is
// validated on the opened descriptor (regular, not a symlink, not hardlinked), so what is copied is
// what was checked.
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

// hashTree fingerprints every file in a tree with SHA-256. Enumeration errors are returned: an
// unlistable directory hides the files beneath it, which would let a commit drop files or a
// read-only copy hide a mutation. The hash must resist collisions because Discard uses it to
// detect a model writing to its read-only copy.
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
