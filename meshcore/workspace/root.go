package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// --- typed refusals ---

// Reason is the stable MACHINE code for a workspace refusal (code-shaped lower_snake, so
// a caller can branch on it and persist it in an audit record).
type Reason string

const (
	// ReasonRootDenied — the ROOT handed to a copy/collect is itself a protected path
	// (e.g. a `.env` directory). Its children must never be judged only by their own
	// names: the whole subtree is the secret.
	ReasonRootDenied Reason = "workspace_root_denied"
	// ReasonHardlink — a regular file with more than one link. Its innocuous in-root name
	// is only one of its names; another may be `~/.ssh/id_rsa`, so basename-based
	// exclusion says nothing about what the bytes are.
	ReasonHardlink Reason = "workspace_hardlink_denied"
	// ReasonReparse — a symlink/junction/mount point where a regular file was required.
	ReasonReparse Reason = "workspace_reparse_denied"
	// ReasonNotRegular — a device/socket/FIFO (or a directory) where a regular file was
	// required.
	ReasonNotRegular Reason = "workspace_not_regular_file"
	// ReasonStagedUnreadable — a staged file in an isolated copy could not be read. It is
	// never skipped silently: a commit that quietly drops part of the reviewed set is a
	// half-applied remediation wearing a success return.
	ReasonStagedUnreadable Reason = "workspace_staged_unreadable"
	// ReasonUnlinkFailed — the REQUIRED unlink before a live write failed. Writing anyway
	// could truncate whatever the existing entry links to.
	ReasonUnlinkFailed Reason = "workspace_unlink_failed"
	// ReasonPathMismatch — the write guard's canonical path for a destination is not the
	// destination itself under the live root (an intermediate component resolves
	// elsewhere, e.g. a symlinked directory).
	ReasonPathMismatch Reason = "workspace_path_mismatch"
	// ReasonExcludedAncestor — the ROOT handed to a copy/collect is itself, or sits under,
	// a protected path (`.git`, `.vscode`, `.claude`, `.aimesh`, …). Judging exclusion on
	// components RELATIVE to the root strips the protection: a root of `/trusted/.vscode`
	// makes `mcp.json` an ordinary relative file.
	ReasonExcludedAncestor Reason = "workspace_excluded_ancestor"
	// ReasonExcluded — a path INSIDE the reviewed tree that the exclusion list drops: a
	// build/artifact directory (`node_modules`, `dist`, `.cache`) or the agent/IDE client
	// config family (`.vscode`, `.claude`, `.cursor`, `.aimesh`, …).
	//
	// It is a CAVEAT, never a refusal — dropping these is correct and the run proceeds. What
	// was wrong is that it used to happen in silence, while a hardlinked file got a caveat
	// for the same class of reason. That silence has grown teeth: in 2026 `.claude/` and
	// `.cursor/` hold rules and prompts that ARE source, so a user can reasonably ask a
	// review to look at them and never learn it did not. docs/security.md makes the argument
	// against exactly this — "a reviewer cannot object to a file it was never shown".
	ReasonExcluded Reason = "workspace_excluded"
	// ReasonRootChanged — the object actually opened as the os.Root boundary is not the
	// object the containment checks were performed against (the path was swapped for a
	// symlink/another directory in between). os.Root confines traversal INSIDE the
	// boundary; it says nothing about which boundary got opened.
	ReasonRootChanged Reason = "workspace_root_identity_changed"
	// ReasonDestinationChanged — a live destination is not the object the pre-write check
	// approved (it appeared, vanished, or was replaced in between). A commit fails rather
	// than clobbering something it never inspected.
	ReasonDestinationChanged Reason = "workspace_destination_changed"
	// ReasonStagedUnenumerable — a staged subtree in an isolated copy could not be listed,
	// so the files beneath it were never considered. Reporting success would be a commit
	// that silently skipped part of the reviewed set.
	ReasonStagedUnenumerable Reason = "workspace_staged_unenumerable"
	// ReasonDestinationContentChanged — a live destination's CONTENT is not the content the
	// caller pinned when it decided what to write. Identity (device+inode) is unchanged, so
	// ReasonDestinationChanged cannot see it: an editor that saves IN PLACE keeps the inode.
	// The staged bytes were derived from the pinned content, so writing them would silently
	// discard whatever was written in between.
	ReasonDestinationContentChanged Reason = "workspace_destination_content_changed"
	// ReasonDestinationUnpinned — the caller supplied a content pin set and a destination is
	// not in it. A write with no recorded "what did this file look like when I decided" is
	// exactly the write a pin set exists to forbid, so an unpinned destination is refused
	// rather than written unchecked.
	ReasonDestinationUnpinned Reason = "workspace_destination_unpinned"
)

