package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// RawTask is the user's per-run intent (design §6.2): the exploration purpose, the criteria it
// must satisfy, and optional prior context (so exploration N+1 can consume exploration N's result,
// §6.4). It is supplied per invocation — NOT roster state — analogous to reviewmesh's workspace
// path. It is persisted alongside the collator's final_prompt as the intent-fidelity audit record.
type RawTask struct {
	Purpose      string   `json:"purpose"`
	Criteria     []string `json:"criteria"`
	PriorContext string   `json:"priorContext,omitempty"`
	// Mode selects the app-owned exploration mode (design §3). Empty ⇒ the default mode ("map"); it is
	// carried as an opaque string here (the mode registry lives in internal/mode to avoid an import
	// cycle, and the surfaces + pipeline resolve it). omitempty so a default/Map task serializes as
	// before. Validate accepts an empty or any mode string; the surfaces + pipeline reject an UNKNOWN
	// mode against the registry with a message listing the known modes.
	Mode string `json:"mode,omitempty"`
	// Artifact is the supplied TYPED ARTIFACT UNDER REVIEW — the thing a mode attacks rather than
	// researches (design §3, the Challenge row). It is user-supplied per invocation (a CLI `--artifact
	// <path|->`, an ACP `_meta.exploremesh.artifact`), never roster state and never model-authored. Most
	// modes ignore it, so Validate does NOT require it here: a mode that NEEDS it declares that through
	// its ModeSpec.ValidateTask hook, which every surface runs BEFORE any spend. omitempty so a task for
	// a mode that ignores it carries no `artifact` key at all.
	Artifact string `json:"artifact,omitempty"`
	// --- the FIXED-SPACE declarations (design §3 Compare + Forecast rows) ---
	//
	// These fields are what MAKES those modes fixed-space: the option set / criteria / estimation target are
	// declared by the USER, per invocation, BEFORE any explorer speaks — which is why neither mode needs a
	// canonicalizer or a confirmation round (there is no emergent universe to resolve, §0 F-B). Like
	// Artifact they are optional at this level and REQUIRED by whichever mode says so through its
	// ModeSpec.ValidateTask, so a task for a mode that is not fixed-space carries none of these keys.

	// Options is Compare's DECLARED OPTION SET — the exact options every explorer evaluates.
	Options []string `json:"options,omitempty"`
	// CompareCriteria are Compare's DECLARED evaluation criteria, each with the DIRECTION its scale runs in
	// and its ROLE (a scored dimension or a pass/fail filter gate), plus an optional user weight. They are
	// deliberately a separate field from Criteria above: Criteria are the constraints the whole exploration
	// must satisfy, while these are the axes the option set is measured on and the host's Pareto rule reads.
	CompareCriteria []CompareCriterion `json:"compareCriteria,omitempty"`
	// Target / Unit / Horizon are Forecast's DECLARED estimation target: what is being estimated, the unit
	// every estimate must be given in, and the period it is for. ConditioningEvent is the optional "assume
	// this holds" clause.
	Target            string `json:"target,omitempty"`
	Unit              string `json:"unit,omitempty"`
	Horizon           string `json:"horizon,omitempty"`
	ConditioningEvent string `json:"conditioningEvent,omitempty"`
}

// Validate checks the raw task is usable: a non-empty purpose and at least one NON-BLANK criterion.
// An all-whitespace criteria list (every entry blank after trimming) is rejected exactly like an
// empty list — the CLI's splitCSV already drops blanks, and the ACP surface must never accept a
// criteria list that carries no real constraint (design §6.2: criteria are load-bearing intent).
// The mode is NOT resolved here (the registry lives in internal/mode); an empty or known mode is
// accepted, and the surfaces + pipeline reject an unknown mode against the registry.
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

// ExplorerTaskPayload is what EVERY explorer receives (design §6.2): the collator-formulated
// final prompt + the expanded response schema. Its serialized form is BYTE-IDENTICAL across
// explorers — the only variable per explorer is (adapter, model, effort) — so "identical" is
// verifiable (Canonical/Hash), not aspirational.
type ExplorerTaskPayload struct {
	FinalPrompt    string `json:"finalPrompt"`
	ExpandedSchema Schema `json:"expandedSchema"`
}

// Canonical returns the deterministic serialized payload persisted, hashed, and handed to every
// explorer. Struct field order + slice order are fixed, so json.Marshal is stable — the byte
// sequence is reproducible and comparable across explorers.
func (p ExplorerTaskPayload) Canonical() ([]byte, error) {
	return json.Marshal(p)
}

// Hash returns the hex SHA-256 of the canonical payload — the audit fingerprint proving every
// explorer received the identical payload.
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

// FormulationSource records who produced the shared payload (design §6.7). Formulation is APP-OWNED:
// every mode supplies its own explorer prompt + explorer schema, so the only source a run can record is
// a formulation-free one. It stays a typed, extensible label (rather than an implied constant) because
// it is a persisted governance fact — the record that no shared collator frame conditioned round 1.
// There is no collator-formulated source and no deterministic-fallback source, because there is no
// collator-formulate leg: no mode could produce either, so they would only advertise a distinction the
// binary cannot make.
type FormulationSource string

const (
	// FormulationFreeMap marks a payload the pipeline built DETERMINISTICALLY from the mode's app-owned
	// prompt + fixed schema, with no collator formulate call at all (design §3). Later comparisons
	// therefore stay honest that no shared collator frame conditioned round-1 — the common cause is
	// removed, not merely bounded (§0 F-A).
	FormulationFreeMap FormulationSource = "formulation-free (map)"
)

// Formulation is the persisted run-state of phase-1 formulation (design §6.7). CollatorAttempted is
// retained (and always false) because it is a positive GOVERNANCE assertion in the run record, not a
// branch flag: §9's derived export reads run directories it did not write and projects it as the
// `collator_attempted` / `formulation_free` columns, so an artifact reader can see the property
// without having to know which modes a given build shipped.
type Formulation struct {
	Source            FormulationSource   `json:"source"`
	CollatorAttempted bool                `json:"collatorAttempted"`
	Payload           ExplorerTaskPayload `json:"payload"`
}

// FormulationFree builds the persisted formulation for a FORMULATION-FREE mode (design §3): the
// payload is the mode's app-owned prompt + fixed explorer schema, assembled deterministically WITHOUT
// any collator formulate call. CollatorAttempted is false — the collator was never asked to formulate
// (unlike the fallback, where it was asked and failed). The source records the formulation-free
// provenance so a later comparison knows round-1 was NOT conditioned on a shared collator frame.
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

// DefaultPrompt derives a plain exploration prompt from raw_task, preserving every purpose and
// criterion verbatim (add nothing, drop nothing) — the deterministic analogue of the collator's
// intent-fidelity instruction.
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
