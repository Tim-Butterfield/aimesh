package pipeline

// Tests for the FIXED-SPACE modes (design §3 Compare + Forecast rows). What they pin, above the individual
// assertions, is that these two modes are governed WITHOUT the canonicalization machinery — one blind
// round, no canonicalizer call, no confirmation round, no partition — and that every number in their
// results is computed by the host before the collator is ever asked anything.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/model/fake"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// --- fixtures ---

// compareTask builds a Compare task over three options. `extra` appends further criteria (a filter gate, a
// weighted pair) so each test declares exactly the space it is about.
func compareTask(extra ...schema.CompareCriterion) schema.RawTask {
	raw := schema.RawTask{
		Purpose:  "choose a store",
		Criteria: []string{"must be operable by a small team"},
		Mode:     mode.Compare,
		Options:  []string{"alpha", "beta", "gamma"},
		CompareCriteria: []schema.CompareCriterion{
			{Name: "throughput", Direction: schema.HigherIsBetter, Role: schema.RoleDimension},
			{Name: "cost", Direction: schema.LowerIsBetter, Role: schema.RoleDimension},
		},
	}
	raw.CompareCriteria = append(raw.CompareCriteria, extra...)
	return raw
}

// forecastTask builds a Forecast task with a fully declared estimation target.
func forecastTask() schema.RawTask {
	return schema.RawTask{
		Purpose:  "size the migration",
		Criteria: []string{"account for the current trend"},
		Mode:     mode.Forecast,
		Target:   "peak requests per second",
		Unit:     "requests/second",
		Horizon:  "the next 12 months",
	}
}

// runTask runs one task through the registered mode.
func runTask(t *testing.T, reg Registry, plan roster.Plan, raw schema.RawTask) (Result, error) {
	t.Helper()
	return Run(context.Background(), reg, plan, raw, Options{}, nil)
}

// compareOutput type-asserts a run's terminal output as a CompareOutput.
func compareOutput(t *testing.T, res Result) mode.CompareOutput {
	t.Helper()
	out, ok := res.Output.(mode.CompareOutput)
	if !ok {
		t.Fatalf("expected a mode.CompareOutput, got %T", res.Output)
	}
	return out
}

// forecastOutput type-asserts a run's terminal output as a ForecastOutput.
func forecastOutput(t *testing.T, res Result) mode.ForecastOutput {
	t.Helper()
	out, ok := res.Output.(mode.ForecastOutput)
	if !ok {
		t.Fatalf("expected a mode.ForecastOutput, got %T", res.Output)
	}
	return out
}

// cellOf finds one cell of the consolidated matrix.
func cellOf(t *testing.T, out mode.CompareOutput, option, criterion string) govern.CompareCell {
	t.Helper()
	for _, c := range out.Cells {
		if c.Option == option && c.Criterion == criterion {
			return c
		}
	}
	t.Fatalf("no cell for %q / %q in %d cell(s)", option, criterion, len(out.Cells))
	return govern.CompareCell{}
}

// paretoOptions lists the frontier's options.
func paretoOptions(out mode.CompareOutput) []string {
	var o []string
	for _, e := range out.Pareto {
		o = append(o, e.Option)
	}
	return o
}

// --- Compare ---

