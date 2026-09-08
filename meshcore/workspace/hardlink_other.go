//go:build !unix

package workspace

import "io/fs"

// noFollowFlag is 0 where the platform has no O_NOFOLLOW. Confinement still rests on
// os.Root plus the explicit reparse-point check in isReparse (which on Windows also
// catches junctions and mount points).
const noFollowFlag = 0

// linkCount is unknowable here: the caller treats "not knowable" as "no hardlink rule to
// apply" rather than refusing every file. The Windows behavior this leans on is exercised
// only by TestWindows_FilesystemSafety, which runs solely on a Windows machine.
func linkCount(fs.FileInfo) (uint64, bool) { return 0, false }
