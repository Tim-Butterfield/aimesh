package profile

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// twoValid is a valid 2-explorer + collator roster (distinct triples), the building block every profile
// fixture reuses.
func twoValid() roster.Roster {
	return roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "fake", Model: "fake-a", Effort: "high"},
			{Adapter: "fake", Model: "fake-b", Effort: "medium"},
		},
		Collator: roster.Collator{Adapter: "fake", Model: "fake-c", Effort: "high"},
	}
}

// oneProfileSet is a minimal valid Set: one `default` profile wrapping twoValid.
func oneProfileSet() Set {
	return Set{
		SchemaVersion:  CurrentSchemaVersion,
		DefaultProfile: DefaultProfileName,
		Profiles: map[string]Profile{
			DefaultProfileName: {Explorers: twoValid().Explorers, Collator: twoValid().Collator},
		},
	}
}

func TestProfile_RosterProjectionAndValidate(t *testing.T) {
	p := Profile{Explorers: twoValid().Explorers, Collator: twoValid().Collator, DefaultMode: "synthesize"}
	if err := p.Validate(); err != nil {
		t.Fatalf("a valid profile with a known defaultMode was rejected: %v", err)
	}
	// Roster() must project the profile's ordered explorers + collator unchanged (the profile reuses the
	// roster's rules, it does not re-implement them).
	r := p.Roster()
	if len(r.Explorers) != 2 || r.Collator.Model != "fake-c" {
		t.Errorf("Roster() projection lost data: %+v", r)
	}
}

// TestProfile_RejectsUnknownDefaultMode: an unknown defaultMode is a config typo caught at validate time,
// never silently coerced at run time.
func TestProfile_RejectsUnknownDefaultMode(t *testing.T) {
	p := Profile{Explorers: twoValid().Explorers, Collator: twoValid().Collator, DefaultMode: "bogus"}
	err := p.Validate()
	if err == nil {
		t.Fatal("an unknown defaultMode must be rejected")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "known modes") {
		t.Errorf("error should name the bad mode + list the known modes: %v", err)
	}
	// An EMPTY defaultMode is legal — it means "the app default map".
	p.DefaultMode = ""
	if err := p.Validate(); err != nil {
		t.Errorf("an empty defaultMode must be allowed: %v", err)
	}
}

// TestProfile_InheritsRosterRules: a profile is validated through roster.Validate, so <2 explorers and a
// duplicate triple are rejected without the profile package restating those rules.
func TestProfile_InheritsRosterRules(t *testing.T) {
	one := Profile{Explorers: twoValid().Explorers[:1], Collator: twoValid().Collator}
	if err := one.Validate(); err == nil {
		t.Error("a single-explorer profile is not a panel — must be rejected")
	}
	dup := Profile{
		Explorers: append(twoValid().Explorers, roster.Explorer{Adapter: "fake", Model: "fake-a", Effort: "high"}),
		Collator:  twoValid().Collator,
	}
	if err := dup.Validate(); err == nil {
		t.Error("a duplicate (adapter,model,effort) triple must be rejected")
	}
}

func TestSet_Validate_Rejections(t *testing.T) {
	// (a) no profiles at all.
	if err := (Set{SchemaVersion: 1, DefaultProfile: "x"}).Validate(); err == nil {
		t.Error("a set with no profiles must be rejected")
	}
	// (b) an empty defaultProfile.
	empty := oneProfileSet()
	empty.DefaultProfile = "   "
	if err := empty.Validate(); err == nil {
		t.Error("an empty defaultProfile must be rejected")
	}
	// (c) a defaultProfile that names a profile that is not defined — the error lists what IS defined.
	missing := oneProfileSet()
	missing.DefaultProfile = "nope"
	err := missing.Validate()
	if err == nil {
		t.Fatal("a defaultProfile naming an undefined profile must be rejected")
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), DefaultProfileName) {
		t.Errorf("error should name the bad default + list the defined profiles: %v", err)
	}
	// (d) a profile whose roster is invalid (only 1 explorer) — the error names the offending profile.
	badRoster := oneProfileSet()
	badRoster.Profiles["broken"] = Profile{Explorers: twoValid().Explorers[:1], Collator: twoValid().Collator}
	if err := badRoster.Validate(); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("an invalid profile roster must be rejected naming the profile, got: %v", err)
	}
	// (e) a profile with an unknown defaultMode.
	badMode := oneProfileSet()
	badMode.Profiles["odd"] = Profile{Explorers: twoValid().Explorers, Collator: twoValid().Collator, DefaultMode: "nope-mode"}
	if err := badMode.Validate(); err == nil || !strings.Contains(err.Error(), "odd") {
		t.Errorf("an unknown defaultMode must be rejected naming the profile, got: %v", err)
	}
}