// TestCompare_ConsolidatedMatrixSurfacesPerCellDisagreement is the headline Compare test: a declared
// option set × criteria run on fakes yields the consolidated matrix, and the ONE cell the evaluators
// disagree about is surfaced AS a disagreement — every value retained, the agreement verdict host-computed,
// and the definitive label WITHHELD. Averaging it into a single score is the failure this mode exists to
// prevent, so the test asserts the individual values are all still there.
func TestCompare_ConsolidatedMatrixSurfacesPerCellDisagreement(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	res, err := runTask(t, reg, plan, compareTask())
	if err != nil {
		t.Fatalf("compare run: %v", err)
	}
	out := compareOutput(t, res)
	if len(out.Cells) != 6 { // 3 options × 2 criteria
		t.Fatalf("expected 6 cells, got %d", len(out.Cells))
	}
	// The fake's deliberate split: evaluator B scores (alpha, throughput) four points lower than A and C.
	split := cellOf(t, out, "alpha", "throughput")
	if split.Agreement != govern.AgreementSplit {
		t.Errorf("the disagreed cell reports agreement %q, want %q", split.Agreement, govern.AgreementSplit)
	}
	if len(split.Values) != 3 {
		t.Fatalf("the disagreed cell must retain EVERY attributed value, got %d", len(split.Values))
	}
	seen := map[float64]bool{}
	for _, v := range split.Values {
		if !v.HasScore {
			t.Errorf("attributed value %+v carries no numeric score", v)
		}
		if v.Explorer.Adapter == "" {
			t.Errorf("attributed value %+v is not attributed", v)
		}
		seen[v.Score] = true
	}
	if !seen[9] || !seen[5] {
		t.Errorf("both disputed values must survive in the record, got %v", seen)
	}
	if split.Claim == nil || split.Claim.Label != govern.LabelWithheldDisagreement {
		t.Errorf("the disagreed cell's claim must WITHHOLD its label, got %+v", split.Claim)
	}
	if split.Claim.Label.Definitive() {
		t.Error("a split cell's label must not be definitive")
	}
	// k is the largest AGREEING group (A and C both said 9), reported with BOTH denominators.
	if split.Claim.Value != 2 || split.Claim.KOfPanel.M != 3 || split.Claim.KOfRespondents.M != 3 {
		t.Errorf("split cell denominators: %+v", split.Claim)
	}
	// A cell the panel agreed on is labeled corroborated — so agreement and disagreement are visibly different.
	agreed := cellOf(t, out, "beta", "throughput")
	if agreed.Agreement != govern.AgreementUnanimous || agreed.Claim.Label != govern.LabelCorroborated {
		t.Errorf("an agreed cell must be corroborated, got agreement=%q label=%q", agreed.Agreement, agreed.Claim.Label)
	}
	// The honest space rendering: no partition, stated rather than blank.
	if out.PartitionRevisionHash != govern.FixedSpaceNoPartition {
		t.Errorf("compare must carry the no-partition statement, got %q", out.PartitionRevisionHash)
	}
	if !strings.Contains(out.SpaceNote, "no entity resolution") {
		t.Errorf("space note must say no entity resolution was performed: %q", out.SpaceNote)
	}
}

// TestCompare_ParetoIsHostComputedAndHonorsDirection pins the frontier rule: dominance is computed by the
// HOST over the scored dimensions under EACH dimension's declared direction. With throughput
// higher-is-better and cost lower-is-better, beta is dominated by alpha and gamma survives on cost — an
// outcome that would be wrong in both directions if the host ignored the declaration.
func TestCompare_ParetoIsHostComputedAndHonorsDirection(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	res, err := runTask(t, reg, plan, compareTask())
	if err != nil {
		t.Fatalf("compare run: %v", err)
	}
	out := compareOutput(t, res)
	got := paretoOptions(out)
	if len(got) != 2 || got[0] != "alpha" || got[1] != "gamma" {
		t.Fatalf("Pareto frontier = %v, want [alpha gamma] (alpha wins throughput, gamma wins cost)", got)
	}
	if len(out.Dominated) != 1 || out.Dominated[0].Option != "beta" {
		t.Fatalf("dominated = %+v, want beta alone", out.Dominated)
	}
	if len(out.Dominated[0].DominatedBy) == 0 || out.Dominated[0].DominatedBy[0] != "alpha" {
		t.Errorf("beta must be recorded as dominated BY alpha, got %+v", out.Dominated[0])
	}
	// The frontier entry that rides on the split cell is flagged CONDITIONAL, naming the cell.
	if !out.Pareto[0].Conditional || len(out.Pareto[0].DisagreedCells) == 0 {
		t.Errorf("alpha's frontier position rides on a split cell and must be conditional: %+v", out.Pareto[0])
	}
	if out.Rules.ParetoRule != govern.ParetoRuleVersion {
		t.Errorf("the Pareto rule version must travel with the result, got %q", out.Rules.ParetoRule)
	}
}

