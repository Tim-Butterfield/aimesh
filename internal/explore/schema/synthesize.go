package schema

// This file holds the synthesize mode's explorer schema and prompt, which ask each explorer for one best
// complete answer, and the terminal SynthesizeOutput the collator composes.

import (
	"fmt"
	"strings"
)

// SynthesizeOutput is the terminal output of the synthesize mode. The collator selects the strongest answer
// and may graft better elements from the others into it; it does not tally. Each grafted element records
// its source explorer, and each rejected alternative is kept with the reason. It satisfies the mode
// package's ModeOutput contract.
type SynthesizeOutput struct {
	// Artifact is the composed answer, the mode's deliverable. It is required.
	Artifact string `json:"artifact"`
	// ComponentProvenance attributes each grafted element to the explorer it came from.
	ComponentProvenance []ProvenanceEntry `json:"componentProvenance"`
	// MinorityReport lists the rejected alternatives and why each was not chosen.
	MinorityReport []MinorityEntry `json:"minorityReport"`
	// Rationale explains the selection and the grafts.
	Rationale string `json:"rationale"`
}

// ProvenanceEntry attributes one grafted component of the composed artifact to its explorer.
type ProvenanceEntry struct {
	Component    string           `json:"component"`
	FromExplorer ExplorerIdentity `json:"fromExplorer"`
}

// MinorityEntry is one rejected alternative answer, attributed to its explorer, with the reason it was
// not selected.
type MinorityEntry struct {
	Alternative  string           `json:"alternative"`
	FromExplorer ExplorerIdentity `json:"fromExplorer"`
	WhyRejected  string           `json:"whyRejected"`
}

// Summary returns the rationale, or the artifact when there is no rationale.
func (o SynthesizeOutput) Summary() string {
	if strings.TrimSpace(o.Rationale) != "" {
		return o.Rationale
	}
	return o.Artifact
}

// Validate checks that the artifact is non-empty. Provenance and the minority report may be empty.
func (o SynthesizeOutput) Validate() error {
	if strings.TrimSpace(o.Artifact) == "" {
		return fmt.Errorf("synthesize output has an empty artifact")
	}
	return nil
}

// synthesizeExplorerFields is the synthesize explorer schema: one complete answer with its rationale and
// assumptions. It is not subject to the minimum-schema guard.
var synthesizeExplorerFields = []Field{
	{Name: "answer", Type: TypeString, Required: true, Repeated: false},
	{Name: "rationale", Type: TypeString, Required: true, Repeated: false},
	{Name: "assumptions", Type: TypeString, Required: false, Repeated: true},
	{Name: "confidence", Type: TypeNumber, Required: false, Repeated: false},
}

// SynthesizeExplorerSchema returns a copy of the synthesize explorer schema.
func SynthesizeExplorerSchema() Schema {
	fields := make([]Field, len(synthesizeExplorerFields))
	copy(fields, synthesizeExplorerFields)
	return Schema{Fields: fields}
}

// SynthesizeExplorerPrompt builds the synthesize explorer prompt from raw, quoting the purpose and
// criteria verbatim, asking for one complete answer, and rendering the schema's field names.
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
