// Package manager is exploremesh's config-only workbench engine: a mutex-guarded holder of the SINGLE
// roster (ordered peer explorers + one collator) and the resolved adapter config, exposing pure read
// projections (Overview / RosterView / AdapterViews / PrivacyView / Doctor) and a small set of governed
// write seams (whole-profile edits + adapter/ACP configuration). It mirrors reviewmesh's SetupManager posture
// — it CONFIGURES the roster and adapters and runs static Doctor/Privacy, but it CANNOT run an
// exploration (the pipeline lives only behind the CLI). Every successful write persists through the
// domain-free stores (roster.Save + meshcore/config/adapterlocations), reloads in-memory state, and
// bumps a config generation so a Client surface can invalidate stale views. The roster GRAMMAR stays
// app-side; meshcore never learns it.
package manager

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	mdoctor "github.com/Tim-Butterfield/aimesh/meshcore/doctor"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"

	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// Manager holds the workbench's mutable state under a single mutex: the current roster, the resolved
// adapter config (shell binary-path overrides + user-defined ACP instances), the paths it persists to,
// and a monotonically-increasing config generation bumped on every successful write. All exported
// methods lock for the duration, so writes are single-flight and reads see a consistent snapshot.
type Manager struct {
	mu sync.Mutex

	// profiles is the FULL multi-profile set the workbench holds (design §7) and the single thing any
	// write touches; roster is a DERIVED, read-only cache of the DEFAULT profile's roster that the roster
	// projections (RosterView / DoctorReport / PrivacyView / RosterPlanView / SnapshotRoster) read. It is
	// re-derived from the set by persistSetLocked after every accepted write, so it can never drift.
	profiles     profile.Set
	roster       roster.Roster
	adapterPaths map[string]string            // resolved shell binary-path overrides (name→path)
	acpInstances map[string]acpagent.Instance // resolved user-defined ACP adapter instances

	cwd        string // the folder the workbench binds to (adapter/roster scope resolution)
	savePath   string // profile.DefaultProfilesPath(cwd): the profiles write target (project scope in a repo, else user)
	sharedPath string // the user-scope shared adapters.yaml write target (adapterlocations.UserLocationsPath)

	// sharedVersion is the combined compare-and-swap token of the shared adapters layers AS RESOLVED
	// (see shared.go). It is refreshed by every reresolve, so "did the shared substrate actually
	// change?" is a string compare rather than a re-parse.
	sharedVersion string

	generation int
}

// BlockedError is a 409-worthy "cannot remove — still in use" outcome carrying the roster slots that
// reference the adapter, so a Client surface can list the blockers before offering a fix. It is a typed
// error (not a plain fault) so a handler can branch on it.
type BlockedError struct {
	Message string
	UsedBy  []AdapterUseDTO
}

func (e *BlockedError) Error() string { return e.Message }

