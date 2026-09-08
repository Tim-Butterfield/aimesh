package mode

// This file is the FORECAST mode (design §3 Forecast row) — the other FIXED-SPACE mode, and the one
// the design calls "genuinely deterministic":
//
//	declare          the USER fixes the target, the unit and the horizon (and any conditioning event)
//	round 1 (BLIND)  each explorer returns a NUMERIC estimate + interval + reasoning
//	HOST             internal/pool computes the aggregate, the interval, the dispersion and the outliers
//	collate          the collator writes NARRATIVE about numbers it cannot change
//
// ONE round, no canonicalizer, no confirmation round — for the same reason as Compare: every explorer was
// asked the same question in the same unit, so their answers are directly comparable quantities and there
// is no entity resolution anywhere in the pipeline.
//
// Two refusals define the mode:
//
//   - A NON-NUMERIC ESTIMATE IS REJECTED, not interpreted. The explorer schema types `estimate`, `low` and
//     `high` as numbers, so a narrative answer fails validation at the envelope boundary and is recorded as
//     a dropped response with a reason. schema.ParseForecast refuses it a second time. A host that read a
//     number out of "somewhere in the low hundreds" would be inventing the precision the mode exists to
//     provide.
//   - THE COLLATOR NEVER COMPUTES THE AGGREGATE. Pooling happens in internal/pool under a declared,
//     versioned rule before the collator is prompted. The failure mode this replaces is specific and
//     familiar: a model asked to "combine these forecasts" splits the difference in prose, and the number
//     that comes out is neither anybody's estimate nor any stated rule's output.
//
// And one thing it is careful to KEEP: an outlier is identified, never deleted, and its own reasoning
// travels with it. A forecaster far from the panel is either wrong or the only one who noticed something,
// and the aggregate alone cannot tell you which — so the record carries both.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pool"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// ForecastRule is the DECLARED aggregation rule this mode contract pins. It is a mode-contract constant
// rather than a runtime option precisely so it cannot be chosen after the estimates are in (§4's freeze
// discipline, applied to the one number this mode produces).
const ForecastRule = pool.RuleMedian

// --- the terminal output (design §3 Forecast row) ---

// ForecastOutput is the FIXED, exploremesh-owned terminal output of the Forecast mode: the declared target,
// the HOST-pooled aggregate + interval + dispersion, every attributed individual estimate, the identified
// outliers WITH their rationale, the assumptions the panel stated, and the pinned governance claim.
type ForecastOutput struct {
	Target            string `json:"target"`
	Unit              string `json:"unit"`
	Horizon           string `json:"horizon"`
	ConditioningEvent string `json:"conditioningEvent,omitempty"`
	// Space + SpaceNote are the honest rendering of what this result IS: a fixed-space pooled estimate with
	// no entity resolution behind it.
	Space     string `json:"space"`
	SpaceNote string `json:"spaceNote"`
	// PartitionRevisionHash carries govern.FixedSpaceNoPartition (see CompareOutput's field of the same name).
	PartitionRevisionHash string `json:"partitionRevisionHash"`
	// Aggregate + Interval are the HOST-pooled values under Method below. The collator never touched them.
	Aggregate float64          `json:"aggregate"`
	Interval  ForecastInterval `json:"interval"`
	// Dispersion is the full spread picture (min/max/quartiles/IQR/stddev/MAD) — reported alongside the
	// aggregate rather than behind it, because a tight and a wildly split panel must never look alike.
	Dispersion pool.Dispersion `json:"dispersion"`
	Method     ForecastMethod  `json:"method"`
	// IndividualEstimates are every pooled estimate, attributed. The aggregate is a view over them.
	IndividualEstimates []pool.Estimate `json:"individualEstimates"`
	// Outliers are the estimates the declared outlier rule flagged, each carrying its own forecaster's
	// reasoning (see the file comment). They are NOT removed from the aggregate.
	Outliers []pool.Outlier `json:"outliers,omitempty"`
	// Assumptions are the panel's stated assumptions, attributed — MODEL PROSE, kept because a pooled
	// number whose assumptions are invisible is a number nobody can audit.
	Assumptions []AttributedAssumption `json:"assumptions,omitempty"`
	// Rejected records responses that could not contribute an estimate, with the host's reason. A rejected
	// estimate is recorded, never silently absent (§4's minority carry-through applied to a pool).
	Rejected []RejectedEstimate `json:"rejected,omitempty"`
	// Claim is the pinned governance claim: how many DISTINCT blind round-1 forecasters this aggregate is
	// built on, with BOTH denominators, the frozen policy, and the exact contributing envelope refs.
	Claim govern.Claim `json:"claim"`
	// PanelSize / Respondents / Abstentions are the participation facts behind the denominators.
	PanelSize   int `json:"panelSize"`
	Respondents int `json:"respondents"`
	Abstentions int `json:"abstentions"`
	// CollatorNarrative is the quarantined MODEL PROSE namespace (§0 F-C).
	CollatorNarrative []govern.Narrative `json:"collatorNarrative,omitempty"`
}

