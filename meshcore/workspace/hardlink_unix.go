//go:build unix

package workspace

import (
	"io/fs"
	"syscall"
)

// noFollowFlag makes an open refuse a symlink in the final path component. With os.Root refusing
// escapes in intermediate components, the object validated by f.Stat() is the object the descriptor
// refers to.
const noFollowFlag = syscall.O_NOFOLLOW

// linkCount returns the number of directory entries pointing at fi's inode, and whether that number is
// knowable on this platform. A file with more than one link may also be named by a protected path
// outside the workspace, which name-based exclusion cannot see.
func linkCount(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}
