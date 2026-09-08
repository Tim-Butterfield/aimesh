package mode

// This file holds the FIXED-SPACE terminal contract (design §0 F-B, §3 Compare + Forecast rows) — the
// third and last terminal seam, alongside the plain CollatorContract and the CanonicalizingContract.
//
// It exists because the fixed-space modes invert the usual order of operations. In every other mode the
// panel's answers are shaped by a model step (a collate, or a canonicalize+confirm) before the host can
// count anything. Here the space was declared by the USER before the fan-out, so the moment the blind round
// returns, the host can compute the entire result — matrix, agreement, Pareto, pooled estimate, dispersion,
// outliers — with no model in the loop at all.
//
// So the contract is deliberately shaped to make the collator's remaining job small and unmistakable:
//
//	Aggregate      the HOST computes every number and every claim, from the recorded blind envelopes
//	CollatorPrompt the collator is shown those numbers AS FINISHED VALUES and asked for prose only
//	ParseNarrative the collator's output is read ONLY into the quarantined collatorNarrative namespace
//	Collate        the terminal output is assembled from the host view + that prose
//
// There is no method here through which a model could return a number that reaches the result. That is the
// point: §0 F-C says a machine governance field carries host-produced values only, and in a fixed-space
// mode that rule can be enforced by the shape of the contract rather than by a review of the prompt.

import (
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// FixedSpaceInput is the record a fixed-space aggregation may read: the RAW TASK (which is where the
// declared space lives — the option set, the criteria, the estimation target), the immutable blind round-1
// baseline, the recorded rounds, the frozen panel, and the formulation hash every claim pins itself to.
// Every field is a host artifact or a user declaration; there is nothing here a model authored except the
// envelope contents, which is exactly the material being measured.
type FixedSpaceInput struct {
	Raw      schema.RawTask
	Baseline govern.BlindBaseline
	// Primary is the verified, non-abstaining panel the aggregation reads (the same primary panel a plain
	// collate receives).
	Primary []schema.Envelope
	Rounds  []round.Round
	Panel   govern.Panel
	// FormulationHash is the hash of the byte-identical payload every explorer received — pinned on each
	// emitted claim, so a count is always tied to the exact question that produced it (§0 F-A).
	FormulationHash string
}

// FixedSpaceView is a mode's HOST-COMPUTED aggregate plus the claims the host must record for it. Value is
// the mode's own concrete structure (Compare → govern.CompareMatrix, Forecast → the pooled estimate), kept
// opaque to the pipeline: the pipeline's job is to record the claims and carry the value to the terminal
// collation, not to understand either.
type FixedSpaceView struct {
	Value  any
	Claims []govern.Claim
}

// FixedSpaceContract is a mode's app-owned FIXED-SPACE terminal behavior (design §3 Compare/Forecast rows).
// The pipeline resolves it from ModeSpec.FixedSpace and drives the four steps in the order documented at
// the top of this file. It is mutually exclusive with Collator and Canonicalizing — a mode sets exactly one.
type FixedSpaceContract interface {
	// Aggregate performs the HOST arithmetic over the recorded blind round-1 envelopes and returns the mode's
	// view plus the governance claims to record. It runs BEFORE the collator is prompted, so every number in
	// the result predates the only model call left in the run.
	Aggregate(in FixedSpaceInput) (FixedSpaceView, error)
	// CollatorPrompt renders the terminal collation instruction over the ALREADY-COMPUTED view. A contract
	// implements it by showing the host's numbers and asking for narrative — never for a recomputation, a
	// re-ranking or a "corrected" aggregate.
	CollatorPrompt(in FixedSpaceInput, view FixedSpaceView) (string, error)
	// ParseNarrative lifts the collator's raw output into the quarantined collatorNarrative namespace (§0
	// F-C), attributed to the collator identity. It returns ONLY prose: there is no path from these bytes
	// into a governance value.
	ParseNarrative(raw []byte, by schema.ExplorerIdentity) ([]govern.Narrative, error)
	// Collate assembles the terminal ModeOutput from the host view + the parsed narrative. It is a pure view
	// assembly — it computes nothing the Aggregate step did not already compute.
	Collate(in FixedSpaceInput, view FixedSpaceView, narrative []govern.Narrative) (ModeOutput, error)
}

// fixedSpaceCollatorPreamble is the shared, host-authored opening of every fixed-space collator prompt. It
// is a constant rather than per-mode prose for the same reason the untrusted-data preamble is: the
// instruction that the numbers are final must be identical in every mode, and a test can assert on it.
const fixedSpaceCollatorPreamble = "You are the COLLATOR. Everything below was computed by the HOST from " +
	"the panel's recorded blind responses under published, versioned rules. It is FINAL. Do NOT recompute, " +
	"re-aggregate, re-rank, average, round, correct or override any number, ordering or label in it — any " +
	"number you emit is discarded, so a \"corrected\" value would only remove your explanation of the real " +
	"one. Your job is NARRATIVE: say what the result means, where the panel disagreed and why that matters, " +
	"which evidence is missing, and what a reader should be careful about."
