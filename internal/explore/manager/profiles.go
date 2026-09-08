package manager

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
)

// This file is the manager's MULTI-profile read/write seam (design §7) and the ONLY seam that writes a
// roster. The roster projections (RosterView / DoctorReport / PrivacyView / …) read the DEFAULT profile;
// these seams manage the FULL profile set (list / get / set-default / save / delete). Every write goes
// through persistSetLocked (validate → atomic write of the whole set → reload → re-derive the cached
// default roster → bump the generation), so a rejected write persists nothing and never bumps.
//
// SaveProfile persists a profile WHOLE — its ordered explorers, its collator, its canonicalizers and its
// default mode — which is why there is no per-slot roster mutator beside it: a slot-scoped seam could only
// ever address the default profile, and two live write paths to the same bytes is the shape this
// deliberately avoids. "Whole" is load-bearing: a caller that omits a field ERASES it, so a surface holding
// only a partial draft must carry the rest forward itself (see the webui's save handler).

// ProfileSummaryDTO projects one profile for the profiles list: its name, whether it is the default, its
// explorer count, and its default mode ("" = the app default map).
type ProfileSummaryDTO struct {
	Name          string `json:"name"`
	IsDefault     bool   `json:"isDefault"`
	ExplorerCount int    `json:"explorerCount"`
	DefaultMode   string `json:"defaultMode"`
}

// ProfilesViewDTO projects the whole profile set: the default profile's name + the profiles (sorted).
type ProfilesViewDTO struct {
	DefaultProfile string              `json:"defaultProfile"`
	Profiles       []ProfileSummaryDTO `json:"profiles"`
}

// ProfileDetailDTO projects one profile's ordered explorers (PREFERENCE order — as authored) + collator +
// canonicalizers + default mode, for the profile editor.
type ProfileDetailDTO struct {
	Name        string            `json:"name"`
	IsDefault   bool              `json:"isDefault"`
	DefaultMode string            `json:"defaultMode"`
	Explorers   []ExplorerViewDTO `json:"explorers"`
	Collator    CollatorViewDTO   `json:"collator"`
	// Canonicalizers is EITHER empty (the host derives them) or exactly two. It is projected as its own
	// list rather than folded into the explorers: canonicalization is a distinct role, and a reader who
	// cannot see which identities hold it cannot check the run's governance.
	Canonicalizers []ExplorerViewDTO `json:"canonicalizers"`
}

// ProfilesView projects the profile set (sorted by name) + the default profile name.
func (m *Manager) ProfilesView() ProfilesViewDTO {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := ProfilesViewDTO{DefaultProfile: m.profiles.DefaultProfile}
	for _, name := range m.profiles.Names() {
		p := m.profiles.Profiles[name]
		out.Profiles = append(out.Profiles, ProfileSummaryDTO{
			Name:          name,
			IsDefault:     name == m.profiles.DefaultProfile,
			ExplorerCount: len(p.Explorers),
			DefaultMode:   p.DefaultMode,
		})
	}
	return out
}

// ProfileView projects one named profile's ordered explorers + collator + default mode (an unknown name
// is an error).
func (m *Manager) ProfileView(name string) (ProfileDetailDTO, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.profiles.Get(name)
	if !ok {
		return ProfileDetailDTO{}, fault.New(fault.Usage, fmt.Sprintf("no profile named %q (have: %s)", name, strings.Join(m.profiles.Names(), ", ")))
	}
	out := ProfileDetailDTO{
		Name:           name,
		IsDefault:      name == m.profiles.DefaultProfile,
		DefaultMode:    p.DefaultMode,
		Explorers:      make([]ExplorerViewDTO, 0, len(p.Explorers)),
		Canonicalizers: make([]ExplorerViewDTO, 0, len(p.Canonicalizers)),
	}
	for i, e := range p.Explorers {
		out.Explorers = append(out.Explorers, ExplorerViewDTO{
			Index: i, Adapter: e.Adapter, AdapterDisplay: m.adapterDisplay(e.Adapter), Model: e.Model, Effort: e.Effort,
		})
	}
	for i, c := range p.Canonicalizers {
		out.Canonicalizers = append(out.Canonicalizers, ExplorerViewDTO{
			Index: i, Adapter: c.Adapter, AdapterDisplay: m.adapterDisplay(c.Adapter), Model: c.Model, Effort: c.Effort,
		})
	}
	c := p.Collator
	out.Collator = CollatorViewDTO{
		Adapter: c.Adapter, AdapterDisplay: m.adapterDisplay(c.Adapter), Model: c.Model, Effort: c.Effort,
	}
	return out, nil
}

// There is deliberately NO SetDefaultProfile: as in reviewmesh, the runtime default is always the
// profile named `default` — to change what a no-flag run does, edit `default` or save another
// profile's panel onto it. A hand-written defaultProfile pointer in an existing file is still honored
// at load (never silently rewritten), but repointing it is not a feature of any surface.

// SaveProfile adds or replaces a named profile (its ordered explorers + collator + canonicalizers +
// default mode). The candidate set is re-validated by profile.Save — an invalid roster (dup triple / <2 /
// empty field), a malformed canonicalizer pair (one entry, or two identical identities) or an unknown
// default mode is rejected and nothing is written. The name must be non-empty.
func (m *Manager) SaveProfile(name string, p profile.Profile) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fault.New(fault.Usage, "a profile name is required")
	}
	next := cloneSet(m.profiles)
	_, existed := next.Profiles[name]
	next.Profiles[name] = profile.Profile{
		Explorers: p.Explorers, Collator: p.Collator,
		// Carried through: a profile's canonicalizers are half of its governance rule, and a write seam that
		// dropped a field it does not itself edit would silently disarm it.
		Canonicalizers: p.Canonicalizers,
		DefaultMode:    strings.TrimSpace(p.DefaultMode),
	}
	if err := m.persistSetLocked(next); err != nil {
		return nil, err
	}
	verb := "Added"
	if existed {
		verb = "Updated"
	}
	return []string{fmt.Sprintf("%s profile %q (%d explorers).", verb, name, len(p.Explorers))}, nil
}

// DeleteProfile removes a named profile. It refuses to delete the DEFAULT profile (set another default
// first) and refuses to remove the LAST profile (a set needs at least one). An unknown name is an error.
func (m *Manager) DeleteProfile(name string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name = strings.TrimSpace(name)
	if _, ok := m.profiles.Get(name); !ok {
		return nil, fault.New(fault.Usage, fmt.Sprintf("no profile named %q (have: %s)", name, strings.Join(m.profiles.Names(), ", ")))
	}
	if name == m.profiles.DefaultProfile {
		return nil, fault.New(fault.Usage, fmt.Sprintf("%q is the default profile and cannot be deleted — a no-flag run always binds to it; edit its panel instead", name))
	}
	if len(m.profiles.Profiles) <= 1 {
		return nil, fault.New(fault.Usage, "cannot delete the only profile — a set needs at least one profile")
	}
	next := cloneSet(m.profiles)
	delete(next.Profiles, name)
	if err := m.persistSetLocked(next); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("Deleted profile %q.", name)}, nil
}

// Profiles returns a deep copy of the current profile set (for a caller that needs the whole set, e.g. a
// CLI `list` projection). The map + each explorer slice are copied so external mutation cannot reach state.
func (m *Manager) Profiles() profile.Set {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneSet(m.profiles)
}
