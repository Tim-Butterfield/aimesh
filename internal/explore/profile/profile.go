// Package profile is exploremesh's MULTI-profile configuration container (design §7): a named set of
// exploration profiles, each an ORDERED explorer roster (preference order) + a collator + a per-profile
// default mode, plus the name of the profile a no-flag run binds to. It is the reviewmesh-style evolution
// of the single roster.Roster — the app-side "grammar" that meshcore never learns — and it reuses
// roster.Roster's Validate/Plan and meshcore/config's domain-free store (strict decode + atomic write).
//
// A profile's explorer slice ORDER is preference (selection priority), decoupled from the attribution
// order the Plan canonicalizes (roster.SelectTopN): reordering the full list never changes the envelope
// IDs of an unchanged selected set. Persistence is versioned. An explicit `--roster <file>` is wrapped
// into a one-profile `default` set by FromRoster — a per-invocation input, never a discovered location.
package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	mcfg "github.com/Tim-Butterfield/aimesh/meshcore/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"gopkg.in/yaml.v3"

	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// CurrentSchemaVersion is the profiles-file schema version this build writes + reads. It is a versioned
// migration seam: a file with a NEWER version than this is refused (the format may have changed
// incompatibly), an ABSENT version (0) is treated as the current version (lenient forward-decode of a
// file this build itself wrote before the field mattered), and a legacy roster.yaml (no version) migrates
// to this version (FromRoster).
const CurrentSchemaVersion = 1

// DefaultProfileName is the name of the profile a fresh install ships (and the name a legacy roster.yaml
// migrates into): the built-in demo `default`.
const DefaultProfileName = "default"

// Profile is one named exploration profile (design §7): an ORDERED explorer roster (the slice order is
// PREFERENCE = selection priority), a single collator, and an OPTIONAL default mode that supplies the
// exploration mode when a run omits --mode. It reuses roster.Roster's rules via Roster().
type Profile struct {
	// Explorers is the ORDERED explorer set — the authored slice order is the preference order
	// SelectTopN selects the top-N from. It is NOT the attribution order (the Plan canonicalizes that).
	Explorers []roster.Explorer `json:"explorers"`
	Collator  roster.Collator   `json:"collator"`
	// Canonicalizers names the two identities that propose the canonicalization for a canonicalizing mode
	// (design §4). EITHER empty — the host derives them — or exactly two; roster.ValidateCanonicalizers
	// states why one is refused. It lives on the profile beside the collator because it is the same kind of
	// fact: a GOVERNED identity chosen before the run, not part of the task.
	Canonicalizers []roster.Explorer `json:"canonicalizers,omitempty"`
	// DefaultMode is the mode a run uses when --mode is omitted (empty → the app default, mode.DefaultName).
	// A non-empty value must name a registered mode (validated).
	DefaultMode string `json:"defaultMode,omitempty"`
}

// Roster projects the profile's ordered explorers + collator + canonicalizers as a roster.Roster, so a
// profile reuses the roster's Validate / Plan / SelectTopN unchanged.
func (p Profile) Roster() roster.Roster {
	return roster.Roster{Explorers: p.Explorers, Collator: p.Collator, Canonicalizers: p.Canonicalizers}
}

// Validate enforces the roster rules (2+ unique explorers, one valid collator) plus the profile-only rule
// that a non-empty DefaultMode names a registered mode (an unknown default mode is a config typo caught
// before it silently coerces at run time).
func (p Profile) Validate() error {
	// An UNCONFIGURED profile — no explorers and no collator — is a valid saved state: the shipped
	// `default` deliberately ships this way (nothing runs until the user configures a panel), exactly
	// like reviewmesh's unconfigured default. Anything PARTIALLY configured is validated strictly;
	// refusing to RUN an unconfigured profile is the run path's job, with a friendlier message.
	if p.IsConfigured() {
		if err := p.Roster().Validate(); err != nil {
			return err
		}
	} else if err := roster.ValidateCanonicalizers(p.Canonicalizers); err != nil {
		// A profile can only be unconfigured-with-canonicalizers if someone hand-wrote a malformed one; say
		// so rather than letting a half-specified governance rule persist unremarked.
		return err
	}
	if m := strings.TrimSpace(p.DefaultMode); m != "" {
		if _, ok := mode.Lookup(m); !ok {
			return fault.New(fault.Config, fmt.Sprintf("profile: unknown defaultMode %q (known modes: %s)", m, strings.Join(mode.Names(), ", ")))
		}
	}
	return nil
}

// IsConfigured reports whether the profile has ANY panel configuration (explorers or a collator).
// The zero value — the shipped unconfigured `default` — reports false.
func (p Profile) IsConfigured() bool {
	return len(p.Explorers) > 0 || p.Collator != (roster.Collator{})
}

// Set is the full multi-profile container persisted as profiles.yaml (design §7): the schema version, the
// name of the default profile a no-flag run binds to, and the named profiles. Profile ORDER within the
// map is not load-bearing (the default is named, not positional); explorer order WITHIN a profile is.
type Set struct {
	SchemaVersion  int                `json:"schemaVersion"`
	DefaultProfile string             `json:"defaultProfile"`
	Profiles       map[string]Profile `json:"profiles"`
}

