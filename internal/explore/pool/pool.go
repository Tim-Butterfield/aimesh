// Package pool is exploremesh's HOST-SIDE ESTIMATE POOLING (design §3 Forecast row, §4): the
// declared, versioned rule that turns a panel of independent numeric estimates into one aggregate, an
// interval, a dispersion picture and an identified outlier set. Nothing here calls a model and nothing
// here reads prose — it is pure arithmetic over recorded, attributed estimates, which is exactly what §0
// F-C requires of a machine governance value.
//
// Three properties are deliberate:
//
//   - THE RULE IS DECLARED AND VERSIONED, NOT CHOSEN AFTER THE FACT. Pool takes the rule as an argument and
//     stamps RulesVersion + the rule name onto its result, so a pooled number read later is interpretable
//     under the rule that produced it. A "pick whichever aggregate looks reasonable" step is precisely how
//     a host quietly authors the answer.
//   - AN OUTLIER IS IDENTIFIED, NEVER DELETED. Outliers are flagged and returned WITH the estimate they
//     came from — reasoning and all — and the aggregate is still computed over EVERY estimate under the
//     declared rule. A forecaster who is far from the panel is sometimes the only one who knows something,
//     so the honest move is to surface them, not to trim them out of the record.
//   - THE COLLATOR NEVER COMPUTES ANY OF THIS. The pooled value exists before the terminal collation is
//     even prompted; the collator is shown it as a finished number and may only write prose about it.
package pool

import (
	"fmt"
	"math"
	"sort"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// RulesVersion is the version of the HOST pooling rules implemented here (aggregation, interval,
// dispersion, outlier identification). Every pooled result pins it (§4/§9).
const RulesVersion = "host-forecast-pooling@v1"

// OutlierRuleVersion is the versioned outlier rule: the MODIFIED Z-SCORE over the median absolute
// deviation (Iglewicz–Hoaglin), threshold 3.5. It is used in preference to a 1.5×IQR fence because an
// exploremesh panel is small — with three or five estimates a quartile fence flags nothing, which would
// make "no outliers" a statement about the sample size rather than about the panel.
const OutlierRuleVersion = "modified-z-score-over-mad@v1"

// IntervalRuleVersion is the versioned rule for the pooled INTERVAL: the median of the panel's stated lows
// and the median of its stated highs. It is deliberately not the min/max envelope — the widest bound any
// single forecaster stated is that forecaster's interval, not the panel's.
const IntervalRuleVersion = "median-of-stated-bounds@v1"

// outlierThreshold is the modified z-score above which an estimate is flagged (the standard 3.5).
const outlierThreshold = 3.5

// Rule is a DECLARED aggregation rule. It is frozen into the mode contract, not chosen at runtime.
type Rule string

const (
	// RuleMedian is the default: the median of the recorded estimates. Robust to a single extreme estimate
	// WITHOUT discarding it — which is why the outlier can be reported rather than removed.
	RuleMedian Rule = "median"
	// RuleTrimmedMean is the declared alternative: the mean after trimming trimFraction of the estimates
	// from each end (nothing is trimmed when that would round to zero, so a small panel degrades to the
	// plain mean rather than to an undefined value).
	RuleTrimmedMean Rule = "trimmed_mean"
)

// trimFraction is the symmetric trim of RuleTrimmedMean (25% from each end).
const trimFraction = 0.25

// Valid reports whether the rule is one this package implements.
func (r Rule) Valid() bool { return r == RuleMedian || r == RuleTrimmedMean }

// Estimate is ONE explorer's recorded numeric estimate, attributed to the blind round-1 envelope it came
// from. Reasoning and Assumptions are the forecaster's own words: they carry no governance value (they are
// never arithmetic input) but they travel with the estimate, because an outlier without its reasoning is
// just a number somebody would like to ignore.
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

// Dispersion is the HOST's spread picture over the pooled estimates. Every field is reported: a reader who
// wants a different spread measure than the one the rule uses can compute it from these without re-reading
// the estimates.
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
	// Range is Max−Min, carried explicitly because it is the number a human reads first.
	Range float64 `json:"range"`
}

