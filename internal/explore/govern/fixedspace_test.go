package govern

// Unit tests for the FIXED-SPACE host arithmetic (design §3 Compare row). They build the evaluations
// directly rather than running a panel, because the rules being pinned here — dominance under a declared
// direction, the gate's "exclusion requires no dissent", the withheld label on a split cell, the refusal to
// rank without weights — are the parts a fixture could accidentally satisfy by luck.

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// who builds an explorer identity.
func who(tag string) schema.ExplorerIdentity {
	return schema.ExplorerIdentity{Adapter: "fake-" + tag, Model: "m-" + tag, Effort: "high"}
}

// fixture builds a 3-member frozen panel and the blind round-1 baseline over it.
func fixture(t *testing.T) (BlindBaseline, Panel) {
	t.Helper()
	ids := []schema.ExplorerIdentity{who("A"), who("B"), who("C")}
	panel, err := Freeze(ids, DefaultCountingPolicy())
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	panel = panel.WithOutcome(Outcome{Dispatched: 3, Eligible: 3})
	envs := make([]schema.Envelope, 0, len(ids))
	for i, id := range ids {
		envs = append(envs, schema.Envelope{Order: i, Identity: id, IdentityStatus: schema.IdentityVerified})
	}
	baseline, err := NewBlindBaseline(round.NewRound(1, true, "payload", envs, nil))
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	return baseline, panel
}

// score builds one attributed numeric evaluation.
func score(tag, option, criterion string, v float64) AttributedEvaluation {
	order := map[string]int{"A": 0, "B": 1, "C": 2}[tag]
	return AttributedEvaluation{
		Explorer: who(tag), EnvelopeRef: schema.EnvelopeRef(1, order),
		Option: option, Criterion: criterion, Score: v, HasScore: true,
		Value: "value", Rationale: "because " + tag,
	}
}

// gate builds one attributed filter verdict.
func gate(tag, option, criterion string, v schema.FilterVerdict) AttributedEvaluation {
	order := map[string]int{"A": 0, "B": 1, "C": 2}[tag]
	return AttributedEvaluation{
		Explorer: who(tag), EnvelopeRef: schema.EnvelopeRef(1, order),
		Option: option, Criterion: criterion, Verdict: v, Value: string(v),
	}
}

// cell finds one cell of a built matrix.
func cell(t *testing.T, m CompareMatrix, option, criterion string) CompareCell {
	t.Helper()
	for _, c := range m.Cells {
		if c.Option == option && c.Criterion == criterion {
			return c
		}
	}
	t.Fatalf("no cell for %q / %q", option, criterion)
	return CompareCell{}
}

func options(entries []ParetoEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Option)
	}
	return out
}

