package schema

// This file holds the COMPARE mode's app-owned TASK INPUTS + round artifacts (design §3 Compare row).
//
// Compare is a FIXED-SPACE mode, and that single fact decides its whole shape. The option set and the
// criteria are DECLARED BY THE USER BEFORE ANY EXPLORER SPEAKS, so:
//
//   - there is nothing to canonicalize. Two explorers writing "Postgres" mean the same option because the
//     host handed them both the string — matching them is arithmetic over a declared universe, not the
//     entity-resolution JUDGMENT §0 F-B exists to keep visible. So Compare runs ONE blind round with no
//     canonicalizer, no confirmation round and no partition revision. Bolting canonicalization onto it
//     would not make it safer; it would manufacture a contestable judgment where none exists.
//   - the CELL is the unit of evidence. Every explorer answers the same options×criteria grid, so a
//     disagreement between two explorers about one cell is a genuine, directly comparable disagreement —
//     which is why the host surfaces it PER CELL and never averages it into a single "score".
//
// Two properties of the criterion vocabulary are load-bearing:
//
//   - DIRECTION is declared, never inferred. A criterion says whether a higher or a lower value is better,
//     because the host's Pareto dominance is computed under it. A model asked to "score" latency cannot be
//     relied on to know which way the scale runs, and a silently inverted axis inverts the frontier.
//   - ROLE separates a GATE from an AXIS. A `filter` criterion is pass/fail and excludes an option BEFORE
//     dominance is computed (an option that fails a hard requirement is not a trade-off, it is out); a
//     `dimension` is scored and participates in the frontier. Collapsing the two would let a hard
//     requirement be traded away against a soft one.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Direction is a criterion's SCALE ORIENTATION — which end of the value range is better (design §3). It is
// declared by the user with the criterion and consumed by the HOST's Pareto rule; no model ever decides it.
type Direction string

const (
	HigherIsBetter Direction = "higher_is_better"
	LowerIsBetter  Direction = "lower_is_better"
)

// Valid reports whether the direction is one of the two declared orientations.
func (d Direction) Valid() bool { return d == HigherIsBetter || d == LowerIsBetter }

// Better reports whether value a is STRICTLY better than b under this direction.
func (d Direction) Better(a, b float64) bool {
	if d == LowerIsBetter {
		return a < b
	}
	return a > b
}

// AtLeastAsGood reports whether value a is at least as good as b under this direction.
func (d Direction) AtLeastAsGood(a, b float64) bool {
	if d == LowerIsBetter {
		return a <= b
	}
	return a >= b
}

// CriterionRole is a declared criterion's ROLE: a scored trade-off axis, or a hard pass/fail gate.
type CriterionRole string

const (
	// RoleDimension is a SCORED axis: it carries a numeric value per option and participates in the host's
	// Pareto dominance under the criterion's direction.
	RoleDimension CriterionRole = "dimension"
	// RoleFilter is a GATE: pass/fail per option, applied BEFORE dominance. An option that fails a filter is
	// EXCLUDED from the frontier rather than traded off against it — that is what makes it a requirement.
	RoleFilter CriterionRole = "filter"
)

// CompareCriterion is ONE declared evaluation criterion (design §3 Compare row): its name, the direction
// its scale runs in, its role, and an OPTIONAL user-supplied weight. The weight is the ONLY thing that can
// license a scalar ranking — absent it the host emits the Pareto/trade-off view and says why (§4: a single
// number over several criteria is a weighting, and a weighting nobody declared is one the host invented).
type CompareCriterion struct {
	Name      string        `json:"name"`
	Direction Direction     `json:"direction,omitempty"`
	Role      CriterionRole `json:"role,omitempty"`
	// Weight is the user's declared relative weight for a scored dimension (0 = none declared).
	Weight float64 `json:"weight,omitempty"`
}

// EffectiveRole returns the criterion's role, defaulting an unset role to RoleDimension — the common case
// (a criterion is an axis unless the user says it is a gate). A gate must be declared explicitly: silently
// promoting an axis to a requirement would exclude options the user never asked to exclude.
func (c CompareCriterion) EffectiveRole() CriterionRole {
	if c.Role == RoleFilter {
		return RoleFilter
	}
	return RoleDimension
}

