//go:build unix

package workspace

import (
	"io/fs"
	"syscall"
)

// noFollowFlag makes an open refuse a symlink in the FINAL path component. Combined with
// os.Root (which refuses a symlink escaping the root in any intermediate component) it
// closes the check-then-open window: the object validated by f.Stat() is the object the
// descriptor refers to.
const noFollowFlag = syscall.O_NOFOLLOW

// linkCount returns the number of directory entries pointing at fi's inode, and whether
// that number is knowable on this platform. A regular file with more than one link has
// more than one NAME: an innocuous one inside the workspace and, potentially, a protected
// one outside it (`~/.ssh/id_rsa`). Basename-based exclusion cannot see the other name,
// so the link count is the only in-band signal.
func linkCount(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}
