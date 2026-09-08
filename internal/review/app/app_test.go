package app

import (
	"io"
	"sync"
	"testing"
)

// TestApp_ConcurrentReloadAndReadNoRace exercises the config-snapshot lock: a web-UI write's
// ReloadConfig must not race concurrent read handlers (SetupManager/Doctor). Run under -race.
func TestApp_ConcurrentReloadAndReadNoRace(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir())
	t.Setenv("REVIEWMESH_ARTIFACT_DIR", t.TempDir())
	a, err := New(Options{})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = a.ReloadConfig() }()
		go func() { defer wg.Done(); _ = a.SetupManager(io.Discard).ConfigOverview() }()
		go func() { defer wg.Done(); _ = a.Doctor("", "", false) }()
	}
	wg.Wait()
}
