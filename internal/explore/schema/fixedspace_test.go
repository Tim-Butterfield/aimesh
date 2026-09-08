package schema

// Tests for the FIXED-SPACE modes' app-owned round artifacts (design §3 Compare + Forecast rows). The
// prompts are asserted the same way every other mode's are — the exact field names must be RENDERED, because
// a prompt that only says "match the schema" gets improvised field names from a real model — plus the two
// things unique to these modes: the declared space must be embedded machine-readably, and the DIRECTION must
// be stated with an instruction not to invert it.

import (
	"encoding/json"
	"strings"
	"testing"
)

func compareTask() RawTask {
	return RawTask{
		Purpose: "choose a store", Criteria: []string{"operability"},
		Options: []string{"alpha", "beta"},
		CompareCriteria: []CompareCriterion{
			{Name: "throughput", Direction: HigherIsBetter, Role: RoleDimension, Weight: 3},
			{Name: "on-prem support", Role: RoleFilter},
		},
	}
}

// TestCompareExplorerPrompt_RendersTheDeclaredSpaceAndFieldNames pins the round-1 contract: the option set
// and criteria appear both for a human and as machine-readable blocks, every response field name is spelled
// out, and the direction instruction tells the explorer NOT to invert its own scale.
func TestCompareExplorerPrompt_RendersTheDeclaredSpaceAndFieldNames(t *testing.T) {
	p := CompareExplorerPrompt(compareTask())
	for _, want := range []string{
		"alpha", "beta", "throughput", "on-prem support",
		"a HIGHER value is better", "GATE: answer \"pass\" or \"fail\"",
		`"evaluations"`, `"optionsEvaluated"`, `"missingEvidence"`, `"notes"`,
		`"option"`, `"criterion"`, `"value"`, `"rationale"`,
		"do NOT invert, normalize or rank anything yourself",
		"Do NOT add an option, drop an option",
		"Output ONLY the JSON object", "no markdown code fences",
		CompareOptionSetMarker, CompareCriteriaMarker,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("compare prompt missing %q", want)
		}
	}
	// The user's WEIGHT is deliberately absent: an evaluator told which criterion the requester cares about
	// most has been told which answer would please them.
	if strings.Contains(p, "weight") {
		t.Errorf("the compare prompt must NOT reveal the requester's weights:\n%s", p)
	}
	// The machine-readable blocks are decodable exactly as a reader (or a fake) would decode them.
	var options []string
	idx := strings.Index(p, CompareOptionSetMarker)
	if err := json.NewDecoder(strings.NewReader(p[idx+len(CompareOptionSetMarker):])).Decode(&options); err != nil {
		t.Fatalf("the declared option set block must be decodable JSON: %v", err)
	}
	if len(options) != 2 || options[0] != "alpha" {
		t.Errorf("declared option set = %v", options)
	}
}

// TestParseEvaluations_IsMechanical pins the parse: numbers are read as numbers, pass/fail is normalized
// onto the closed enum, an unreadable value is CARRIED (never coerced to zero), and an entry with no option
// or criterion is skipped because there is no cell to attach it to.
func TestParseEvaluations_IsMechanical(t *testing.T) {
	got := ParseEvaluations(map[string]any{"evaluations": []any{
		map[string]any{"option": "alpha", "criterion": "throughput", "value": 8.5, "rationale": "benchmarks"},
		map[string]any{"option": "alpha", "criterion": "on-prem support", "value": "PASS"},
		map[string]any{"option": "beta", "criterion": "on-prem support", "value": false},
		map[string]any{"option": "beta", "criterion": "throughput", "value": "about seven"},
		map[string]any{"option": "beta", "criterion": "throughput", "value": "7"},
		map[string]any{"option": "", "criterion": "throughput", "value": 1},
	}})
	if len(got) != 5 {
		t.Fatalf("expected 5 placeable evaluations, got %d: %+v", len(got), got)
	}
	if !got[0].HasScore || got[0].Score != 8.5 {
		t.Errorf("a JSON number must be read as the score: %+v", got[0])
	}
	if got[1].Verdict != VerdictPass {
		t.Errorf("a case-insensitive pass must normalize onto the enum: %+v", got[1])
	}
	if got[2].Verdict != VerdictFail {
		t.Errorf("a JSON boolean false is an unambiguous fail: %+v", got[2])
	}
	if got[3].HasScore {
		t.Errorf("prose must NOT be mined for a number: %+v", got[3])
	}
	if got[3].Value != "about seven" {
		t.Errorf("an unreadable value must be carried verbatim: %+v", got[3])
	}
	if !got[4].HasScore || got[4].Score != 7 {
		t.Errorf("a numeric STRING is an unambiguous number: %+v", got[4])
	}
}