// TestCompare_FilterGateExcludesAnOptionBeforeDominance pins the role separation: a `filter` criterion is a
// GATE, so an option that fails it is removed BEFORE dominance rather than traded off against the scored
// criteria. The fake fails the last declared option on every gate, unanimously.
func TestCompare_FilterGateExcludesAnOptionBeforeDominance(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	raw := compareTask(schema.CompareCriterion{Name: "on-prem support", Role: schema.RoleFilter})
	res, err := runTask(t, reg, plan, raw)
	if err != nil {
		t.Fatalf("compare run: %v", err)
	}
	out := compareOutput(t, res)
	if len(out.ExcludedByFilter) != 1 || out.ExcludedByFilter[0].Option != "gamma" {
		t.Fatalf("filter exclusions = %+v, want gamma excluded", out.ExcludedByFilter)
	}
	if out.ExcludedByFilter[0].Criterion != "on-prem support" {
		t.Errorf("the exclusion must name the gate that caused it: %+v", out.ExcludedByFilter[0])
	}
	for _, e := range out.Pareto {
		if e.Option == "gamma" {
			t.Error("an option excluded by a filter gate must not appear on the frontier")
		}
	}
	for _, d := range out.Dominated {
		if d.Option == "gamma" {
			t.Error("an option excluded by a filter gate is EXCLUDED, not dominated — the gate is not a trade-off")
		}
	}
	// The gate cell itself is still in the matrix, with its verdict and the tallies behind it.
	gate := cellOf(t, out, "gamma", "on-prem support")
	if gate.Verdict != schema.VerdictFail || gate.Fails != 3 || gate.Passes != 0 {
		t.Errorf("gate cell: %+v", gate)
	}
	if passing := cellOf(t, out, "alpha", "on-prem support"); passing.Verdict != schema.VerdictPass {
		t.Errorf("alpha must pass the gate, got %q", passing.Verdict)
	}
}

// TestCompare_NoWeightsMeansNoScalarRanking pins §3's rule: absent user weights there is NO scalar ranking,
// and the result SAYS SO rather than leaving a reader to notice the absence.
func TestCompare_NoWeightsMeansNoScalarRanking(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	res, err := runTask(t, reg, plan, compareTask())
	if err != nil {
		t.Fatalf("compare run: %v", err)
	}
	out := compareOutput(t, res)
	if out.ScalarRanking != nil {
		t.Fatalf("no weights were declared, so there must be NO scalar ranking: %+v", out.ScalarRanking)
	}
	for _, want := range []string{"NO SCALAR RANKING", "weight", "Pareto"} {
		if !strings.Contains(out.RankingWithheld, want) {
			t.Errorf("the withheld-ranking explanation must contain %q: %q", want, out.RankingWithheld)
		}
	}
	if !strings.Contains(out.Summary(), "no scalar ranking") {
		t.Errorf("the summary must state the absence: %q", out.Summary())
	}
}

// TestCompare_DeclaredWeightsLicenseAScalarRanking is the other half of the same rule: when the user DOES
// weight every scored criterion, the host computes the ranking under its versioned rule.
func TestCompare_DeclaredWeightsLicenseAScalarRanking(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	raw := compareTask()
	for i := range raw.CompareCriteria {
		raw.CompareCriteria[i].Weight = 1
	}
	res, err := runTask(t, reg, plan, raw)
	if err != nil {
		t.Fatalf("compare run: %v", err)
	}
	out := compareOutput(t, res)
	if len(out.ScalarRanking) != 3 {
		t.Fatalf("expected a ranking over 3 options, got %+v", out.ScalarRanking)
	}
	if out.RankingWithheld != "" {
		t.Errorf("a computed ranking must not also carry a withheld explanation: %q", out.RankingWithheld)
	}
	if out.ScalarRanking[0].Rank != 1 || out.Rules.ScalarRankRule != govern.ScalarRankingRuleVersion {
		t.Errorf("ranking header: %+v / rule %q", out.ScalarRanking[0], out.Rules.ScalarRankRule)
	}
}

