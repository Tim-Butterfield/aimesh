package manager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// testEnv points AIMESH_HOME at a temp dir so a manager under test never touches the real ~/.aimesh, and
// returns a non-repo cwd so resolution uses the user scope under AIMESH_HOME.
func testEnv(t *testing.T) (home, cwd string) {
	t.Helper()
	home = t.TempDir()
	cwd = t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	// Most fixtures use fakeRoster; the fake adapter resolves only under the internal gate.
	t.Setenv("AIMESH_INTERNAL_FAKE", "1")
	return home, cwd
}

// fakeRoster is a valid two-explorer roster backed by the deterministic `fake` adapter.
func fakeRoster() roster.Roster {
	return roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "fake", Model: "fake-a", Effort: "high"},
			{Adapter: "fake", Model: "fake-b", Effort: "medium"},
		},
		Collator: roster.Collator{Adapter: "fake", Model: "fake-c", Effort: "high"},
	}
}

func newTestManager(t *testing.T, r roster.Roster) *Manager {
	t.Helper()
	_, cwd := testEnv(t)
	m, err := New(cwd, r)
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	return m
}

// defaultName returns the manager's default profile name.
func defaultName(m *Manager) string { return m.Profiles().DefaultProfile }

// defaultProfileDraft returns a deep copy of the default profile, the starting point for a roster write,
// since SaveProfile replaces a profile whole.
func defaultProfileDraft(m *Manager, t *testing.T) profile.Profile {
	t.Helper()
	set := m.Profiles()
	p, ok := set.Get(set.DefaultProfile)
	if !ok {
		t.Fatalf("no default profile %q in the set (have: %v)", set.DefaultProfile, set.Names())
	}
	return p
}

// Saving the default profile re-derives the cached roster the views read.
func TestSaveDefaultProfile_EditsTheCachedRoster(t *testing.T) {
	m := newTestManager(t, fakeRoster())

	p := defaultProfileDraft(m, t)
	p.Explorers = append(p.Explorers, roster.Explorer{Adapter: "fake", Model: "fake-d", Effort: "low"})
	p.Explorers[0].Model = "fake-z"
	p.Collator = roster.Collator{Adapter: "fake", Model: "fake-collate", Effort: "high"}
	if _, err := m.SaveProfile(defaultName(m), p); err != nil {
		t.Fatalf("SaveProfile default: %v", err)
	}

	v := m.RosterView()
	if got := len(v.Explorers); got != 3 {
		t.Fatalf("explorers after save = %d, want 3", got)
	}
	if got := v.Explorers[0].Model; got != "fake-z" {
		t.Fatalf("explorer 0 model after save = %q, want fake-z", got)
	}
	if v.Collator.Model != "fake-collate" || v.Collator.Effort != "high" {
		t.Fatalf("collator after save = %+v, want model fake-collate at high effort", v.Collator)
	}
}

// A duplicate (adapter, model, effort) triple is refused on the profile write and persists nothing.
func TestSaveDefaultProfile_RejectsDuplicateTriple(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	p := defaultProfileDraft(m, t)
	p.Explorers = append(p.Explorers, p.Explorers[0]) // exact duplicate of explorer 0
	if _, err := m.SaveProfile(defaultName(m), p); err == nil {
		t.Fatal("expected a duplicate-triple rejection, got nil")
	}
	if got := len(m.RosterView().Explorers); got != 2 {
		t.Fatalf("explorers after rejected save = %d, want 2", got)
	}
}

// A profile with fewer than two explorers is refused.
func TestSaveDefaultProfile_RejectsBelowTwo(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	p := defaultProfileDraft(m, t)
	p.Explorers = p.Explorers[:1]
	if _, err := m.SaveProfile(defaultName(m), p); err == nil {
		t.Fatal("expected a <2-explorers rejection, got nil")
	}
	if got := len(m.RosterView().Explorers); got != 2 {
		t.Fatalf("explorers after rejected save = %d, want 2", got)
	}
}

// A manager created over the same cwd and home loads the profiles another manager saved.
func TestPersistsAndReloads(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("AIMESH_HOME", home)

	m1, err := New(cwd, fakeRoster())
	if err != nil {
		t.Fatalf("New m1: %v", err)
	}
	p := defaultProfileDraft(m1, t)
	p.Explorers = append(p.Explorers, roster.Explorer{Adapter: "fake", Model: "fake-d", Effort: "low"})
	if _, err := m1.SaveProfile(defaultName(m1), p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	m2, err := New(cwd, roster.Roster{
		Explorers: []roster.Explorer{{Adapter: "fake", Model: "x", Effort: "high"}, {Adapter: "fake", Model: "y", Effort: "low"}},
		Collator:  roster.Collator{Adapter: "fake", Model: "z", Effort: "high"},
	})
	if err != nil {
		t.Fatalf("New m2: %v", err)
	}
	if got := len(m2.RosterView().Explorers); got != 3 {
		t.Fatalf("loaded explorers = %d, want 3 (persisted by m1)", got)
	}
}

func TestConfigureAdapterPath_WritesSharedYAML(t *testing.T) {
	home, cwd := testEnv(t)
	m, err := New(cwd, fakeRoster())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "codexbin")
	if werr := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); werr != nil {
		t.Fatalf("write fake bin: %v", werr)
	}

	if _, err := m.ConfigureAdapterPath("codex-cli", bin); err != nil {
		t.Fatalf("ConfigureAdapterPath: %v", err)
	}

	// The path landed in the user-scope shared adapters.yaml (under AIMESH_HOME=home).
	loc, err := adapterlocations.Load(filepath.Join(home, ".aimesh", "adapters.yaml"))
	if err != nil {
		t.Fatalf("load shared adapters: %v", err)
	}
	e, ok := loc.Adapters["codex-cli"]
	if !ok || e.Path == nil || *e.Path != bin {
		t.Fatalf("shared adapters codex-cli = %+v, want path %q", e, bin)
	}
}

func TestRemoveAdapter_BlockedWhenUsed(t *testing.T) {
	// A roster whose explorer 0 uses codex-cli — removing it must be blocked with the using slot.
	r := roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "codex-cli", Model: "gpt-x", Effort: "high"},
			{Adapter: "fake", Model: "fake-b", Effort: "medium"},
		},
		Collator: roster.Collator{Adapter: "fake", Model: "fake-c", Effort: "high"},
	}
	m := newTestManager(t, r)
	_, err := m.RemoveAdapter("codex-cli")
	if err == nil {
		t.Fatal("expected a blocked error, got nil")
	}
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("error type = %T, want *BlockedError", err)
	}
	if len(be.UsedBy) == 0 || be.UsedBy[0].Ref != "explorer-0" {
		t.Fatalf("blocked usedBy = %+v, want the explorer-0 slot", be.UsedBy)
	}
}

// The fallback display name drops a trailing `cli` word, which is not part of a product name.
func TestTitleCaseKey(t *testing.T) {
	cases := map[string]string{
		"new-vendor-cli": "New Vendor",
		"opencode-cli":   "Opencode",
		"single":         "Single",
		"under_score":    "Under Score",
		"":               "",
		"some-code":      "Some Code", // only `cli` is dropped
		"cli":            "Cli",       // the only word is kept
	}
	for key, want := range cases {
		if got := titleCaseKey(key); got != want {
			t.Errorf("titleCaseKey(%q) = %q, want %q", key, got, want)
		}
	}
}
