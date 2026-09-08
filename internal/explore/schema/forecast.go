package schema

// This file holds the FORECAST mode's app-owned TASK INPUTS + round artifact (design §3 Forecast row).
//
// Forecast is the other FIXED-SPACE mode, and it is the most deterministic thing exploremesh does. The
// TARGET, the UNIT and the HORIZON are declared by the user before any explorer speaks, so every explorer
// answers the same question in the same units and the panel's answers are directly poolable arithmetic —
// no entity resolution, no partition, no confirmation round.
//
// Two rules do the work here, and both are about refusing a tempting shortcut:
//
//   - THE ESTIMATE IS MACHINE-READABLE OR IT IS NOT AN ESTIMATE. `estimate`, `low` and `high` are typed
//     NUMBERS in the explorer schema, so a narrative answer ("somewhere in the low hundreds") fails
//     validation and the response is dropped with a reason instead of being interpreted. A host that
//     extracted a number out of prose would be inventing precision; a collator that "split the difference"
//     in words is exactly what this mode exists to replace.
//   - THE HOST POOLS; THE COLLATOR NARRATES. The aggregate, the interval, the dispersion and the outlier
//     set are computed in internal/pool by a declared, versioned rule over the recorded blind estimates.
//     The collator sees those numbers already final and may only write prose about them.

import (
	"fmt"
	"strings"
)