// TestCompare_MissingEvidenceIsExplicit pins that a cell nobody could judge is REPORTED as missing, and the
// option it belongs to is carried as INCOMPARABLE rather than silently losing the comparison.
func TestCompare_MissingEvidenceIsExplicit(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.CompareSparse, fake.CompareSparse, fake.CompareSparse}, fake.Valid)
	res, err := runTask(t, reg, plan, compareTask())
	if err != nil {
		t.Fatalf("compare run: %v", err)
	}
	out := compareOutput(t, res)
	if len(out.MissingEvidence) != 2 { // gamma × 2 criteria
		t.Fatalf("expected 2 missing-evidence cells, got %+v", out.MissingEvidence)
	}
	empty := cellOf(t, out, "gamma", "cost")
	if empty.Agreement != govern.AgreementNoEvidence || empty.HasPoint {
		t.Errorf("an unevidenced cell must report no evidence and no point estimate: %+v", empty)
	}
	if empty.Claim != nil {
		t.Error("an unevidenced cell must carry NO claim — a claim about nothing would be a claim")
	}
	if len(out.Incomparable) != 1 || out.Incomparable[0].Option != "gamma" {
		t.Fatalf("incomparable = %+v, want gamma carried", out.Incomparable)
	}
	if len(out.ReportedMissingEvidence) == 0 {
		t.Error("the explorers' own missing-evidence notes must be carried")
	}
	for _, e := range out.Pareto {
		if e.Option == "gamma" {
			t.Error("an option nobody could score must not be placed on the frontier")
		}
	}
}

// --- Forecast ---

// TestForecast_HostPooledEstimateWithOutlierAndRationale is the headline Forecast test: the estimates are
// pooled by the DECLARED rule, dispersion is reported, and the far estimate is identified as an outlier
// WITH its own forecaster's reasoning intact — and still inside the aggregate's input set, because the
// outlier is flagged, never deleted.
func TestForecast_HostPooledEstimateWithOutlierAndRationale(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	res, err := runTask(t, reg, plan, forecastTask())
	if err != nil {
		t.Fatalf("forecast run: %v", err)
	}
	out := forecastOutput(t, res)
	// The fake panel estimates 100 / 110 / 300 → median 110 under the declared rule.
	if out.Aggregate != 110 {
		t.Errorf("aggregate = %v, want the median 110 of {100,110,300}", out.Aggregate)
	}
	if out.Method.Rule != string(mode.ForecastRule) || out.Method.RulesVersion == "" {
		t.Errorf("the pooling rule + version must travel with the result: %+v", out.Method)
	}
	if !strings.Contains(out.Method.ComputedBy, "HOST") {
		t.Errorf("the result must state who computed it: %q", out.Method.ComputedBy)
	}
	if out.Dispersion.N != 3 || out.Dispersion.Min != 100 || out.Dispersion.Max != 300 || out.Dispersion.Range != 200 {
		t.Errorf("dispersion = %+v", out.Dispersion)
	}
	if out.Interval.Low != 95 || out.Interval.High != 130 || out.Interval.Sources != 3 {
		t.Errorf("pooled interval = %+v, want the median of the stated bounds", out.Interval)
	}
	if len(out.Outliers) != 1 || out.Outliers[0].Estimate.Value != 300 {
		t.Fatalf("expected the 300 estimate to be identified as an outlier, got %+v", out.Outliers)
	}
	// The rationale is the whole point of surfacing an outlier rather than trimming it.
	if !strings.Contains(out.Outliers[0].Estimate.Reasoning, "step change") {
		t.Errorf("the outlier's OWN reasoning must be preserved: %q", out.Outliers[0].Estimate.Reasoning)
	}
	if out.Outliers[0].Estimate.Explorer.Adapter == "" {
		t.Error("the outlier must stay attributed")
	}
	if !strings.Contains(out.Outliers[0].Reason, "IDENTIFIED, not discarded") {
		t.Errorf("the host reason must say the outlier was not discarded: %q", out.Outliers[0].Reason)
	}
	if len(out.IndividualEstimates) != 3 {
		t.Errorf("the outlier must still be among the pooled estimates, got %d", len(out.IndividualEstimates))
	}
	if out.Claim.Query != govern.EstimateQuery || out.Claim.Value != 3 || out.Claim.Label != govern.LabelCorroborated {
		t.Errorf("pooled-estimate claim: %+v", out.Claim)
	}
	if out.Claim.PartitionRevisionHash != govern.FixedSpaceNoPartition {
		t.Errorf("a fixed-space claim must carry the no-partition statement, got %q", out.Claim.PartitionRevisionHash)
	}
	if len(out.Assumptions) != 3 {
		t.Errorf("every forecaster's assumptions must be carried, got %d", len(out.Assumptions))
	}
}

