// Package remediation is the deterministic RemediationEngine fallback: it turns an accepted
// finding into a concrete, appliable review-marker edit. It is pure (no I/O, no writes) — the
// Manager applies the returned edits through WorkspaceAccess. This marker is the FALLBACK path:
// with a real (non-fake) author_remediator host lane the Manager first attempts model-driven
// remediation (internal/engine/remediationprompt + schema.ParseRemediationResult), which produces
// an actual anchored code fix; the marker is used only when no real host lane is configured or the
// model declines / cannot produce a safe edit.
package remediation

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// Propose returns the edits for an accepted finding (empty if it targets no file).
func Propose(f review.Finding) []review.Edit {
	if f.File == "" {
		return nil
	}
	marker := fmt.Sprintf("// reviewmesh[%s]: %s\n", f.ID, f.Title)
	return []review.Edit{{
		File:        f.File,
		Anchor:      "", // empty anchor = prepend
		Replacement: marker,
		Occurrence:  1,
	}}
}
