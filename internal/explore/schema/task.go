package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// RawTask is the user's intent for one run: the purpose, the criteria the response must satisfy, and
// optional prior context such as an earlier exploration's result. It is supplied per invocation and
// persisted with the run as the record of what was asked.
type RawTask struct {
	Purpose      string   `json:"purpose"`
	Criteria     []string `json:"criteria"`
	PriorContext string   `json:"priorContext,omitempty"`
	// Mode names the exploration mode; empty means "map". The mode registry lives in package mode, so
	// Validate accepts any value and the surfaces and pipeline reject an unknown mode.
	Mode string `json:"mode,omitempty"`
	// Artifact is the user-supplied artifact a mode such as challenge examines. Modes that require it
	// check for it in their ModeSpec.ValidateTask hook, which every surface runs before any model call.
	Artifact string `json:"artifact,omitempty"`

	// The fields below are the declarations of the fixed-space modes: the user fixes the option set or the
	// estimation target before any explorer answers, so those modes need no canonicalizer. Each is
	// optional here and required by the mode that uses it.

	// Options is the option set every compare explorer evaluates.
	Options []string `json:"options,omitempty"`
	// CompareCriteria are the axes compare measures the options on, each with a direction, a role (a scored
	// dimension or a pass/fail filter) and an optional weight. Criteria, by contrast, constrain the whole
	// exploration.
	CompareCriteria []CompareCriterion `json:"compareCriteria,omitempty"`
	// Target, Unit and Horizon declare what a forecast estimates, the unit every estimate uses, and the
	// period. ConditioningEvent is an optional assumption the forecast holds fixed.
	Target            string `json:"target,omitempty"`
	Unit              string `json:"unit,omitempty"`
	Horizon           string `json:"horizon,omitempty"`
	ConditioningEvent string `json:"conditioningEvent,omitempty"`
}

// Validate checks for a non-empty purpose and at least one non-blank criterion. It does not resolve the
// mode.
func (t RawTask) Validate() error {
	if strings.TrimSpace(t.Purpose) == "" {
		return fmt.Errorf("raw_task: empty purpose")
	}
	nonBlank := 0
	for _, c := range t.Criteria {
		if strings.TrimSpace(c) != "" {
			nonBlank++
		}
	}
	if nonBlank == 0 {
		return fmt.Errorf("raw_task: no criteria (need at least one non-blank criterion)")
	}
	return nil
}

// ExplorerTaskPayload is what every explorer receives: the final prompt and the response schema. Its
// serialized form is identical for every explorer, which Canonical and Hash make checkable.
type ExplorerTaskPayload struct {
	FinalPrompt    string `json:"finalPrompt"`
	ExpandedSchema Schema `json:"expandedSchema"`
}

// Canonical returns the payload's deterministic JSON encoding, which is persisted, hashed and sent to
// every explorer.
func (p ExplorerTaskPayload) Canonical() ([]byte, error) {
	return json.Marshal(p)
}

// Hash returns the hex SHA-256 of the canonical payload.
func (p ExplorerTaskPayload) Hash() (string, error) {
	b, err := p.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Validate checks the payload is usable: a non-empty final prompt and an expanded schema that
// satisfies the minimum-schema guard.
func (p ExplorerTaskPayload) Validate() error {
	if strings.TrimSpace(p.FinalPrompt) == "" {
		return fmt.Errorf("explorer_task_payload: empty finalPrompt")
	}
	return ExpandedSatisfiesMinimum(p.ExpandedSchema)
}

// FormulationSource records who produced the shared payload. Every mode supplies its own prompt and
// schema, so the recorded source is always formulation-free: no collator framed round 1.
type FormulationSource string

const (
	// FormulationFreeMap marks a payload built deterministically from the mode's prompt and schema, with
	// no collator call.
	FormulationFreeMap FormulationSource = "formulation-free (map)"
)

// Formulation is the persisted record of how the round-1 payload was formed. CollatorAttempted is always
// false; it is recorded so the evidence export can show that no collator framed the task.
type Formulation struct {
	Source            FormulationSource   `json:"source"`
	CollatorAttempted bool                `json:"collatorAttempted"`
	Payload           ExplorerTaskPayload `json:"payload"`
}

// FormulationFree builds the formulation whose payload is the mode's own prompt and explorer schema,
// assembled without a collator call.
func FormulationFree(source FormulationSource, prompt string, explorerSchema Schema) Formulation {
	return Formulation{
		Source:            source,
		CollatorAttempted: false,
		Payload: ExplorerTaskPayload{
			FinalPrompt:    prompt,
			ExpandedSchema: explorerSchema,
		},
	}
}

// DefaultPrompt builds the map explorer prompt from raw, quoting the purpose and every criterion verbatim.
func DefaultPrompt(raw RawTask) string {
	var b strings.Builder
	b.WriteString("Explore the following task and respond with a SINGLE JSON object. Output ONLY the JSON object — no prose before or after it, and no markdown code fences.\n\n")
	b.WriteString("Purpose:\n")
	b.WriteString(raw.Purpose)
	b.WriteString("\n\nCriteria the response must satisfy:\n")
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
	b.WriteString(RenderSchema(MinimumSchema()))
	b.WriteString("Where: \"claims\" is your list of key assertions/findings (the substance of your answer, as many as needed); " +
		"\"evidence\" is the supporting reasoning; \"confidence\" is a number 0..1; " +
		"\"sources\" lists the fields/frameworks/literature you drew on; " +
		"\"assumptions\" are assumptions you made; \"uncertainties\" are open questions or weak points.\n")
	return b.String()
}