// TestBuildMatrix_ParetoHonorsEachCriterionsDeclaredDirection is the frontier rule. The SAME numbers under
// the OPPOSITE direction must produce the opposite frontier — which is the whole reason direction is
// declared by the user rather than inferred from a model's scores.
func TestBuildMatrix_ParetoHonorsEachCriterionsDeclaredDirection(t *testing.T) {
	baseline, panel := fixture(t)
	evals := []AttributedEvaluation{
		score("A", "alpha", "speed", 9), score("B", "alpha", "speed", 9),
		score("A", "beta", "speed", 3), score("B", "beta", "speed", 3),
	}
	higher, err := BuildMatrix(MatrixInput{
		Options:  []string{"alpha", "beta"},
		Criteria: []schema.CompareCriterion{{Name: "speed", Direction: schema.HigherIsBetter}},
		Baseline: baseline, Evaluations: evals, Panel: panel, FormulationHash: "h",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := options(higher.Pareto); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("higher_is_better frontier = %v, want [alpha]", got)
	}
	lower, err := BuildMatrix(MatrixInput{
		Options:  []string{"alpha", "beta"},
		Criteria: []schema.CompareCriterion{{Name: "speed", Direction: schema.LowerIsBetter}},
		Baseline: baseline, Evaluations: evals, Panel: panel, FormulationHash: "h",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := options(lower.Pareto); len(got) != 1 || got[0] != "beta" {
		t.Errorf("lower_is_better frontier = %v, want [beta] — the SAME numbers under the opposite direction", got)
	}
	if len(lower.Dominated) != 1 || lower.Dominated[0].Option != "alpha" {
		t.Errorf("dominated = %+v, want alpha", lower.Dominated)
	}
	if !strings.Contains(lower.Dominated[0].Reason, ParetoRuleVersion) {
		t.Errorf("the dominance reason must name the rule version: %q", lower.Dominated[0].Reason)
	}
}

// TestBuildMatrix_SplitCellIsSurfacedNotAveraged pins the honesty rule: a cell whose sources disagree keeps
// EVERY value, reports the split, and withholds the definitive label. k is the largest agreeing group.
func TestBuildMatrix_SplitCellIsSurfacedNotAveraged(t *testing.T) {
	baseline, panel := fixture(t)
	m, err := BuildMatrix(MatrixInput{
		Options:  []string{"alpha"},
		Criteria: []schema.CompareCriterion{{Name: "speed", Direction: schema.HigherIsBetter}},
		Baseline: baseline, Panel: panel, FormulationHash: "h",
		Evaluations: []AttributedEvaluation{
			score("A", "alpha", "speed", 9),
			score("B", "alpha", "speed", 1),
			score("C", "alpha", "speed", 9),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := cell(t, m, "alpha", "speed")
	if c.Agreement != AgreementSplit {
		t.Errorf("agreement = %q, want %q", c.Agreement, AgreementSplit)
	}
	if len(c.Values) != 3 || c.Low != 1 || c.High != 9 || c.Spread != 8 {
		t.Errorf("the cell must retain every value and report the observed spread: %+v", c)
	}
	if c.PointEstimate != 9 {
		t.Errorf("point estimate = %v, want the median 9 (a DERIVED view for the dominance rule)", c.PointEstimate)
	}
	if c.Claim == nil || c.Claim.Label != LabelWithheldDisagreement || c.Claim.Label.Definitive() {
		t.Fatalf("a split cell must WITHHOLD its label: %+v", c.Claim)
	}
	if c.Claim.Value != 2 {
		t.Errorf("k = %d, want 2 (the largest agreeing group, not the respondent count)", c.Claim.Value)
	}
	if !strings.Contains(c.Claim.Rendering(), "DISAGREE") {
		t.Errorf("the rendering must state the disagreement: %q", c.Claim.Rendering())
	}
	if m.DisagreedCells() != 1 {
		t.Errorf("DisagreedCells = %d, want 1", m.DisagreedCells())
	}
}

// TestBuildMatrix_FilterGateExcludesOnlyWithoutDissent pins the gate rule in both directions: a unanimous
// fail EXCLUDES the option before dominance, while a SPLIT gate does not — throwing an option out on a
// disputed requirement is a decision the panel did not make, so the dispute is surfaced instead.
func TestBuildMatrix_FilterGateExcludesOnlyWithoutDissent(t *testing.T) {
	baseline, panel := fixture(t)
	criteria := []schema.CompareCriterion{
		{Name: "speed", Direction: schema.HigherIsBetter},
		{Name: "on-prem", Role: schema.RoleFilter},
	}
	build := func(gates ...AttributedEvaluation) CompareMatrix {
		t.Helper()
		evals := []AttributedEvaluation{
			score("A", "alpha", "speed", 9), score("B", "alpha", "speed", 9),
			score("A", "beta", "speed", 1), score("B", "beta", "speed", 1),
			gate("A", "alpha", "on-prem", schema.VerdictPass), gate("B", "alpha", "on-prem", schema.VerdictPass),
		}
		m, err := BuildMatrix(MatrixInput{
			Options: []string{"alpha", "beta"}, Criteria: criteria, Baseline: baseline,
			Evaluations: append(evals, gates...), Panel: panel, FormulationHash: "h",
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return m
	}

	unanimous := build(gate("A", "beta", "on-prem", schema.VerdictFail), gate("B", "beta", "on-prem", schema.VerdictFail))
	if len(unanimous.ExcludedByFilter) != 1 || unanimous.ExcludedByFilter[0].Option != "beta" {
		t.Fatalf("a unanimous fail must EXCLUDE the option: %+v", unanimous.ExcludedByFilter)
	}
	if !strings.Contains(unanimous.ExcludedByFilter[0].Reason, FilterGateRuleVersion) {
		t.Errorf("the exclusion must name the gate rule version: %q", unanimous.ExcludedByFilter[0].Reason)
	}
	for _, e := range unanimous.Pareto {
		if e.Option == "beta" {
			t.Error("an excluded option must not reach the frontier")
		}
	}

	split := build(gate("A", "beta", "on-prem", schema.VerdictFail), gate("B", "beta", "on-prem", schema.VerdictPass))
	if len(split.ExcludedByFilter) != 0 {
		t.Errorf("a SPLIT gate must not exclude — exclusion requires no dissent: %+v", split.ExcludedByFilter)
	}
	gc := cell(t, split, "beta", "on-prem")
	if !gc.Contested || gc.Verdict != schema.VerdictUnknown || gc.Passes != 1 || gc.Fails != 1 {
		t.Errorf("a split gate cell must be recorded as contested: %+v", gc)
	}
}

// TestBuildMatrix_NoWeightsNoRanking pins §3's rule and its inverse: no weights → no scalar ranking, WITH a
// stated reason; a weight on every scored criterion → a ranking under the versioned rule.
func TestBuildMatrix_NoWeightsNoRanking(t *testing.T) {
	baseline, panel := fixture(t)
	evals := []AttributedEvaluation{
		score("A", "alpha", "speed", 9), score("B", "alpha", "speed", 9),
		score("A", "alpha", "cost", 8), score("B", "alpha", "cost", 8),
		score("A", "beta", "speed", 3), score("B", "beta", "speed", 3),
		score("A", "beta", "cost", 1), score("B", "beta", "cost", 1),
	}
	unweighted := []schema.CompareCriterion{
		{Name: "speed", Direction: schema.HigherIsBetter},
		{Name: "cost", Direction: schema.LowerIsBetter},
	}
	m, err := BuildMatrix(MatrixInput{
		Options: []string{"alpha", "beta"}, Criteria: unweighted, Baseline: baseline,
		Evaluations: evals, Panel: panel, FormulationHash: "h",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if m.ScalarRanking != nil {
		t.Fatalf("unweighted criteria must yield NO scalar ranking: %+v", m.ScalarRanking)
	}
	for _, want := range []string{"NO SCALAR RANKING", "weighting", "Pareto"} {
		if !strings.Contains(m.RankingWithheld, want) {
			t.Errorf("the refusal must explain itself (%q missing): %q", want, m.RankingWithheld)
		}
	}
	// Both survive on the frontier: alpha wins speed, beta wins cost. That is the trade-off view.
	if len(m.Pareto) != 2 {
		t.Errorf("frontier = %v, want both options", options(m.Pareto))
	}

	weighted := []schema.CompareCriterion{
		{Name: "speed", Direction: schema.HigherIsBetter, Weight: 3},
		{Name: "cost", Direction: schema.LowerIsBetter, Weight: 1},
	}
	w, err := BuildMatrix(MatrixInput{
		Options: []string{"alpha", "beta"}, Criteria: weighted, Baseline: baseline,
		Evaluations: evals, Panel: panel, FormulationHash: "h",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(w.ScalarRanking) != 2 || w.RankingWithheld != "" {
		t.Fatalf("declared weights must license a ranking: %+v / %q", w.ScalarRanking, w.RankingWithheld)
	}
	if w.ScalarRanking[0].Option != "alpha" || w.ScalarRanking[0].Rank != 1 {
		t.Errorf("speed weighted 3:1 puts alpha first, got %+v", w.ScalarRanking)
	}
	if w.ScalarRankingRuleVersion != ScalarRankingRuleVersion {
		t.Errorf("a computed ranking must carry its rule version, got %q", w.ScalarRankingRuleVersion)
	}
}

// TestBuildMatrix_NoEvidenceIsReportedNotScored pins the missing-evidence path: an unevidenced cell gets no
// point estimate and NO claim, and the option it belongs to is carried as incomparable rather than losing.
func TestBuildMatrix_NoEvidenceIsReportedNotScored(t *testing.T) {
	baseline, panel := fixture(t)
	m, err := BuildMatrix(MatrixInput{
		Options: []string{"alpha", "beta"},
		Criteria: []schema.CompareCriterion{
			{Name: "speed", Direction: schema.HigherIsBetter},
			{Name: "cost", Direction: schema.LowerIsBetter},
		},
		Baseline: baseline, Panel: panel, FormulationHash: "h",
		Evaluations: []AttributedEvaluation{
			score("A", "alpha", "speed", 9), score("B", "alpha", "speed", 9),
			score("A", "alpha", "cost", 1), score("B", "alpha", "cost", 1),
			score("A", "beta", "speed", 3), score("B", "beta", "speed", 3),
			// nothing at all about beta's cost
		},
		Notes: []AttributedNote{{Explorer: who("A"), EnvelopeRef: "envelope#0", Note: "beta / cost: no pricing published"}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	empty := cell(t, m, "beta", "cost")
	if empty.Agreement != AgreementNoEvidence || empty.HasPoint || empty.Claim != nil {
		t.Errorf("an unevidenced cell must have no point estimate and NO claim: %+v", empty)
	}
	if len(m.MissingEvidence) != 1 || m.MissingEvidence[0].Option != "beta" {
		t.Fatalf("missing evidence = %+v", m.MissingEvidence)
	}
	if len(m.Incomparable) != 1 || m.Incomparable[0].Option != "beta" {
		t.Fatalf("beta must be CARRIED as incomparable, not dropped: %+v", m.Incomparable)
	}
	if len(m.ReportedMissingEvidence) != 1 {
		t.Error("the explorer's own note must be carried")
	}
	if got := options(m.Pareto); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("frontier = %v, want [alpha] (beta could not be compared)", got)
	}
}

// TestBuildMatrix_UnrecognizedEvaluationsAreRecordedNotReattached pins the one thing a fixed-space mode must
// never do: guess what an explorer meant. An evaluation naming something outside the DECLARED space is
// recorded as unrecognized — re-attaching it to the nearest declared name would be exactly the entity
// resolution this mode is free of (§0 F-B).
func TestBuildMatrix_UnrecognizedEvaluationsAreRecordedNotReattached(t *testing.T) {
	baseline, panel := fixture(t)
	m, err := BuildMatrix(MatrixInput{
		Options:  []string{"alpha"},
		Criteria: []schema.CompareCriterion{{Name: "speed", Direction: schema.HigherIsBetter}},
		Baseline: baseline, Panel: panel, FormulationHash: "h",
		Evaluations: []AttributedEvaluation{
			score("A", "alpha", "speed", 9),
			score("B", "ALPHA ", "Speed", 8), // the DECLARED names, differently cased/spaced: still the same cell
			score("C", "alpha-prime", "speed", 1),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := cell(t, m, "alpha", "speed")
	if c.Sources != 2 {
		t.Errorf("a trimmed, case-folded match against the DECLARED name is arithmetic: sources = %d, want 2", c.Sources)
	}
	if len(m.Unrecognized) != 1 || m.Unrecognized[0].Option != "alpha-prime" {
		t.Fatalf("an option outside the declared set must be recorded as unrecognized: %+v", m.Unrecognized)
	}
	for _, v := range c.Values {
		if v.Score == 1 {
			t.Error("an unrecognized evaluation must NOT be folded into a declared cell")
		}
	}
}

// TestBuildMatrix_AntiEchoAndPreconditions pins the two refusals: an evaluation whose envelope is not in the
// blind baseline cannot enter a cell, and a matrix cannot be built without a baseline or a declared space.
func TestBuildMatrix_AntiEchoAndPreconditions(t *testing.T) {
	baseline, panel := fixture(t)
	later := score("A", "alpha", "speed", 1)
	later.EnvelopeRef = schema.EnvelopeRef(2, 0) // a round-2 envelope: not in the blind baseline
	m, err := BuildMatrix(MatrixInput{
		Options:  []string{"alpha"},
		Criteria: []schema.CompareCriterion{{Name: "speed", Direction: schema.HigherIsBetter}},
		Baseline: baseline, Panel: panel, FormulationHash: "h",
		Evaluations: []AttributedEvaluation{score("B", "alpha", "speed", 9), later},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := cell(t, m, "alpha", "speed")
	if c.Sources != 1 || c.PointEstimate != 9 {
		t.Errorf("only BLIND round-1 evidence may enter a cell: %+v", c)
	}
	if _, err := BuildMatrix(MatrixInput{Options: []string{"a"}, Criteria: []schema.CompareCriterion{{Name: "c"}}, Panel: panel}); err == nil {
		t.Error("a matrix without a blind baseline must be refused")
	}
	if _, err := BuildMatrix(MatrixInput{Baseline: baseline, Panel: panel}); err == nil {
		t.Error("a matrix over an empty declared space must be refused")
	}
}

// TestFixedSpaceClaims_CarryTheNoPartitionStatement pins §0 F-C for the fixed-space path: a claim with no
// partition says so, in words, rather than carrying an empty field a reader would have to interpret.
func TestFixedSpaceClaims_CarryTheNoPartitionStatement(t *testing.T) {
	baseline, panel := fixture(t)
	m, err := BuildMatrix(MatrixInput{
		Options:  []string{"alpha"},
		Criteria: []schema.CompareCriterion{{Name: "speed", Direction: schema.HigherIsBetter}},
		Baseline: baseline, Panel: panel, FormulationHash: "formulation-hash",
		Evaluations: []AttributedEvaluation{score("A", "alpha", "speed", 9), score("B", "alpha", "speed", 9)},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if m.PartitionRevisionHash != FixedSpaceNoPartition {
		t.Errorf("the matrix must state that there is no partition, got %q", m.PartitionRevisionHash)
	}
	if len(m.Claims) != 1 {
		t.Fatalf("expected one cell claim, got %d", len(m.Claims))
	}
	c := m.Claims[0]
	if c.Query != AgreementQuery || c.Subject != CellSubject("alpha", "speed") {
		t.Errorf("claim identity: %+v", c)
	}
	if c.PartitionRevisionHash != FixedSpaceNoPartition || c.FormulationHash != "formulation-hash" ||
		c.PolicyHash == "" || c.BaselineRoundID != "round-1" {
		t.Errorf("a fixed-space claim must still be fully pinned: %+v", c)
	}
	if c.Label != LabelCorroborated || len(c.ContributingSourceIDs) != 2 {
		t.Errorf("two agreeing blind sources are corroboration: %+v", c)
	}
	r := c.Rendering()
	if !strings.Contains(r, "NONE (fixed space") {
		t.Errorf("rendering must name the absent partition rather than truncating the statement to a hash-shaped fragment: %q", r)
	}
	// §0 F-B: no count is ever rendered as independent consensus. The sanctioned sentence says the opposite.
	if strings.Contains(r, "independent consensus") || !strings.Contains(r, "not a consensus claim") {
		t.Errorf("rendering must disclaim consensus, not assert it: %q", r)
	}
	if !strings.Contains(r, "evaluations of the same DECLARED cell") {
		t.Errorf("a fixed-space cell count is over EVALUATIONS of a declared cell, not nominations: %q", r)
	}

	// The forecast claim takes the same shape over the same baseline.
	fc, err := EstimateClaim(EstimateClaimInput{
		Target: "peak rps", Unit: "requests/second", Baseline: baseline, Panel: panel,
		FormulationHash: "formulation-hash",
		EnvelopeRefs:    []string{schema.EnvelopeRef(1, 0), schema.EnvelopeRef(1, 1), schema.EnvelopeRef(2, 0)},
		Sources:         []schema.ExplorerIdentity{who("A"), who("B")},
	})
	if err != nil {
		t.Fatalf("estimate claim: %v", err)
	}
	if fc.Query != EstimateQuery || fc.Value != 2 || fc.PartitionRevisionHash != FixedSpaceNoPartition {
		t.Errorf("estimate claim: %+v", fc)
	}
	if len(fc.ContributingSourceIDs) != 2 {
		t.Errorf("a round-2 envelope ref must be filtered out of the sources: %v", fc.ContributingSourceIDs)
	}
}
