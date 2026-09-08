package workspace

import (
	"fmt"
	"io"
	"strings"
)

// ReasonTooLarge — a bounded read found more bytes than the caller allowed. It is a
// REFUSAL rather than a truncation on purpose: a caller that asked for a whole artifact
// and silently received part of one has no way to tell the difference, and a truncated
// artifact that still parses is worse than an error.
const ReasonTooLarge Reason = "workspace_read_too_large"

// ReasonUnsafeRel — the root-relative name handed to a bounded read is absolute, carries a
// volume, or climbs out of its root. It fails closed rather than being cleaned into
// something that happens to stay inside.
const ReasonUnsafeRel Reason = "workspace_unsafe_relative_path"

// MaxReadUnderBytes is ReadUnder's default bound when a caller passes 0.
const MaxReadUnderBytes int64 = 8 << 20 // 8 MiB

// ReadUnder reads the root-relative path `rel` through an IDENTITY-BOUND handle on `root`,
// refusing anything larger than maxBytes (0 → MaxReadUnderBytes).
//
// It is the exported form of the rule this package already applies to every containment
// read: the directory is stat-ed, opened, and fstat-ed through its own handle so the object
// opened is the object approved (os.SameFile); every component of `rel` is resolved against
// that open root and cannot leave it; the final component is opened O_NOFOLLOW; and the
// result is validated on the OPENED DESCRIPTOR rather than on a pre-open path lookup a
// racing rename could invalidate. Symlinks and other reparse points, non-regular files and
// hardlinked regular files are refused.
//
// It exists because reading a file back OUT of a directory this process wrote — a run
// record's artifacts, served over a protocol — is the same act as reading one into a
// prompt, and re-deriving the rule at a second call site is how two code paths that both
// look careful end up disagreeing.
func ReadUnder(root, rel string, maxBytes int64) ([]byte, error) {
	const op = "read"
	if maxBytes <= 0 {
		maxBytes = MaxReadUnderBytes
	}
	if strings.TrimSpace(root) == "" {
		return nil, &Refusal{
			Op: op, Path: rel, Reason: ReasonUnsafeRel,
			Rule: "a bounded read needs the directory it is read through",
		}
	}
	if !safeRel(rel) {
		return nil, &Refusal{
			Op: op, Path: rel, Reason: ReasonUnsafeRel,
			Rule: "a read target must be a relative path that stays inside its root",
		}
	}
	r, err := openRootBound(op, root, nil)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, fi, err := openRegular(r, op, rel, true)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi.Size() > maxBytes {
		return nil, &Refusal{
			Op: op, Path: rel, Reason: ReasonTooLarge,
			Rule:   "a bounded read refuses an oversized artifact rather than truncating it",
			Detail: fmt.Sprintf("%d bytes exceeds the %d-byte limit", fi.Size(), maxBytes),
		}
	}
	// The size check above is on the OPENED descriptor, but a file being appended to between
	// the fstat and the read would still slip past it, so the read itself is bounded too:
	// one extra byte is requested, and its presence is the refusal.
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, &Refusal{
			Op: op, Path: rel, Reason: ReasonTooLarge,
			Rule:   "a bounded read refuses an oversized artifact rather than truncating it",
			Detail: fmt.Sprintf("the file grew past the %d-byte limit during the read", maxBytes),
		}
	}
	return b, nil
}

// StatUnder reports the size of the root-relative path `rel` through the same
// identity-bound handle ReadUnder uses, applying the same reparse/regular-file rules. It is
// for a caller that must publish an artifact's size without reading it.
func StatUnder(root, rel string) (int64, error) {
	const op = "stat"
	if !safeRel(rel) {
		return 0, &Refusal{
			Op: op, Path: rel, Reason: ReasonUnsafeRel,
			Rule: "a read target must be a relative path that stays inside its root",
		}
	}
	r, err := openRootBound(op, root, nil)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	f, fi, err := openRegular(r, op, rel, true)
	if err != nil {
		return 0, err
	}
	_ = f.Close()
	return fi.Size(), nil
}