// Outlier is one estimate the declared outlier rule flagged, carried WITH the estimate itself (see the
// package comment): the deviation score that flagged it, and the host's stated reason.
type Outlier struct {
	Estimate Estimate `json:"estimate"`
	// ModifiedZ is the estimate's modified z-score under OutlierRuleVersion.
	ModifiedZ float64 `json:"modifiedZ"`
	// Reason is the HOST's arithmetic reason — never a model's words, and never a judgment about whether
	// the outlier is WRONG. It says what the arithmetic found, and nothing more.
	Reason string `json:"reason"`
}

// Pooled is the terminal pooling result: the aggregate, the interval, the dispersion, the identified
// outliers, and every contributing estimate. The versions are on the value, not in a comment, so a
// persisted result is self-describing (§9).
type Pooled struct {
	Rule                Rule    `json:"rule"`
	RulesVersion        string  `json:"rulesVersion"`
	OutlierRuleVersion  string  `json:"outlierRuleVersion"`
	IntervalRuleVersion string  `json:"intervalRuleVersion"`
	Aggregate           float64 `json:"aggregate"`
	IntervalLow         float64 `json:"intervalLow"`
	IntervalHigh        float64 `json:"intervalHigh"`
	// IntervalSources is how many of the pooled estimates actually stated an interval — the interval's own
	// denominator, kept because "3 of 3 stated a bound" and "1 of 3 stated a bound" are different results.
	IntervalSources int        `json:"intervalSources"`
	Dispersion      Dispersion `json:"dispersion"`
	Outliers        []Outlier  `json:"outliers,omitempty"`
	Estimates       []Estimate `json:"estimates"`
}

// Pool computes the pooled result over the recorded estimates under the declared rule (design §3 Forecast
// row). It REFUSES an empty panel rather than returning a zero aggregate: an aggregate over no estimates is
// not a forecast, and a 0 that looks like one is worse than an error.
//
// Estimates are sorted by value for the order statistics but the returned Estimates keep the ATTRIBUTION
// ORDER they arrived in, so the record still reads as "who said what" rather than as an anonymous sample.
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

// dispersion computes the spread picture over the SORTED values. Quartiles use the inclusive-median method
// (each half INCLUDES the median for an odd count), which is the convention most readers expect and is
// stable for the small samples an exploremesh panel produces.
func dispersion(sorted []float64) Dispersion {
	n := len(sorted)
	med := median(sorted)
	mid := (n + 1) / 2 // inclusive halves: the median belongs to both for an odd n
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

// median returns the median of a SORTED slice (the mean of the two middle values for an even count — the
// standard definition, stated here because it is the one place this package averages anything).
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

// trimmedMean drops frac of the SORTED values from each end (floor) and averages the rest. When the trim
// would remove everything — or would round to zero — it degrades to the plain mean rather than to an
// undefined value, and that degradation is visible in the result's N.
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

// stdDev is the POPULATION standard deviation of the recorded estimates: the panel is the whole population
// of estimates this run produced, not a sample drawn from a larger one.
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

// mad is the median absolute deviation from the median — the scale the outlier rule is measured in.
func mad(sorted []float64, med float64) float64 {
	devs := make([]float64, len(sorted))
	for i, v := range sorted {
		devs[i] = math.Abs(v - med)
	}
	sort.Float64s(devs)
	return median(devs)
}

// meanAbsDev is the MEAN absolute deviation from the median — Iglewicz–Hoaglin's stated fallback scale for
// the case MAD == 0 (a majority of identical estimates). Without it, a panel where two of three forecasters
// gave the same number would divide by zero and flag the third by construction.
func meanAbsDev(values []float64, med float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += math.Abs(v - med)
	}
	return sum / float64(len(values))
}

// outliers identifies the estimates the declared rule flags. It returns them in the input's attribution
// order, each carrying its whole Estimate — reasoning included (see the package comment).
func outliers(estimates []Estimate, d Dispersion) []Outlier {
	if len(estimates) < 3 {
		// With two estimates there is no majority to be far FROM: either one is as much the outlier as the
		// other, and flagging one would be the host picking a side. The dispersion still reports the gap.
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
		return nil // every estimate is identical — there is nothing to be far from
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

// pooledInterval computes the pooled bounds under IntervalRuleVersion: the median of the stated lows and
// the median of the stated highs, over the estimates that actually stated an interval. When NOBODY stated
// one the interval collapses to the aggregate itself and IntervalSources is 0 — an honest "no interval was
// reported" rather than a fabricated spread.
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
