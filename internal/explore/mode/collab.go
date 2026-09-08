package mode

// This file ships the AI-COLLAB COMPOSITION as a real, registered, runnable mode (design §11). The owner's
// ai-collab decomposes into three stages exploremesh already has:
//
//	1. each agent SHORTLISTS its own findings, blind        → a blind round-1 findings fan-out
//	2. the agents CHALLENGE each other's findings           → the collator-mediated cross-review round
//	3. one final collate                                    → the severity-triaged register
//
// The point of shipping it as a mode rather than as prose is that the decomposition is then TESTABLE: this
// file adds exactly ONE artifact of its own — the blind round-1 prompt (schema.CollabFindingsPrompt, "your own
// findings" rather than "attack this artifact") — and reuses Challenge's findings schema, closed severity
// enum, cross-review contract, dual-canonicalizer + confirmation policy and terminal register UNCHANGED. If
// the composition needed its own copy of any of those, the claim that ai-collab decomposes into exploremesh's
// modes would be false, and the diff would say so.
//
// The one genuine task-level difference: ai-collab does NOT require a supplied artifact. The panel is
// generating the findings, not attacking a document — so ValidateTask is nil here where Challenge sets
// requireArtifact. An artifact may still be supplied (a panel often collaborates ON something) and the
// round-1 prompt renders it behind the same untrusted framing when it is.

import "github.com/Tim-Butterfield/aimesh/internal/explore/schema"

func init() {
	// ai-collab (design §11). Two FIXED rounds — blind self-shortlist, then mediated cross-review — with the
	// ranking-grade governance policy, because the register it produces is count-bearing exactly like
	// Challenge's and §3 requires confirmation for EVERY count-bearing mode.
	register(ModeSpec{
		Name:            AICollab,
		FormulationFree: true,
		// The ONLY artifact this composition owns. Everything below is Challenge's, by value.
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
