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
	"slices"
	"strings"
)

// --- typed refusals ---

// Reason is the stable machine code for a workspace refusal, suitable for branching and audit
// records.
type Reason string

// Workspace refusal reasons.
const (
	// ReasonRootDenied — the root given to a copy or collection is itself a secret path, such
	// as a `.env` directory, so its whole subtree is secret.
	ReasonRootDenied Reason = "workspace_root_denied"
	// ReasonHardlink — a regular file with more than one link; another of its names may be a
	// secret.
	ReasonHardlink Reason = "workspace_hardlink_denied"
	// ReasonReparse — a symlink/junction/mount point where a regular file was required.
	ReasonReparse Reason = "workspace_reparse_denied"
	// ReasonNotRegular — a device/socket/FIFO (or a directory) where a regular file was
	// required.
	ReasonNotRegular Reason = "workspace_not_regular_file"
	// ReasonStagedUnreadable — a staged file in an isolated copy could not be read; a commit
	// never skips it.
	ReasonStagedUnreadable Reason = "workspace_staged_unreadable"
	// ReasonUnlinkFailed — the required unlink before a live write failed. Writing anyway
	// could truncate whatever the existing entry links to.
	ReasonUnlinkFailed Reason = "workspace_unlink_failed"
	// ReasonPathMismatch — the write guard's canonical path for a destination is not the
	// destination itself under the live root (an intermediate component resolves
	// elsewhere, e.g. a symlinked directory).
	ReasonPathMismatch Reason = "workspace_path_mismatch"
	// ReasonExcludedAncestor — the root given to a copy or collection is, or sits under, a
	// protected path such as `.git` or `.vscode`.
	ReasonExcludedAncestor Reason = "workspace_excluded_ancestor"
	// ReasonExcluded — a path inside the workspace dropped by the exclusion list. It is
	// recorded as a caveat, not a refusal; the run proceeds.
	ReasonExcluded Reason = "workspace_excluded"
	// ReasonRootChanged — the object opened as the os.Root boundary is not the object the
	// containment checks approved; the path was swapped in between.
	ReasonRootChanged Reason = "workspace_root_identity_changed"
	// ReasonDestinationChanged — a live destination is not the object the pre-write check
	// approved (it appeared, vanished, or was replaced in between). A commit fails rather
	// than clobbering something it never inspected.
	ReasonDestinationChanged Reason = "workspace_destination_changed"
	// ReasonStagedUnenumerable — a staged subtree in an isolated copy could not be listed, so
	// its files were never considered.
	ReasonStagedUnenumerable Reason = "workspace_staged_unenumerable"
	// ReasonDestinationContentChanged — a live destination's content no longer matches the
	// caller's pin although its identity is unchanged, as after an in-place save.
	ReasonDestinationContentChanged Reason = "workspace_destination_content_changed"
	// ReasonDestinationUnpinned — a commit with a pin set has a destination that is not in it.
	ReasonDestinationUnpinned Reason = "workspace_destination_unpinned"
)

// --- content pins ---
//
// A pin records a live destination's content when the caller decided what to write to it. See
// CommitExpecting.

// ContentAbsent is the pin value for a destination that did not exist when the pin was taken, so
// a file created in between is a mismatch.
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

// verifyPinned checks a live destination's content against the caller's pin, immediately
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

// Error renders the refusal with its reason, rule and detail.
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
	if r, ok := errors.AsType[*Refusal](err); ok {
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
// Every file operation in this package goes through an *os.Root opened once on a root, so a
// component swapped to a symlink between a check and its use cannot be followed out of the root.
//
// os.Root confines traversal inside a boundary but does not choose which boundary is opened: a
// directory swapped for a symlink between the checks and the open would become the boundary.
// openRootBound therefore compares, with os.SameFile, the identity captured before the open with a
// stat of the opened handle.

// The two hooks below are test seams for check-to-use windows that cannot be made atomic, so a test
// can enter each window deterministically. Both are nil outside tests.
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

// openRegular opens rel through r for reading and returns the file with the opened descriptor's
// FileInfo, so validation applies to what was opened. It refuses symlinks and reparse points
// (O_NOFOLLOW), non-regular files and, when checkLinks is set, hardlinked files. checkLinks is off
// only for the commit backup read, whose bytes go back to the same path and are never shown to a
// model.
func openRegular(r *os.Root, op, rel string, checkLinks bool) (*os.File, fs.FileInfo, error) {
	name := toRootPath(rel)
	// Lstat first so a symlink is refused with a reparse reason rather than the platform's ELOOP.
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

// mkdirParentsTracked creates rel's parent directories through r and returns those it created,
// outermost first, so a rolled-back commit can remove them. MkdirAll does not report which were new.
func mkdirParentsTracked(r *os.Root, rel string) ([]string, error) {
	dir := filepath.Dir(toRootPath(rel))
	if dir == "." || dir == "" || dir == string(filepath.Separator) {
		return nil, nil
	}
	var created []string
	cur := ""
	for part := range strings.SplitSeq(filepath.ToSlash(dir), "/") {
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

// removeCreatedDirs undoes mkdirParentsTracked, deepest first. It is best effort: a directory that
// is no longer empty holds a file this commit did not create.
func removeCreatedDirs(r *os.Root, created []string) {
	for _, c := range slices.Backward(created) {
		_ = r.Remove(c)
	}
}

// verifyDestination re-checks, immediately before a replacement, that the destination is still the
// inspected object (by identity) or, when want is nil, still absent. Identity does not prove the
// bytes are unchanged, so CommitExpecting also compares content with the backup.
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

// removeExisting unlinks rel through r. Any failure other than not-exist is a refusal: if the entry
// survives, the following write could truncate whatever it links to.
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
// It is more forgiving than removeExisting: the write being undone may have failed before creating
// anything, so nothing at the path is success.
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

// writeThroughRootTracked replaces rel with data by unlinking it and then creating it exclusively
// without following links, so an existing entry is never truncated in place. It returns the parent
// directories it created, outermost first, even on failure, so a rollback can remove them.
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
