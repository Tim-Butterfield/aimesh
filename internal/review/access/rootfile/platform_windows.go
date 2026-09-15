//go:build windows

package rootfile

import (
	"io/fs"
	"os"
	"syscall"
)

// noFollowFlag has no Windows equivalent in the syscall package; the reparse-point checks
// (Lstat before the open, and the attribute check on the opened descriptor) carry the rule
// there instead.
const noFollowFlag = 0

// fileAttributeReparsePoint marks junctions, mount points, and other reparse points (not
// only symlinks) on Windows.
const fileAttributeReparsePoint = 0x400

// hasReparseAttr reports whether a Windows entry carries the reparse-point attribute,
// catching junctions/mount points that os.ModeSymlink alone may miss.
func hasReparseAttr(info os.FileInfo) bool {
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return d.FileAttributes&fileAttributeReparsePoint != 0
}

// linkCount reports that the link count is unknown: Win32FileAttributeData does not carry it, so the
// hardlink rule does not apply on Windows.
func linkCount(fs.FileInfo) (uint64, bool) { return 0, false }
