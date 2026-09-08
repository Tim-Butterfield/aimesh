//go:build windows

package run

// See rootidentity_unix.go for what this is and why it exists.
//
// Windows DOES have a durable identity — volume serial number plus the 64-bit file index — but
// `fs.FileInfo.Sys()` does not carry it: that returns a `*syscall.Win32FileAttributeData`, which
// holds timestamps, attributes and size, and no index. The identity is only reachable from an OPEN
// HANDLE, so this build opens one rather than reporting no key.
//
// Reporting no key here was not a neutral choice. It disabled the durable check in
// `BindWorkspace`, and — because `os.Stat` on Windows defers loading the file index to a lazy
// `loadFileId` that reopens BY PATH the first time `os.SameFile` is called — it also made the
// in-process comparison in `verifyAgainst` re-resolve the path instead of comparing the captured
// object. Both ends of the report→apply binding therefore rested on the canonical path plus the
// content pins, and a substituted directory reached by the same path with byte-identical content
// satisfies both. The first native Windows run caught exactly that: a swapped workspace was accepted
// with no reason code. Capturing the key eagerly is what closes it.

import (
	"fmt"
	"io/fs"
	"syscall"
)

// rootIdentityKey returns the durable volume+file-index key for the object at path, or "" if it
// cannot be opened. The FileInfo is unused here — Windows does not expose the index through it —
// and is taken for a signature the unix build can also satisfy.
//
// FILE_FLAG_BACKUP_SEMANTICS is required: without it CreateFile refuses to open a DIRECTORY, and a
// workspace root is always a directory. Access is 0 (metadata only), so this neither reads the
// directory nor needs read permission on it, and the share mode is fully permissive so that merely
// identifying the root cannot make a concurrent write, rename or delete fail.
func rootIdentityKey(path string, _ fs.FileInfo) string {
	if path == "" {
		return ""
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	h, err := syscall.CreateFile(
		p,
		0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return ""
	}
	// The index is reported as two 32-bit halves; join them into the single 64-bit value they are.
	idx := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fmt.Sprintf("vol:%d/idx:%d", uint64(info.VolumeSerialNumber), idx)
}
