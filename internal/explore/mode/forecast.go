package mode

// This file implements the Forecast mode, a fixed-space mode:
//
//	declare          the user fixes the target, unit, horizon and any conditioning event
//	round 1 (blind)  each explorer returns a numeric estimate, interval and reasoning
//	host             package pool computes the aggregate, interval, dispersion and outliers
//	collate          the collator adds narrative
//
// There is one round and no canonicalization. Non-numeric estimates are rejected and recorded, not
// interpreted. Outliers are flagged but stay in the aggregate, with their reasoning.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pool"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// ForecastRule is the aggregation rule Forecast uses. It is fixed so it cannot be chosen after the estimates
// are in.
const ForecastRule = pool.RuleMedian

// ForecastOutput is the Forecast mode's output: the declared target, the pooled aggregate, interval and
// dispersion, each attributed estimate, outliers, assumptions and the governance claim.
type ForecastOutput struct {
	Target            string `json:"target"`
	Unit              string `json:"unit"`
	Horizon           string `json:"horizon"`
	ConditioningEvent string `json:"conditioningEvent,omitempty"`
	// Space and SpaceNote state that this is a fixed-space pooled estimate with no entity resolution.
	Space     string `json:"space"`
	SpaceNote string `json:"spaceNote"`
	// PartitionRevisionHash holds govern.FixedSpaceNoPartition.
	PartitionRevisionHash string `json:"partitionRevisionHash"`
	// Aggregate and Interval are pooled by the host under Method.
	Aggregate float64          `json:"aggregate"`
	Interval  ForecastInterval `json:"interval"`
	// Dispersion describes the spread of the estimates.
	Dispersion pool.Dispersion `json:"dispersion"`
	Method     ForecastMethod  `json:"method"`
	// IndividualEstimates are the attributed estimates that were pooled.
	IndividualEstimates []pool.Estimate `json:"individualEstimates"`
	// Outliers are flagged estimates with their reasoning. They remain in the aggregate.
	Outliers []pool.Outlier `json:"outliers,omitempty"`
	// Assumptions are the attributed assumptions the explorers stated.
	Assumptions []AttributedAssumption `json:"assumptions,omitempty"`
	// Rejected lists responses that yielded no usable estimate, with the reason.
	Rejected []RejectedEstimate `json:"rejected,omitempty"`
	// Claim records how many distinct blind round-1 forecasters the aggregate rests on, with both
	// denominators and the contributing envelope refs.
	Claim govern.Claim `json:"claim"`
	// PanelSize, Respondents and Abstentions describe participation.
	PanelSize   int `json:"panelSize"`
	Respondents int `json:"respondents"`
	Abstentions int `json:"abstentions"`
	// CollatorNarrative holds the collator's prose.
	CollatorNarrative []govern.Narrative `json:"collatorNarrative,omitempty"`
}

// ForecastInterval is the pooled interval and the number of forecasters that gave one.
type ForecastInterval struct {
	Low     float64 `json:"low"`
	High    float64 `json:"high"`
	Sources int     `json:"sources"`
}

// ForecastMethod records the rule versions the pooled values were computed under.
type ForecastMethod struct {
	Rule            string `json:"rule"`
	RulesVersion    string `json:"rulesVersion"`
	OutlierRule     string `json:"outlierRule"`
	IntervalRule    string `json:"intervalRule"`
	GovernanceRules string `json:"governanceRulesVersion"`
	// ComputedBy states who produced the aggregate.
	ComputedBy string `json:"computedBy"`
}

// AttributedAssumption is one assumption a forecaster stated, with its source.
type AttributedAssumption struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Assumption  string                  `json:"assumption"`
}

// RejectedEstimate is a response that could not be pooled, with the reason.
type RejectedEstimate struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Reason      string                  `json:"reason"`
}

// Summary returns a one-line summary of the pooled value, rule, dispersion and outlier count.
func (o ForecastOutput) Summary() string {
	return fmt.Sprintf("forecast: %s = %g %s over %s — HOST-pooled by %s over %d estimate(s) (%s / %s); interval %g..%g, spread %g (min %g, max %g, IQR %g); %d outlier(s) identified and CARRIED with their reasoning [%s; fixed space: no partition]",
		o.Target, o.Aggregate, o.Unit, o.Horizon, o.Method.Rule, len(o.IndividualEstimates),
		o.Claim.KOfPanel, o.Claim.KOfRespondents, o.Interval.Low, o.Interval.High,
		o.Dispersion.Range, o.Dispersion.Min, o.Dispersion.Max, o.Dispersion.IQR,
		len(o.Outliers), o.Method.RulesVersion)
}

// Validate requires at least one pooled estimate, a target and unit, and the fixed-space note.
func (o ForecastOutput) Validate() error {
	if len(o.IndividualEstimates) == 0 {
		return fmt.Errorf("forecast output pooled no estimates — an aggregate over an empty panel is not a forecast")
	}
	if strings.TrimSpace(o.Target) == "" || strings.TrimSpace(o.Unit) == "" {
		return fmt.Errorf("forecast output carries no declared target/unit — a pooled number without its unit is not a quantity")
	}
	if o.SpaceNote == "" || o.PartitionRevisionHash == "" {
		return fmt.Errorf("forecast output carries no space rendering — a fixed-space result must state that no entity resolution was performed")
	}
	return nil
}

