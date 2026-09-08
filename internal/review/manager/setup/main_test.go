package setup

import (
	"os"
	"testing"
)

// TestMain pins AIMESH_HOME to an isolated empty temp dir so the setup package's governed writes never
// resolve — or mutate — the developer's real ~/.aimesh state (the shared-file adapter-path write seam
// strips a now-shadowed legacy path from the user-scope config, which is home-anchored). ONE home
// covers both the review config and the shared adapters file now that they live under the same root.
// Individual tests override with t.Setenv for per-test isolation where they assert file contents.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "setup-aimesh-home")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("AIMESH_HOME", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
