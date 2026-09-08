package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
)

// This file is the SHARED-SUBSTRATE seam. `~/.aimesh/adapters.yaml` (and its root-anchored project
// sibling) is not reviewmesh's private file: exploremesh reads and writes it too, as does the CLI and
// the occasional hand edit. So the workbench must be able to (a) notice it changed underneath, (b)
// refuse a save composed against a version that is no longer on disk, and (c) say cheaply whether the
// change BROKE anything.
//
// Everything here is PURE CONFIG — file reads, hashes and map lookups. Nothing on this path starts a
// process, and nothing that calls it may: availability probing (does the binary run? is the CLI logged
// in?) stays on demand in `doctor` and the ACP flows, because a background file event that spawned a
// CLI could park the workbench behind an interactive login prompt.

// SharedVersion is the compare-and-swap token for one shared adapters file: the sha256 of its bytes —
// the SAME token adapterlocations.Update compares internally, so there is exactly one staleness
// currency rather than a second invented one. A missing file hashes to "" (which is how
// adapterlocations already treats an absent optional layer), so "created" and "deleted" are ordinary
// version changes.
func SharedVersion(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CombinedSharedVersion is the version of the whole shared substrate: every watched layer's token, in
// path order. Any layer changing changes the combined token, because what the user is looking at is the
// OVERLAY of all of them (project overlays user).
func CombinedSharedVersion(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte('=')
		b.WriteString(SharedVersion(p))
		b.WriteByte('\n')
	}
	return b.String()
}

// StaleBaseError is the optimistic-concurrency refusal: the caller's write was composed against a
// version of the shared adapters file that is no longer on disk. Typed, so a surface answers
// 409-with-reconcile rather than an opaque 400.
type StaleBaseError struct{ Message string }

func (e *StaleBaseError) Error() string { return e.Message }

// StaleBaseMessage is the reconcile text a refused stale write carries. It states plainly that nothing
// was written — a refusal must never leave the user wondering whether their edit half-landed.
const StaleBaseMessage = "the shared adapter configuration changed on disk after this form was loaded, " +
	"so this save was refused rather than overwriting the newer file. Nothing was written. " +
	"Reload the Adapters tab to see the current configuration, then re-apply your change."

