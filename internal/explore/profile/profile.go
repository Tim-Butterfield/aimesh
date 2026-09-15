// Package profile stores named exploration profiles. A profile is an ordered explorer list, a collator,
// optional canonicalizers and a default mode; a Set names the default profile. Validation reuses
// roster.Roster, and persistence uses meshcore/config's strict decoding and atomic writes.
//
// A profile's explorer order is preference order for selection. It is independent of the attribution
// order the plan uses, so reordering the list does not change the envelope ids of an unchanged panel.
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

// CurrentSchemaVersion is the profiles-file schema version this build reads and writes. A newer version
// is refused; an absent version is treated as current.
const CurrentSchemaVersion = 1

// DefaultProfileName is the name of the profile a fresh install ships.
const DefaultProfileName = "default"

// Profile is one named exploration profile.
type Profile struct {
	// Explorers is the explorer list in preference order.
	Explorers []roster.Explorer `json:"explorers"`
	Collator  roster.Collator   `json:"collator"`
	// Canonicalizers is either empty, so the host derives them, or exactly two identities (see
	// roster.ValidateCanonicalizers).
	Canonicalizers []roster.Explorer `json:"canonicalizers,omitempty"`
	// DefaultMode is the mode used when a run names none; empty means mode.DefaultName. It must name a
	// registered mode.
	DefaultMode string `json:"defaultMode,omitempty"`
}

// Roster returns the profile's explorers, collator and canonicalizers as a roster.Roster.
func (p Profile) Roster() roster.Roster {
	return roster.Roster{Explorers: p.Explorers, Collator: p.Collator, Canonicalizers: p.Canonicalizers}
}

// Validate applies the roster rules to a configured profile and checks that DefaultMode, if set, is a
// registered mode.
func (p Profile) Validate() error {
	// An unconfigured profile (no explorers, no collator) is valid to save; the shipped default is one.
	// Refusing to run it is the run path's job.
	if p.IsConfigured() {
		if err := p.Roster().Validate(); err != nil {
			return err
		}
	} else if err := roster.ValidateCanonicalizers(p.Canonicalizers); err != nil {
		return err
	}
	if m := strings.TrimSpace(p.DefaultMode); m != "" {
		if _, ok := mode.Lookup(m); !ok {
			return fault.New(fault.Config, fmt.Sprintf("profile: unknown defaultMode %q (known modes: %s)", m, strings.Join(mode.Names(), ", ")))
		}
	}
	return nil
}

// IsConfigured reports whether the profile has any explorers or a collator.
func (p Profile) IsConfigured() bool {
	return len(p.Explorers) > 0 || p.Collator != (roster.Collator{})
}

// Set is the contents of profiles.yaml: the schema version, the default profile's name and the profiles.
type Set struct {
	SchemaVersion  int                `json:"schemaVersion"`
	DefaultProfile string             `json:"defaultProfile"`
	Profiles       map[string]Profile `json:"profiles"`
}

// Validate checks that the set has at least one profile, that DefaultProfile names one of them, and that
// every profile has a name and is valid.
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

// Names returns the profile names in sorted order.
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

// Default validates the set and returns its default profile.
func (s Set) Default() (Profile, error) {
	if err := s.Validate(); err != nil {
		return Profile{}, err
	}
	return s.Profiles[s.DefaultProfile], nil
}

// FromRoster returns a set with a single `default` profile built from r.
func FromRoster(r roster.Roster) Set {
	return Set{
		SchemaVersion:  CurrentSchemaVersion,
		DefaultProfile: DefaultProfileName,
		Profiles: map[string]Profile{
			DefaultProfileName: {Explorers: r.Explorers, Collator: r.Collator, Canonicalizers: r.Canonicalizers},
		},
	}
}

// DefaultSet returns the set a fresh install ships: one unconfigured `default` profile. Nothing runs until
// a panel is configured.
func DefaultSet() Set {
	return Set{
		SchemaVersion:  CurrentSchemaVersion,
		DefaultProfile: DefaultProfileName,
		Profiles: map[string]Profile{
			DefaultProfileName: {},
		},
	}
}

// Decode strictly decodes profiles data, as YAML or JSON according to path's extension. An absent schema
// version becomes the current one and a newer version is refused. It does not call Validate.
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
		s.SchemaVersion = CurrentSchemaVersion
	}
	if s.SchemaVersion > CurrentSchemaVersion {
		return s, fault.New(fault.Config, fmt.Sprintf("profiles %q: schema version %d is newer than this build supports (%d) — upgrade exploremesh", path, s.SchemaVersion, CurrentSchemaVersion))
	}
	return s, nil
}

// Load reads, decodes and validates the profiles file at path.
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

// Save validates s and writes it atomically to path as YAML, setting an unset schema version to the
// current one.
func Save(path string, s Set) error {
	if s.SchemaVersion == 0 {
		s.SchemaVersion = CurrentSchemaVersion
	}
	if err := s.Validate(); err != nil {
		return err
	}
	// Marshal through the json tags so YAML keys match what Decode expects.
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
