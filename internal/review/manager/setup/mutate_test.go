package setup

import (
	"io"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// userSharedPath is the current user-scope shared adapters.yaml path (AIMESH_HOME-anchored). Adapter
// binary PATHS now persist here, not in config.yaml.
func userSharedPath(t *testing.T) string {
	t.Helper()
	p, err := config.SharedUserLocationsPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sharedAdapterPath loads the shared adapters.yaml at path and returns name's saved binary path
// (nil = no entry / not set). Helpers return *string so non-config test files need no direct
// adapterlocations import.
func sharedAdapterPath(t *testing.T, path, name string) *string {
	t.Helper()
	loc, err := adapterlocations.Load(path)
	if err != nil {
		t.Fatalf("load shared adapters.yaml %q: %v", path, err)
	}
	return loc.Adapters[name].Path
}

// mutMgr builds a Manager wired with the full adapter registry + the shipped seed config,
// pointing AIMESH_HOME at a temp dir so user-scope writes are isolated.
func mutMgr(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	return &Manager{
		Adapters: map[string]model.Adapter{
			"fake": fake.New(fake.Valid), "claude-code": fake.New(fake.Valid),
			"codex-cli": fake.New(fake.Valid), "agy-cli": fake.New(fake.Valid),
			"devin-cli": fake.New(fake.Valid), "ollama": fake.New(fake.Valid),
			"gemini-cli": fake.New(fake.Valid),
		},
		Out: io.Discard,
		// Tests exercise the opt-in/smoke profiles (native-three-provider, fully-local-ollama,
		// …) which are not in the shipped seed — merge them in as the example profiles a
		// user would have created.
		Cfg: config.WithExampleProfiles(config.Default()),
	}
}

func TestIsFakeOnlyProfile(t *testing.T) {
	m := mutMgr(t)
	if !m.IsFakeOnlyProfile(config.FakeProfile) {
		t.Error("the shipped hidden fake profile is fake-only")
	}
	if m.IsFakeOnlyProfile("default") {
		t.Error("the shipped default profile is now UNCONFIGURED — it must not be fake-only")
	}
	if m.IsFakeOnlyProfile("fully-local-ollama") {
		t.Error("a fully-local Ollama profile is local but NOT fake-only")
	}
	if m.IsFakeOnlyProfile("native-three-provider") {
		t.Error("a cloud profile is not fake-only")
	}
	if m.IsFakeOnlyProfile("does-not-exist") {
		t.Error("a missing profile is not fake-only")
	}
}
