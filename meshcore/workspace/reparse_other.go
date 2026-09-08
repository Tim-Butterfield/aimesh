//go:build !windows

package workspace

import "os"

// hasReparseAttr is a no-op on non-Windows systems: symlinks are already covered
// by the os.ModeSymlink bit in isReparse.
func hasReparseAttr(os.FileInfo) bool { return false }
