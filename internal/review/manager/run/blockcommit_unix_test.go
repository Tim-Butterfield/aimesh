//go:build !windows

package run

// blockCommit makes the commit's write to the live tree fail, so the receipt can be checked against a
// commit that could not be performed. The mechanism is platform-specific; see
// blockcommit_windows_test.go.

import (
	"os"
	"sync"
	"testing"
)

// blockCommit takes away the workspace directory's write permission, so the unlink the commit needs
// is refused. It returns an idempotent release, and also registers one, so a test that fails early
// still leaves a removable directory behind.
func blockCommit(t *testing.T, ws, _ string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not refuse a write")
	}
	if err := os.Chmod(ws, 0o555); err != nil {
		t.Errorf("chmod: %v", err)
		return func() {}
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = os.Chmod(ws, 0o755) }) }
	t.Cleanup(release)
	return release
}
