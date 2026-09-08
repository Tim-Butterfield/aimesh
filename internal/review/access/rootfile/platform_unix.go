//go:build unix

package rootfile

import (
	"io/fs"
	"os"
	"syscall"
)

// noFollowFlag makes an open refuse a symlink in the FINAL path component. Combined with
// os.Root (which refuses a symlink escaping the root in any intermediate component) it
// closes the check-then-open window: the object validated by f.Stat() is the object the
// descriptor refers to.
const noFollowFlag = syscall.O_NOFOLLOW

// linkCount returns the number of directory entries pointing at fi's inode, and whether that
// number is knowable on this platform.
func linkCount(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}

// hasReparseAttr is a no-op here: symlinks are already covered by the os.ModeSymlink bit.
func hasReparseAttr(os.FileInfo) bool { return false }
