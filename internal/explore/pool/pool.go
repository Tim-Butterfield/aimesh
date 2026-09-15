// Package pool combines a panel's numeric estimates into an aggregate, an interval, dispersion statistics
// and a set of outliers, using a declared, versioned rule. It is pure arithmetic over recorded estimates.
//
//   - The rule is an argument and is recorded with the result.
//   - Outliers are reported with their estimates and reasoning, and are still included in the aggregate.
//   - The collator only sees the finished result.
package pool

import (
	"fmt"
	"math"
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// RulesVersion identifies the pooling rules in this package. Every pooled result records it.
const RulesVersion = "host-forecast-pooling@v1"

// OutlierRuleVersion identifies the outlier rule: a modified z-score over the median absolute deviation
// (Iglewicz and Hoaglin) above 3.5. An interquartile fence flags nothing on panels this small.
const OutlierRuleVersion = "modified-z-score-over-mad@v1"

// IntervalRuleVersion identifies the interval rule: the medians of the stated lows and highs, rather than
// the widest bounds any single forecaster gave.
const IntervalRuleVersion = "median-of-stated-bounds@v1"

// outlierThreshold is the modified z-score above which an estimate is flagged.
const outlierThreshold = 3.5

// Rule is an aggregation rule, fixed by the mode contract.
type Rule string

// Aggregation rules.
const (
	// RuleMedian aggregates with the median, which tolerates an extreme estimate without discarding it.
	RuleMedian Rule = "median"
	// RuleTrimmedMean aggregates with the mean after trimming trimFraction from each end, or the plain mean
	// when the trim would remove nothing or everything.
	RuleTrimmedMean Rule = "trimmed_mean"
)

// trimFraction is the fraction RuleTrimmedMean trims from each end.
const trimFraction = 0.25

// Valid reports whether r is a known rule.
func (r Rule) Valid() bool { return r == RuleMedian || r == RuleTrimmedMean }

// Estimate is one explorer's numeric estimate and the envelope it came from. Assumptions and Reasoning are
// carried with the estimate but never used in the arithmetic.
type Estimate struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Value       float64                 `json:"value"`
	Low         float64                 `json:"low,omitempty"`
	High        float64                 `json:"high,omitempty"`
	HasInterval bool                    `json:"hasInterval"`
	Confidence  float64                 `json:"confidence,omitempty"`
	Assumptions []string                `json:"assumptions,omitempty"`
	Reasoning   string                  `json:"reasoning,omitempty"`
}

// Dispersion describes the spread of the pooled estimates.
type Dispersion struct {
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Q1     float64 `json:"q1"`
	Median float64 `json:"median"`
	Q3     float64 `json:"q3"`
	IQR    float64 `json:"iqr"`
	StdDev float64 `json:"stdDev"`
	MAD    float64 `json:"mad"`
	// Range is Max minus Min.
	Range float64 `json:"range"`
}

// Outlier is an estimate flagged by the outlier rule, with its score and the host's reason.
type Outlier struct {
	Estimate Estimate `json:"estimate"`
	// ModifiedZ is the estimate's modified z-score under OutlierRuleVersion.
	ModifiedZ float64 `json:"modifiedZ"`
	// Reason states the arithmetic that flagged the estimate; it does not judge the estimate wrong.
	Reason string `json:"reason"`
}

// Pooled is a pooling result: the aggregate, interval, dispersion, outliers and every estimate, with the
// rule versions used.
type Pooled struct {
	Rule                Rule    `json:"rule"`
	RulesVersion        string  `json:"rulesVersion"`
	OutlierRuleVersion  string  `json:"outlierRuleVersion"`
	IntervalRuleVersion string  `json:"intervalRuleVersion"`
	Aggregate           float64 `json:"aggregate"`
	IntervalLow         float64 `json:"intervalLow"`
	IntervalHigh        float64 `json:"intervalHigh"`
	// IntervalSources is the number of estimates that stated an interval.
	IntervalSources int        `json:"intervalSources"`
	Dispersion      Dispersion `json:"dispersion"`
	Outliers        []Outlier  `json:"outliers,omitempty"`
	Estimates       []Estimate `json:"estimates"`
}