// TestForecast_NonNumericEstimateIsRejected pins the mode's first refusal: a narrative estimate fails the
// TYPED explorer schema and is dropped with a reason — never interpreted into a number, and never pooled.
func TestForecast_NonNumericEstimateIsRejected(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.ForecastNonNumeric}, fake.Valid)
	res, err := runTask(t, reg, plan, forecastTask())
	if err != nil {
		t.Fatalf("forecast run: %v", err)
	}
	if len(res.Dropped) != 1 {
		t.Fatalf("expected the non-numeric response to be dropped, got %d drop(s)", len(res.Dropped))
	}
	if !strings.Contains(res.Dropped[0].Reason, "schema-invalid") || !strings.Contains(res.Dropped[0].Reason, "estimate") {
		t.Errorf("the drop must name the offending field: %q", res.Dropped[0].Reason)
	}
	out := forecastOutput(t, res)
	if len(out.IndividualEstimates) != 2 {
		t.Fatalf("only the machine-readable estimates may be pooled, got %d", len(out.IndividualEstimates))
	}
	for _, e := range out.IndividualEstimates {
		if e.Value == 0 {
			t.Error("a rejected estimate must never enter the pool as a zero")
		}
	}
	// The drop is a technical absence, so it leaves the panel denominator alone and shrinks the respondents one.
	if out.Claim.KOfPanel.M != 3 || out.Claim.KOfRespondents.M != 2 {
		t.Errorf("denominators after a drop: %+v", out.Claim)
	}
	// schema.ParseForecast refuses it a second time, independently of the envelope-level schema check.
	if _, perr := schema.ParseForecast(map[string]any{"estimate": "about one hundred"}); perr == nil {
		t.Error("schema.ParseForecast must refuse a non-numeric estimate")
	}
}

// TestForecast_AbstentionIsReflectedInTheDenominators pins the dual denominators over a DELIBERATE
// abstention (§1): the panel denominator is untouched, the respondents denominator shrinks, and the two are
// reported side by side so "2 of 3" can never be read as "2 of 2".
func TestForecast_AbstentionIsReflectedInTheDenominators(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Abstain}, fake.Valid)
	res, err := runTask(t, reg, plan, forecastTask())
	if err != nil {
		t.Fatalf("forecast run: %v", err)
	}
	out := forecastOutput(t, res)
	if out.Abstentions != 1 || out.PanelSize != 3 || out.Respondents != 2 {
		t.Errorf("participation: panel=%d respondents=%d abstentions=%d", out.PanelSize, out.Respondents, out.Abstentions)
	}
	if out.Claim.KOfPanel.M != 3 || out.Claim.KOfRespondents.M != 2 || out.Claim.Value != 2 {
		t.Errorf("dual denominators over an abstention: %+v", out.Claim)
	}
	if len(out.IndividualEstimates) != 2 {
		t.Errorf("an abstaining forecaster contributes no estimate, got %d", len(out.IndividualEstimates))
	}
	if strings.Contains(out.Summary(), "of 2 panel") {
		t.Errorf("the summary must report BOTH denominators, not just the respondents one: %q", out.Summary())
	}
}

