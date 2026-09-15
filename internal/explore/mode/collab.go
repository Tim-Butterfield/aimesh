package mode

// This file registers the ai-collab mode:
//
//	1. each explorer lists its own findings, blind   → blind round-1 findings
//	2. explorers review each other's findings        → mediated cross-review round
//	3. final collation                               → severity-triaged register
//
// Apart from its round-1 prompt, ai-collab reuses Challenge's schema, cross-review contract, canonicalization
// policy and register. Unlike Challenge it does not require an artifact; one may still be supplied.

import "github.com/Tim-Butterfield/aimesh/internal/explore/schema"

func init() {
	register(ModeSpec{
		Name:             AICollab,
		FormulationFree:  true,
		Prompt:           schema.CollabFindingsPrompt,
		ExplorerSchema:   schema.ChallengeExplorerSchema,
		Objective:        ObjectiveCanonicalizeMediateCollate,
		Canonicalizing:   challengeCollator{},
		Canonicalization: CanonicalizationPolicy{Dual: true, Confirm: true},
		Rounds:           2,
		LaterRound:       challengeReview{},
		Class:            schema.EmergentSpace,
	})
}
