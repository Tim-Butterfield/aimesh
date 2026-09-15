package schema

// This file holds the Compare mode's task inputs, round-1 prompt and schema, and response parsing.
//
// The user declares the options and criteria up front, so every explorer fills in the same
// options×criteria grid and each cell can be compared directly. Each criterion declares a direction
// (whether higher or lower is better), since Pareto dominance depends on it. Its role is either a scored
// dimension or a pass/fail filter applied before dominance.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Direction states which end of a criterion's scale is better.
type Direction string

// Directions a criterion may declare.
const (
	HigherIsBetter Direction = "higher_is_better"
	LowerIsBetter  Direction = "lower_is_better"
)

// Valid reports whether d is HigherIsBetter or LowerIsBetter.
func (d Direction) Valid() bool { return d == HigherIsBetter || d == LowerIsBetter }

// Better reports whether a is strictly better than b under d.
func (d Direction) Better(a, b float64) bool {
	if d == LowerIsBetter {
		return a < b
	}
	return a > b
}

// AtLeastAsGood reports whether a is at least as good as b under d.
func (d Direction) AtLeastAsGood(a, b float64) bool {
	if d == LowerIsBetter {
		return a <= b
	}
	return a >= b
}

// CriterionRole is whether a criterion is scored or a pass/fail gate.
type CriterionRole string

const (
	// RoleDimension is a scored criterion that takes part in Pareto dominance.
	RoleDimension CriterionRole = "dimension"
	// RoleFilter is a pass/fail criterion. Options that fail it are excluded before dominance is computed.
	RoleFilter CriterionRole = "filter"
)

// CompareCriterion is one declared Compare criterion. A scalar ranking is produced only when every scored
// criterion has a weight.
type CompareCriterion struct {
	Name      string        `json:"name"`
	Direction Direction     `json:"direction,omitempty"`
	Role      CriterionRole `json:"role,omitempty"`
	// Weight is the relative weight of a scored criterion; 0 means none was declared.
	Weight float64 `json:"weight,omitempty"`
}

// EffectiveRole returns the criterion's role. Anything other than RoleFilter is treated as RoleDimension.
func (c CompareCriterion) EffectiveRole() CriterionRole {
	if c.Role == RoleFilter {
		return RoleFilter
	}
	return RoleDimension
}

// Validate checks the criterion. A scored dimension must declare a direction; a filter need not.
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

// ValidateCompareTask is Compare's ValidateTask. It requires at least two distinct options and a set of
// valid, distinct criteria that includes a scored dimension.
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

// NonBlank returns the trimmed, non-empty entries of s.
func NonBlank(s []string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// FilterVerdict is an explorer's answer for a filter criterion.
type FilterVerdict string

// Filter verdicts.
const (
	VerdictPass FilterVerdict = "pass"
	VerdictFail FilterVerdict = "fail"
	// VerdictUnknown marks a missing or unrecognized answer, which is distinct from fail.
	VerdictUnknown FilterVerdict = "unknown"
)

// NormalizeVerdict converts a model's filter answer to a FilterVerdict. It accepts "pass", "fail" and JSON
// booleans; anything else is VerdictUnknown.
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

// compareExplorerFields is the Compare round-1 explorer schema. Each evaluation is one cell object;
// optionsEvaluated lists the options covered and keys the degraded register.
var compareExplorerFields = []Field{
	{Name: "evaluations", Type: TypeObject, Required: true, Repeated: true},
	{Name: "optionsEvaluated", Type: TypeString, Required: true, Repeated: true},
	{Name: "missingEvidence", Type: TypeString, Required: false, Repeated: true},
	{Name: "notes", Type: TypeString, Required: false, Repeated: false},
}

// CompareExplorerSchema returns a new copy of the Compare round-1 explorer schema.
func CompareExplorerSchema() Schema {
	fields := make([]Field, len(compareExplorerFields))
	copy(fields, compareExplorerFields)
	return Schema{Fields: fields}
}

// Markers that introduce the JSON declarations in the Compare prompt. The fake model parses them.
const (
	CompareOptionSetMarker = "declared option set (JSON):\n"
	CompareCriteriaMarker  = "declared criteria (JSON):\n"
)

// CompareExplorerPrompt builds the Compare round-1 prompt. The options and criteria appear both as lists
// and as JSON blocks, and the prompt asks for raw values because the host applies each criterion's
// direction itself.
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

// directionPhrase describes d for the prompt.
func directionPhrase(d Direction) string {
	if d == LowerIsBetter {
		return "a LOWER value is better"
	}
	return "a HIGHER value is better"
}

// criterionWire is one criterion in the prompt's JSON block. Weight is left out so evaluators cannot tell
// which criterion the requester values most.
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

// mustJSON returns v as compact JSON, or "[]" if marshaling fails.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// CompareEvaluation is one cell from an explorer's response. Names are not matched against the declared
// options or criteria here.
type CompareEvaluation struct {
	Option    string `json:"option"`
	Criterion string `json:"criterion"`
	// Value is the value as the explorer wrote it.
	Value string `json:"value"`
	// Score is the numeric value; HasScore is false when Value is not a number.
	Score    float64 `json:"score,omitempty"`
	HasScore bool    `json:"hasScore,omitempty"`
	// Verdict is the normalized filter answer.
	Verdict   FilterVerdict `json:"verdict,omitempty"`
	Rationale string        `json:"rationale,omitempty"`
}

// ParseEvaluations returns the evaluations in a validated round-1 response. Entries missing an option or
// criterion are skipped; non-numeric values are kept.
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

// numericValue returns v as a number if it is a JSON number or a string that parses entirely as one. Numbers
// are never extracted from surrounding prose.
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

// ParseMissingEvidence returns the non-empty missingEvidence entries in a validated response.
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
