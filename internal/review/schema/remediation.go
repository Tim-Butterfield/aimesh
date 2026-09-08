package schema

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// RemediationResult is the JSON a model-driven remediation call returns: the anchored edit(s)
// that FIX one accepted finding in its target file. The host author_remediator produces it in
// apply/patch modes; the Manager applies the edits to the isolated remediation copy.
type RemediationResult struct {
	SchemaVersion int           `json:"schemaVersion"`
	Role          string        `json:"role"`
	Phase         string        `json:"phase"`
	Edits         []review.Edit `json:"edits"`
	NoEdit        bool          `json:"noEdit,omitempty"` // the model safely DECLINED to edit
	Reason        string        `json:"reason,omitempty"` // why it declined / what it changed
}

// ParseRemediationResult unmarshals and validates a remediation response. Unknown top-level
// fields are TOLERATED (see ParseReviewerResult — the same "judge content, don't choke on noise"
// posture). Guardrails that DO hold:
//   - schemaVersion==1 and role=="author_remediator".
//   - every edit targets EXACTLY `wantFile` (the finding's own file) — remediation may never
//     touch another path; the caller has already gated wantFile to a shown, non-excluded file.
//   - every edit carries a non-empty anchor (an exact pre-image to replace) whose replacement
//     differs — no blind prepends, no no-ops. Targeted-edit-only keeps a fix from injecting
//     arbitrary text; the deterministic marker (empty anchor) remains the fallback path.
//
// A result with NoEdit=true, or with an empty edits array, is VALID: the model is allowed to
// decline when it cannot produce a safe fix, and the caller falls back to the review marker.
func ParseRemediationResult(b []byte, wantFile string) (RemediationResult, error) {
	var r RemediationResult
	if err := json.Unmarshal(b, &r); err != nil {
		return RemediationResult{}, fmt.Errorf("remediation result is not valid JSON for the schema: %w", err)
	}
	if r.SchemaVersion != 1 {
		return RemediationResult{}, fmt.Errorf("schemaVersion must be 1, got %d", r.SchemaVersion)
	}
	if r.Role != string(review.RoleAuthorRemediator) {
		return RemediationResult{}, fmt.Errorf("invalid remediation role %q (want author_remediator)", r.Role)
	}
	if r.NoEdit || len(r.Edits) == 0 {
		return RemediationResult{SchemaVersion: 1, Role: r.Role, Phase: r.Phase, NoEdit: true, Reason: r.Reason}, nil
	}
	for i, e := range r.Edits {
		if e.File != wantFile {
			return RemediationResult{}, fmt.Errorf("edit[%d] targets %q, but remediation may only edit the finding's file %q", i, e.File, wantFile)
		}
		if strings.TrimSpace(e.Anchor) == "" {
			return RemediationResult{}, fmt.Errorf("edit[%d]: a non-empty anchor (exact pre-image to replace) is required", i)
		}
		if e.Anchor == e.Replacement {
			return RemediationResult{}, fmt.Errorf("edit[%d]: replacement is identical to anchor (no-op)", i)
		}
	}
	return r, nil
}