// --- content pins ---
//
// A pin is a caller's record of what a live destination looked like when the caller DECIDED
// what to write to it. Identity re-verification (verifyDestination) answers "is this still
// the same object"; a pin answers "is it still the same bytes" — the question an in-place
// save (same inode, new content) leaves open. See CommitExpecting.

// ContentAbsent is the pin value for a destination that did NOT exist when the pin was
// taken. It is a value rather than an omission so that "this file appeared in between" is a
// mismatch like any other rather than an untracked key.
const ContentAbsent = "absent"

// ContentPin is the pin format for existing bytes: "sha256:" + lowercase hex.
func ContentPin(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pinKey normalizes a workspace-relative path to the slash form pin maps are keyed by, so a
// caller that spelled its pins with slashes and a walker that produced native separators
// agree on Windows.
func pinKey(rel string) string { return filepath.ToSlash(rel) }

// verifyPinned checks a live destination's CONTENT against the caller's pin, immediately
// before the destination is replaced. `existed` and `data` are what the pre-write inspection
// just read (data is nil when the destination was absent).
func verifyPinned(op, rel string, existed bool, data []byte, pins map[string]string) error {
	want, ok := pins[pinKey(rel)]
	if !ok {
		return &Refusal{
			Op: op, Path: rel, Reason: ReasonDestinationUnpinned,
			Rule:   "every destination of a pinned commit must carry a recorded pre-write content digest",
			Detail: "no pin was recorded for this destination",
		}
	}
	got := ContentAbsent
	if existed {
		got = ContentPin(data)
	}
	if got == want {
		return nil
	}
	return &Refusal{
		Op: op, Path: rel, Reason: ReasonDestinationContentChanged,
		Rule:   "a destination's content must still be the content the caller pinned when it decided what to write",
		Detail: fmt.Sprintf("pinned %s, found %s", want, got),
	}
}

// Refusal is the typed containment refusal of this package. It names the rule that
// refused so the message teaches, and carries a machine Reason so a caller can classify
// it without parsing prose.
type Refusal struct {
	Op     string // "copy", "collect", "commit", "edit"
	Path   string // the path as the caller supplied / the workspace-relative path
	Reason Reason
	Rule   string // the rule that matched, when there is one
	Detail string
}

func (r *Refusal) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s refused for %q [%s]", r.Op, r.Path, r.Reason)
	if r.Rule != "" {
		fmt.Fprintf(&sb, ": rule %s", r.Rule)
	}
	if r.Detail != "" {
		sb.WriteString(": " + r.Detail)
	}
	return sb.String()
}

// AsRefusal extracts a *Refusal from an error chain.
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// ReasonOf returns the machine Reason for an error produced by this package, or "".
func ReasonOf(err error) Reason {
	if r, ok := AsRefusal(err); ok {
		return r.Reason
	}
	return ""
}

// --- root-relative filesystem access ---
//
// Every file operation in this package goes through an *os.Root opened once on the
// workspace root. Path-STRING traversal (check a path, then re-open it by name) is
// racy by construction: a component swapped to a symlink between the check and the use
// is followed. os.Root resolves each component against the open root and refuses to
// leave it, so the check and the use are the same act.