// forecastPooled is Forecast's host view: the pooled result plus assumptions, rejections and the claim.
type forecastPooled struct {
	Pooled      pool.Pooled
	Assumptions []AttributedAssumption
	Rejected    []RejectedEstimate
	Claim       govern.Claim
}

// forecastCollator is the Forecast mode's FixedSpaceContract.
type forecastCollator struct{}

// Aggregate parses each blind round-1 estimate, records the unusable ones, pools the rest under
// ForecastRule and builds the governance claim.
func (forecastCollator) Aggregate(in FixedSpaceInput) (FixedSpaceView, error) {
	var estimates []pool.Estimate
	var assumptions []AttributedAssumption
	var rejected []RejectedEstimate
	var refs []string
	var sources []schema.ExplorerIdentity
	for _, env := range in.Primary {
		ref := schema.EnvelopeRef(1, env.Order)
		est, err := schema.ParseForecast(env.Response)
		if err != nil {
			rejected = append(rejected, RejectedEstimate{Explorer: env.Identity, EnvelopeRef: ref, Reason: err.Error()})
			continue
		}
		estimates = append(estimates, pool.Estimate{
			Explorer: env.Identity, EnvelopeRef: ref, Value: est.Estimate,
			Low: est.Low, High: est.High, HasInterval: est.HasInterval,
			Confidence: est.Confidence, Assumptions: est.Assumptions, Reasoning: est.Reasoning,
		})
		for _, a := range est.Assumptions {
			assumptions = append(assumptions, AttributedAssumption{Explorer: env.Identity, EnvelopeRef: ref, Assumption: a})
		}
		refs = append(refs, ref)
		sources = append(sources, env.Identity)
	}
	pooled, perr := pool.Pool(estimates, ForecastRule)
	if perr != nil {
		return FixedSpaceView{}, fmt.Errorf("forecast: %w (%d response(s) could not contribute a numeric estimate)", perr, len(rejected))
	}
	claim, cerr := govern.EstimateClaim(govern.EstimateClaimInput{
		Target: strings.TrimSpace(in.Raw.Target), Unit: strings.TrimSpace(in.Raw.Unit),
		Baseline: in.Baseline, Panel: in.Panel, FormulationHash: in.FormulationHash,
		EnvelopeRefs: refs, Sources: sources,
	})
	if cerr != nil {
		return FixedSpaceView{}, cerr
	}
	return FixedSpaceView{
		Value:  forecastPooled{Pooled: pooled, Assumptions: assumptions, Rejected: rejected, Claim: claim},
		Claims: []govern.Claim{claim},
	}, nil
}

// forecastNarrativeWire is the collator's response shape. It has no numeric fields.
type forecastNarrativeWire struct {
	Reading      string   `json:"reading"`
	OutlierNotes []string `json:"outlierNotes"`
	KeyDrivers   []string `json:"keyDrivers"`
	Cautions     []string `json:"cautions"`
}

// CollatorPrompt shows the collator the pooled result as JSON and asks for narrative, including on outliers.
func (forecastCollator) CollatorPrompt(in FixedSpaceInput, view FixedSpaceView) (string, error) {
	fp, ok := view.Value.(forecastPooled)
	if !ok {
		return "", fmt.Errorf("forecast: host view is %T, not the pooled forecast", view.Value)
	}
	b, err := json.Marshal(fp.Pooled)
	if err != nil {
		return "", fmt.Errorf("marshal pooled forecast: %w", err)
	}
	var s strings.Builder
	s.WriteString(fixedSpaceCollatorPreamble)
	s.WriteString("\n\nThis is a FIXED-SPACE forecast: the target, the unit and the horizon were declared by ")
	s.WriteString("the requester BEFORE any forecaster was asked, and the aggregate below was pooled by the ")
	s.WriteString("HOST under a published, versioned rule. Do NOT average anything, do NOT split any ")
	s.WriteString("difference, and do NOT propose your own number — the pooled value is the answer.\n\n")
	s.WriteString("The OUTLIERS listed below were identified by the host and deliberately NOT removed from ")
	s.WriteString("the aggregate. Each carries its own forecaster's reasoning. Take them seriously: an ")
	s.WriteString("estimate far from the panel is sometimes the only one that noticed something. Explain what ")
	s.WriteString("the outlier is claiming and what would have to be true for it to be right — do not dismiss ")
	s.WriteString("it as an error, and do not endorse it as the answer.\n\n")
	fmt.Fprintf(&s, "TARGET: %s\nUNIT: %s\nHORIZON: %s\n", strings.TrimSpace(in.Raw.Target), strings.TrimSpace(in.Raw.Unit), strings.TrimSpace(in.Raw.Horizon))
	if c := strings.TrimSpace(in.Raw.ConditioningEvent); c != "" {
		fmt.Fprintf(&s, "CONDITIONED ON: %s\n", c)
	}
	s.WriteString("\nPurpose of the forecast:\n")
	s.WriteString(in.Raw.Purpose)
	s.WriteString("\n\nOutput ONLY a SINGLE JSON object — no prose before or after it, and no markdown code ")
	s.WriteString("fences. It MUST have EXACTLY these fields:\n")
	s.WriteString("- \"reading\": string — what this pooled estimate means, in plain language.\n")
	s.WriteString("- \"outlierNotes\": array of strings — one entry per identified outlier: what it is ")
	s.WriteString("claiming and what would have to be true for it to be right.\n")
	s.WriteString("- \"keyDrivers\": array of strings — the assumptions the estimate is most sensitive to.\n")
	s.WriteString("- \"cautions\": array of strings — what a reader should be careful about.\n\n")
	s.WriteString("host-pooled forecast:\n")
	s.Write(b)
	s.WriteString("\n")
	return s.String(), nil
}

