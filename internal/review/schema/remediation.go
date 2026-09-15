package schema

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// RemediationResult is the JSON a remediation call returns: the anchored edits that fix one accepted
// finding in its target file, produced by the author_remediator in patch and apply modes.
type RemediationResult struct {
	SchemaVersion int           `json:"schemaVersion"`
	Role          string        `json:"role"`
	Phase         string        `json:"phase"`
	Edits         []review.Edit `json:"edits"`
	NoEdit        bool          `json:"noEdit,omitempty"` // the model declined to edit
	Reason        string        `json:"reason,omitempty"` // why it declined, or what it changed
}

// ParseRemediationResult unmarshals and validates a remediation response. Unknown top-level fields
// are ignored. It requires schemaVersion 1 and role author_remediator, and every edit must target
// exactly wantFile with a non-empty anchor and a different replacement, so a fix cannot touch another
// file or inject unanchored text.
//
// NoEdit, or an empty edits list, is valid: the model may decline, and the caller falls back to the
// review marker.
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
