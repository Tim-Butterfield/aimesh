package workspace

import "os"

// isReparse reports whether a filesystem entry is a symlink or other reparse point (on Windows,
// junctions and mount points), which can redirect outside the workspace and so is never followed when
// copying or written through when committing. The Windows reparse attribute is checked in
// reparse_windows.go; elsewhere the mode bits suffice.
func isReparse(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	return reparseAttr(info)
}

// reparseAttr is the reparse-attribute check, held in a variable so tests can simulate a Windows
// junction on any platform. A junction carries the reparse attribute without the symlink mode bit, so
// only this check stops a copy from following it out of the workspace. Production code never assigns
// to it.
var reparseAttr = hasReparseAttr
