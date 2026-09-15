package setup

import (
	"os"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

// TestConfigureAdapterPath_DoesNotCreateProfiles is the item-2 guarantee: configuring an adapter
// records adapter configuration ONLY — it never adds/removes/changes the profile set.
func TestConfigureAdapterPath_DoesNotCreateProfiles(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir())
	m := mgrWithAdapters()
	base := t.TempDir()
	bin := execFile(t, "claude")
	if _, err := m.SetAdapterPath(base, "claude-code", bin); err != nil {
		t.Fatalf("SetAdapterPath: %v", err)
	}
	// Recording a path writes ONLY the path-only shared adapters.yaml — it never creates a
	// config.yaml (which is where profiles would live), so the profile set can't change.
	if _, err := os.Stat(config.ProjectConfigPath(base)); !os.IsNotExist(err) {
		t.Errorf("configuring an adapter must not create a project config.yaml (err=%v)", err)
	}
	if got := sharedAdapterPath(t, userSharedPath(t), "claude-code"); got == nil || *got != bin {
		t.Errorf("adapter path not recorded in the shared file: %v", got)
	}
}
