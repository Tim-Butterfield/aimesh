package manager

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
)

// This file is the governed WRITE surface for the shell-adapter PATHS. Each mutation writes the
// user-scope shared adapters.yaml through adapterlocations.Update (content-hash CAS), re-resolves the
// layered adapter config, and bumps the generation. A validation/usage failure returns an error and
// persists NOTHING.
//
// ROSTER writes are not here: they all go through the profile seam in profiles.go (SaveProfile /
// SetDefaultProfile / DeleteProfile), which persists a whole named profile. There is deliberately no
// per-slot roster mutator — a second write path to the same bytes could only ever address the DEFAULT
// profile, so the seam that can express every profile is the only one.

// ConfigureAdapterPath records a validated binary path for a shell adapter in the user-scope shared
// adapters.yaml (the single seam adapter paths live in), then re-resolves. `fake` needs no path and is
// rejected.
func (m *Manager) ConfigureAdapterPath(name, binPath string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fault.New(fault.Usage, "an adapter name is required")
	}
	if name == registry.FakeAdapter {
		return nil, fault.New(fault.Usage, "the built-in `fake` adapter needs no path")
	}
	if err := validateBinaryPath(binPath); err != nil {
		return nil, err
	}
	p := binPath
	if err := adapterlocations.Update(m.sharedPath, func(loc *adapterlocations.Locations) {
		loc.Adapters[name] = adapterlocations.Entry{Path: &p}
	}); err != nil {
		return nil, err
	}
	if err := m.reresolveLocked(); err != nil {
		return nil, err
	}
	m.generation++
	return []string{fmt.Sprintf("Recorded adapters.%s.path in %s.", name, m.sharedPath)}, nil
}

// RemoveAdapter clears a shell adapter's saved path from the user-scope shared adapters.yaml. It is
// BLOCKED (a *BlockedError carrying the using slots) while the roster references the adapter, `fake` can
// never be removed, and an adapter with no saved path to clear is reported (nothing to do), never a
// phantom write.
func (m *Manager) RemoveAdapter(name string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fault.New(fault.Usage, "an adapter name is required")
	}
	if name == registry.FakeAdapter {
		return nil, fault.New(fault.Usage, "the built-in `fake` adapter cannot be removed")
	}
	if uses := m.adapterUsageLocked()[name]; len(uses) > 0 {
		return nil, &BlockedError{
			Message: fmt.Sprintf("%s is used by %d roster slot(s); reconfigure the roster first", m.adapterDisplay(name), len(uses)),
			UsedBy:  uses,
		}
	}
	loc, err := adapterlocations.Load(m.sharedPath)
	if err != nil {
		return nil, err
	}
	if e, ok := loc.Adapters[name]; !ok || e.Path == nil {
		return nil, fault.New(fault.Config, fmt.Sprintf("%s has no saved path in the shared adapters file to clear", m.adapterDisplay(name)))
	}
	if err := adapterlocations.Update(m.sharedPath, func(loc *adapterlocations.Locations) {
		delete(loc.Adapters, name)
	}); err != nil {
		return nil, err
	}
	if err := m.reresolveLocked(); err != nil {
		return nil, err
	}
	m.generation++
	return []string{fmt.Sprintf("Cleared the saved path for %s.", m.adapterDisplay(name))}, nil
}
