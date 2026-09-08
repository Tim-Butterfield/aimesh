package profile

// The PROFILE half of the canonicalizer spec (design §4): a profile may name the two identities that
// propose the canonicalization, the 0-or-2 rule is enforced on the way in AND on the way out, and the
// pair survives a save/load round trip. A profile is where a half-specified governance rule would persist
// unnoticed, so the validation lives at the persistence boundary rather than only at the surfaces.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

func canonProfile() Profile {
	return Profile{
		Explorers: []roster.Explorer{
			{Adapter: "claude-code", Model: "opus"},
			{Adapter: "codex-cli", Model: "gpt-5-codex"},
		},
		Collator: roster.Collator{Adapter: "claude-code", Model: "opus"},
		Canonicalizers: []roster.Explorer{
			{Adapter: "claude-code", Model: "opus"},
			{Adapter: "codex-cli", Model: "gpt-5-codex"},
		},
	}
}

func TestProfile_CanonicalizersValidateAndProject(t *testing.T) {
	p := canonProfile()
	if err := p.Validate(); err != nil {
		t.Fatalf("a two-identity canonicalizer spec was rejected: %v", err)
	}
	if got := p.Roster().Canonicalizers; len(got) != 2 || got[1].Model != "gpt-5-codex" {
		t.Errorf("Roster() dropped or reordered the canonicalizers: %+v", got)
	}
	// The plan the pipeline consumes carries them, so no surface has to re-plumb the spec.
	plan, err := p.Roster().Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Canonicalizers) != 2 {
		t.Errorf("plan canonicalizers = %+v, want both", plan.Canonicalizers)
	}
}

func TestProfile_RejectsHalfACanonicalizerSpec(t *testing.T) {
	p := canonProfile()
	p.Canonicalizers = p.Canonicalizers[:1]
	err := p.Validate()
	if err == nil {
		t.Fatal("a one-entry canonicalizer spec must be rejected")
	}
	if !strings.Contains(err.Error(), "which slot it fills") {
		t.Errorf("the refusal must teach why one entry is ambiguous, got %q", err)
	}

	// The same refusal applies to an UNCONFIGURED profile that somehow carries one: an unconfigured
	// profile is a valid saved state, a half-specified governance rule is not.
	bare := Profile{Canonicalizers: []roster.Explorer{{Adapter: "a", Model: "m"}}}
	if bare.Validate() == nil {
		t.Error("an unconfigured profile with half a canonicalizer spec must still be rejected")
	}
}

func TestProfile_CanonicalizersSurviveSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.yaml")
	set := Set{SchemaVersion: CurrentSchemaVersion, DefaultProfile: "default",
		Profiles: map[string]Profile{"default": canonProfile()}}
	if err := Save(path, set); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := loaded.Profiles["default"].Canonicalizers
	if len(got) != 2 || got[0].Model != "opus" || got[1].Model != "gpt-5-codex" {
		t.Fatalf("canonicalizers did not survive the round trip: %+v", got)
	}
}

// A profile that names NONE round-trips as none — the derived path is the default and must not acquire a
// spec by accident.
func TestProfile_NoCanonicalizersRoundTripsAsNone(t *testing.T) {
	p := canonProfile()
	p.Canonicalizers = nil
	path := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := Save(path, Set{SchemaVersion: CurrentSchemaVersion, DefaultProfile: "default",
		Profiles: map[string]Profile{"default": p}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Profiles["default"].Canonicalizers; len(got) != 0 {
		t.Errorf("canonicalizers = %+v, want none", got)
	}
}
