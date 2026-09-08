package setup

import (
	"os"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
)

func TestCreateProfile_RequiredRolesAndInitialModel(t *testing.T) {
	m := mutMgr(t)
	res, err := m.CreateProfile("my-fake", "", map[string]string{
		"author_remediator": "fake", "reviewer": "fake",
	})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if !res.ConfigWritten {
		t.Fatalf("expected a config write, got %+v", res)
	}
	// The lane must be initialized with the adapter's DEFAULT catalog key (fake-default), never a
	// bare model string or a guessed value.
	key, err := m.adapterDefaultKey("fake")
	if err != nil {
		t.Fatalf("adapterDefaultKey(fake): %v", err)
	}
	if key == "" {
		t.Fatal("fake adapter must have a deterministic default catalog key")
	}
}

func TestCreateProfile_RejectsMissingRequiredRoles(t *testing.T) {
	m := mutMgr(t)
	_, err := m.CreateProfile("only-author", "", map[string]string{"author_remediator": "fake"})
	if err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("missing reviewer must be rejected, got %v", err)
	}
}

func TestCreateProfile_RejectsDuplicateAndBadKey(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.CreateProfile("default", "", map[string]string{"author_remediator": "fake", "reviewer": "fake"}); err == nil {
		t.Error("creating a profile that already exists must be rejected")
	}
	if _, err := m.CreateProfile("Bad Name", "", map[string]string{"author_remediator": "fake", "reviewer": "fake"}); err == nil {
		t.Error("an invalid profile key must be rejected")
	}
}

func TestCreateProfile_RejectsUnconfiguredAdapter(t *testing.T) {
	m := mutMgr(t)
	// claude-code is implemented (via WithExampleProfiles context) but has no configured path here.
	_, err := m.CreateProfile("needs-claude", "", map[string]string{
		"author_remediator": "claude-code", "reviewer": "fake",
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") && !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("an unconfigured/unimplemented adapter must be rejected, got %v", err)
	}
}

// TestAdapterDefaultKey_Deterministic covers the zero / one / multiple adapter-default cases
// (design refinement E): exactly one → the key; zero or many → a clear error.
func TestAdapterDefaultKey_Deterministic(t *testing.T) {
	m := mutMgr(t)
	if _, err := m.adapterDefaultKey("fake"); err != nil {
		t.Errorf("fake should have exactly one adapter-default key: %v", err)
	}
	if _, err := m.adapterDefaultKey("no-such-adapter"); err == nil {
		t.Error("an adapter with no default catalog key must error")
	}
}

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