// New seeds a manager with the profiles set effective for cwd and resolves its persistence targets: the
// profiles save path (profile.DefaultProfilesPath — project scope inside a repo, else the user scope under
// AIMESH_HOME) and the user-scope shared adapters.yaml (under AIMESH_HOME). It PREFERS a persisted set
// (profiles.yaml, or a legacy roster.yaml migrated to a `default` profile) so the profile seams see every
// saved profile; when nothing is persisted it seeds a one-profile `default` set from the caller's already-
// resolved starting roster r (the CLI resolves --roster / Discover / default before calling). It resolves
// the layered adapter config once so the first projections are populated; each subsequent write re-resolves.
func New(cwd string, r roster.Roster) (*Manager, error) {
	// A profiles.yaml at the state-directory ROOT is refused here too, not just on the run path: the
	// workbench is exactly where someone would open the config they just hand-wrote in the wrong place,
	// and silently editing a DIFFERENT file is the worst possible response to that.
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
	// Prefer a persisted profiles.yaml (the workbench's own saved multi-profile config) so the profile seams
	// see every saved profile. Otherwise seed a one-profile `default` set from the caller's already-resolved
	// starting roster r — the caller has already accounted for --roster and a legacy roster.yaml migration,
	// so r IS the default profile (the legacy single-roster config migrates in as a `default` profile here).
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

// reresolveLocked re-reads the layered adapter config (shell binary-path overrides + user-defined ACP
// instances) effective for cwd into the in-memory maps. Caller holds the mutex. A missing config is an
// absent layer (empty maps); a present-but-malformed file IS an error.
//
// The swap is WHOLESALE: ResolvePaths / ResolveACPInstances build fresh maps and the fields are
// REPLACED, never mutated in place, so anything already holding the previous maps keeps a consistent
// snapshot. That is what makes a reload affect only the next run.
//
// The version token is captured BEFORE the read, so a file rewritten between the token read and the
// parse is recorded as the OLDER version — the next reload then still sees a difference and re-reads.
// Recording it after would be the losing order: it could stamp new bytes onto an older parse.
func (m *Manager) reresolveLocked() error {
	version := CombinedSharedVersion(m.watchedPathsLocked())
	paths, err := adapterlocations.ResolvePaths(m.cwd)
	if err != nil {
		return err
	}
	acpInsts, err := registry.ResolveACPInstances(m.cwd)
	if err != nil {
		return err
	}
	m.adapterPaths, m.acpInstances, m.sharedVersion = paths, acpInsts, version
	return nil
}

// persistSetLocked writes the whole profiles set through profile.Save (validate → atomic write), reloads
// it, re-derives the cached default-profile roster, re-resolves adapters, and bumps the generation. A
// validation failure returns the error and persists NOTHING. Caller holds the mutex.
func (m *Manager) persistSetLocked(next profile.Set) error {
	if err := profile.Save(m.savePath, next); err != nil {
		return err // invalid set (dup triple / <2 / empty field / bad default) — nothing written
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
	_ = m.reresolveLocked() // the shared adapters file is unchanged by a profile write; keep prior on any read error
	m.generation++
	return nil
}

// Roster returns a copy of the current roster (the slice header is copied so a caller cannot mutate the
// backing array in place).
func (m *Manager) Roster() roster.Roster {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneRoster(m.roster)
}

// Generation returns the current config generation (bumped on every successful write).
func (m *Manager) Generation() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

// reload re-reads the persisted profiles set (when the save file exists) and re-resolves the adapter
// config, so a test can confirm a write round-tripped to disk. It is NOT a write and does not bump the
// generation. Unexported: New() already loads on construction, so nothing outside this package needs it.
func (m *Manager) reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Stat(m.savePath); err == nil {
		loaded, lerr := profile.Load(m.savePath)
		if lerr != nil {
			return lerr
		}
		def, derr := loaded.Default()
		if derr != nil {
			return derr
		}
		m.profiles, m.roster = loaded, def.Roster()
	}
	return m.reresolveLocked()
}

// cloneRoster deep-copies the explorer slice so external mutation cannot reach the manager's state.
func cloneRoster(r roster.Roster) roster.Roster {
	out := r
	out.Explorers = make([]roster.Explorer, len(r.Explorers))
	copy(out.Explorers, r.Explorers)
	out.Canonicalizers = append([]roster.Explorer(nil), r.Canonicalizers...)
	return out
}

// cloneSet deep-copies a profiles set (the map + each profile's explorer slice) so a candidate mutation
// cannot reach the manager's committed state until persistSetLocked accepts it.
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

// --- Doctor (static readiness; no model call) ---

// CheckDTO is one doctor check.
type CheckDTO struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// DoctorDTO is the static readiness report for the current roster + resolved adapters.
type DoctorDTO struct {
	OK     bool       `json:"ok"`
	Checks []CheckDTO `json:"checks"`
}

// DoctorReport runs exploremesh's static adapter-readiness checks against the current roster and
// resolved adapter config (no model call, no token spend): the roster must plan, every named adapter
// must resolve to a known recipe / configured ACP instance (or the hidden test-only `fake`, gated by
// the internal harness env), and each resolved binary must be present. It reuses meshcore/doctor
// exactly as the CLI `doctor` does.
func (m *Manager) DoctorReport() DoctorDTO {
	m.mu.Lock()
	defer m.mu.Unlock()

	plan, err := m.roster.Plan()
	if err != nil {
		return DoctorDTO{OK: false, Checks: []CheckDTO{{Name: "roster valid", OK: false, Detail: err.Error()}}}
	}
	reg, unknown := registry.Build(plan, m.adapterPaths, m.acpInstances, 0)
	adapters := map[string]model.Adapter{}
	var names []string
	required := map[string]bool{}
	for name, a := range reg {
		adapters[name] = a
		names = append(names, name)
		required[name] = true
	}
	sort.Strings(names)

	var rep mdoctor.Report
	rep.Checks = append(rep.Checks, mdoctor.Check{Name: "roster: explorers >= 2", OK: len(plan.Explorers) >= 2, Detail: fmt.Sprintf("%d explorers", len(plan.Explorers))})
	for _, u := range unknown {
		rep.Checks = append(rep.Checks, mdoctor.Check{Name: "adapter: " + u, OK: false, Detail: "unknown adapter — not a CLI recipe or a configured ACP instance; configure it or fix the roster"})
	}
	rep.Checks = append(rep.Checks, mdoctor.AdapterAvailability(adapters, names, required)...)
	rep.Finalize()

	dto := DoctorDTO{OK: rep.OK}
	for _, c := range rep.Checks {
		dto.Checks = append(dto.Checks, CheckDTO{Name: c.Name, OK: c.OK, Detail: c.Detail})
	}
	return dto
}

// validateBinaryPath checks a configured adapter binary path: it must exist, not be a directory, and
// (on POSIX) be executable. Mirrors the CLI's adapter-configuration validation (kept app-side; meshcore
// carries no adapter-configuration UX).
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
