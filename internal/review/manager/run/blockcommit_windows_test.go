//go:build windows

package run

// See blockcommit_unix_test.go. On Windows a read-only directory does not refuse creating or deleting
// its entries, so the Unix mechanism would not block the commit. Instead, a handle held without
// FILE_SHARE_DELETE makes any delete or rename-over of the file fail with a sharing violation, and
// needs no privilege.

import (
	"sync"
	"syscall"
	"testing"
)

// blockCommit opens target with delete sharing denied, so the commit's unlink is refused. It returns an
// idempotent release and also registers it as cleanup, because a leaked handle would break
// t.TempDir's cleanup.
func blockCommit(t *testing.T, _ string, target string) func() {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		t.Errorf("target path: %v", err)
		return func() {}
	}
	// Omitting FILE_SHARE_DELETE is the mechanism, so the share mode is spelled out rather than taken
	// from os.Open.
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Errorf("hold %s open against deletion: %v", target, err)
		return func() {}
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = syscall.CloseHandle(h) }) }
	t.Cleanup(release)
	return release
}
