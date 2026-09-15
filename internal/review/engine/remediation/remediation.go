// Package remediation is the deterministic remediation fallback: it turns an accepted finding into a
// review-marker edit without I/O. The Manager first attempts a model-driven fix (remediationprompt
// and schema.ParseRemediationResult) when a real author_remediator lane is configured, and uses the
// marker when there is none or the model declines.
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
