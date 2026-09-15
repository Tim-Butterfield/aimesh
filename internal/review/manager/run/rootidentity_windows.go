//go:build windows

package run

// On Windows the durable identity is the volume serial number plus the 64-bit file index.
// fs.FileInfo.Sys() does not carry the index, so it is read from an open handle. Capturing it eagerly
// matters: os.SameFile loads the index lazily by path, so it cannot detect a directory swapped in under
// the same path (see WorkspaceIdentity.Key).

import (
	"fmt"
	"io/fs"
	"syscall"
)

// rootIdentityKey returns the volume and file-index key for the object at path, or "" if it cannot be
// opened. The FileInfo is unused; it keeps the signature shared with the Unix build.
//
// FILE_FLAG_BACKUP_SEMANTICS is needed to open a directory. Access 0 reads only metadata, and the fully
// permissive share mode keeps concurrent writes, renames and deletes from failing.
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
