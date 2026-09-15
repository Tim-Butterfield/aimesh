// Package manager manages explore configuration: the profile set and the resolved adapter settings. It
// provides read-only views of the default roster and the adapters, and writes for profiles and adapter
// configuration. It does not run explorations. Every successful write persists and reloads state.
package manager

import (
	"fmt"
	"os"
	"runtime"
	"sync"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// Manager holds explore configuration state under one mutex. Exported methods hold the lock for their
// whole duration, so readers see a consistent snapshot.
type Manager struct {
	mu sync.Mutex

	// profiles is the profile set every write modifies. roster caches the default profile's roster for the
	// views and is re-derived after each write.
	profiles     profile.Set
	roster       roster.Roster
	adapterPaths map[string]string            // resolved adapter binary paths, by name
	acpInstances map[string]acpagent.Instance // resolved ACP adapter instances

	cwd        string // directory used for scope resolution
	savePath   string // profiles write target (profile.DefaultProfilesPath)
	sharedPath string // user-scope adapters.yaml write target
}

// BlockedError reports that an adapter cannot be removed because roster slots still use it.
type BlockedError struct {
	Message string
	UsedBy  []AdapterUseDTO
}

func (e *BlockedError) Error() string { return e.Message }

// New returns a manager for cwd. It loads the discovered profiles file if one exists, otherwise a
// single-profile set built from r, and resolves the adapter configuration.
func New(cwd string, r roster.Roster) (*Manager, error) {
	if err := profile.CheckMisplaced(cwd); err != nil {
		return nil, err
	}
	savePath, err := profile.DefaultProfilesPath(cwd)
	if err != nil {
		return nil, err
	}
	sharedPath, err := adapterlocations.UserLocationsPath()
	if err != nil {
		return nil, err
	}
	set := profile.FromRoster(r)
	if p, ok := profile.DiscoverProfiles(cwd); ok {
		loaded, lerr := profile.Load(p)
		if lerr != nil {
			return nil, lerr
		}
		set = loaded
	}
	def, err := set.Default()
	if err != nil {
		return nil, err
	}
	m := &Manager{profiles: set, roster: def.Roster(), cwd: cwd, savePath: savePath, sharedPath: sharedPath}
	if err := m.reresolveLocked(); err != nil {
		return nil, err
	}
	return m, nil
}

// reresolveLocked re-reads the adapter configuration for cwd, replacing the maps rather than mutating
// them so earlier holders keep a consistent snapshot. A missing file is an empty layer; a malformed one
// is an error. The caller holds m.mu.
func (m *Manager) reresolveLocked() error {
	paths, err := adapterlocations.ResolvePaths(m.cwd)
	if err != nil {
		return err
	}
	acpInsts, err := registry.ResolveACPInstances(m.cwd)
	if err != nil {
		return err
	}
	m.adapterPaths, m.acpInstances = paths, acpInsts
	return nil
}

// persistSetLocked saves next, reloads it, and refreshes the cached roster and adapters. An invalid set
// writes nothing. The caller holds m.mu.
func (m *Manager) persistSetLocked(next profile.Set) error {
	if err := profile.Save(m.savePath, next); err != nil {
		return err
	}
	loaded, err := profile.Load(m.savePath)
	if err != nil {
		return err
	}
	def, err := loaded.Default()
	if err != nil {
		return err
	}
	m.profiles, m.roster = loaded, def.Roster()
	_ = m.reresolveLocked() // a profile write does not change adapters; keep the prior state on error
	return nil
}

// cloneSet returns a deep copy of s, so a candidate change cannot affect committed state.
func cloneSet(s profile.Set) profile.Set {
	out := s
	out.Profiles = make(map[string]profile.Profile, len(s.Profiles))
	for name, p := range s.Profiles {
		cp := p
		cp.Explorers = make([]roster.Explorer, len(p.Explorers))
		copy(cp.Explorers, p.Explorers)
		cp.Canonicalizers = append([]roster.Explorer(nil), p.Canonicalizers...)
		out.Profiles[name] = cp
	}
	return out
}

// validateBinaryPath checks that p exists, is not a directory, and is executable (outside Windows).
func validateBinaryPath(p string) error {
	if p == "" {
		return fault.New(fault.Usage, "a binary path is required")
	}
	fi, err := os.Stat(p)
	if err != nil {
		return fault.New(fault.Config, fmt.Sprintf("path %q not found", p))
	}
	if fi.IsDir() {
		return fault.New(fault.Config, fmt.Sprintf("path %q is a directory, not a binary", p))
	}
	if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
		return fault.New(fault.Config, fmt.Sprintf("path %q is not executable", p))
	}
	return nil
}
