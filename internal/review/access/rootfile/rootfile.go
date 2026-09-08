// Package rootfile reads a file THROUGH a root handle bound to the directory that
// authorized it, instead of by re-opening a path STRING that some earlier check approved.
//
// # Why a path string is not a read
//
// A confinement check that resolves a path and then hands the resulting STRING to
// os.ReadFile is racy by construction. Between the resolution and the read, the approved
// file — or any directory component above it — can be replaced with a symlink, and
// os.ReadFile follows it without complaint. The object that was JUDGED and the object that
// is READ are then two different things, and on this code path the second one's bytes land
// in a prompt that is shipped to an external model CLI. A content pin (`expectedHash`)
// catches that, but pins are optional; the mechanism must hold without one.
//
// # The mechanism
//
// This is the same mechanism meshcore/workspace uses for the containment copy, applied to
// the two reads that still went by string (authority documents, remediation file content):
//
//  1. Pick the AUTHORIZED root the resolved path sits under — the longest containing one.
//  2. Open that root as an *os.Root, BOUND BY IDENTITY: stat the directory, open it, fstat
//     the OPENED handle (r.Stat("."), not a second path lookup), and require os.SameFile.
//     os.Root hardens traversal INSIDE a boundary; it does not decide WHICH boundary got
//     opened, so this binding is what makes the open the same act as the check. os.SameFile
//     is the binding primitive because it is exactly this comparison on every platform
//     (device+inode on unix, volume serial + file index on Windows).
//  3. Read the path RELATIVE to that handle. Every component is resolved against the open
//     root and cannot leave it; the final component is opened O_NOFOLLOW; and the result is
//     validated on the OPENED DESCRIPTOR (f.Stat()), never on a pre-open path lookup a
//     racing rename could invalidate.
//
// # Refusal taxonomy
//
// Refusals are *workspace.Refusal values carrying meshcore's own machine reason codes, so a
// caller classifies a refusal from here exactly as it classifies one from the containment
// copy — halt class M6 with the refusing rule's own code — rather than inventing a second
// taxonomy for the same kind of event.
package rootfile

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// op labels every refusal this package raises.
const op = "read"

// ReasonEscapesRoot — the path handed in does not sit under the boundary it was supposed to
// be read through. It is a caller bug or a resolver disagreement, and it fails CLOSED rather
// than falling back to a path-string read.
const ReasonEscapesRoot = workspace.Reason("read_path_escapes_root")

// testHookBeforeOpen is this package's only test seam. It names the check→use window that
// this package narrows but cannot make atomic — between capturing the boundary's identity
// and opening it — and exists ONLY so that window can be entered deterministically in a
// test: staging a real race would be flaky and would prove nothing when it passed. It is nil
// in every non-test build and is reachable from no exported API.
var testHookBeforeOpen func(dir string)

// Read reads an ALREADY-RESOLVED, canonical absolute path through a handle on whichever of
// `roots` contains it (the longest one, so the most specific consent wins).
//
// `abs` must be what a scope.Resolver returned: the resolution decided that this path is
// allowed, and this function decides that the bytes returned really come from that path.
// The two are separate jobs and both are required.
func Read(roots []string, abs string) ([]byte, error) {
	dir, rel, err := boundary(roots, abs)
	if err != nil {
		return nil, err
	}
	return ReadUnder(dir, rel)
}

