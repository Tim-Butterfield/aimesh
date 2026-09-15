package cli

// This file holds the CLI parts of the fixed-space modes: the `--criterion` spec parser and the compare and
// forecast renderers. The renderers avoid reading as a scoreboard: they lead with the trade-off frontier,
// show every explorer's value for disagreed cells, state when no scalar ranking was computed, and print
// each forecast outlier with its forecaster's reasoning.

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// criterionSpecs collects the repeatable --criterion specs in order; that order is used in the prompt and
// the matrix.
type criterionSpecs []string

func (s *criterionSpecs) String() string { return strings.Join(*s, "; ") }
func (s *criterionSpecs) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseCriterion parses one `name=<n>,direction=<d>[,role=<r>][,weight=<w>]` spec. Like the `--explorer`
// grammar, fields split on commas and each field on its first '=', so a name may contain '=' or ':'.
func parseCriterion(spec string) (schema.CompareCriterion, error) {
	var out schema.CompareCriterion
	seen := map[string]bool{}
	for field := range strings.SplitSeq(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			return schema.CompareCriterion{}, fmt.Errorf("invalid field %q (want key=value)", field)
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if seen[key] {
			return schema.CompareCriterion{}, fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true
		switch key {
		case "name":
			out.Name = val
		case "direction":
			out.Direction = schema.Direction(val)
		case "role":
			out.Role = schema.CriterionRole(val)
		case "weight":
			w, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return schema.CompareCriterion{}, fmt.Errorf("invalid weight %q (want a number)", val)
			}
			out.Weight = w
		default:
			return schema.CompareCriterion{}, fmt.Errorf("unknown key %q (want name, direction, role, or weight)", key)
		}
	}
	// Validate here rather than at the pipeline: the user typed this, so the error belongs at the flag.
	if err := out.Validate(); err != nil {
		return schema.CompareCriterion{}, err
	}
	return out, nil
}

