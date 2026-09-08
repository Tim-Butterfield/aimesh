package pool

import (
	"math"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// est builds one attributed estimate.
func est(tag string, value, low, high float64, reasoning string) Estimate {
	return Estimate{
		Explorer:    schema.ExplorerIdentity{Adapter: "fake-" + tag, Model: "m-" + tag, Effort: "high"},
		EnvelopeRef: "envelope#" + tag,
		Value:       value, Low: low, High: high, HasInterval: true,
		Reasoning: reasoning,
	}
}

// TestPool_MedianRuleIsTheDeclaredAggregate pins the default rule and the interval rule together: the
// aggregate is the median of the estimates and the interval is the median of the STATED bounds — never the
// min/max envelope, which would report the widest single forecaster's interval as the panel's.
func TestPool_MedianRuleIsTheDeclaredAggregate(t *testing.T) {
	p, err := Pool([]Estimate{
		est("A", 100, 90, 120, "trend"),
		est("B", 110, 95, 130, "trend + seasonality"),
		est("C", 300, 250, 400, "a step change nobody else priced in"),
	}, RuleMedian)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if p.Aggregate != 110 {
		t.Errorf("aggregate = %v, want the median 110", p.Aggregate)
	}
	if p.IntervalLow != 95 || p.IntervalHigh != 130 || p.IntervalSources != 3 {
		t.Errorf("interval = %v..%v over %d source(s), want 95..130 over 3", p.IntervalLow, p.IntervalHigh, p.IntervalSources)
	}
	if p.Rule != RuleMedian || p.RulesVersion != RulesVersion || p.OutlierRuleVersion != OutlierRuleVersion {
		t.Errorf("the rule + versions must travel with the value: %+v", p)
	}
	if p.Dispersion.N != 3 || p.Dispersion.Min != 100 || p.Dispersion.Max != 300 || p.Dispersion.Range != 200 {
		t.Errorf("dispersion = %+v", p.Dispersion)
	}
	if p.Dispersion.MAD != 10 {
		t.Errorf("MAD = %v, want 10 (the median of |100-110|, 0, |300-110|)", p.Dispersion.MAD)
	}
}

// TestPool_OutlierIsIdentifiedNotDiscarded is the mode's substantive commitment: a far estimate is FLAGGED,
// keeps its own reasoning, stays attributed — and is still inside the set the aggregate was computed over.
// Trimming it would delete the one forecaster who might know something.
func TestPool_OutlierIsIdentifiedNotDiscarded(t *testing.T) {
	estimates := []Estimate{
		est("A", 100, 90, 120, "trend"),
		est("B", 110, 95, 130, "trend + seasonality"),
		est("C", 300, 250, 400, "a step change nobody else priced in"),
	}
	p, err := Pool(estimates, RuleMedian)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if len(p.Outliers) != 1 || p.Outliers[0].Estimate.Value != 300 {
		t.Fatalf("expected the 300 estimate to be flagged, got %+v", p.Outliers)
	}
	o := p.Outliers[0]
	if o.Estimate.Reasoning != "a step change nobody else priced in" {
		t.Errorf("the outlier's OWN reasoning must be preserved: %q", o.Estimate.Reasoning)
	}
	if o.Estimate.Explorer.Adapter == "" || o.Estimate.EnvelopeRef == "" {
		t.Error("the outlier must stay attributed to its forecaster and envelope")
	}
	if o.ModifiedZ <= outlierThreshold {
		t.Errorf("modified z = %v, must exceed the threshold %v to be flagged", o.ModifiedZ, outlierThreshold)
	}
	if len(p.Estimates) != 3 {
		t.Errorf("the outlier must remain among the pooled estimates, got %d", len(p.Estimates))
	}
	// The aggregate is unchanged by the outlier's presence in the SET — that is what makes reporting it safe.
	if p.Aggregate != 110 {
		t.Errorf("aggregate = %v, want 110 (the median is robust; the outlier is reported, not removed)", p.Aggregate)
	}
}

// TestPool_OutlierRuleFallsBackWhenMADIsZero pins the small-panel case the MAD rule alone gets wrong: when a
// majority gave the SAME number the median absolute deviation is 0, and dividing by it would flag every
// differing estimate by construction. The declared fallback scale keeps the rule usable.
func TestPool_OutlierRuleFallsBackWhenMADIsZero(t *testing.T) {
	p, err := Pool([]Estimate{
		est("A", 5, 4, 6, ""), est("B", 5, 4, 6, ""), est("C", 5, 4, 6, ""),
		est("D", 5, 4, 6, ""), est("E", 80, 70, 90, "a different regime"),
	}, RuleMedian)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if p.Dispersion.MAD != 0 {
		t.Fatalf("this fixture needs MAD == 0, got %v", p.Dispersion.MAD)
	}
	if len(p.Outliers) != 1 || p.Outliers[0].Estimate.Value != 80 {
		t.Fatalf("expected exactly the 80 estimate to be flagged, got %+v", p.Outliers)
	}
	// Identical estimates have nothing to be far FROM, so nothing is flagged.
	same, _ := Pool([]Estimate{est("A", 7, 7, 7, ""), est("B", 7, 7, 7, ""), est("C", 7, 7, 7, "")}, RuleMedian)
	if len(same.Outliers) != 0 {
		t.Errorf("a unanimous panel has no outlier, got %+v", same.Outliers)
	}
}

// TestPool_TwoEstimatesFlagNeither pins a deliberate abstention from judgment: with two estimates there is
// no majority to be far from, so flagging one would be the host picking a side. The dispersion still shows
// the gap.
func TestPool_TwoEstimatesFlagNeither(t *testing.T) {
	p, err := Pool([]Estimate{est("A", 10, 9, 11, ""), est("B", 1000, 900, 1100, "")}, RuleMedian)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if len(p.Outliers) != 0 {
		t.Errorf("two estimates must flag neither as the outlier, got %+v", p.Outliers)
	}
	if p.Dispersion.Range != 990 {
		t.Errorf("the gap must still be reported: %+v", p.Dispersion)
	}
}

// TestPool_TrimmedMeanIsTheDeclaredAlternative pins the second declared rule (and its documented degradation
// on a panel too small to trim).
func TestPool_TrimmedMeanIsTheDeclaredAlternative(t *testing.T) {
	estimates := []Estimate{
		est("A", 10, 0, 0, ""), est("B", 20, 0, 0, ""), est("C", 30, 0, 0, ""),
		est("D", 40, 0, 0, ""), est("E", 1000, 0, 0, ""),
	}
	p, err := Pool(estimates, RuleTrimmedMean)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	// 25% of 5 trims one from each end: (20+30+40)/3.
	if math.Abs(p.Aggregate-30) > 1e-9 {
		t.Errorf("trimmed mean = %v, want 30", p.Aggregate)
	}
	if p.Rule != RuleTrimmedMean {
		t.Errorf("the rule must travel with the value, got %q", p.Rule)
	}
	// Too small to trim: it degrades to the plain mean rather than to an undefined value.
	small, _ := Pool([]Estimate{est("A", 10, 0, 0, ""), est("B", 20, 0, 0, "")}, RuleTrimmedMean)
	if small.Aggregate != 15 {
		t.Errorf("a panel too small to trim degrades to the mean, got %v", small.Aggregate)
	}
}

// TestPool_RefusesAnEmptyPanelAndAnUnknownRule pins the two refusals: an aggregate over nothing is not a
// forecast, and an undeclared rule is not a rule.
func TestPool_RefusesAnEmptyPanelAndAnUnknownRule(t *testing.T) {
	if _, err := Pool(nil, RuleMedian); err == nil {
		t.Error("an empty panel must be refused, never pooled to zero")
	}
	if _, err := Pool([]Estimate{est("A", 1, 0, 0, "")}, Rule("mean-of-the-two-i-like")); err == nil {
		t.Error("an undeclared aggregation rule must be refused")
	}
}

// TestPool_NoStatedIntervalCollapsesHonestly pins that a panel which stated no bounds reports no interval,
// rather than a fabricated spread around the aggregate.
func TestPool_NoStatedIntervalCollapsesHonestly(t *testing.T) {
	a, b := est("A", 10, 0, 0, ""), est("B", 20, 0, 0, "")
	a.HasInterval, b.HasInterval = false, false
	p, err := Pool([]Estimate{a, b}, RuleMedian)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if p.IntervalSources != 0 || p.IntervalLow != p.Aggregate || p.IntervalHigh != p.Aggregate {
		t.Errorf("an unstated interval must collapse to the aggregate with 0 sources, got %v..%v over %d",
			p.IntervalLow, p.IntervalHigh, p.IntervalSources)
	}
}