// Validate checks one declared criterion is usable. A DIMENSION must declare its direction (the host's
// dominance rule reads it); a FILTER need not, because pass/fail has no scale.
func (c CompareCriterion) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("compare criterion: empty name")
	}
	switch c.Role {
	case "", RoleDimension, RoleFilter:
	default:
		return fmt.Errorf("compare criterion %q: unknown role %q (want %q or %q)", c.Name, c.Role, RoleDimension, RoleFilter)
	}
	if c.EffectiveRole() == RoleDimension && !c.Direction.Valid() {
		return fmt.Errorf("compare criterion %q: a scored dimension must declare its direction (%q or %q) — the host's Pareto dominance is computed under it and may not be guessed",
			c.Name, HigherIsBetter, LowerIsBetter)
	}
	if c.Direction != "" && !c.Direction.Valid() {
		return fmt.Errorf("compare criterion %q: unknown direction %q (want %q or %q)", c.Name, c.Direction, HigherIsBetter, LowerIsBetter)
	}
	if c.Weight < 0 {
		return fmt.Errorf("compare criterion %q: weight %v is negative", c.Name, c.Weight)
	}
	return nil
}

// ValidateCompareTask checks the DECLARED comparison space is usable BEFORE any spend (design §3): a
// non-empty option set with no duplicates, and a non-empty, individually valid criterion set with no
// duplicates. It is the mode's ValidateTask, so every surface rejects a malformed declaration with one
// message and a panel is never paid for a comparison that has nothing to compare.
func ValidateCompareTask(raw RawTask) error {
	opts := NonBlank(raw.Options)
	if len(opts) < 2 {
		return fmt.Errorf("mode %q requires a declared OPTION SET of at least 2 options (CLI: --options a,b,c; ACP: _meta.exploremesh.options) — a comparison of one option is not a comparison", "compare")
	}
	seen := map[string]bool{}
	for _, o := range opts {
		if seen[strings.ToLower(o)] {
			return fmt.Errorf("compare: duplicate option %q in the declared option set", o)
		}
		seen[strings.ToLower(o)] = true
	}
	if len(raw.CompareCriteria) == 0 {
		return fmt.Errorf("mode %q requires declared CRITERIA, each with a direction and a role (CLI: --criterion name=<n>,direction=higher_is_better|lower_is_better,role=dimension|filter[,weight=<w>]; ACP: _meta.exploremesh.compareCriteria)", "compare")
	}
	names := map[string]bool{}
	dimensions := 0
	for _, c := range raw.CompareCriteria {
		if err := c.Validate(); err != nil {
			return err
		}
		if names[strings.ToLower(c.Name)] {
			return fmt.Errorf("compare: duplicate criterion %q in the declared criterion set", c.Name)
		}
		names[strings.ToLower(c.Name)] = true
		if c.EffectiveRole() == RoleDimension {
			dimensions++
		}
	}
	if dimensions == 0 {
		return fmt.Errorf("compare: the declared criteria contain no scored dimension — a comparison made only of pass/fail gates has no trade-off frontier to report")
	}
	return nil
}