// ParseNarrative converts the collator's output into narrative, as compareCollator.ParseNarrative does.
func (forecastCollator) ParseNarrative(raw []byte, by schema.ExplorerIdentity) ([]govern.Narrative, error) {
	return fixedSpaceNarrative(raw, by, func(obj []byte) ([]string, error) {
		var w forecastNarrativeWire
		if err := json.Unmarshal(obj, &w); err != nil {
			return nil, err
		}
		out := make([]string, 0, 4)
		if strings.TrimSpace(w.Reading) != "" {
			out = append(out, w.Reading)
		}
		for _, n := range w.OutlierNotes {
			if strings.TrimSpace(n) != "" {
				out = append(out, "outlier: "+n)
			}
		}
		for _, d := range w.KeyDrivers {
			if strings.TrimSpace(d) != "" {
				out = append(out, "driver: "+d)
			}
		}
		for _, c := range w.Cautions {
			if strings.TrimSpace(c) != "" {
				out = append(out, "caution: "+c)
			}
		}
		return out, nil
	})
}

// Collate builds the ForecastOutput from the pooled result and narrative.
func (forecastCollator) Collate(in FixedSpaceInput, view FixedSpaceView, narrative []govern.Narrative) (ModeOutput, error) {
	fp, ok := view.Value.(forecastPooled)
	if !ok {
		return nil, fmt.Errorf("forecast: host view is %T, not the pooled forecast", view.Value)
	}
	out := ForecastOutput{
		Target: strings.TrimSpace(in.Raw.Target), Unit: strings.TrimSpace(in.Raw.Unit),
		Horizon: strings.TrimSpace(in.Raw.Horizon), ConditioningEvent: strings.TrimSpace(in.Raw.ConditioningEvent),
		Space: string(schema.FixedSpace),
		SpaceNote: fmt.Sprintf(
			"FIXED-SPACE forecast: the target, unit and horizon were DECLARED by the requester before any explorer was asked, so every estimate answers the same question in the same unit. No entity resolution was performed, no canonicalization ran, and there is no partition to pin. The aggregate is HOST-pooled by %s under %s — no model computed it.",
			ForecastRule, pool.RulesVersion),
		PartitionRevisionHash: govern.FixedSpaceNoPartition,
		Aggregate:             fp.Pooled.Aggregate,
		Interval:              ForecastInterval{Low: fp.Pooled.IntervalLow, High: fp.Pooled.IntervalHigh, Sources: fp.Pooled.IntervalSources},
		Dispersion:            fp.Pooled.Dispersion,
		Method: ForecastMethod{
			Rule: string(fp.Pooled.Rule), RulesVersion: fp.Pooled.RulesVersion,
			OutlierRule: fp.Pooled.OutlierRuleVersion, IntervalRule: fp.Pooled.IntervalRuleVersion,
			GovernanceRules: govern.RulesVersion,
			ComputedBy:      "the HOST, from the recorded blind round-1 estimates — the collator was shown this value already final and contributed narrative only",
		},
		IndividualEstimates: fp.Pooled.Estimates,
		Outliers:            fp.Pooled.Outliers,
		Assumptions:         fp.Assumptions,
		Rejected:            fp.Rejected,
		Claim:               fp.Claim,
		PanelSize:           in.Panel.Selected,
		Respondents:         in.Panel.Respondents(),
		Abstentions:         in.Panel.Outcome.DeliberateAbstention,
		CollatorNarrative:   append([]govern.Narrative(nil), narrative...),
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

func init() {
	register(ModeSpec{
		Name:               Forecast,
		FormulationFree:    true,
		Prompt:             schema.ForecastExplorerPrompt,
		ExplorerSchema:     schema.ForecastExplorerSchema,
		Objective:          ObjectiveHostAggregateCollate,
		FixedSpace:         forecastCollator{},
		ValidateTask:       schema.ValidateForecastTask,
		Class:              schema.FixedSpace,
		FixedSpaceKeyField: "estimate",
	})
}