// Validate enforces: at least one profile; a non-empty DefaultProfile that is present in the map; every
// profile name non-empty; every profile valid.
func (s Set) Validate() error {
	if len(s.Profiles) == 0 {
		return fault.New(fault.Config, "profiles: need at least one profile")
	}
	if strings.TrimSpace(s.DefaultProfile) == "" {
		return fault.New(fault.Config, "profiles: defaultProfile is required")
	}
	if _, ok := s.Profiles[s.DefaultProfile]; !ok {
		return fault.New(fault.Config, fmt.Sprintf("profiles: defaultProfile %q is not one of the defined profiles (%s)", s.DefaultProfile, strings.Join(s.Names(), ", ")))
	}
	for name, p := range s.Profiles {
		if strings.TrimSpace(name) == "" {
			return fault.New(fault.Config, "profiles: a profile name must be non-empty")
		}
		if err := p.Validate(); err != nil {
			return fault.Wrap(fault.Config, fmt.Sprintf("profile %q", name), err)
		}
	}
	return nil
}

// Names returns the profile names in sorted order (a stable listing + error-message order).
func (s Set) Names() []string {
	out := make([]string, 0, len(s.Profiles))
	for n := range s.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Get returns the profile registered under name (exact match).
func (s Set) Get(name string) (Profile, bool) {
	p, ok := s.Profiles[name]
	return p, ok
}

// Default returns the default profile (Profiles[DefaultProfile]) or an error if the set does not resolve.
func (s Set) Default() (Profile, error) {
	if err := s.Validate(); err != nil {
		return Profile{}, err
	}
	return s.Profiles[s.DefaultProfile], nil
}

// FromRoster wraps a single roster.Roster as a one-profile Set named `default` (the versioned MIGRATION of
// a legacy single-roster config into the multi-profile model). DefaultMode is empty (the app default map).
func FromRoster(r roster.Roster) Set {
	return Set{
		SchemaVersion:  CurrentSchemaVersion,
		DefaultProfile: DefaultProfileName,
		Profiles: map[string]Profile{
			DefaultProfileName: {Explorers: r.Explorers, Collator: r.Collator, Canonicalizers: r.Canonicalizers},
		},
	}
}

// DefaultSet is the profiles set a fresh install ships: a single `default` profile that is deliberately
// UNCONFIGURED — no explorers and no collator — mirroring reviewmesh's unconfigured shipped default.
// Nothing runs, and no adapter (not even the deterministic `fake`) is selected, until the user
// configures the panel themselves (workbench, `setup --profile`, or the profiles file). The `fake`
// adapter still resolves BY NAME for anyone who writes it into a profile — tests, the golden run, and
// hermetic demos do — but it is a test fixture, not a shipped default and not a workbench choice.
func DefaultSet() Set {
	return Set{
		SchemaVersion:  CurrentSchemaVersion,
		DefaultProfile: DefaultProfileName,
		Profiles: map[string]Profile{
			DefaultProfileName: {},
		},
	}
}

// Decode strict-decodes profiles bytes (YAML or JSON by extension) into a Set, defaulting an absent schema
// version to the current one and REFUSING a version newer than this build supports. It does NOT run
// Validate (callers decide when), matching roster.Decode.
func Decode(path string, b []byte) (Set, error) {
	var s Set
	if mcfg.IsYAML(path) {
		jb, err := mcfg.YAMLToJSON(b)
		if err != nil {
			return s, fault.Wrap(fault.Config, fmt.Sprintf("parse YAML profiles %q", path), err)
		}
		b = jb
	}
	if err := mcfg.DecodeStrict(b, &s); err != nil {
		return s, fault.Wrap(fault.Config, fmt.Sprintf("parse profiles %q", path), err)
	}
	if s.SchemaVersion == 0 {
		s.SchemaVersion = CurrentSchemaVersion // absent → treat as current (forward-lenient)
	}
	if s.SchemaVersion > CurrentSchemaVersion {
		return s, fault.New(fault.Config, fmt.Sprintf("profiles %q: schema version %d is newer than this build supports (%d) — upgrade exploremesh", path, s.SchemaVersion, CurrentSchemaVersion))
	}
	return s, nil
}

// Load reads + strict-decodes + validates the profiles file at path.
func Load(path string) (Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Set{}, fault.Wrap(fault.Config, fmt.Sprintf("read profiles %q", path), err)
	}
	s, err := Decode(path, b)
	if err != nil {
		return Set{}, err
	}
	if verr := s.Validate(); verr != nil {
		return Set{}, verr
	}
	return s, nil
}

// Save validates the set and writes it atomically as YAML (camelCase keys via json tags) through
// meshcore/config's atomic writer. It refuses to persist an invalid set, and normalizes the on-disk schema
// version to the current one.
func Save(path string, s Set) error {
	if s.SchemaVersion == 0 {
		s.SchemaVersion = CurrentSchemaVersion
	}
	if err := s.Validate(); err != nil {
		return err
	}
	// Route through the json tags so on-disk keys match the decode schema (YAML is a JSON superset).
	jb, err := json.Marshal(s)
	if err != nil {
		return fault.Wrap(fault.Internal, "marshal profiles", err)
	}
	var v any
	if err := json.Unmarshal(jb, &v); err != nil {
		return fault.Wrap(fault.Internal, "marshal profiles", err)
	}
	yb, err := yaml.Marshal(v)
	if err != nil {
		return fault.Wrap(fault.Internal, "marshal YAML profiles", err)
	}
	return mcfg.WriteFileAtomic(path, yb)
}
