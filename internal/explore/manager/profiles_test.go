package manager

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// secondProfile is a valid 3-explorer profile distinct from fakeRoster (so an added profile is
// recognizable after a persistence round-trip), with a non-default defaultMode.
func secondProfile() profile.Profile {
	return profile.Profile{
		Explorers: []roster.Explorer{
			{Adapter: "fake", Model: "wide-1", Effort: "high"},
			{Adapter: "fake", Model: "wide-2", Effort: "medium"},
			{Adapter: "fake", Model: "wide-3", Effort: "low"},
		},
		Collator:    roster.Collator{Adapter: "fake", Model: "wide-collate"},
		DefaultMode: "synthesize",
	}
}

// A fresh manager over an empty home seeds a one-profile `default` set from the caller's starting roster.
func TestProfilesView_SeedsDefaultProfile(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	v := m.ProfilesView()
	if v.DefaultProfile != profile.DefaultProfileName {
		t.Fatalf("default profile = %q, want %q", v.DefaultProfile, profile.DefaultProfileName)
	}
	if len(v.Profiles) != 1 || !v.Profiles[0].IsDefault || v.Profiles[0].ExplorerCount != 2 {
		t.Fatalf("seeded profiles = %+v, want one 2-explorer default", v.Profiles)
	}
}

// TestSaveProfile_AddReplaceAndRoundTrip: add then replace a named profile, each bumping the generation,
// with the result surviving a reload from the persisted file.
func TestSaveProfile_AddReplaceAndRoundTrip(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	t.Setenv("AIMESH_HOME", home)

	m, err := New(cwd, fakeRoster())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msgs, err := m.SaveProfile("wide", secondProfile())
	if err != nil {
		t.Fatalf("SaveProfile add: %v", err)
	}
	if len(msgs) == 0 || !strings.Contains(msgs[0], "Added") {
		t.Errorf("add should report Added, got %v", msgs)
	}
	if got := m.Generation(); got != 1 {
		t.Fatalf("generation after add = %d, want 1", got)
	}

	// Replacing an existing profile reports Updated and bumps again.
	replaced := secondProfile()
	replaced.Explorers = replaced.Explorers[:2]
	replaced.DefaultMode = "map"
	msgs, err = m.SaveProfile("wide", replaced)
	if err != nil {
		t.Fatalf("SaveProfile replace: %v", err)
	}
	if len(msgs) == 0 || !strings.Contains(msgs[0], "Updated") {
		t.Errorf("replace should report Updated, got %v", msgs)
	}
	if got := m.Generation(); got != 2 {
		t.Fatalf("generation after replace = %d, want 2", got)
	}

	// The write round-tripped through the persisted file: a second manager over the same cwd+home reloads it.
	m2, err := New(cwd, fakeRoster())
	if err != nil {
		t.Fatalf("New m2: %v", err)
	}
	if err := m2.reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	d, err := m2.ProfileView("wide")
	if err != nil {
		t.Fatalf("ProfileView after reload: %v", err)
	}
	if len(d.Explorers) != 2 || d.DefaultMode != "map" || d.IsDefault {
		t.Fatalf("reloaded profile = %+v, want the 2-explorer map profile (not the default)", d)
	}
	// The profile editor sees the AUTHORED (preference) order, not a canonicalized one.
	if d.Explorers[0].Model != "wide-1" || d.Explorers[1].Model != "wide-2" {
		t.Errorf("profile explorers must keep their authored order: %+v", d.Explorers)
	}
}