// parseCriteria parses every --criterion spec, naming the offending one on failure.
func parseCriteria(specs []string) ([]schema.CompareCriterion, error) {
	out := make([]schema.CompareCriterion, 0, len(specs))
	for _, spec := range specs {
		c, err := parseCriterion(spec)
		if err != nil {
			return nil, fmt.Errorf("--criterion %q: %w", spec, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// printCompareOutput writes the compare result: the Pareto frontier first, then the matrix with every
// explorer's value for each cell, and either the weighted ranking or the reason none was computed.
func printCompareOutput(w io.Writer, res pipeline.Result, out mode.CompareOutput) {
	fmt.Fprintf(w, "\nCompare: %s\n", out.Summary())
	fmt.Fprintf(w, "  %s\n", out.SpaceNote)
	printGovernanceHeader(w, res)
	fmt.Fprintf(w, "  rules: cells %s; gate %s; frontier %s\n", out.Rules.CellPointRule, out.Rules.FilterGateRule, out.Rules.ParetoRule)

	fmt.Fprintln(w, "\nPareto frontier (HOST-computed, honoring each criterion's declared direction — a SET of non-dominated options, not a ranking):")
	for _, e := range out.Pareto {
		fmt.Fprintf(w, "  - %s%s\n", e.Option, conditionalTag(e))
		for _, c := range out.Criteria {
			if c.EffectiveRole() != schema.RoleDimension {
				continue
			}
			fmt.Fprintf(w, "      %s: %g (%s)\n", c.Name, e.Scores[c.Name], c.Direction)
		}
		if len(e.DisagreedCells) > 0 {
			fmt.Fprintf(w, "      CONDITIONAL: this position rides on %d cell(s) the panel disagreed about: %s\n",
				len(e.DisagreedCells), strings.Join(e.DisagreedCells, "; "))
		}
	}
	if len(out.Dominated) > 0 {
		fmt.Fprintln(w, "\nDominated (carried with the host's reason — a dominated option is the answer to \"why not this one\"):")
		for _, d := range out.Dominated {
			fmt.Fprintf(w, "  - %s: %s\n", d.Option, d.Reason)
		}
	}
	if len(out.ExcludedByFilter) > 0 {
		fmt.Fprintln(w, "\nExcluded by a FILTER GATE (removed BEFORE dominance — a hard requirement is not a trade-off):")
		for _, e := range out.ExcludedByFilter {
			fmt.Fprintf(w, "  - %s: %s\n", e.Option, e.Reason)
		}
	}
	if len(out.Incomparable) > 0 {
		fmt.Fprintln(w, "\nIncomparable (carried, NOT ranked):")
		for _, i := range out.Incomparable {
			fmt.Fprintf(w, "  - %s: %s\n", i.Option, i.Reason)
		}
	}

	fmt.Fprintln(w, "\nMatrix (every cell keeps EVERY explorer's value — a disagreement is reported, never averaged):")
	for _, c := range out.Cells {
		fmt.Fprintf(w, "  %s / %s [%s] — %s%s\n", c.Option, c.Criterion, c.Role, agreementDisplay(c), cellValueDisplay(c))
		if c.Claim != nil {
			fmt.Fprintf(w, "      %s — %s / %s\n", labelDisplay(c.Claim.Label), c.Claim.KOfPanel, c.Claim.KOfRespondents)
		}
		for _, v := range c.Values {
			fmt.Fprintf(w, "      · %s ← %s%s\n", valueDisplay(v), identity(v.Explorer), rationaleSuffix(v.Rationale))
		}
	}
	if len(out.MissingEvidence) > 0 {
		fmt.Fprintln(w, "\nMissing evidence (explicit — a cell nobody could judge is NOT a zero):")
		for _, m := range out.MissingEvidence {
			fmt.Fprintf(w, "  - %s / %s: %s\n", m.Option, m.Criterion, m.Reason)
		}
	}
	if len(out.Unrecognized) > 0 {
		fmt.Fprintln(w, "\nUnrecognized evaluations (named something outside the DECLARED space — recorded, never re-attached to the nearest match):")
		for _, u := range out.Unrecognized {
			fmt.Fprintf(w, "  - %q / %q ← %s\n", u.Option, u.Criterion, identity(u.Explorer))
		}
	}

	if len(out.ScalarRanking) > 0 {
		fmt.Fprintf(w, "\nWeighted ranking (%s — a view of the SAME matrix under the weights YOU declared, not a verdict):\n", out.Rules.ScalarRankRule)
		for _, e := range out.ScalarRanking {
			fmt.Fprintf(w, "  %d. %s — %.3f\n", e.Rank, e.Option, e.Score)
		}
	} else {
		fmt.Fprintf(w, "\n%s\n", out.RankingWithheld)
	}
	printNarrative(w, out.CollatorNarrative)
}

// conditionalTag marks a frontier position that depends on a disagreed cell.
func conditionalTag(e govern.ParetoEntry) string {
	if e.Conditional {
		return " [CONDITIONAL — see below]"
	}
	return ""
}

// agreementDisplay describes a cell's agreement, marking a split cell prominently.
func agreementDisplay(c govern.CompareCell) string {
	if c.Agreement == govern.AgreementSplit {
		return fmt.Sprintf("DISAGREEMENT across %d explorer(s)", c.Sources)
	}
	return fmt.Sprintf("%s (%d source(s))", c.Agreement, c.Sources)
}

// cellValueDisplay renders the cell's derived value: the point estimate + observed range for a scored
// dimension, or the gate verdict for a filter.
func cellValueDisplay(c govern.CompareCell) string {
	switch {
	case c.Role == schema.RoleFilter:
		return fmt.Sprintf(", gate: %s (%d pass / %d fail)", c.Verdict, c.Passes, c.Fails)
	case c.HasPoint && c.Spread > 0:
		return fmt.Sprintf(", %g (range %g..%g)", c.PointEstimate, c.Low, c.High)
	case c.HasPoint:
		return fmt.Sprintf(", %g", c.PointEstimate)
	default:
		return ""
	}
}

// valueDisplay renders one explorer's reported value verbatim.
func valueDisplay(v govern.AttributedEvaluation) string {
	if v.Verdict == schema.VerdictPass || v.Verdict == schema.VerdictFail {
		return string(v.Verdict)
	}
	if v.HasScore {
		return fmt.Sprintf("%g", v.Score)
	}
	return fmt.Sprintf("%q (not a number — no cell value)", v.Value)
}

// rationaleSuffix appends an explorer's stated reason when it gave one.
func rationaleSuffix(r string) string {
	if strings.TrimSpace(r) == "" {
		return ""
	}
	return ": " + r
}

// printForecastOutput writes the forecast result: the host-pooled estimate with its method and dispersion,
// the individual estimates, and each outlier with its forecaster's reasoning.
func printForecastOutput(w io.Writer, res pipeline.Result, out mode.ForecastOutput) {
	fmt.Fprintf(w, "\nForecast: %s\n", out.Summary())
	fmt.Fprintf(w, "  %s\n", out.SpaceNote)
	printGovernanceHeader(w, res)
	fmt.Fprintf(w, "\n  %s = %g %s over %s\n", out.Target, out.Aggregate, out.Unit, out.Horizon)
	if out.ConditioningEvent != "" {
		fmt.Fprintf(w, "  conditioned on: %s\n", out.ConditioningEvent)
	}
	fmt.Fprintf(w, "  interval: %g .. %g (%s; %d of %d forecaster(s) stated one)\n",
		out.Interval.Low, out.Interval.High, out.Method.IntervalRule, out.Interval.Sources, len(out.IndividualEstimates))
	fmt.Fprintf(w, "  dispersion: min %g, max %g, range %g, IQR %g, stddev %.3f, MAD %g\n",
		out.Dispersion.Min, out.Dispersion.Max, out.Dispersion.Range, out.Dispersion.IQR, out.Dispersion.StdDev, out.Dispersion.MAD)
	fmt.Fprintf(w, "  method: %s [%s] — computed by %s\n", out.Method.Rule, out.Method.RulesVersion, out.Method.ComputedBy)
	fmt.Fprintf(w, "  %s — %s / %s\n", labelDisplay(out.Claim.Label), out.Claim.KOfPanel, out.Claim.KOfRespondents)

	fmt.Fprintln(w, "\nIndividual estimates (attributed; the aggregate is a view over these):")
	for _, e := range out.IndividualEstimates {
		interval := ""
		if e.HasInterval {
			interval = fmt.Sprintf(" [%g .. %g]", e.Low, e.High)
		}
		fmt.Fprintf(w, "  · %g%s ← %s\n", e.Value, interval, identity(e.Explorer))
		if strings.TrimSpace(e.Reasoning) != "" {
			fmt.Fprintf(w, "      %s\n", e.Reasoning)
		}
	}
	if len(out.Outliers) > 0 {
		fmt.Fprintln(w, "\nOutliers (IDENTIFIED, not discarded — still inside the aggregate, and carrying their own reasoning):")
		for _, o := range out.Outliers {
			fmt.Fprintf(w, "  - %g ← %s (modified z %.1f)\n", o.Estimate.Value, identity(o.Estimate.Explorer), o.ModifiedZ)
			fmt.Fprintf(w, "      host: %s\n", o.Reason)
			if strings.TrimSpace(o.Estimate.Reasoning) != "" {
				fmt.Fprintf(w, "      forecaster's own reasoning: %s\n", o.Estimate.Reasoning)
			}
		}
	}
	if len(out.Rejected) > 0 {
		fmt.Fprintln(w, "\nRejected responses (recorded, never silently absent):")
		for _, r := range out.Rejected {
			fmt.Fprintf(w, "  - %s: %s\n", identity(r.Explorer), r.Reason)
		}
	}
	if len(out.Assumptions) > 0 {
		fmt.Fprintln(w, "\nStated assumptions (attributed):")
		for _, a := range out.Assumptions {
			fmt.Fprintf(w, "  - %s ← %s\n", a.Assumption, identity(a.Explorer))
		}
	}
	printNarrative(w, out.CollatorNarrative)
}
