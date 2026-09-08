// Package adjudication is the AdjudicationEngine: it judges reviewer findings —
// dedup by fingerprint, validity rubric, severity baseline — into decisions.
// Batch 1 adjudicates deterministically/locally (the host model-call adjudication
// path arrives with the real host lane); the host remains the sole authority.
package adjudication

import (
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
)

// Result is the adjudication outcome: deduped findings aligned with decisions.
type Result struct {
	Findings  []review.Finding
	Decisions []review.Decision
}

var sevRank = map[review.Severity]int{
	review.SeverityInfo: 0, review.SeverityLow: 1, review.SeverityMedium: 2,
	review.SeverityHigh: 3, review.SeverityCritical: 4,
}

// Judge dedups findings by fingerprint (keeping the highest severity, union of
// suggestions) and decides each one's validity. `addressed` is the addressed-context:
// fingerprints already applied/decided in prior outer cycles, which become
// already_addressed (not actionable) so the loop converges. Decision.State is
// provisional here; the Manager finalizes it per mode/apply result.
func Judge(findings []review.Finding, addressed map[string]review.DecisionState) Result {
	order := []string{}
	byFP := map[string]review.Finding{}
	for _, f := range findings {
		fp := schema.Fingerprint(f)
		if existing, ok := byFP[fp]; ok {
			// dedup: keep the higher severity, but never drop a suggestion (union).
			merged := existing
			if sevRank[f.Severity] > sevRank[existing.Severity] {
				merged = f
			}
			if merged.Suggestion == "" {
				if existing.Suggestion != "" {
					merged.Suggestion = existing.Suggestion
				} else {
					merged.Suggestion = f.Suggestion
				}
			}
			byFP[fp] = merged
			continue
		}
		byFP[fp] = f
		order = append(order, fp)
	}

	var res Result
	for _, fp := range order {
		f := byFP[fp]
		d := review.Decision{FindingID: f.ID, Valid: true, SeverityAdjusted: f.Severity}
		switch {
		case f.Kind == review.KindPass:
			d.State = review.StateSkipped // valid but not actionable
		case addressed[fp] != "":
			d.State = review.StateAlreadyAddr // already applied/decided in a prior cycle
			d.Reasoning = "already addressed in a prior outer cycle"
		}
		res.Findings = append(res.Findings, f)
		res.Decisions = append(res.Decisions, d)
	}
	return res
}

// nonApplyStates are decision states that must never produce an edit, even when a
// (model) host marked the finding valid — they are explicit non-apply dispositions.
var nonApplyStates = map[review.DecisionState]bool{
	review.StateSkipped:          true,
	review.StateAlreadyAddr:      true,
	review.StateInvalid:          true,
	review.StateReportedValid:    true, // report-only: surfaced to the user, never auto-applied
	review.StateReportedInvalid:  true,
	review.StateUpstreamConflict: true,
	review.StateWithheldClassE:   true,
}

// Actionable reports whether a decision should produce an edit: it must be valid and
// not in any non-apply terminal state. (Deterministic provisional decisions carry an
// empty state, which is actionable when valid.)
func Actionable(d review.Decision) bool {
	return d.Valid && !nonApplyStates[d.State]
}