// TestValidateCompareTask_RefusesAnUnderDeclaredSpace pins the ValidateTask seam every surface runs before
// any spend.
func TestValidateCompareTask_RefusesAnUnderDeclaredSpace(t *testing.T) {
	ok := compareTask()
	// A comparison made only of gates has no trade-off frontier, so it is refused too.
	gatesOnly := ok
	gatesOnly.CompareCriteria = []CompareCriterion{{Name: "on-prem", Role: RoleFilter}}
	for _, tc := range []struct {
		name string
		raw  RawTask
		want string
	}{
		{"no options", RawTask{CompareCriteria: ok.CompareCriteria}, "OPTION SET"},
		{"one option", RawTask{Options: []string{"alpha"}, CompareCriteria: ok.CompareCriteria}, "OPTION SET"},
		{"duplicate options", RawTask{Options: []string{"alpha", "ALPHA"}, CompareCriteria: ok.CompareCriteria}, "duplicate option"},
		{"no criteria", RawTask{Options: ok.Options}, "declared CRITERIA"},
		{"dimension with no direction", RawTask{Options: ok.Options, CompareCriteria: []CompareCriterion{{Name: "cost"}}}, "must declare its direction"},
		{"unknown role", RawTask{Options: ok.Options, CompareCriteria: []CompareCriterion{{Name: "cost", Direction: LowerIsBetter, Role: "vibes"}}}, "unknown role"},
		{"gates only", gatesOnly, "no scored dimension"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCompareTask(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if err := ValidateCompareTask(ok); err != nil {
		t.Errorf("a fully declared comparison must validate: %v", err)
	}
}

// TestForecastExplorerPrompt_DemandsMachineReadableNumbers pins the round-1 contract of the mode whose whole
// point is a machine-readable quantity: the declared target/unit/horizon, the field names, the explicit
// rejection warning, and the ABSTENTION offered as the honest alternative to a guess.
func TestForecastExplorerPrompt_DemandsMachineReadableNumbers(t *testing.T) {
	p := ForecastExplorerPrompt(RawTask{
		Purpose: "size the migration", Criteria: []string{"trend"},
		Target: "peak rps", Unit: "requests/second", Horizon: "12 months",
		ConditioningEvent: "the constraint lifts",
	})
	for _, want := range []string{
		"peak rps", "requests/second", "12 months", "the constraint lifts",
		`"estimate"`, `"low"`, `"high"`, `"confidence"`, `"assumptions"`, `"reasoning"`,
		"MUST be bare JSON numbers", "is REJECTED",
		`"abstain": true`, "deliberate abstention",
		"pooled by the HOST", "Do NOT try to be the consensus",
		ForecastTargetMarker,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("forecast prompt missing %q", want)
		}
	}
	// The schema types the numerics — the type IS the rejection mechanism.
	for _, name := range []string{"estimate", "low", "high"} {
		f, ok := fieldOf(ForecastExplorerSchema(), name)
		if !ok || f.Type != TypeNumber || !f.Required {
			t.Errorf("%q must be a REQUIRED number in the forecast schema: %+v", name, f)
		}
	}
}

// TestParseForecast_RefusesANonNumericEstimate pins the mode's first refusal, independently of the envelope
// schema check: a narrative estimate is an ERROR, never a zero.
func TestParseForecast_RefusesANonNumericEstimate(t *testing.T) {
	if _, err := ParseForecast(map[string]any{"estimate": "about one hundred", "low": 90.0, "high": 120.0}); err == nil {
		t.Fatal("a narrative estimate must be refused")
	} else if !strings.Contains(err.Error(), "not a number") {
		t.Errorf("the refusal must name the problem: %v", err)
	}
	got, err := ParseForecast(map[string]any{
		"estimate": 110.0, "low": 95.0, "high": 130.0, "confidence": 0.8,
		"assumptions": []any{"steady traffic"}, "reasoning": "trend",
	})
	if err != nil {
		t.Fatalf("a well-formed estimate must parse: %v", err)
	}
	if got.Estimate != 110 || !got.HasInterval || got.Low != 95 || got.High != 130 || got.Confidence != 0.8 {
		t.Errorf("parsed estimate: %+v", got)
	}
	if len(got.Assumptions) != 1 || got.Reasoning != "trend" {
		t.Errorf("the forecaster's own words must be carried: %+v", got)
	}
	// A missing or inverted interval does not invalidate a usable point estimate — it just means this
	// forecaster contributed no bound, which the pooled interval then reports over a smaller denominator.
	inverted, err := ParseForecast(map[string]any{"estimate": 10.0, "low": 20.0, "high": 5.0, "reasoning": "x"})
	if err != nil {
		t.Fatalf("an inverted interval must not invalidate the point estimate: %v", err)
	}
	if inverted.HasInterval {
		t.Errorf("an inverted interval must be dropped, not carried: %+v", inverted)
	}
}

// TestValidateForecastTask_RequiresTargetUnitAndHorizon pins the second ValidateTask seam.
func TestValidateForecastTask_RequiresTargetUnitAndHorizon(t *testing.T) {
	full := RawTask{Target: "t", Unit: "u", Horizon: "h"}
	if err := ValidateForecastTask(full); err != nil {
		t.Errorf("a fully declared forecast must validate: %v", err)
	}
	for _, tc := range []struct {
		name string
		raw  RawTask
		want string
	}{
		{"no target", RawTask{Unit: "u", Horizon: "h"}, "target"},
		{"no unit", RawTask{Target: "t", Horizon: "h"}, "unit"},
		{"no horizon", RawTask{Target: "t", Unit: "u"}, "horizon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateForecastTask(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestDirection_OrdersUnderTheDeclaredScale pins the two comparisons the host's Pareto rule is built on.
func TestDirection_OrdersUnderTheDeclaredScale(t *testing.T) {
	if !HigherIsBetter.Better(9, 3) || HigherIsBetter.Better(3, 9) {
		t.Error("higher_is_better must prefer the larger value")
	}
	if !LowerIsBetter.Better(3, 9) || LowerIsBetter.Better(9, 3) {
		t.Error("lower_is_better must prefer the smaller value")
	}
	if !HigherIsBetter.AtLeastAsGood(5, 5) || !LowerIsBetter.AtLeastAsGood(5, 5) {
		t.Error("an equal value is at least as good under either direction")
	}
	if Direction("sideways").Valid() {
		t.Error("an undeclared direction must not validate")
	}
}

// fieldOf finds a schema field by name.
func fieldOf(s Schema, name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}
