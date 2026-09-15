//go:build !windows

package run

// On Unix the durable identity of a reviewed root is its device and inode, recorded as a string so a
// stored decision set can carry it (see WorkspaceIdentity.Key).

import (
	"fmt"
	"io/fs"
	"syscall"
)

// rootIdentityKey returns the device and inode key for a stat result, or "" when Sys() does not carry
// one. The path is unused; it keeps the signature shared with the Windows build. The uint64 conversions
// only need to be consistent, because the key is compared for equality.
func rootIdentityKey(_ string, fi fs.FileInfo) string {
	if fi == nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("dev:%d/ino:%d", uint64(st.Dev), uint64(st.Ino))
}