// --- the fixed-space governance shape ---

// TestFixedSpaceModes_RunOneBlindRoundWithNoCanonicalizerOrConfirmation is the structural claim these two
// modes rest on: Compare and Forecast are governed WITHOUT entity resolution. No canonicalize call, no
// confirm call, one round, no partition, no confirmation record — and the identity pre-flight probes ONE
// governed role (the collator) because there is no canonicalizer to probe.
func TestFixedSpaceModes_RunOneBlindRoundWithNoCanonicalizerOrConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  schema.RawTask
	}{{"compare", compareTask()}, {"forecast", forecastTask()}} {
		t.Run(tc.name, func(t *testing.T) {
			reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
			counters := map[string]*countingAdapter{}
			for name, a := range reg {
				c := counted(a)
				counters[name], reg[name] = c, c
			}
			res, err := runTask(t, reg, plan, tc.raw)
			if err != nil {
				t.Fatalf("%s run: %v", tc.name, err)
			}
			// The strongest form of the claim: the canonicalize / confirm / ballot calls were never MADE.
			for _, banned := range []string{schema.PhaseCanonicalize, schema.PhaseConfirm, schema.PhaseBallot} {
				for name, c := range counters {
					if n := c.count(banned); n > 0 {
						t.Errorf("adapter %s made %d %q call(s) — a fixed-space mode's space was declared, not resolved", name, n, banned)
					}
				}
			}
			if res.Canonicalization != nil || res.Confirmation != nil || res.Provisional != nil {
				t.Error("a fixed-space run must record no partition, no provisional revision and no confirmation")
			}
			if len(res.CanonicalizerCalls) != 0 {
				t.Errorf("a fixed-space run must make no canonicalizer call, got %d", len(res.CanonicalizerCalls))
			}
			if len(res.Rounds) != 1 || !res.Rounds[0].Blind() {
				t.Errorf("a fixed-space mode runs exactly ONE blind round, got %d", len(res.Rounds))
			}
			if len(res.Preflight) != 1 {
				t.Errorf("only the collator is a governed role here, so pre-flight probes 1 role, got %d", len(res.Preflight))
			}
			if res.Decision != nil {
				t.Error("a fixed-space mode holds no ballot")
			}
			// The claims are still recorded in the same append-only ledger every other mode uses.
			if res.Governance == nil || len(res.Governance.Claims) == 0 || res.Governance.ClaimsHash == "" {
				t.Fatal("a fixed-space run must still emit pinned governance claims")
			}
			for _, c := range res.Governance.Claims {
				if c.PartitionRevisionHash != govern.FixedSpaceNoPartition {
					t.Errorf("claim %q carries %q, want the explicit no-partition statement", c.Subject, c.PartitionRevisionHash)
				}
				if c.BaselineRoundID != "round-1" || c.PolicyHash == "" || c.FormulationHash == "" {
					t.Errorf("claim %q is not fully pinned: %+v", c.Subject, c)
				}
				if strings.Contains(c.Rendering(), "at partition none —") {
					t.Errorf("the rendering must not truncate the no-partition statement: %q", c.Rendering())
				}
			}
		})
	}
}

