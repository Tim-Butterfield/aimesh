//go:build windows

package run

// See blockcommit_unix_test.go. On Windows a read-only directory does NOT refuse the creation or
// deletion of entries within it — os.Chmod only toggles the read-only ATTRIBUTE, and on a directory
// that attribute is advisory — so the unix mechanism would leave the commit succeeding and the test
// asserting nothing.
//
// The Windows equivalent of "this file cannot be unlinked" is a mandatory sharing lock: a handle
// held without FILE_SHARE_DELETE makes any delete or rename-over of that file fail with a sharing
// violation, for every other handle in the system. That is the real condition the receipt has to
// describe, and it needs no privilege and no attribute games.

import (
	"sync"
	"syscall"
	"testing"
)

// blockCommit opens target with delete sharing DENIED, so the commit's unlink is refused. It returns
// an idempotent release, and also registers one, so a test that fails early still lets t.TempDir
// clean up — a leaked handle would make the cleanup fail too.
func blockCommit(t *testing.T, _ string, target string) func() {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		t.Errorf("target path: %v", err)
		return func() {}
	}
	// FILE_SHARE_READ|FILE_SHARE_WRITE, deliberately WITHOUT FILE_SHARE_DELETE: that omission is the
	// entire mechanism, so it is spelled out here rather than inherited from whatever share mode
	// os.Open happens to use.
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