// Pool pools estimates under rule. It returns an error for an empty panel rather than a zero aggregate.
// The returned Estimates keep their input order.
func Pool(estimates []Estimate, rule Rule) (Pooled, error) {
	if len(estimates) == 0 {
		return Pooled{}, fmt.Errorf("pool: no estimates to pool — an aggregate over an empty panel is not a forecast")
	}
	if !rule.Valid() {
		return Pooled{}, fmt.Errorf("pool: unknown aggregation rule %q (want %q or %q)", rule, RuleMedian, RuleTrimmedMean)
	}
	values := make([]float64, len(estimates))
	for i, e := range estimates {
		values[i] = e.Value
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	disp := dispersion(sorted)
	out := Pooled{
		Rule: rule, RulesVersion: RulesVersion,
		OutlierRuleVersion: OutlierRuleVersion, IntervalRuleVersion: IntervalRuleVersion,
		Dispersion: disp,
		Estimates:  append([]Estimate(nil), estimates...),
	}
	switch rule {
	case RuleTrimmedMean:
		out.Aggregate = trimmedMean(sorted, trimFraction)
	default:
		out.Aggregate = disp.Median
	}
	out.IntervalLow, out.IntervalHigh, out.IntervalSources = pooledInterval(estimates, out.Aggregate)
	out.Outliers = outliers(estimates, disp)
	return out, nil
}

// dispersion computes spread statistics over sorted values. Quartiles use the inclusive-median method: for
// an odd count, both halves include the median.
func dispersion(sorted []float64) Dispersion {
	n := len(sorted)
	med := median(sorted)
	mid := (n + 1) / 2
	d := Dispersion{
		N: n, Min: sorted[0], Max: sorted[n-1], Median: med,
		Q1: median(sorted[:mid]), Q3: median(sorted[n-mid:]),
	}
	d.IQR = d.Q3 - d.Q1
	d.Range = d.Max - d.Min
	d.StdDev = stdDev(sorted)
	d.MAD = mad(sorted, med)
	return d
}

// median returns the median of a sorted slice, averaging the two middle values for an even count.
func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// trimmedMean drops floor(frac·n) sorted values from each end and averages the rest, falling back to the
// plain mean when that would remove every value.
func trimmedMean(sorted []float64, frac float64) float64 {
	n := len(sorted)
	k := int(math.Floor(float64(n) * frac))
	if k*2 >= n {
		k = 0
	}
	rest := sorted[k : n-k]
	sum := 0.0
	for _, v := range rest {
		sum += v
	}
	return sum / float64(len(rest))
}

// stdDev returns the population standard deviation of values; the panel is the whole population.
func stdDev(values []float64) float64 {
	n := float64(len(values))
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= n
	sum := 0.0
	for _, v := range values {
		sum += (v - mean) * (v - mean)
	}
	return math.Sqrt(sum / n)
}

// mad returns the median absolute deviation from med.
func mad(sorted []float64, med float64) float64 {
	devs := make([]float64, len(sorted))
	for i, v := range sorted {
		devs[i] = math.Abs(v - med)
	}
	sort.Float64s(devs)
	return median(devs)
}

// meanAbsDev returns the mean absolute deviation from med, the fallback scale when the median absolute
// deviation is zero.
func meanAbsDev(values []float64, med float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += math.Abs(v - med)
	}
	return sum / float64(len(values))
}

// outliers returns the estimates flagged by the outlier rule, in input order. Fewer than three estimates
// have no majority to deviate from, so none are flagged.
func outliers(estimates []Estimate, d Dispersion) []Outlier {
	if len(estimates) < 3 {
		return nil
	}
	values := make([]float64, len(estimates))
	for i, e := range estimates {
		values[i] = e.Value
	}
	scale, scaleName := d.MAD*1.4826, "MAD"
	if d.MAD == 0 {
		scale, scaleName = meanAbsDev(values, d.Median)*1.2533, "mean absolute deviation (MAD was 0)"
	}
	if scale == 0 {
		return nil // all estimates are identical
	}
	var out []Outlier
	for _, e := range estimates {
		z := math.Abs(e.Value-d.Median) / scale
		if z <= outlierThreshold {
			continue
		}
		out = append(out, Outlier{
			Estimate: e, ModifiedZ: z,
			Reason: fmt.Sprintf("estimate %g is %.1f robust standard deviations from the panel median %g (rule %s, threshold %.1f, scale from the %s) — it is IDENTIFIED, not discarded: the aggregate is computed over every estimate, and this forecaster's reasoning is carried with it",
				e.Value, z, d.Median, OutlierRuleVersion, outlierThreshold, scaleName),
		})
	}
	return out
}

// pooledInterval returns the medians of the stated lows and highs and the number of estimates that stated
// an interval. With none, both bounds equal aggregate and the count is 0.
func pooledInterval(estimates []Estimate, aggregate float64) (low, high float64, sources int) {
	var lows, highs []float64
	for _, e := range estimates {
		if !e.HasInterval {
			continue
		}
		lows = append(lows, e.Low)
		highs = append(highs, e.High)
	}
	if len(lows) == 0 {
		return aggregate, aggregate, 0
	}
	sort.Float64s(lows)
	sort.Float64s(highs)
	return median(lows), median(highs), len(lows)
}
