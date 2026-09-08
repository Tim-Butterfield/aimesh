//go:build windows

package workspace

import (
	"os"
	"syscall"
)

// FILE_ATTRIBUTE_REPARSE_POINT marks junctions, mount points, and other reparse
// points (not only symlinks) on Windows.
const fileAttributeReparsePoint = 0x400

// hasReparseAttr reports whether a Windows entry carries the reparse-point
// attribute, catching junctions/mount points that os.ModeSymlink alone may miss.
func hasReparseAttr(info os.FileInfo) bool {
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return d.FileAttributes&fileAttributeReparsePoint != 0
}
