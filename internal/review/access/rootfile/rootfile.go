// Package rootfile reads a file through a root handle bound to the directory that authorized it,
// rather than re-opening a path string an earlier check approved. Between a check and an
// os.ReadFile, the file or a parent directory can be replaced with a symlink; reading through the
// handle closes that window.
//
// Read picks the longest authorized root containing the path and opens it as an *os.Root. The open
// is bound by identity: the directory is stat'ed, opened, and the opened handle must be os.SameFile
// with it. The file is then opened relative to that handle with no-follow, and validated on the
// opened descriptor.
//
// Refusals are *workspace.Refusal values with meshcore reason codes, classified like containment
// copy refusals (halt class M6).
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

// ReasonEscapesRoot means the path does not lie under the root it should be read through. The read
// is refused rather than falling back to a path-string read.
const ReasonEscapesRoot = workspace.Reason("read_path_escapes_root")

// testHookBeforeOpen lets a test enter the window between capturing a root's identity and opening
// it. It is nil outside tests.
var testHookBeforeOpen func(dir string)

// Read reads abs, a canonical absolute path returned by a scope.Resolver, through a handle on the
// longest of roots that contains it.
func Read(roots []string, abs string) ([]byte, error) {
	dir, rel, err := boundary(roots, abs)
	if err != nil {
		return nil, err
	}
	return ReadUnder(dir, rel)
}

// ReadUnder reads the root-relative path rel through an identity-bound handle on root.
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

// boundary returns the longest root containing abs and abs's path relative to it.
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
		// The root is the file itself (`--authority ./spec.md`), so bind to its parent directory.
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

// openBound opens dir as an *os.Root and requires the opened directory to be the one dir named
// when its identity was captured. The second stat goes through the open handle, so it reports what
// was opened.
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

// readThrough reads rel through r, validating the opened file.
func readThrough(r *os.Root, rel string) ([]byte, error) {
	name := filepath.FromSlash(filepath.ToSlash(rel))
	// Lstat first so a symlink yields a reparse refusal rather than the platform's raw ELOOP.
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
	// A file with more than one link may also be named by a protected path the denylist cannot
	// see (e.g. `~/.ssh/id_rsa`), so it is refused.
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

// foldPaths is true on Windows, whose filesystem is case-insensitive. It is a variable so tests can
// exercise the Windows branch on any platform.
var foldPaths = runtime.GOOS == "windows"

// samePathPrefix compares two path prefixes, folding case only on Windows.
func samePathPrefix(a, b string) bool {
	if foldPaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}
