package manager

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
)

// This file holds the profile writes, the only code that changes a roster. Every write goes through
// persistSetLocked, so a rejected write persists nothing. The default profile is the profile named
// `default`; there is no way to choose another.

// SaveProfile adds or replaces the named profile whole: a field the caller omits is erased. The resulting
// set is validated before it is written, and an invalid set writes nothing.
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
		Explorers:      p.Explorers,
		Collator:       p.Collator,
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

// DeleteProfile removes the named profile. It refuses to delete the default profile or the last profile.
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

// Profiles returns a deep copy of the profile set.
func (m *Manager) Profiles() profile.Set {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneSet(m.profiles)
}