// TestSaveProfile_Rejections: an empty name and an invalid roster are refused, persisting nothing.
func TestSaveProfile_Rejections(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	if _, err := m.SaveProfile("  ", secondProfile()); err == nil {
		t.Error("an empty profile name must be rejected")
	}
	bad := secondProfile()
	bad.Explorers = bad.Explorers[:1] // <2 explorers
	if _, err := m.SaveProfile("thin", bad); err == nil {
		t.Error("a <2-explorer profile must be rejected")
	}
	badMode := secondProfile()
	badMode.DefaultMode = "not-a-mode"
	if _, err := m.SaveProfile("odd", badMode); err == nil {
		t.Error("an unknown defaultMode must be rejected")
	}
	if got := m.Generation(); got != 0 {
		t.Fatalf("generation after rejected writes = %d, want 0", got)
	}
	if got := len(m.ProfilesView().Profiles); got != 1 {
		t.Fatalf("profiles after rejected writes = %d, want just the seeded default", got)
	}
}

// TestDefaultProfileIsPinned: there is no set-default seam — as in reviewmesh, a no-flag run always
// binds to the profile NAMED `default`. Saving another profile must not move the default, and the
// cached default roster keeps tracking `default`.
func TestDefaultProfileIsPinned(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	if _, err := m.SaveProfile("wide", secondProfile()); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if got := m.ProfilesView().DefaultProfile; got != profile.DefaultProfileName {
		t.Fatalf("default profile = %q — saving another profile must never move the default", got)
	}
	// The cached roster still tracks `default` (2 explorers), not the newly saved 3-explorer profile.
	if got := len(m.Roster().Explorers); got != 2 {
		t.Fatalf("default roster explorers = %d, want the default profile's 2", got)
	}
}

// TestDeleteProfile: an unknown name errors; the DEFAULT profile is refused (set another default first);
// a non-default profile deletes and bumps. The "last profile" guard is a defensive second gate — a set's
// only profile is necessarily its default, so the default rule always fires first.
func TestDeleteProfile(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	if _, err := m.DeleteProfile("nope"); err == nil {
		t.Error("deleting an unknown profile must error")
	}
	// The only profile IS the default → refused, with guidance to re-point the default first.
	_, err := m.DeleteProfile(m.ProfilesView().DefaultProfile)
	if err == nil {
		t.Fatal("deleting the default profile must be refused")
	}
	if !strings.Contains(err.Error(), "default profile") {
		t.Errorf("refusal should explain the default-profile rule: %v", err)
	}
	if got := m.Generation(); got != 0 {
		t.Fatalf("generation after refused deletes = %d, want 0", got)
	}

	// Add a second profile; the non-default one deletes cleanly.
	if _, err := m.SaveProfile("wide", secondProfile()); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if _, err := m.DeleteProfile("wide"); err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	if got := m.Generation(); got != 2 {
		t.Fatalf("generation after save+delete = %d, want 2", got)
	}
	if got := len(m.ProfilesView().Profiles); got != 1 {
		t.Fatalf("profiles after delete = %d, want 1", got)
	}
	// …and the deletion round-tripped to disk.
	if err := m.reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, err := m.ProfileView("wide"); err == nil {
		t.Error("the deleted profile must be gone from the persisted set")
	}
}

// TestProfiles_ReturnsADeepCopy: the caller-facing set must not alias the manager's state.
func TestProfiles_ReturnsADeepCopy(t *testing.T) {
	m := newTestManager(t, fakeRoster())
	got := m.Profiles()
	// DefaultProfile is a value field — a by-value return can never alias it. The aliasing risks are
	// the Profiles MAP and the explorer SLICES, so those are what this test mutates.
	delete(got.Profiles, profile.DefaultProfileName)
	if v := m.ProfilesView(); v.DefaultProfile != profile.DefaultProfileName || len(v.Profiles) != 1 {
		t.Fatalf("mutating the returned set reached the manager's state: %+v", v)
	}
	// The explorer slices are copied too.
	ex := m.Profiles().Profiles[profile.DefaultProfileName].Explorers
	ex[0].Model = "clobbered"
	if m.Roster().Explorers[0].Model == "clobbered" {
		t.Error("mutating a returned profile's explorer slice reached the manager's roster")
	}
}