func TestSet_NamesGetDefault(t *testing.T) {
	s := oneProfileSet()
	s.Profiles["alpha"] = Profile{Explorers: twoValid().Explorers, Collator: twoValid().Collator}
	// Names is sorted — a stable listing + error-message order regardless of map iteration.
	if got := s.Names(); len(got) != 2 || got[0] != "alpha" || got[1] != DefaultProfileName {
		t.Errorf("Names() = %v, want sorted [alpha default]", got)
	}
	if _, ok := s.Get("alpha"); !ok {
		t.Error("Get must find a defined profile")
	}
	if _, ok := s.Get("Alpha"); ok {
		t.Error("Get is an EXACT match — a case variant must not resolve")
	}
	def, err := s.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if len(def.Explorers) != 2 {
		t.Errorf("Default() returned the wrong profile: %+v", def)
	}
	// Default validates first: a broken set surfaces the validation error, not a zero profile.
	broken := oneProfileSet()
	broken.DefaultProfile = "gone"
	if _, err := broken.Default(); err == nil {
		t.Error("Default must surface a validation failure")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	orig := oneProfileSet()
	// A second profile with a NON-alphabetical explorer order proves the authored (preference) order
	// survives persistence — it is load-bearing for --count top-N selection.
	orig.Profiles["wide"] = Profile{
		Explorers: []roster.Explorer{
			{Adapter: "fake", Model: "z-first", Effort: "high"},
			{Adapter: "fake", Model: "a-second", Effort: "low"},
			{Adapter: "fake", Model: "m-third"},
		},
		Collator:    roster.Collator{Adapter: "fake", Model: "fake-c", Effort: "high"},
		DefaultMode: "synthesize",
	}
	if err := Save(path, orig); err != nil {
		t.Fatalf("save: %v", err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if back.SchemaVersion != CurrentSchemaVersion || back.DefaultProfile != DefaultProfileName {
		t.Errorf("round-trip lost the version/default: %+v", back)
	}
	w, ok := back.Get("wide")
	if !ok {
		t.Fatal("round-trip lost the `wide` profile")
	}
	if w.DefaultMode != "synthesize" || w.Collator.Effort != "high" {
		t.Errorf("round-trip lost profile fields: %+v", w)
	}
	if len(w.Explorers) != 3 || w.Explorers[0].Model != "z-first" || w.Explorers[2].Model != "m-third" {
		t.Errorf("round-trip must preserve the AUTHORED explorer order, got %+v", w.Explorers)
	}
}

// TestSave_RefusesInvalid: Save validates first, so an invalid set never reaches disk.
func TestSave_RefusesInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	bad := oneProfileSet()
	bad.DefaultProfile = "not-defined"
	if err := Save(path, bad); err == nil {
		t.Fatal("Save must refuse to persist an invalid set")
	}
	if _, err := Load(path); err == nil {
		t.Error("a refused Save must have written nothing")
	}
}

// TestDecode_SchemaVersion covers the versioned migration seam: an ABSENT version defaults to the current
// one (forward-lenient), and a NEWER version is refused rather than mis-parsed.
func TestDecode_SchemaVersion(t *testing.T) {
	body := `{"defaultProfile":"default","profiles":{"default":{"explorers":[{"adapter":"fake","model":"a"},{"adapter":"fake","model":"b"}],"collator":{"adapter":"fake","model":"c"}}}}`
	s, err := Decode("profiles.json", []byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("an absent schemaVersion must default to %d, got %d", CurrentSchemaVersion, s.SchemaVersion)
	}
	newer := `{"schemaVersion":999,"defaultProfile":"default","profiles":{}}`
	_, err = Decode("profiles.json", []byte(newer))
	if err == nil {
		t.Fatal("a schemaVersion newer than this build supports must be refused")
	}
	if !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("the refusal should say the file is newer: %v", err)
	}
}

// TestDecode_RejectsUnknownField: strict decode — an unknown key is a typo, not a silently ignored field.
func TestDecode_RejectsUnknownField(t *testing.T) {
	if _, err := Decode("profiles.json", []byte(`{"defaultProfile":"d","bogusField":true}`)); err == nil {
		t.Fatal("strict decode must reject an unknown field")
	}
}

// TestFromRoster_MigratesToDefaultProfile: the versioned migration of a legacy single roster into the
// multi-profile model — one profile named `default`, which is also the default profile.
func TestFromRoster_MigratesToDefaultProfile(t *testing.T) {
	s := FromRoster(twoValid())
	if err := s.Validate(); err != nil {
		t.Fatalf("a migrated set must be valid: %v", err)
	}
	if s.SchemaVersion != CurrentSchemaVersion || s.DefaultProfile != DefaultProfileName {
		t.Errorf("migration must stamp the current version + the `default` profile: %+v", s)
	}
	if len(s.Profiles) != 1 {
		t.Errorf("migration must produce exactly one profile, got %d", len(s.Profiles))
	}
	p, _ := s.Get(DefaultProfileName)
	if len(p.Explorers) != 2 || p.Collator.Model != "fake-c" || p.DefaultMode != "" {
		t.Errorf("migrated profile mismatch (defaultMode must stay empty = the app default map): %+v", p)
	}
}

// TestDefaultSet_ShipsUnconfigured: a fresh install ships a single `default` profile that is
// deliberately EMPTY — it validates (unconfigured is a legal saved state, so the workbench can open
// on it) but is not configured, so nothing can run and no adapter is pre-selected. Mirrors
// reviewmesh's unconfigured shipped default.
func TestDefaultSet_ShipsUnconfigured(t *testing.T) {
	s := DefaultSet()
	if err := s.Validate(); err != nil {
		t.Fatalf("DefaultSet must be valid: %v", err)
	}
	p, err := s.Default()
	if err != nil {
		t.Fatal(err)
	}
	if p.IsConfigured() {
		t.Errorf("the shipped default must be unconfigured, got %+v", p)
	}
	if len(p.Explorers) != 0 || p.Collator.Adapter != "" {
		t.Errorf("no explorer or collator may ship pre-selected, got %+v", p)
	}
}

// TestProfileValidate_UnconfiguredVsPartial: a fully EMPTY profile is a valid saved state, but a
// PARTIALLY configured one (some panel members, below the minimum) is still rejected strictly.
func TestProfileValidate_UnconfiguredVsPartial(t *testing.T) {
	if err := (Profile{}).Validate(); err != nil {
		t.Errorf("an empty (unconfigured) profile must validate: %v", err)
	}
	partial := Profile{Explorers: []roster.Explorer{{Adapter: "fake", Model: "fake-a"}}}
	if err := partial.Validate(); err == nil {
		t.Error("a partially configured profile (1 explorer) must fail validation")
	}
}