// TestFixedSpaceModes_CollatorNarrativeCarriesNoGovernanceValue pins §0 F-C for the fixed-space path: the
// collator's contribution lands in the quarantined narrative namespace, and the result is identical whether
// or not the collator says anything usable — because every number predates the call.
func TestFixedSpaceModes_CollatorNarrativeCarriesNoGovernanceValue(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	good, err := runTask(t, reg, plan, forecastTask())
	if err != nil {
		t.Fatalf("forecast run: %v", err)
	}
	goodOut := forecastOutput(t, good)
	if len(goodOut.CollatorNarrative) == 0 {
		t.Fatal("the collator's prose must be carried in the narrative namespace")
	}

	// The same run with a collator that returns an unusable body: the RESULT must be unchanged, and the
	// missing explanation must be visible as a host note rather than silently absent.
	reg2, plan2 := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
	reg2["collator"] = garbleCollator{inner: reg2["collator"]}
	bad, err := runTask(t, reg2, plan2, forecastTask())
	if err != nil {
		t.Fatalf("forecast run with an unusable narrative must still succeed: %v", err)
	}
	badOut := forecastOutput(t, bad)
	if badOut.Aggregate != goodOut.Aggregate || badOut.Dispersion != goodOut.Dispersion || len(badOut.Outliers) != len(goodOut.Outliers) {
		t.Error("an unusable collator narrative must not change a single host-computed value")
	}
	if len(badOut.CollatorNarrative) != 1 || !strings.Contains(badOut.CollatorNarrative[0].Prose, "[host note]") {
		t.Errorf("the missing narrative must be recorded as a host note, got %+v", badOut.CollatorNarrative)
	}
}

// garbleCollator answers the terminal narrative call with prose that is not JSON, leaving every other phase
// to the wrapped fake.
type garbleCollator struct{ inner model.Adapter }

func (g garbleCollator) Name() string                    { return g.inner.Name() }
func (g garbleCollator) Available() (bool, string)       { return g.inner.Available() }
func (g garbleCollator) Evidence() core.IdentityEvidence { return core.EvidenceInvocationTag }
func (g garbleCollator) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	r, err := g.inner.Invoke(ctx, c)
	if err == nil && c.Phase == schema.PhaseSynthesize {
		r.Stdout = []byte("I would rather explain this in prose than JSON.")
	}
	return r, err
}

// --- regression: the six modes that do not use the fixed-space contract ---

// TestEmergentSpaceModes_UnaffectedByTheFixedSpaceContract runs the six modes that do not use the
// fixed-space terminal contract and pins that each produces its OWN terminal output type under its own
// governance shape: the third terminal contract on the mode spec, and its branch in the pipeline, reach
// none of them.
func TestEmergentSpaceModes_UnaffectedByTheFixedSpaceContract(t *testing.T) {
	artifact := "A design doc with a shutdown path and no lock around it."
	for _, tc := range []struct {
		mode      string
		artifact  string
		wantType  string
		canonical bool
		rounds    int
	}{
		{mode.Map, "", "schema.CollatorOutput", false, 1},
		{mode.Synthesize, "", "schema.SynthesizeOutput", false, 1},
		{mode.Catalog, "", "schema.CatalogOutput", true, 1},
		{mode.Challenge, artifact, "mode.ChallengeOutput", true, 2},
		{mode.Shortlist, "", "mode.ShortlistOutput", true, 2},
		{mode.AICollab, "", "mode.ChallengeOutput", true, 2},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.Valid}, fake.Valid)
			raw := schema.RawTask{
				Purpose: "explore X", Criteria: []string{"c1"}, Mode: tc.mode, Artifact: tc.artifact,
			}
			res, err := runTask(t, reg, plan, raw)
			if err != nil {
				t.Fatalf("%s run: %v", tc.mode, err)
			}
			if got := fmt.Sprintf("%T", res.Output); got != tc.wantType {
				t.Errorf("%s terminal output is %s, want %s", tc.mode, got, tc.wantType)
			}
			if (res.Canonicalization != nil) != tc.canonical {
				t.Errorf("%s canonicalization present=%v, want %v", tc.mode, res.Canonicalization != nil, tc.canonical)
			}
			if len(res.Rounds) != tc.rounds {
				t.Errorf("%s ran %d round(s), want %d", tc.mode, len(res.Rounds), tc.rounds)
			}
			// The FIXED-SPACE contract must not have leaked into any of them.
			spec, _ := mode.Resolve(tc.mode)
			if spec.FixedSpace != nil {
				t.Errorf("%s must not declare a fixed-space contract", tc.mode)
			}
			if spec.ModeClass() != schema.EmergentSpace {
				t.Errorf("%s must stay emergent-space, got %q", tc.mode, spec.ModeClass())
			}
		})
	}
}