// ReadUnder reads the root-relative path `rel` through an identity-bound handle on `root`.
// It is the form for a caller that already holds the boundary (an isolated workspace copy
// and a workspace-relative file name).
func ReadUnder(root, rel string) ([]byte, error) {
	if !workspace.SafeRel(rel) {
		return nil, &workspace.Refusal{
			Op: op, Path: rel, Reason: ReasonEscapesRoot,
			Rule: "a read target must be a relative path that stays inside its root",
		}
	}
	r, err := openBound(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return readThrough(r, rel)
}

// boundary picks the root `abs` must be read through, and abs's path relative to it.
//
// The longest containing root wins: when a caller consented to both a workspace and a
// document inside it, the narrower consent is the honest boundary.
func boundary(roots []string, abs string) (string, string, error) {
	abs = filepath.Clean(strings.TrimSpace(abs))
	if abs == "" || abs == "." {
		return "", "", &workspace.Refusal{
			Op: op, Path: abs, Reason: ReasonEscapesRoot,
			Rule: "a read target must be a resolved absolute path",
		}
	}
	best := ""
	for _, root := range roots {
		rc := filepath.Clean(strings.TrimSpace(root))
		if rc == "" || rc == "." {
			continue
		}
		withSep := rc
		if !strings.HasSuffix(withSep, string(filepath.Separator)) {
			withSep += string(filepath.Separator)
		}
		if len(abs) <= len(withSep) || !samePathPrefix(abs[:len(withSep)], withSep) {
			continue
		}
		if len(rc) > len(best) {
			best = rc
		}
	}
	if best == "" {
		// No authorized root is a DIRECTORY above this path. That is the single-file-consent
		// shape: a human may name exactly one path (`--authority ./spec.md`), and the scope
		// resolver then records that FILE as a root, so there is no consented directory to
		// bind to. The boundary is the file's own canonical parent — which still gives the
		// identity binding and the no-follow, descriptor-validated read on the file itself.
		// It is weaker only against a rename of that parent's own ancestors, which no
		// boundary derived from this path could detect either.
		best = filepath.Dir(abs)
	}
	rel, err := filepath.Rel(best, abs)
	if err != nil {
		return "", "", &workspace.Refusal{
			Op: op, Path: abs, Reason: ReasonEscapesRoot,
			Rule: "a read target must sit under the root it is read through", Detail: err.Error(),
		}
	}
	return best, rel, nil
}

// openBound opens dir as an *os.Root and requires the OPENED object to be the object dir
// named when its identity was captured. Swapping the path back after the open cannot help an
// attacker: the second stat goes through the already-open handle, so it reports what was
// opened, not what the name points at now.
func openBound(dir string) (*os.Root, error) {
	want, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if testHookBeforeOpen != nil {
		testHookBeforeOpen(dir)
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
		return nil, &workspace.Refusal{
			Op: op, Path: dir, Reason: workspace.ReasonRootChanged,
			Rule:   "the opened root must be the very object the containment checks approved",
			Detail: "the path was replaced between the check and the open",
		}
	}
	return r, nil
}

// readThrough reads rel through r, validating what was actually OPENED.
func readThrough(r *os.Root, rel string) ([]byte, error) {
	name := filepath.FromSlash(filepath.ToSlash(rel))
	// Lstat first so a symlink is reported as a REPARSE refusal (which teaches) rather than
	// as the platform's raw ELOOP from the O_NOFOLLOW open below.
	if fi, err := r.Lstat(name); err == nil && isReparse(fi) {
		return nil, &workspace.Refusal{
			Op: op, Path: rel, Reason: workspace.ReasonReparse,
			Rule: "symlinks/reparse points are never followed",
		}
	}
	f, err := r.OpenFile(name, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if isReparse(fi) || !fi.Mode().IsRegular() {
		reason, rule := workspace.ReasonNotRegular, "only regular files are read"
		if isReparse(fi) {
			reason, rule = workspace.ReasonReparse, "symlinks/reparse points are never followed"
		}
		return nil, &workspace.Refusal{Op: op, Path: rel, Reason: reason, Rule: rule}
	}
	// A regular file with more than one link has more than one NAME: an innocuous one here
	// and, potentially, a protected one elsewhere (`~/.ssh/id_rsa`). The denylist matches
	// path COMPONENTS, so it cannot see the other name — the link count is the only in-band
	// signal, and these bytes are headed for a prompt.
	if n, ok := linkCount(fi); ok && n > 1 {
		return nil, &workspace.Refusal{
			Op: op, Path: rel, Reason: workspace.ReasonHardlink,
			Rule:   "a regular file with more than one link may name a protected file under another path",
			Detail: fmt.Sprintf("link count %d", n),
		}
	}
	return io.ReadAll(f)
}

// isReparse reports whether an entry is a symlink or any other reparse point (on Windows:
// junctions and mount points, which os.ModeSymlink alone can miss).
func isReparse(fi fs.FileInfo) bool {
	if fi == nil {
		return false
	}
	if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return true
	}
	return hasReparseAttr(fi)
}

// foldPaths is true only on Windows, where the filesystem itself is case-insensitive — the same
// rule meshcore/scope uses. It is a VAR so the Windows branch of the boundary picker can be
// exercised on any platform: this repository does not gate on Windows, so an inline
// `runtime.GOOS ==` would be a containment branch compiled everywhere and executed nowhere.
var foldPaths = runtime.GOOS == "windows"

// samePathPrefix compares two path prefixes, folding case only on Windows.
func samePathPrefix(a, b string) bool {
	if foldPaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}