// ForecastInterval is the pooled interval + the number of forecasters that actually stated one.
type ForecastInterval struct {
	Low     float64 `json:"low"`
	High    float64 `json:"high"`
	Sources int     `json:"sources"`
}

// ForecastMethod names the versioned HOST rules the pooled values were computed under (§9).
type ForecastMethod struct {
	Rule            string `json:"rule"`
	RulesVersion    string `json:"rulesVersion"`
	OutlierRule     string `json:"outlierRule"`
	IntervalRule    string `json:"intervalRule"`
	GovernanceRules string `json:"governanceRulesVersion"`
	// ComputedBy states plainly WHO produced the aggregate. It is a persisted field rather than a comment
	// because that is the single most important fact about this number.
	ComputedBy string `json:"computedBy"`
}

// AttributedAssumption is one assumption a forecaster stated, with its source.
type AttributedAssumption struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Assumption  string                  `json:"assumption"`
}

// RejectedEstimate is one response that could not enter the pool, with the HOST's reason.
type RejectedEstimate struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Reason      string                  `json:"reason"`
}

// Summary returns the one-line human summary: the pooled value with its rule, the dispersion, the
// denominators and the outlier count. It never calls the aggregate a prediction or a consensus.
func (o ForecastOutput) Summary() string {
	return fmt.Sprintf("forecast: %s = %g %s over %s — HOST-pooled by %s over %d estimate(s) (%s / %s); interval %g..%g, spread %g (min %g, max %g, IQR %g); %d outlier(s) identified and CARRIED with their reasoning [%s; fixed space: no partition]",
		o.Target, o.Aggregate, o.Unit, o.Horizon, o.Method.Rule, len(o.IndividualEstimates),
		o.Claim.KOfPanel, o.Claim.KOfRespondents, o.Interval.Low, o.Interval.High,
		o.Dispersion.Range, o.Dispersion.Min, o.Dispersion.Max, o.Dispersion.IQR,
		len(o.Outliers), o.Method.RulesVersion)
}

// Validate checks the forecast is usable: at least one pooled estimate, a declared target, and the honest
// space rendering. A forecast with no estimates is not a forecast — the aggregation refuses earlier, and
// this is the guard that keeps a zero from ever reaching a reader as a number.
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

// --- the fixed-space contract ---

// forecastPooled is the mode's host view: the pooled result plus the attributed extras the pool itself has
// no business carrying (the panel's assumptions, and the responses that could not contribute).
type forecastPooled struct {
	Pooled      pool.Pooled
	Assumptions []AttributedAssumption
	Rejected    []RejectedEstimate
	Claim       govern.Claim
}

// forecastCollator is the Forecast mode's FixedSpaceContract.
type forecastCollator struct{}

// Aggregate is the HOST step: lift each blind round-1 response into a typed estimate, REFUSE the ones that
// are not numeric (recording them rather than dropping them), pool the rest under the declared rule, and
// pin the whole thing to a governance claim. The collator has not been called at this point and every
// number in the result already exists.
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
			// Recorded, not silently absent: a response that could not be pooled is evidence about the run.
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

// forecastNarrativeWire is the SHAPE the collator is asked for: prose only. As with Compare there is no
// numeric field, so there is nowhere for a "corrected" aggregate to go.
type forecastNarrativeWire struct {
	Reading      string   `json:"reading"`
	OutlierNotes []string `json:"outlierNotes"`
	KeyDrivers   []string `json:"keyDrivers"`
	Cautions     []string `json:"cautions"`
}

// CollatorPrompt shows the collator the finished pooled result and asks for narrative. The instruction is
// blunt about the two failure modes this mode exists to prevent: recomputing the aggregate, and dismissing
// the outlier.
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

// ParseNarrative reads the collator's output into the quarantined narrative namespace (see Compare's
// ParseNarrative: an unusable narrative degrades the prose, never the host-computed result).
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

// Collate assembles the ForecastOutput as a deterministic VIEW over the pooled result. It computes nothing.
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
	// Forecast (design §3 Forecast row). Formulation-free, ONE round, no canonicalization policy at all
	// (see the file comment). FIXED-space class with the ESTIMATE as the degraded register's key field: the
	// estimate is a value in a declared unit, so a register of "who estimated what" is a genuine comparison
	// rather than covert entity resolution (§1).
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