// os.Root hardens traversal INSIDE the boundary; it does not decide WHICH boundary is
// opened. Opening the root by path after the containment checks ran against that same path
// leaves a window: swap the approved directory for a symlink in between and the
// REPLACEMENT becomes the boundary, after which every interior operation is faithfully
// confined to the attacker's tree. openRootBound closes that window by binding the checks
// to the opened object.
//
// Mechanism: capture the approved object's identity with a stat BEFORE the open, then
// stat the OPENED root (an fstat on the descriptor, not a second path lookup) and require
// the two to be the same file. os.SameFile is the binding primitive rather than hand-rolled
// syscall code because it is exactly this comparison on every platform — device+inode on
// unix, volume serial + file index on Windows — so no build tag is needed and Windows is
// covered by the same code path. Swapping the path back after the open cannot help an
// attacker: the second stat goes through the already-open handle, so it reports what was
// opened, not what the name points at now.

// The two hooks below are the package's only test seams. Each names a check→use window
// this package narrows but cannot make atomic, and each exists ONLY so that window can be
// entered deterministically in a test: staging a real race would make the test flaky and
// would prove nothing when it passed. Both are nil in every non-test build, and neither is
// reachable from any exported API.
//
// testHookBeforeOpenRoot runs between the root identity capture and the open.
var testHookBeforeOpenRoot func(dir string)

// testHookBeforeDestinationVerify runs between a commit destination's inspection/backup and
// the re-verification that precedes its replacement.
var testHookBeforeDestinationVerify func(rel string)

// openRootBound opens dir as an os.Root and requires the opened object to be `want` — the
// object the caller's containment checks approved. A nil want means "no prior check to bind
// to" and the identity is captured here instead (see openRootDir).
func openRootBound(op, dir string, want os.FileInfo) (*os.Root, error) {
	if want == nil {
		fi, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}
		want = fi
	}
	if testHookBeforeOpenRoot != nil {
		testHookBeforeOpenRoot(dir)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	got, err := r.Stat(".")
	if err != nil {
		r.Close()
		return nil, err
	}
	if !os.SameFile(want, got) {
		r.Close()
		return nil, &Refusal{
			Op: op, Path: dir, Reason: ReasonRootChanged,
			Rule:   "the opened root must be the very object the containment checks approved",
			Detail: "the path was replaced between the check and the open",
		}
	}
	return r, nil
}

// openRootDir opens dir as an os.Root, binding the handle to the object the path named at
// capture time (see openRootBound). Callers that performed containment checks on a
// specific object pass it explicitly via openRootBound; this form is for the interior
// roots this package created itself (an isolated copy).
func openRootDir(dir string) (*os.Root, error) {
	return openRootBound("open", dir, nil)
}

// toRootPath converts an OS-native or slash-separated workspace-relative path into the
// form the os.Root methods take.
func toRootPath(rel string) string { return filepath.FromSlash(filepath.ToSlash(rel)) }

