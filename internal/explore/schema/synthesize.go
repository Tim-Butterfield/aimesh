package schema

// This file holds the Synthesize mode's app-owned artifacts (design §3): the FIXED explorer
// schema + deterministic explorer prompt (converge on one best complete answer), and the terminal
// SynthesizeOutput the collator composes. Synthesize is FORMULATION-FREE like Map — the collator
// authors NO round-1 schema (§5): both the explorer prompt and the explorer schema are app-owned.

import (
	"fmt"
	"strings"
)

// SynthesizeOutput is the FIXED, exploremesh-owned terminal output of the Synthesize mode (design §3).
// The collator SELECTS the strongest candidate answer and, where clearly beneficial, GRAFTS
// superior elements from the others into a single composed artifact — it does NOT tally. Every grafted
// element records which explorer it came from (componentProvenance), and rejected alternatives are
// preserved with a reason (minorityReport) so a minority position is never silently dropped. It
// satisfies the mode-package ModeOutput contract via Summary().
type SynthesizeOutput struct {
	// Artifact is the chosen/composed best answer — the single deliverable of the mode. Required.
	Artifact string `json:"artifact"`
	// ComponentProvenance records, per grafted element, which explorer it was taken from — so the
	// composed artifact is attributable rather than an unsourced blend.
	ComponentProvenance []ProvenanceEntry `json:"componentProvenance"`
	// MinorityReport preserves the rejected alternatives + why each was not chosen (design §3: minority
	// carried, never dropped).
	MinorityReport []MinorityEntry `json:"minorityReport"`
	// Rationale explains the selection/composition decision (why this candidate, why these grafts).
	Rationale string `json:"rationale"`
}

// ProvenanceEntry attributes one grafted component of the composed artifact to the explorer it came
// from, keyed on the full (adapter, model, effort) identity.
type ProvenanceEntry struct {
	Component    string           `json:"component"`
	FromExplorer ExplorerIdentity `json:"fromExplorer"`
}

// MinorityEntry is one rejected alternative answer, attributed to its explorer, with the reason it was
// not selected — the Synthesize analogue of Map's disagreement register.
type MinorityEntry struct {
	Alternative  string           `json:"alternative"`
	FromExplorer ExplorerIdentity `json:"fromExplorer"`
	WhyRejected  string           `json:"whyRejected"`
}

// Summary returns the one-line human summary of a Synthesize result — the composition rationale when
// present, else the composed artifact itself. Implements the mode-package ModeOutput contract.
func (o SynthesizeOutput) Summary() string {
	if strings.TrimSpace(o.Rationale) != "" {
		return o.Rationale
	}
	return o.Artifact
}

// Validate checks the composed output is usable: a non-empty artifact (the mode's whole deliverable).
// Provenance + minority report may legitimately be empty (a single dominant answer with no worthwhile
// grafts and no substantive alternatives), so they are not required.
func (o SynthesizeOutput) Validate() error {
	if strings.TrimSpace(o.Artifact) == "" {
		return fmt.Errorf("synthesize output has an empty artifact")
	}
	return nil
}

// synthesizeExplorerFields is the Synthesize mode's FIXED explorer-response schema: one best complete
// answer with its rationale + assumptions (design §3). It is app-owned (never collator-authored, §5);
// it is NOT the Map minimum schema — a formulation-free mode's explorer schema is its own contract and
// is not held to the minimum-schema guard (that guard governs a collator-EXPANDED Map schema).
var synthesizeExplorerFields = []Field{
	{Name: "answer", Type: TypeString, Required: true, Repeated: false},
	{Name: "rationale", Type: TypeString, Required: true, Repeated: false},
	{Name: "assumptions", Type: TypeString, Required: false, Repeated: true},
	{Name: "confidence", Type: TypeNumber, Required: false, Repeated: false},
}

// SynthesizeExplorerSchema returns a fresh copy of the Synthesize explorer schema (mirroring
// MinimumSchema's copy-per-call contract so a caller can never mutate the shared baseline).
func SynthesizeExplorerSchema() Schema {
	fields := make([]Field, len(synthesizeExplorerFields))
	copy(fields, synthesizeExplorerFields)
	return Schema{Fields: fields}
}

// SynthesizeExplorerPrompt derives the deterministic, app-owned explorer prompt for the Synthesize mode
// from raw_task, preserving every purpose + criterion verbatim (add nothing, drop nothing). It asks for
// the explorer's SINGLE best COMPLETE answer + rationale + assumptions and renders the exact fixed
// schema field names into the prompt (RenderSchema) — the hard-won Map lesson: a prompt that merely
// says "match the schema" gives the model nothing to match, so it improvises names and fails validation.
func SynthesizeExplorerPrompt(raw RawTask) string {
	var b strings.Builder
	b.WriteString("Give your SINGLE best COMPLETE answer to the following task, with your rationale and assumptions. ")
	b.WriteString("Do not hedge across options — commit to one answer. Respond with a SINGLE JSON object. ")
	b.WriteString("Output ONLY the JSON object — no prose before or after it, and no markdown code fences.\n\n")
	b.WriteString("Purpose:\n")
	b.WriteString(raw.Purpose)
	b.WriteString("\n\nCriteria the answer must satisfy:\n")
	for _, c := range raw.Criteria {
		b.WriteString("- ")
		b.WriteString(c)
		b.WriteString("\n")
	}
	if strings.TrimSpace(raw.PriorContext) != "" {
		b.WriteString("\nPrior context:\n")
		b.WriteString(raw.PriorContext)
		b.WriteString("\n")
	}
	b.WriteString("\nThe JSON object MUST contain EXACTLY these fields — put your FULL answer into them:\n")
	b.WriteString(RenderSchema(SynthesizeExplorerSchema()))
	b.WriteString("Where: \"answer\" is your single best complete answer to the task (the substance — as long as needed); " +
		"\"rationale\" is why this answer is best; \"assumptions\" are the assumptions it rests on; " +
		"\"confidence\" is a number 0..1.\n")
	return b.String()
}
