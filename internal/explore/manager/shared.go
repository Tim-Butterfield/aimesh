package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
)

// This file is the SHARED-SUBSTRATE seam: `~/.aimesh/adapters.yaml` (and its root-anchored project
// sibling) is written by the review domain and by the CLI too, so a version token is needed to tell
// whether the effective adapter set changed underneath a resolved config.
//
// It used to be much larger. The optimistic-concurrency guard, the integrity verdict and the reload
// diff all existed for the web workbench — a long-lived process holding a form the user might save
// against a stale base — and went with it. What remains is the token itself, which the manager still
// stamps onto a resolution.
//
// Everything here is PURE CONFIG: nothing in this file starts a process. Availability probing (does
// the binary run? is the CLI logged in?) stays on demand in `doctor` / the ACP flows.

// SharedVersion is the compare-and-swap token for one shared adapters file: the sha256 of its bytes —
// the SAME token adapterlocations.Update compares internally. A missing file is the empty token (which
// is what adapterlocations treats as "absent optional layer"), so "created" and "deleted" are both
// ordinary version changes. Reused rather than reinvented: there is exactly one staleness currency.
func SharedVersion(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CombinedSharedVersion is the version of the whole shared substrate: every watched layer's token, in
// path order. Any layer changing changes the combined token, because the EFFECTIVE adapter set the user
// is looking at is the overlay of all of them.
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
// version of the shared adapters file that is no longer on disk. It is a typed error so a surface can
// answer 409-with-reconcile rather than 400.
type StaleBaseError struct{ Message string }

func (e *StaleBaseError) Error() string { return e.Message }

// StaleBaseMessage is the reconcile text a rejected stale write carries. It says plainly that nothing
// was written and what to do next — a refused save must never leave the user guessing whether their
// edit half-landed.
const StaleBaseMessage = "the shared adapter configuration changed on disk after this form was loaded, " +
	"so this save was refused rather than overwriting the newer file. Nothing was written. " +
	"Reload the Adapters panel to see the current configuration, then re-apply your change."

// WatchedPaths are the shared adapters files this workbench reads: the user-scope file it writes to,
// plus the root-anchored project-scope file when the workbench is bound to a repository. Both are
// watched because either can change the effective adapter set.
func (m *Manager) WatchedPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.watchedPathsLocked()
}

func (m *Manager) watchedPathsLocked() []string {
	out := []string{m.sharedPath}
	if pp, ok := adapterlocations.ProjectLocationsPath(m.cwd); ok && pp != m.sharedPath {
		out = append(out, pp)
	}
	return out
}