// ValidateForecastTask checks the DECLARED estimation target is usable BEFORE any spend (design §3): what
// is being estimated, in what unit, over what horizon. It is the mode's ValidateTask, so every surface
// refuses an under-declared forecast with the same message — pooling numbers whose units nobody fixed is
// arithmetic over things that are not the same quantity.
func ValidateForecastTask(raw RawTask) error {
	missing := make([]string, 0, 3)
	if strings.TrimSpace(raw.Target) == "" {
		missing = append(missing, "target (WHAT is being estimated)")
	}
	if strings.TrimSpace(raw.Unit) == "" {
		missing = append(missing, "unit (the unit every estimate must be given in)")
	}
	if strings.TrimSpace(raw.Horizon) == "" {
		missing = append(missing, "horizon (the period the estimate is for)")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("mode %q requires a declared estimation target — missing: %s (CLI: --target <what> --unit <unit> --horizon <when> [--conditioning <event>]; ACP: _meta.exploremesh.{target,unit,horizon,conditioningEvent})",
		"forecast", strings.Join(missing, "; "))
}

// forecastExplorerFields is the Forecast mode's FIXED round-1 explorer schema. `estimate`, `low` and `high`
// are TYPED NUMBERS and REQUIRED: the type is the rejection mechanism for a non-numeric answer, applied at
// the envelope boundary before any pooling can see it (see the file comment).
var forecastExplorerFields = []Field{
	{Name: "estimate", Type: TypeNumber, Required: true, Repeated: false},
	{Name: "low", Type: TypeNumber, Required: true, Repeated: false},
	{Name: "high", Type: TypeNumber, Required: true, Repeated: false},
	{Name: "confidence", Type: TypeNumber, Required: false, Repeated: false},
	{Name: "assumptions", Type: TypeString, Required: false, Repeated: true},
	{Name: "reasoning", Type: TypeString, Required: true, Repeated: false},
}

// ForecastExplorerSchema returns a fresh copy of the Forecast round-1 explorer schema.
func ForecastExplorerSchema() Schema {
	fields := make([]Field, len(forecastExplorerFields))
	copy(fields, forecastExplorerFields)
	return Schema{Fields: fields}
}

// ForecastTargetMarker prefixes the machine-readable declaration block in the prompt (a constant so the
// exact bytes are assertable and a deterministic fake reads the same declaration a real model is shown).
const ForecastTargetMarker = "declared estimation target (JSON):\n"

// forecastTargetWire is the declared target rendered as JSON into the prompt.
type forecastTargetWire struct {
	Target            string `json:"target"`
	Unit              string `json:"unit"`
	Horizon           string `json:"horizon"`
	ConditioningEvent string `json:"conditioningEvent,omitempty"`
}

// ForecastExplorerPrompt is the deterministic, app-owned round-1 prompt: one numeric estimate, one
// interval, and the reasoning behind them. It states the unit twice (in prose and in the JSON block)
// because a pooled aggregate over mixed units is not an aggregate, and it tells the explorer plainly that
// a hedged non-numeric answer will be REJECTED — a refusal it can make deliberately, by abstaining, rather
// than by writing prose into a numeric field.
func ForecastExplorerPrompt(raw RawTask) string {
	var b strings.Builder
	b.WriteString("You are an independent FORECASTER. Give your own numeric estimate for the target below. ")
	b.WriteString("Work ALONE — you are not being shown any other forecaster's estimate, and you are not ")
	b.WriteString("being asked to agree with anyone. Respond with a SINGLE JSON object. Output ONLY the ")
	b.WriteString("JSON object — no prose before or after it, and no markdown code fences.\n\n")
	b.WriteString("Purpose of the forecast:\n")
	b.WriteString(raw.Purpose)
	b.WriteString("\n\nCriteria the forecast must satisfy:\n")
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
	fmt.Fprintf(&b, "\nTARGET (what to estimate): %s\n", strings.TrimSpace(raw.Target))
	fmt.Fprintf(&b, "UNIT (every number you give MUST be in this unit): %s\n", strings.TrimSpace(raw.Unit))
	fmt.Fprintf(&b, "HORIZON (the period the estimate is for): %s\n", strings.TrimSpace(raw.Horizon))
	if c := strings.TrimSpace(raw.ConditioningEvent); c != "" {
		fmt.Fprintf(&b, "CONDITIONED ON (assume this holds): %s\n", c)
	}
	b.WriteString("\n")
	b.WriteString(ForecastTargetMarker)
	b.WriteString(mustJSON(forecastTargetWire{
		Target:            strings.TrimSpace(raw.Target),
		Unit:              strings.TrimSpace(raw.Unit),
		Horizon:           strings.TrimSpace(raw.Horizon),
		ConditioningEvent: strings.TrimSpace(raw.ConditioningEvent),
	}))
	b.WriteString("\n\nThe JSON object MUST contain EXACTLY these fields:\n")
	b.WriteString(RenderSchema(ForecastExplorerSchema()))
	b.WriteString("Where: \"estimate\" is your single best point estimate as a NUMBER in the declared unit; " +
		"\"low\" and \"high\" are the NUMERIC bounds of your interval (low <= estimate <= high); " +
		"\"confidence\" is a number 0..1 stating how much of the probability mass you place inside that " +
		"interval; \"assumptions\" are the assumptions your estimate rests on; \"reasoning\" is how you got " +
		"there.\n" +
		"\"estimate\", \"low\" and \"high\" MUST be bare JSON numbers — not text, not a range in a string, " +
		"not a number with the unit appended. A non-numeric estimate is REJECTED and your response is " +
		"recorded as unusable. If you genuinely cannot estimate this, do not guess: abstain by returning " +
		"\"abstain\": true instead, which is recorded as a deliberate abstention rather than a missing " +
		"answer.\n" +
		"The panel's estimates are pooled by the HOST under a fixed, published rule. Do NOT try to be the " +
		"consensus, do not hedge toward a round number, and do not soften an estimate you believe: an " +
		"estimate far from the others is reported WITH your reasoning attached, because a forecaster who " +
		"knows something the rest do not is exactly the signal this is for.\n")
	return b.String()
}

// ForecastEstimate is ONE explorer's estimate, lifted out of a validated round-1 response. It is a
// mechanical projection of typed fields — the host reads numbers that are already numbers.
type ForecastEstimate struct {
	Estimate    float64  `json:"estimate"`
	Low         float64  `json:"low"`
	High        float64  `json:"high"`
	HasInterval bool     `json:"hasInterval"`
	Confidence  float64  `json:"confidence,omitempty"`
	Assumptions []string `json:"assumptions,omitempty"`
	Reasoning   string   `json:"reasoning,omitempty"`
}

// ParseForecast lifts ONE validated response into a typed estimate. It REFUSES a non-numeric estimate with
// an error rather than returning a zero value: the pipeline's schema validation already rejects such a
// response at the envelope boundary, and this is the second, explicit refusal — a zero silently pooled as
// an estimate is the single worst thing this mode could do.
//
// The INTERVAL is treated separately from the point estimate: a missing or inverted interval does not
// invalidate a usable point estimate, it just means this explorer contributes no bound (HasInterval false),
// which the pooled interval then reports over a smaller denominator.
func ParseForecast(response map[string]any) (ForecastEstimate, error) {
	est, ok := numericValue(response["estimate"])
	if !ok {
		return ForecastEstimate{}, fmt.Errorf("forecast response: \"estimate\" is not a number (got %v) — a forecast is a machine-readable quantity, and a narrative estimate is rejected rather than interpreted", response["estimate"])
	}
	out := ForecastEstimate{Estimate: est, Reasoning: stringField(response, "reasoning")}
	low, lok := numericValue(response["low"])
	high, hok := numericValue(response["high"])
	if lok && hok && low <= high {
		out.Low, out.High, out.HasInterval = low, high, true
	}
	if c, cok := numericValue(response["confidence"]); cok {
		out.Confidence = c
	}
	if arr, aok := response["assumptions"].([]any); aok {
		for _, a := range arr {
			if s := strings.TrimSpace(fmt.Sprintf("%v", a)); s != "" {
				out.Assumptions = append(out.Assumptions, s)
			}
		}
	}
	return out, nil
}
