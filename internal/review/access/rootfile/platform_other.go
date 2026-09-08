//go:build !unix && !windows

package rootfile

import (
	"io/fs"
	"os"
)

// noFollowFlag: no equivalent on this platform; the Lstat/descriptor reparse checks carry
// the rule instead.
const noFollowFlag = 0

// hasReparseAttr: the os.ModeSymlink/ModeIrregular bits are the only signal here.
func hasReparseAttr(os.FileInfo) bool { return false }

// linkCount reports "unknown" rather than a wrong number.
func linkCount(fs.FileInfo) (uint64, bool) { return 0, false }