// NonBlank returns the entries of s that are non-empty after trimming, trimmed. It is the one place the
// surfaces and the host agree on what "declared" means, so a stray blank in a CSV flag can never become an
// option named "".
func NonBlank(s []string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// --- the blind round-1 explorer contract ---

// FilterVerdict is the CLOSED pass/fail vocabulary an explorer answers a `filter` criterion with. It is
// closed for the same reason Challenge's severity enum is: the host's gate rule reads it, and an
// unrecognized value must be recorded as unrecognized rather than guessed at.
type FilterVerdict string

const (
	VerdictPass FilterVerdict = "pass"
	VerdictFail FilterVerdict = "fail"
	// VerdictUnknown is the HOST normalization of a filter answer outside the enum (or a missing one) — a
	// real value, because "the explorer gave no verdict we recognize" is different from "the explorer said
	// this option fails".
	VerdictUnknown FilterVerdict = "unknown"
)

// NormalizeVerdict maps a model-supplied filter answer onto the closed enum. A JSON boolean is accepted
// (true = pass) because it is an unambiguous statement of the same thing; anything else is `unknown`.
func NormalizeVerdict(v any) FilterVerdict {
	switch t := v.(type) {
	case bool:
		if t {
			return VerdictPass
		}
		return VerdictFail
	case string:
		switch FilterVerdict(strings.ToLower(strings.TrimSpace(t))) {
		case VerdictPass:
			return VerdictPass
		case VerdictFail:
			return VerdictFail
		}
	}
	return VerdictUnknown
}

// compareExplorerFields is the Compare mode's FIXED round-1 explorer schema. `evaluations` is a repeated
// OBJECT because a cell is a record — flattening it into parallel arrays would make the option↔criterion↔
// value pairing an inference. `optionsEvaluated` is the explorer's explicit COVERAGE declaration over the
// DECLARED option universe: it is the field the fixed-space degraded register is keyed on (§1 — a real host
// register is honest here precisely because the key universe was given to the explorers).
var compareExplorerFields = []Field{
	{Name: "evaluations", Type: TypeObject, Required: true, Repeated: true},
	{Name: "optionsEvaluated", Type: TypeString, Required: true, Repeated: true},
	{Name: "missingEvidence", Type: TypeString, Required: false, Repeated: true},
	{Name: "notes", Type: TypeString, Required: false, Repeated: false},
}

// CompareExplorerSchema returns a fresh copy of the Compare round-1 explorer schema (the copy-per-call
// contract MinimumSchema establishes).
func CompareExplorerSchema() Schema {
	fields := make([]Field, len(compareExplorerFields))
	copy(fields, compareExplorerFields)
	return Schema{Fields: fields}
}

// Prompt markers for the machine-readable declarations. They are constants so the exact bytes are
// assertable and a deterministic fake can recover the same declared space a real model is shown.
const (
	CompareOptionSetMarker = "declared option set (JSON):\n"
	CompareCriteriaMarker  = "declared criteria (JSON):\n"
)

// CompareExplorerPrompt is the deterministic, app-owned round-1 prompt: evaluate the GIVEN options against
// the GIVEN criteria. Both declarations are rendered twice — once for a human reader and once as an
// explicit JSON block — because the grid is the contract: an explorer that invents an option or renames a
// criterion produces a cell the host cannot place, and the host records that as unrecognized rather than
// quietly re-attaching it to something (which would be the entity resolution this mode exists without).
//
// The instruction about direction is not decoration. The host applies the declared direction itself, so an
// explorer that "helpfully" inverts a lower-is-better score would invert the frontier.
func CompareExplorerPrompt(raw RawTask) string {
	opts := NonBlank(raw.Options)
	var b strings.Builder
	b.WriteString("You are an independent EVALUATOR. Evaluate EXACTLY the options listed below against ")
	b.WriteString("EXACTLY the criteria listed below. Do NOT add an option, drop an option, rename a ")
	b.WriteString("criterion, or invent a criterion — the option set and the criteria were fixed by the ")
	b.WriteString("requester before you were asked, and anything outside them cannot be used. Work ALONE — ")
	b.WriteString("you are not being shown any other evaluator's answers. Respond with a SINGLE JSON object. ")
	b.WriteString("Output ONLY the JSON object — no prose before or after it, and no markdown code fences.\n\n")
	b.WriteString("Purpose of the comparison:\n")
	b.WriteString(raw.Purpose)
	b.WriteString("\n\nCriteria the comparison must satisfy:\n")
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
	b.WriteString("\nOPTIONS (evaluate every one of them):\n")
	for _, o := range opts {
		b.WriteString("- ")
		b.WriteString(o)
		b.WriteString("\n")
	}
	b.WriteString("\nEVALUATION CRITERIA (evaluate every option against every one of them):\n")
	for _, c := range raw.CompareCriteria {
		if c.EffectiveRole() == RoleFilter {
			fmt.Fprintf(&b, "- %s [GATE: answer \"pass\" or \"fail\" — this is a hard requirement, not a trade-off]\n", c.Name)
			continue
		}
		fmt.Fprintf(&b, "- %s [SCORED: answer with a NUMBER; %s]\n", c.Name, directionPhrase(c.Direction))
	}
	b.WriteString("\n")
	b.WriteString(CompareOptionSetMarker)
	b.WriteString(mustJSON(opts))
	b.WriteString("\n\n")
	b.WriteString(CompareCriteriaMarker)
	b.WriteString(mustJSON(criteriaWire(raw.CompareCriteria)))
	b.WriteString("\n\nThe JSON object MUST contain EXACTLY these fields:\n")
	b.WriteString(RenderSchema(CompareExplorerSchema()))
	b.WriteString("Each entry of \"evaluations\" MUST be a JSON object with EXACTLY these fields: " +
		"{\"option\": string (copied EXACTLY from the option set above), " +
		"\"criterion\": string (copied EXACTLY from the criteria above), " +
		"\"value\": for a SCORED criterion a NUMBER (the option's value on that criterion — its natural " +
		"unit if it has one, otherwise a 0..10 rating). Report the value as it IS: the direction above tells " +
		"the host which end is better, so do NOT invert, normalize or rank anything yourself. For a GATE " +
		"criterion the value is EXACTLY the string \"pass\" or \"fail\", " +
		"\"rationale\": string (why — the evidence behind this cell)}.\n" +
		"\"optionsEvaluated\" lists the options you actually evaluated, copied exactly.\n" +
		"\"missingEvidence\" lists the cells you could NOT judge, one entry per cell, as " +
		"\"<option> / <criterion>: <why>\". Leaving a cell out and saying so here is CORRECT and useful; " +
		"guessing a value to fill the grid is not.\n" +
		"\"notes\" is any brief context on your coverage.\n")
	return b.String()
}

// directionPhrase renders a scored criterion's direction as the sentence an evaluator reads.
func directionPhrase(d Direction) string {
	if d == LowerIsBetter {
		return "a LOWER value is better"
	}
	return "a HIGHER value is better"
}

// criterionWire is the per-criterion shape rendered into the prompt's machine-readable block: the fields a
// reader (or a deterministic fake) needs to reproduce the declared grid, and nothing else. The user's WEIGHT
// is deliberately absent — an evaluator that knew which criterion the requester cares about most has been
// told which answer would please them.
type criterionWire struct {
	Name      string        `json:"name"`
	Direction Direction     `json:"direction,omitempty"`
	Role      CriterionRole `json:"role"`
}

func criteriaWire(cs []CompareCriterion) []criterionWire {
	out := make([]criterionWire, 0, len(cs))
	for _, c := range cs {
		w := criterionWire{Name: c.Name, Role: c.EffectiveRole()}
		if w.Role == RoleDimension {
			w.Direction = c.Direction
		}
		out = append(out, w)
	}
	return out
}

// mustJSON renders a value as compact JSON for a prompt block. The inputs here are plain strings/structs,
// so marshaling cannot realistically fail; an empty array is safer than a half-rendered block.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// CompareEvaluation is ONE cell an explorer reported, lifted out of a validated response by
// ParseEvaluations. It is a mechanical projection: the host normalizes a filter verdict and reads a numeric
// score, and does nothing else — it does not match the names to the declared universe (that is the caller's
// job, over the DECLARED set) and it never repairs, rounds or rescales a value.
type CompareEvaluation struct {
	Option    string `json:"option"`
	Criterion string `json:"criterion"`
	// Value is the raw value exactly as the explorer wrote it, retained verbatim for the record even when
	// the host could read a number out of it.
	Value string `json:"value"`
	// Score / HasScore carry the numeric reading for a scored dimension. HasScore is false when the value
	// was not a number — the host then has no cell value, and says so, rather than coercing one.
	Score    float64 `json:"score,omitempty"`
	HasScore bool    `json:"hasScore,omitempty"`
	// Verdict is the normalized gate answer for a filter criterion (`unknown` when unrecognized).
	Verdict   FilterVerdict `json:"verdict,omitempty"`
	Rationale string        `json:"rationale,omitempty"`
}

// ParseEvaluations lifts the typed cell evaluations out of ONE validated round-1 response. Entries with no
// option or no criterion are skipped (there is no cell to attach them to); everything else is carried,
// including a value the host could not read as a number — an unreadable value is evidence about the panel
// and is reported, not dropped.
func ParseEvaluations(response map[string]any) []CompareEvaluation {
	raw, _ := response["evaluations"].([]any)
	out := make([]CompareEvaluation, 0, len(raw))
	for _, entry := range raw {
		obj, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		option := strings.TrimSpace(stringField(obj, "option"))
		criterion := strings.TrimSpace(stringField(obj, "criterion"))
		if option == "" || criterion == "" {
			continue
		}
		ev := CompareEvaluation{
			Option:    option,
			Criterion: criterion,
			Value:     strings.TrimSpace(stringField(obj, "value")),
			Verdict:   NormalizeVerdict(obj["value"]),
			Rationale: stringField(obj, "rationale"),
		}
		ev.Score, ev.HasScore = numericValue(obj["value"])
		out = append(out, ev)
	}
	return out
}

// numericValue reads a JSON value as a number. A JSON number is taken directly; a STRING that parses
// cleanly as a number is accepted too (a model that wrote "12.5" said 12.5), and anything else yields
// ok=false — the host does not extract digits out of prose, because "about 12, maybe more" is not 12.
func numericValue(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// ParseMissingEvidence lifts an explorer's explicit "I could not judge this" statements out of a validated
// response. They are a first-class part of the terminal output: a cell nobody could judge and a cell
// everybody scored the same must never look alike.
func ParseMissingEvidence(response map[string]any) []string {
	raw, _ := response["missingEvidence"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