// WatchedSharedPaths are the shared adapters files this workbench reads: the user-scope file, plus the
// root-anchored project-scope file when the folder is inside a repository. Both are watched because
// either can change the effective adapter set.
func (m *Manager) WatchedSharedPaths() []string {
	var out []string
	if p := m.Layers.SharedUserPath; p != "" {
		out = append(out, p)
	} else if p, err := sharedUserFallback(); err == nil && p != "" {
		out = append(out, p)
	}
	if m.Layers.SharedProjectHasRoot && m.Layers.SharedProjectPath != "" && !contains(out, m.Layers.SharedProjectPath) {
		out = append(out, m.Layers.SharedProjectPath)
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// sharedUserFallback is the canonical user-scope adapters.yaml when the layer snapshot has no path
// recorded (nothing has been written yet). Kept here so WatchedSharedPaths still watches the file the
// first write will create.
func sharedUserFallback() (string, error) { return adapterlocations.UserLocationsPath() }

// SharedVersion is the current combined compare-and-swap token, read FRESH from disk: a surface hands
// it to a client as the base a later write must still match.
func (m *Manager) SharedVersion() string {
	return CombinedSharedVersion(m.WatchedSharedPaths())
}

// SharedNames is the set of adapter names the shared substrate CONFIGURES — a recorded binary path, or
// a user-defined ACP instance — read from this Manager's LOADED SNAPSHOT, not from disk. That is
// deliberate: "which adapters appeared / disappeared?" compares the snapshot the workbench was serving
// against the one it just loaded, and a disk re-read would answer both sides with the new file.
func (m *Manager) SharedNames() []string {
	set := map[string]bool{}
	for n, a := range m.Cfg.Adapters {
		if a.Path != "" {
			set[n] = true
		}
	}
	for n := range m.Layers.ACPInstances {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// CheckSharedBase is layer (b): optimistic concurrency for a write composed against a known version.
// An empty base means the caller supplied none (the CLI, an older client) and is accepted unchanged.
// Otherwise the base must still match what is on disk, or the write is REFUSED — never
// last-write-wins.
//
// This is a guard, not a lock. Deliberately: (1) the durable no-clobber guarantee is
// adapterlocations.Update's content-hash CAS, which every adapter/ACP write already goes through — it
// re-reads immediately before the atomic write and retries from fresh state, so an unrelated
// concurrent edit can never be lost even if one lands in the microseconds between this check and the
// write; (2) this check exists for the USER-VISIBLE staleness the CAS cannot see — a form loaded
// seconds or minutes ago against a file the other app has since rewritten. No cross-process flock is
// used or needed: these are single-user localhost workbenches, and the CAS already makes concurrent
// writers safe without one.
func (m *Manager) CheckSharedBase(base string) error {
	if base == "" {
		return nil
	}
	if m.SharedVersion() != base {
		return &StaleBaseError{Message: StaleBaseMessage}
	}
	return nil
}

// BrokenRef is one configured lane/seat whose adapter no longer resolves.
type BrokenRef struct {
	Profile string `json:"profile"`
	Ref     string `json:"ref"`  // "author_remediator" | "reviewers[1]" …
	Role    string `json:"role"` // the human label
	Adapter string `json:"adapter"`
	Detail  string `json:"detail"`
}

// ConfigIntegrity is layer (c): the CHEAP config-integrity verdict. It answers exactly one question —
// does any configured lane or panel seat, in any profile, now name an adapter that no longer exists? —
// with map lookups over already-loaded config. It runs no binary, opens no socket, spends nothing.
type ConfigIntegrity struct {
	OK     bool        `json:"ok"`
	Broken []BrokenRef `json:"broken,omitempty"`
}

// ConfigIntegrity runs the cheap check over every profile's lanes and reviewer seats.
//
// "Exists" means: present in the live adapter registry — a code-owned shell recipe, or a configured
// ACP instance. It deliberately does NOT mean "the binary is present and runnable": that is
// AVAILABILITY, it changes without the configuration changing, and proving it costs a process launch.
// Doctor answers availability, on demand.
func (m *Manager) ConfigIntegrity() ConfigIntegrity {
	known := func(name string) bool {
		// An unset lane is an editor state, not a break; `fake` is the hidden internal harness, which
		// adapters.yaml can neither define nor remove.
		if name == "" || name == "fake" {
			return true
		}
		_, ok := m.Adapters[name]
		return ok
	}
	detail := func(adapter string) string {
		return fmt.Sprintf("adapter %q is no longer defined in the shared adapter configuration "+
			"(removed or renamed) — choose another adapter for this lane", adapter)
	}

	profiles := make([]string, 0, len(m.Cfg.Profiles))
	for n := range m.Cfg.Profiles {
		profiles = append(profiles, n)
	}
	sort.Strings(profiles)

	var broken []BrokenRef
	for _, pname := range profiles {
		p := m.Cfg.Profiles[pname]
		roles := make([]string, 0, len(p.Lanes))
		for r := range p.Lanes {
			roles = append(roles, r)
		}
		sort.Strings(roles)
		for _, role := range roles {
			if a := p.Lanes[role].Adapter; !known(a) {
				broken = append(broken, BrokenRef{
					Profile: pname, Ref: role, Role: role, Adapter: a, Detail: detail(a),
				})
			}
		}
		for i, seat := range p.Reviewers {
			if a := seat.Adapter; !known(a) {
				broken = append(broken, BrokenRef{
					Profile: pname, Ref: fmt.Sprintf("reviewers[%d]", i), Role: fmt.Sprintf("reviewer seat %d", i+1),
					Adapter: a, Detail: detail(a),
				})
			}
		}
	}
	return ConfigIntegrity{OK: len(broken) == 0, Broken: broken}
}
