package mode

// This file defines the fixed-space terminal contract used by compare and forecast. The user declares the
// space before the fan-out, so the host computes the whole result from the blind round and the collator
// only adds prose:
//
//	Aggregate      the host computes every number and claim from the blind envelopes
//	CollatorPrompt the collator is shown the finished values and asked for prose
//	ParseNarrative the collator's output goes only into collatorNarrative
//	Collate        the output is assembled from the host view and that prose
//
// No method returns a model-produced number into the result.

import (
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// FixedSpaceInput is what a fixed-space aggregation reads: the raw task holding the declared space, the
// blind baseline, the rounds, the panel and the formulation hash.
type FixedSpaceInput struct {
	Raw      schema.RawTask
	Baseline govern.BlindBaseline
	// Primary is the verified, non-abstaining panel.
	Primary []schema.Envelope
	Rounds  []round.Round
	Panel   govern.Panel
	// FormulationHash is the hash of the payload every explorer received, pinned on each claim.
	FormulationHash string
}

// FixedSpaceView is a mode's host-computed aggregate and the claims to record for it. Value is
// mode-specific and opaque to the pipeline.
type FixedSpaceView struct {
	Value  any
	Claims []govern.Claim
}

// FixedSpaceContract is a fixed-space mode's terminal behavior, set in ModeSpec.FixedSpace. A mode sets
// exactly one of Collator, Canonicalizing and FixedSpace.
type FixedSpaceContract interface {
	// Aggregate computes the mode's view and claims from the blind round-1 envelopes, before the collator
	// runs.
	Aggregate(in FixedSpaceInput) (FixedSpaceView, error)
	// CollatorPrompt builds the collator prompt over the computed view, asking for narrative only.
	CollatorPrompt(in FixedSpaceInput, view FixedSpaceView) (string, error)
	// ParseNarrative converts the collator's output into narrative attributed to by.
	ParseNarrative(raw []byte, by schema.ExplorerIdentity) ([]govern.Narrative, error)
	// Collate assembles the ModeOutput from the view and narrative without computing anything new.
	Collate(in FixedSpaceInput, view FixedSpaceView, narrative []govern.Narrative) (ModeOutput, error)
}

// fixedSpaceCollatorPreamble opens every fixed-space collator prompt.
const fixedSpaceCollatorPreamble = "You are the COLLATOR. Everything below was computed by the HOST from " +
	"the panel's recorded blind responses under published, versioned rules. It is FINAL. Do NOT recompute, " +
	"re-aggregate, re-rank, average, round, correct or override any number, ordering or label in it — any " +
	"number you emit is discarded, so a \"corrected\" value would only remove your explanation of the real " +
	"one. Your job is NARRATIVE: say what the result means, where the panel disagreed and why that matters, " +
	"which evidence is missing, and what a reader should be careful about."
