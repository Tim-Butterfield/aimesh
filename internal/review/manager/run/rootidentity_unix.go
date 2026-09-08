//go:build !windows

package run

// The DURABLE form of a reviewed root's filesystem identity.
//
// `WorkspaceIdentity.Info` is an `fs.FileInfo`, and a decision set that outlives its process has to
// carry the identity somehow — otherwise the report→apply binding degrades to a pathname, which is
// precisely what WorkspaceIdentity exists to refuse. So the same pair `os.SameFile` compares on this
// platform (device + inode) is recorded as a string, and compared as a string when the set is read
// back.
//
// The key is also what makes the comparison a POINT-IN-TIME capture rather than a deferred one; see
// CaptureWorkspaceIdentity for why that distinction is load-bearing and not merely a serialization
// convenience.

import (
	"fmt"
	"io/fs"
	"syscall"
)

// rootIdentityKey returns the durable device+inode key for a stat result, or "" when the platform's
// `Sys()` does not carry one. The path is unused here — the identity is already in the stat result —
// and is taken for a signature the Windows build can also satisfy.
//
// The conversions to uint64 are deliberate and are not a claim about the field's signedness: the
// value is an EQUALITY KEY, never arithmetic, and a fixed conversion produces the same string for
// the same inode on the same machine, which is the only property it needs.
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