// TestModeRegistry_ExposesTheFixedSpaceModes pins that both new modes flow generically into the registry,
// so `list`, `--mode`, `_meta.mode` and a profile defaultMode pick them up without any surface change.
func TestModeRegistry_ExposesTheFixedSpaceModes(t *testing.T) {
	names := strings.Join(mode.Names(), ",")
	for _, want := range []string{"map", "synthesize", "catalog", "challenge", "shortlist", "ai-collab", "compare", "forecast"} {
		if !strings.Contains(names, want) {
			t.Errorf("mode.Names() is missing %q: %s", want, names)
		}
	}
	for _, name := range []string{mode.Compare, mode.Forecast} {
		spec, ok := mode.Resolve(name)
		if !ok {
			t.Fatalf("mode %q is not registered", name)
		}
		if spec.FixedSpace == nil || spec.Canonicalizing != nil || spec.Collator != nil {
			t.Errorf("%s must declare EXACTLY the fixed-space terminal contract", name)
		}
		if spec.Canonicalization.Dual || spec.Canonicalization.Confirm {
			t.Errorf("%s must declare no canonicalization policy at all", name)
		}
		if spec.Rounds > 1 || spec.LaterRound != nil || spec.Ballot != nil {
			t.Errorf("%s must be a single-round, ballot-free mode", name)
		}
		if spec.ModeClass() != schema.FixedSpace || spec.FixedSpaceKeyField == "" {
			t.Errorf("%s must be fixed-space with a declared key field for the degraded register", name)
		}
	}
}

// TestFixedSpaceModes_RefuseAnUnderDeclaredTaskBeforeAnySpend pins the ValidateTask seam: a comparison with
// no option set, or a forecast with no unit, is refused BEFORE the panel is touched, with a message naming
// what to supply on both surfaces.
func TestFixedSpaceModes_RefuseAnUnderDeclaredTaskBeforeAnySpend(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  schema.RawTask
		want string
	}{
		{"compare without options", schema.RawTask{Purpose: "p", Criteria: []string{"c"}, Mode: mode.Compare,
			CompareCriteria: []schema.CompareCriterion{{Name: "cost", Direction: schema.LowerIsBetter}}}, "OPTION SET"},
		{"compare without criteria", schema.RawTask{Purpose: "p", Criteria: []string{"c"}, Mode: mode.Compare,
			Options: []string{"a", "b"}}, "declared CRITERIA"},
		{"compare dimension without a direction", schema.RawTask{Purpose: "p", Criteria: []string{"c"}, Mode: mode.Compare,
			Options: []string{"a", "b"}, CompareCriteria: []schema.CompareCriterion{{Name: "cost"}}}, "must declare its direction"},
		{"forecast without a unit", schema.RawTask{Purpose: "p", Criteria: []string{"c"}, Mode: mode.Forecast,
			Target: "t", Horizon: "h"}, "unit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The surface-level check (the same one the CLI and ACP run) refuses it with the same message.
			err := mode.ValidateTask(tc.raw.Mode, tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateTask error = %v, want one containing %q", err, tc.want)
			}
			// And the pipeline refuses it too, so no caller can skip the check — at ZERO model calls, which is
			// the property that makes "before any spend" mechanical rather than aspirational.
			reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
			counters := map[string]*countingAdapter{}
			for name, a := range reg {
				c := counted(a)
				counters[name], reg[name] = c, c
			}
			if _, rerr := runTask(t, reg, plan, tc.raw); rerr == nil {
				t.Fatal("the pipeline must refuse an under-declared fixed-space task")
			}
			for name, c := range counters {
				for _, phase := range []string{schema.PhasePreflight, schema.PhaseFormulate, schema.PhaseExplore, schema.PhaseSynthesize} {
					if n := c.count(phase); n > 0 {
						t.Errorf("the refusal cost %d %q call(s) on %s — it must cost none", n, phase, name)
					}
				}
			}
		})
	}
}