// openRegular opens rel through r for reading and returns the file together with the
// *opened object's* FileInfo — validation is performed on what was actually opened
// (f.Stat(), an fstat on the descriptor), never on a pre-open path lookup that a racing
// rename could invalidate. It refuses symlinks/reparse points (O_NOFOLLOW on the final
// component), non-regular files, and — when checkLinks is set — hardlinked regular files.
//
// checkLinks is off only for the commit-time backup read, whose bytes are never shown to
// a model and are written back to the very path they came from.
func openRegular(r *os.Root, op, rel string, checkLinks bool) (*os.File, fs.FileInfo, error) {
	name := toRootPath(rel)
	// Lstat first so a symlink is reported as a REPARSE refusal (teaching) rather than
	// as the platform's raw ELOOP from the O_NOFOLLOW open below.
	if fi, err := r.Lstat(name); err == nil && isReparse(fi) {
		return nil, nil, &Refusal{Op: op, Path: rel, Reason: ReasonReparse, Rule: "symlinks/reparse points are never followed"}
	}
	f, err := r.OpenFile(name, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if isReparse(fi) || !fi.Mode().IsRegular() {
		f.Close()
		reason := ReasonNotRegular
		rule := "only regular files are read"
		if isReparse(fi) {
			reason, rule = ReasonReparse, "symlinks/reparse points are never followed"
		}
		return nil, nil, &Refusal{Op: op, Path: rel, Reason: reason, Rule: rule}
	}
	if checkLinks {
		if n, ok := linkCount(fi); ok && n > 1 {
			f.Close()
			return nil, nil, &Refusal{
				Op: op, Path: rel, Reason: ReasonHardlink,
				Rule:   "a regular file with more than one link may name a protected file under another path",
				Detail: fmt.Sprintf("link count %d", n),
			}
		}
	}
	return f, fi, nil
}

// readRegular reads rel through r, applying the openRegular rules.
func readRegular(r *os.Root, op, rel string) ([]byte, error) {
	f, _, err := openRegular(r, op, rel, true)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// readBackup reads rel through r for a rollback snapshot: same no-follow/regular-file
// rules, but the hardlink rule does not apply (the bytes go back to the same path).
func readBackup(r *os.Root, op, rel string) ([]byte, fs.FileMode, error) {
	f, fi, err := openRegular(r, op, rel, false)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	return b, fi.Mode().Perm(), err
}

// createExclusive creates rel through r for writing, refusing to write through anything
// that already exists (O_EXCL) or through a symlink (O_NOFOLLOW). Callers unlink first.
func createExclusive(r *os.Root, rel string, perm fs.FileMode) (*os.File, error) {
	return r.OpenFile(toRootPath(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, perm)
}

// mkdirParents creates rel's parent directories through r.
func mkdirParents(r *os.Root, rel string) error {
	dir := filepath.Dir(toRootPath(rel))
	if dir == "." || dir == "" || dir == string(filepath.Separator) {
		return nil
	}
	return r.MkdirAll(dir, 0o755)
}

// mkdirParentsTracked creates rel's parent directories through r and returns the ones it
// actually CREATED, outermost first, so a rolled-back commit can remove them again.
// MkdirAll cannot be used here: it reports nothing about which components were new, and a
// rollback that leaves behind directories the commit invented has not restored the tree.
func mkdirParentsTracked(r *os.Root, rel string) ([]string, error) {
	dir := filepath.Dir(toRootPath(rel))
	if dir == "." || dir == "" || dir == string(filepath.Separator) {
		return nil, nil
	}
	var created []string
	cur := ""
	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == "" || part == "." {
			continue
		}
		if cur == "" {
			cur = part
		} else {
			cur += "/" + part
		}
		name := filepath.FromSlash(cur)
		if _, err := r.Lstat(name); err == nil {
			continue // already there: not ours to undo
		}
		if err := r.Mkdir(name, 0o755); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // created concurrently: not ours to undo
			}
			return created, err
		}
		created = append(created, name)
	}
	return created, nil
}

// removeCreatedDirs undoes mkdirParentsTracked, deepest first. It is BEST EFFORT by
// design: a directory that is no longer empty holds something this commit did not create,
// and removing it would destroy a third party's file — the opposite of a rollback.
func removeCreatedDirs(r *os.Root, created []string) {
	for i := len(created) - 1; i >= 0; i-- {
		_ = r.Remove(created[i])
	}
}

