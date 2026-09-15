//go:build unix

package rootfile

import (
	"io/fs"
	"os"
	"syscall"
)

// noFollowFlag makes an open refuse a symlink in the final path component; os.Root already refuses
// an escaping symlink in any intermediate component.
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
