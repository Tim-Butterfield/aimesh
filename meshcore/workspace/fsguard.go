package workspace

import "os"

// isReparse reports whether a filesystem entry is a symlink or any other reparse
// point (on Windows: junctions, mount points, and other reparse points) — i.e.
// anything that can redirect outside the workspace and therefore must never be
// followed when copying, or written through when committing. The Windows reparse
// attribute is checked in reparse_windows.go; on other OSes the symlink/irregular
// mode bits are sufficient (reparse_other.go is a no-op).
func isReparse(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	return reparseAttr(info)
}

// reparseAttr is the reparse-ATTRIBUTE probe, held in a var for one reason: on unix the
// attribute half of isReparse is unreachable (reparse_other.go returns false), so the guards
// that depend on it are otherwise provably untestable — deleting the `isReparse` call in the
// copy walk fails nothing there, because a POSIX symlink is caught by the `IsRegular` filter
// one line below regardless. A Windows JUNCTION is not: it carries the
// reparse attribute WITHOUT the symlink mode bit, so it looks like an ordinary entry to every
// other check in this package, and that guard is the only thing standing between it and a
// copy that follows it out of the workspace.
//
// Substituting it lets a test simulate exactly that entry on any platform. It is the same
// pattern `streamAliasing` already uses here and in meshcore/scope for the other Windows-only
// aliasing rule; production code never assigns to it.
var reparseAttr = hasReparseAttr