// verifyDestination re-checks, immediately before a replacement, that the destination is
// still the object the pre-write inspection approved: the same file (compared by identity,
// not by name), or still absent when it was absent.
//
// It is what makes "fail rather than clobber" true for a destination CREATED after the
// pre-write Lstat, which would otherwise be recorded as "did not exist" and then unlinked by
// the rollback, and for one redirected to a different object.
//
// What identity alone does NOT prove: that the bytes are unchanged. An in-place save keeps
// the inode, and on filesystems that recycle inode numbers (ext4 does, immediately) even a
// delete-and-recreate comes back with the same identity. CommitExpecting therefore also
// re-reads every existing destination and compares it with the backup; this function is the
// cheap first gate, not the whole check.
//
// want == nil means "the destination did not exist at check time".
func verifyDestination(r *os.Root, op, rel string, want fs.FileInfo) error {
	got, err := r.Lstat(toRootPath(rel))
	switch {
	case want == nil && err == nil:
		return &Refusal{
			Op: op, Path: rel, Reason: ReasonDestinationChanged,
			Rule:   "a destination that did not exist at check time must still be absent when it is written",
			Detail: "the destination was created concurrently",
		}
	case want == nil:
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return &Refusal{
			Op: op, Path: rel, Reason: ReasonDestinationChanged,
			Rule:   "a destination that did not exist at check time must still be absent when it is written",
			Detail: err.Error(),
		}
	case err != nil:
		return &Refusal{
			Op: op, Path: rel, Reason: ReasonDestinationChanged,
			Rule:   "the destination must still be the object that was inspected and backed up",
			Detail: "the destination vanished: " + err.Error(),
		}
	case !os.SameFile(want, got):
		return &Refusal{
			Op: op, Path: rel, Reason: ReasonDestinationChanged,
			Rule:   "the destination must still be the object that was inspected and backed up",
			Detail: "the destination was replaced between the check and the write",
		}
	}
	return nil
}

// removeExisting unlinks rel through r. A failure other than "does not exist" is a HALT,
// never an ignored error: if the existing entry survives, the subsequent write could
// truncate whatever it links to (a hardlink to a protected file outside the workspace).
func removeExisting(r *os.Root, op, rel string) error {
	err := r.Remove(toRootPath(rel))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return &Refusal{
		Op: op, Path: rel, Reason: ReasonUnlinkFailed,
		Rule:   "the existing destination must be unlinked before a write",
		Detail: err.Error(),
	}
}

// rollbackRemove undoes the creation of a destination that did not exist before a commit.
// It is deliberately more forgiving than removeExisting: the write it is undoing may have
// failed BEFORE creating anything (it may even have been refused for leaving the root), so
// "nothing is there" is success, however the removal itself was answered.
func rollbackRemove(r *os.Root, rel string) error {
	name := toRootPath(rel)
	err := r.Remove(name)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, lerr := r.Lstat(name); lerr != nil {
		return nil // nothing reachable at that path: the undo is already true
	}
	return &Refusal{
		Op: "rollback", Path: rel, Reason: ReasonUnlinkFailed,
		Rule:   "a rolled-back commit must leave no file it created",
		Detail: err.Error(),
	}
}

// writeThroughRootTracked replaces rel with data: required unlink, then an exclusive
// no-follow create. It never truncates an existing entry in place. It also returns the
// parent directories it CREATED (outermost first) so a rolled-back commit can remove them;
// the list is returned even on failure, because a partially-created chain still has to be
// undone.
func writeThroughRootTracked(r *os.Root, op, rel string, data []byte, perm fs.FileMode) ([]string, error) {
	created, err := mkdirParentsTracked(r, rel)
	if err != nil {
		return created, err
	}
	if err := removeExisting(r, op, rel); err != nil {
		return created, err
	}
	f, err := createExclusive(r, rel, perm)
	if err != nil {
		return created, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return created, err
	}
	return created, f.Close()
}

// writeThroughRoot is writeThroughRootTracked for callers with nothing to roll back.
func writeThroughRoot(r *os.Root, op, rel string, data []byte, perm fs.FileMode) error {
	_, err := writeThroughRootTracked(r, op, rel, data, perm)
	return err
}
