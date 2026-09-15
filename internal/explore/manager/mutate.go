package manager

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
)

// This file holds the writes for shell adapter paths. Each writes the user-scope shared adapters.yaml
// through adapterlocations.Update and re-resolves the adapter configuration; a failed validation writes
// nothing. Roster changes go through the profile writes in profiles.go.

// ConfigureAdapterPath records a validated binary path for a shell adapter in the user-scope shared
// adapters.yaml, then re-resolves. The `fake` adapter needs no path and is rejected.
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
	return []string{fmt.Sprintf("Recorded adapters.%s.path in %s.", name, m.sharedPath)}, nil
}

// RemoveAdapter clears a shell adapter's saved path from the user-scope shared adapters.yaml. It returns
// a *BlockedError while a roster slot uses the adapter, refuses `fake`, and reports an error when there is
// no saved path to clear.
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
	return []string{fmt.Sprintf("Cleared the saved path for %s.", m.adapterDisplay(name))}, nil
}
