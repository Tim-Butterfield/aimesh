package govern

// This file holds the host arithmetic for the fixed-space modes (compare and forecast): the
// options × criteria matrix with per-cell agreement, the filter gate, the Pareto frontier, the optional
// weighted ranking, and their claims.
//
//   - There is no partition: the user declared the options and criteria, so matching names is string
//     comparison rather than entity resolution. Claims carry FixedSpaceNoPartition instead of a hash.
//   - Disagreement is reported, not averaged: every attributed value stays on its cell and a split cell's
//     label is withheld. The point estimate exists only for the Pareto comparison.
//   - A filter criterion excludes options before dominance is computed, so it cannot be traded off.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Versioned rules of the fixed-space path. Each is recorded on the value it produced.
const (
	// FixedSpaceRulesVersion versions the fixed-space arithmetic as a whole.
	FixedSpaceRulesVersion = "exploremesh-fixed-space-rules@v1"
	// AgreementQuery is the query id of a per-cell agreement claim.
	AgreementQuery = "cell-agreement-over-blind-round-1@v1"
	// EstimateQuery is the query id of a pooled-estimate claim.
	EstimateQuery = "pooled-estimate-over-blind-round-1@v1"
	// CellPointRule derives a cell's comparable value: the median of its blind values.
	CellPointRule = "median-of-blind-round-1-values@v1"
	// FilterGateRuleVersion excludes an option when at least one evaluation says fail and none says pass.
	// A split gate keeps the option and marks the cell contested.
	FilterGateRuleVersion = "host-filter-gate-unanimous-among-respondents@v1"
	// ParetoRuleVersion: after the filter gate, A dominates B when A is at least as good on every scored
	// dimension (in its declared direction) and strictly better on one.
	ParetoRuleVersion = "host-pareto-over-declared-dimensions@v1"
	// ScalarRankingRuleVersion is the weighted ranking, computed only when every scored dimension has a
	// weight: min-max normalization oriented so best is 1, then a weighted mean.
	ScalarRankingRuleVersion = "host-weighted-scalar@v1"
	// FixedSpaceNoPartition is recorded in place of a partition revision hash on fixed-space claims.
	FixedSpaceNoPartition = "none — fixed space: the option set and criteria were declared by the user before any explorer spoke, so no entity resolution was performed and there is no partition to pin"
)

// LabelWithheldDisagreement means the blind sources disagree about a cell's value. It differs from
// LabelWithheldContested, which is a dispute about what the entities are.
const LabelWithheldDisagreement Label = "withheld_cell_disagreement"

// Agreement is the host's verdict over the blind evaluations of one cell.
type Agreement string

// Cell agreement verdicts.
const (
	// AgreementUnanimous means two or more sources reported the same value or gate verdict.
	AgreementUnanimous Agreement = "unanimous"
	// AgreementSplit means two or more sources disagree.
	AgreementSplit Agreement = "split"
	// AgreementSingleSource means exactly one source evaluated the cell.
	AgreementSingleSource Agreement = "single_source"
	// AgreementNoEvidence means no source produced a usable value; it is reported as missing evidence.
	AgreementNoEvidence Agreement = "no_evidence"
)

// AttributedEvaluation is one explorer's evaluation of one cell, with the envelope it came from and the raw
// value as written.
type AttributedEvaluation struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Option      string                  `json:"option"`
	Criterion   string                  `json:"criterion"`
	Value       string                  `json:"value"`
	Score       float64                 `json:"score,omitempty"`
	HasScore    bool                    `json:"hasScore,omitempty"`
	Verdict     schema.FilterVerdict    `json:"verdict,omitempty"`
	Rationale   string                  `json:"rationale,omitempty"`
}

// AttributedNote is an explorer's free-text missing-evidence note. It is model prose with no governance
// value.
type AttributedNote struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Note        string                  `json:"note"`
}

// CompareCell is one options × criteria cell: every attributed value, the agreement verdict, the spread,
// the point estimate (dimensions) or gate verdict (filters), and the claim.
type CompareCell struct {
	Option    string               `json:"option"`
	Criterion string               `json:"criterion"`
	Role      schema.CriterionRole `json:"role"`
	Direction schema.Direction     `json:"direction,omitempty"`
	Agreement Agreement            `json:"agreement"`
	// Sources is the number of distinct explorers that produced a usable value.
	Sources int `json:"sources"`
	// Values are every attributed evaluation of the cell, in panel order.
	Values []AttributedEvaluation `json:"values"`
	// PointEstimate is the value under CellPointRule, for scored dimensions; HasPoint reports whether one
	// exists.
	PointEstimate float64 `json:"pointEstimate,omitempty"`
	HasPoint      bool    `json:"hasPoint,omitempty"`
	Low           float64 `json:"low,omitempty"`
	High          float64 `json:"high,omitempty"`
	Spread        float64 `json:"spread,omitempty"`
	// Verdict is the gate verdict under FilterGateRuleVersion, for filter criteria.
	Verdict schema.FilterVerdict `json:"verdict,omitempty"`
	Passes  int                  `json:"passes,omitempty"`
	Fails   int                  `json:"fails,omitempty"`
	// Contested marks a filter cell whose evaluations split between pass and fail; the option is kept.
	Contested bool `json:"contested,omitempty"`
	// Claim is the cell's claim; nil for a cell with no evidence.
	Claim *Claim `json:"claim,omitempty"`
}

// Disagrees reports whether the cell's sources split.
func (c CompareCell) Disagrees() bool { return c.Agreement == AgreementSplit }

// MissingEvidence is a cell no explorer filled, with the host's reason.
type MissingEvidence struct {
	Option    string `json:"option"`
	Criterion string `json:"criterion"`
	Reason    string `json:"reason"`
}

// ExcludedOption is an option the filter gate removed, with the criterion that removed it.
type ExcludedOption struct {
	Option    string `json:"option"`
	Criterion string `json:"criterion"`
	Reason    string `json:"reason"`
}

// IncomparableOption is an option that passed the gate but lacks a value on some scored dimension, so it
// is reported rather than placed on the frontier.
type IncomparableOption struct {
	Option        string   `json:"option"`
	MissingScores []string `json:"missingScores"`
	Reason        string   `json:"reason"`
}

// ParetoEntry is one option on the host-computed frontier.
type ParetoEntry struct {
	Option string `json:"option"`
	// Scores are the option's per-dimension point estimates, in the declared criterion order.
	Scores map[string]float64 `json:"scores"`
	// Dominates names the options this one dominates. The frontier itself is unordered.
	Dominates []string `json:"dominates,omitempty"`
	// Conditional marks a position that depends on at least one split cell.
	Conditional bool `json:"conditional,omitempty"`
	// DisagreedCells names the split cells behind Conditional.
	DisagreedCells []string `json:"disagreedCells,omitempty"`
}

// DominatedOption is an option off the frontier, with the options that dominate it.
type DominatedOption struct {
	Option      string             `json:"option"`
	Scores      map[string]float64 `json:"scores"`
	DominatedBy []string           `json:"dominatedBy"`
	Reason      string             `json:"reason"`
}

// ScalarEntry is one option's position in the weighted ranking.
type ScalarEntry struct {
	Rank   int     `json:"rank"`
	Option string  `json:"option"`
	Score  float64 `json:"score"`
}

// CompareMatrix is the complete host-computed comparison built by BuildMatrix: the declared space, cells,
// gate outcome, frontier, optional weighted ranking, missing evidence and claims.
type CompareMatrix struct {
	RulesVersion             string `json:"rulesVersion"`
	CellPointRule            string `json:"cellPointRule"`
	FilterGateRuleVersion    string `json:"filterGateRuleVersion"`
	ParetoRuleVersion        string `json:"paretoRuleVersion"`
	ScalarRankingRuleVersion string `json:"scalarRankingRuleVersion,omitempty"`
	// PartitionRevisionHash is always FixedSpaceNoPartition.
	PartitionRevisionHash string                    `json:"partitionRevisionHash"`
	Options               []string                  `json:"options"`
	Criteria              []schema.CompareCriterion `json:"criteria"`
	Cells                 []CompareCell             `json:"cells"`
	Pareto                []ParetoEntry             `json:"pareto"`
	Dominated             []DominatedOption         `json:"dominated,omitempty"`
	ExcludedByFilter      []ExcludedOption          `json:"excludedByFilter,omitempty"`
	Incomparable          []IncomparableOption      `json:"incomparable,omitempty"`
	MissingEvidence       []MissingEvidence         `json:"missingEvidence,omitempty"`
	// ReportedMissingEvidence are the explorers' own missing-evidence notes.
	ReportedMissingEvidence []AttributedNote `json:"reportedMissingEvidence,omitempty"`
	// Unrecognized are evaluations naming an option or criterion outside the declared space. They are
	// recorded as is, never matched to the nearest declared name.
	Unrecognized []AttributedEvaluation `json:"unrecognized,omitempty"`
	// ScalarRanking is nil unless every scored dimension has a weight; RankingWithheld then says why.
	ScalarRanking   []ScalarEntry `json:"scalarRanking,omitempty"`
	RankingWithheld string        `json:"rankingWithheld,omitempty"`
	// Claims are the per-cell claims, in cell order.
	Claims []Claim `json:"claims"`
}

// DisagreedCells returns the number of split cells.
func (m CompareMatrix) DisagreedCells() int {
	n := 0
	for _, c := range m.Cells {
		if c.Disagrees() {
			n++
		}
	}
	return n
}

// cellKey identifies a cell by its declared option and criterion names.
type cellKey struct{ option, criterion string }

// MatrixInput is the input to BuildMatrix.
type MatrixInput struct {
	Options         []string
	Criteria        []schema.CompareCriterion
	Baseline        BlindBaseline
	Evaluations     []AttributedEvaluation
	Notes           []AttributedNote
	Panel           Panel
	FormulationHash string
}

// BuildMatrix computes the comparison from the blind round-1 evaluations, in order: place evaluations into
// declared cells, compute per-cell agreement and claims, apply the filter gate, compute the Pareto
// frontier, and compute the weighted ranking if every scored dimension has a weight.
func BuildMatrix(in MatrixInput) (CompareMatrix, error) {
	if in.Baseline.Size() == 0 {
		return CompareMatrix{}, fmt.Errorf("compare matrix: no blind round-1 baseline (build it with NewBlindBaseline)")
	}
	if len(in.Options) == 0 || len(in.Criteria) == 0 {
		return CompareMatrix{}, fmt.Errorf("compare matrix: the declared space is empty (%d option(s), %d criterion(a)) — a fixed-space comparison is defined by its declaration", len(in.Options), len(in.Criteria))
	}
	m := CompareMatrix{
		RulesVersion: FixedSpaceRulesVersion, CellPointRule: CellPointRule,
		FilterGateRuleVersion: FilterGateRuleVersion, ParetoRuleVersion: ParetoRuleVersion,
		PartitionRevisionHash:   FixedSpaceNoPartition,
		Options:                 append([]string(nil), in.Options...),
		Criteria:                append([]schema.CompareCriterion(nil), in.Criteria...),
		ReportedMissingEvidence: append([]AttributedNote(nil), in.Notes...),
	}

	// Match evaluations to declared cells by trimmed, case-folded name; anything else is unrecognized.
	optionOf := declaredIndex(in.Options)
	criterionName := make(map[string]string, len(in.Criteria))
	for _, c := range in.Criteria {
		criterionName[normalizeName(c.Name)] = c.Name
	}
	byCell := map[cellKey][]AttributedEvaluation{}
	for _, ev := range in.Evaluations {
		// Only blind round-1 evaluations feed a cell, even though fixed-space modes run one round.
		if !in.Baseline.Contains(ev.EnvelopeRef) {
			continue
		}
		opt, optOK := optionOf[normalizeName(ev.Option)]
		crit, critOK := criterionName[normalizeName(ev.Criterion)]
		if !optOK || !critOK {
			m.Unrecognized = append(m.Unrecognized, ev)
			continue
		}
		ev.Option, ev.Criterion = opt, crit // use the declared spelling
		byCell[cellKey{opt, crit}] = append(byCell[cellKey{opt, crit}], ev)
	}

	// Build cells in declared order so the output is deterministic.
	points := map[cellKey]float64{}   // dimension point estimates, for the frontier
	split := map[cellKey]bool{}       // split cells, for the conditional flag
	gate := map[cellKey]CompareCell{} // filter cells, for the gate
	for _, opt := range in.Options {
		for _, crit := range in.Criteria {
			key := cellKey{opt, crit.Name}
			cell := buildCell(opt, crit, byCell[key], in.Panel, in.Baseline, in.FormulationHash)
			m.Cells = append(m.Cells, cell)
			if cell.Claim != nil {
				m.Claims = append(m.Claims, *cell.Claim)
			}
			if cell.Agreement == AgreementNoEvidence {
				m.MissingEvidence = append(m.MissingEvidence, MissingEvidence{
					Option: opt, Criterion: crit.Name,
					Reason: "no explorer produced a usable value for this cell — it is reported as missing evidence rather than scored as zero",
				})
			}
			if cell.Disagrees() {
				split[key] = true
			}
			switch crit.EffectiveRole() {
			case schema.RoleFilter:
				gate[key] = cell
			default:
				if cell.HasPoint {
					points[key] = cell.PointEstimate
				}
			}
		}
	}

	// The filter gate runs before dominance.
	surviving := make([]string, 0, len(in.Options))
	for _, opt := range in.Options {
		excluded := false
		for _, crit := range in.Criteria {
			if crit.EffectiveRole() != schema.RoleFilter {
				continue
			}
			cell := gate[cellKey{opt, crit.Name}]
			if cell.Verdict != schema.VerdictFail {
				continue
			}
			m.ExcludedByFilter = append(m.ExcludedByFilter, ExcludedOption{
				Option: opt, Criterion: crit.Name,
				Reason: fmt.Sprintf("failed the filter gate %q: %d of %d evaluating explorer(s) judged it FAIL and none judged it PASS (rule %s) — a hard requirement is not traded off against the scored criteria, so this option is excluded before dominance",
					crit.Name, cell.Fails, cell.Sources, FilterGateRuleVersion),
			})
			excluded = true
			break // one failed gate is enough; the reason names the first one
		}
		if !excluded {
			surviving = append(surviving, opt)
		}
	}

	// An option missing a value on any scored dimension cannot be placed on the frontier.
	dims := scoredDimensions(in.Criteria)
	comparable := make([]string, 0, len(surviving))
	for _, opt := range surviving {
		var missing []string
		for _, d := range dims {
			if _, ok := points[cellKey{opt, d.Name}]; !ok {
				missing = append(missing, d.Name)
			}
		}
		if len(missing) > 0 {
			m.Incomparable = append(m.Incomparable, IncomparableOption{
				Option: opt, MissingScores: missing,
				Reason: fmt.Sprintf("no usable value on %d scored dimension(s) (%s) — the option is CARRIED, not ranked: an option nobody could score has not lost a comparison, it has not been in one",
					len(missing), strings.Join(missing, ", ")),
			})
			continue
		}
		comparable = append(comparable, opt)
	}

	m.Pareto, m.Dominated = paretoFrontier(comparable, dims, points, split)
	m.ScalarRanking, m.RankingWithheld = scalarRanking(comparable, dims, points)
	if m.ScalarRanking != nil {
		m.ScalarRankingRuleVersion = ScalarRankingRuleVersion
	}
	return m, nil
}

// buildCell computes one cell's agreement verdict, point estimate or gate verdict, and claim. A cell with
// no usable value gets no claim.
func buildCell(option string, crit schema.CompareCriterion, evals []AttributedEvaluation, panel Panel, baseline BlindBaseline, formulationHash string) CompareCell {
	cell := CompareCell{
		Option: option, Criterion: crit.Name, Role: crit.EffectiveRole(),
		Values: append([]AttributedEvaluation(nil), evals...),
	}
	if cell.Role == schema.RoleDimension {
		cell.Direction = crit.Direction
	}
	// Sources are counted by distinct explorer, not by evaluation.
	sources := map[schema.ExplorerIdentity]bool{}
	refs := map[string]bool{}
	var scores []float64
	groups := map[string][]schema.ExplorerIdentity{} // agreeing groups, keyed by the exact reported position
	for _, ev := range evals {
		usable := false
		switch cell.Role {
		case schema.RoleFilter:
			if ev.Verdict == schema.VerdictPass || ev.Verdict == schema.VerdictFail {
				usable = true
				groups[string(ev.Verdict)] = append(groups[string(ev.Verdict)], ev.Explorer)
				if ev.Verdict == schema.VerdictPass {
					cell.Passes++
				} else {
					cell.Fails++
				}
			}
		default:
			if ev.HasScore {
				usable = true
				scores = append(scores, ev.Score)
				groups[fmt.Sprintf("%g", ev.Score)] = append(groups[fmt.Sprintf("%g", ev.Score)], ev.Explorer)
			}
		}
		if !usable {
			continue
		}
		sources[ev.Explorer] = true
		refs[ev.EnvelopeRef] = true
	}
	cell.Sources = len(sources)

	switch {
	case cell.Sources == 0:
		cell.Agreement = AgreementNoEvidence
		if cell.Role == schema.RoleFilter {
			cell.Verdict = schema.VerdictUnknown
		}
		return cell
	case cell.Sources == 1:
		cell.Agreement = AgreementSingleSource
	case len(groups) == 1:
		cell.Agreement = AgreementUnanimous
	default:
		cell.Agreement = AgreementSplit
	}

	if cell.Role == schema.RoleFilter {
		switch {
		case cell.Fails > 0 && cell.Passes == 0:
			cell.Verdict = schema.VerdictFail
		case cell.Passes > 0 && cell.Fails == 0:
			cell.Verdict = schema.VerdictPass
		default:
			cell.Verdict, cell.Contested = schema.VerdictUnknown, true
		}
	} else if len(scores) > 0 {
		sorted := append([]float64(nil), scores...)
		sort.Float64s(sorted)
		cell.PointEstimate, cell.HasPoint = medianOf(sorted), true
		cell.Low, cell.High = sorted[0], sorted[len(sorted)-1]
		cell.Spread = cell.High - cell.Low
	}

	// k is the size of the largest agreeing group of distinct sources; a split cell's label is withheld.
	k := 0
	for _, g := range groups {
		if n := distinct(g); n > k {
			k = n
		}
	}
	panelDen, respDen := panel.Denominators(k)
	claim := Claim{
		Query: AgreementQuery, Subject: CellSubject(option, crit.Name),
		SubjectLabel: fmt.Sprintf("%s on %s", option, crit.Name), Value: k,
		KOfPanel: panelDen, KOfRespondents: respDen,
		FormulationHash: formulationHash, PartitionRevisionHash: FixedSpaceNoPartition,
		RulesVersion: RulesVersion, PolicyHash: panel.PolicyHash,
		BaselineRoundID: baseline.RoundID(), ContributingSourceIDs: sortedKeys(refs),
	}
	switch {
	case !panel.QuorumMet():
		claim.Label = LabelWithheldBelowQuorum
	case cell.Agreement == AgreementSplit:
		claim.Label = LabelWithheldDisagreement
	case k >= 2:
		claim.Label = LabelCorroborated
	default:
		claim.Label = LabelSingleSource
	}
	cell.Claim = &claim
	return cell
}

// CellSubject returns the subject id of a cell claim, `<option> / <criterion>`.
func CellSubject(option, criterion string) string { return option + " / " + criterion }

// paretoFrontier returns the non-dominated options under ParetoRuleVersion and the dominated remainder,
// each with its per-dimension point estimates.
func paretoFrontier(options []string, dims []schema.CompareCriterion, points map[cellKey]float64, split map[cellKey]bool) ([]ParetoEntry, []DominatedOption) {
	scoresOf := func(opt string) map[string]float64 {
		out := make(map[string]float64, len(dims))
		for _, d := range dims {
			out[d.Name] = points[cellKey{opt, d.Name}]
		}
		return out
	}
	// dominates reports whether a is at least as good as b on every dimension and better on one.
	dominates := func(a, b string) bool {
		strict := false
		for _, d := range dims {
			av, bv := points[cellKey{a, d.Name}], points[cellKey{b, d.Name}]
			if !d.Direction.AtLeastAsGood(av, bv) {
				return false
			}
			if d.Direction.Better(av, bv) {
				strict = true
			}
		}
		return strict
	}
	var frontier []ParetoEntry
	var dominated []DominatedOption
	for _, opt := range options {
		var by []string
		for _, other := range options {
			if other != opt && dominates(other, opt) {
				by = append(by, other)
			}
		}
		if len(by) > 0 {
			dominated = append(dominated, DominatedOption{
				Option: opt, Scores: scoresOf(opt), DominatedBy: by,
				Reason: fmt.Sprintf("dominated by %s — at least as good on every scored dimension (under each dimension's declared direction) and strictly better on at least one (rule %s)",
					strings.Join(by, ", "), ParetoRuleVersion),
			})
			continue
		}
		entry := ParetoEntry{Option: opt, Scores: scoresOf(opt)}
		for _, other := range options {
			if other != opt && dominates(opt, other) {
				entry.Dominates = append(entry.Dominates, other)
			}
		}
		for _, d := range dims {
			if split[cellKey{opt, d.Name}] {
				entry.Conditional = true
				entry.DisagreedCells = append(entry.DisagreedCells, CellSubject(opt, d.Name))
			}
		}
		frontier = append(frontier, entry)
	}
	return frontier, dominated
}

// scalarRanking returns the weighted ranking, or nil and the reason it was withheld. A ranking requires a
// user-declared weight on every scored dimension, since a single score over several criteria implies a
// weighting.
func scalarRanking(options []string, dims []schema.CompareCriterion, points map[cellKey]float64) ([]ScalarEntry, string) {
	unweighted := make([]string, 0, len(dims))
	total := 0.0
	for _, d := range dims {
		if d.Weight <= 0 {
			unweighted = append(unweighted, d.Name)
			continue
		}
		total += d.Weight
	}
	if len(unweighted) > 0 || total <= 0 {
		return nil, fmt.Sprintf(
			"NO SCALAR RANKING was computed: %d of %d scored criterion(a) carry no user-declared weight (%s). Collapsing several criteria into one number IS a weighting, and a weighting the user did not declare would be one the host invented — so the result is the Pareto/trade-off view above. Supply a weight per scored criterion to obtain a ranking (CLI: --criterion name=…,direction=…,weight=…).",
			len(unweighted), len(dims), strings.Join(unweighted, ", "))
	}
	if len(options) == 0 {
		return nil, "NO SCALAR RANKING was computed: no option survived the filter gate with a usable value on every scored criterion."
	}
	// Min-max normalize each dimension so best is 1. A dimension where all options are equal contributes 1
	// to each, so no option is penalized on it.
	scores := map[string]float64{}
	for _, d := range dims {
		lo, hi := points[cellKey{options[0], d.Name}], points[cellKey{options[0], d.Name}]
		for _, opt := range options {
			v := points[cellKey{opt, d.Name}]
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		for _, opt := range options {
			v := points[cellKey{opt, d.Name}]
			n := 1.0
			if hi != lo {
				n = (v - lo) / (hi - lo)
				if d.Direction == schema.LowerIsBetter {
					n = 1 - n
				}
			}
			scores[opt] += n * d.Weight
		}
	}
	out := make([]ScalarEntry, 0, len(options))
	for _, opt := range options {
		out = append(out, ScalarEntry{Option: opt, Score: scores[opt] / total})
	}
	// Order by score, then by option name for determinism.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Option < out[j].Option
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out, ""
}

// EstimateClaimInput is the input to EstimateClaim. The pooled numbers are computed by package pool; the
// claim counts the sources behind them.
type EstimateClaimInput struct {
	Target          string
	Unit            string
	Baseline        BlindBaseline
	Panel           Panel
	FormulationHash string
	// EnvelopeRefs are the blind round-1 envelopes whose estimates were pooled.
	EnvelopeRefs []string
	// Sources are the distinct explorers behind them.
	Sources []schema.ExplorerIdentity
}

// EstimateClaim returns the claim for a pooled forecast: the number of distinct blind round-1 forecasters,
// both denominators and the contributing envelope refs.
func EstimateClaim(in EstimateClaimInput) (Claim, error) {
	if in.Baseline.Size() == 0 {
		return Claim{}, fmt.Errorf("estimate claim: no blind round-1 baseline (build it with NewBlindBaseline)")
	}
	refs := map[string]bool{}
	for _, r := range in.EnvelopeRefs {
		if in.Baseline.Contains(r) {
			refs[r] = true
		}
	}
	k := distinct(in.Sources)
	panelDen, respDen := in.Panel.Denominators(k)
	c := Claim{
		Query: EstimateQuery, Subject: in.Target,
		SubjectLabel: strings.TrimSpace(in.Target + " (" + in.Unit + ")"), Value: k,
		KOfPanel: panelDen, KOfRespondents: respDen,
		FormulationHash: in.FormulationHash, PartitionRevisionHash: FixedSpaceNoPartition,
		RulesVersion: RulesVersion, PolicyHash: in.Panel.PolicyHash,
		BaselineRoundID: in.Baseline.RoundID(), ContributingSourceIDs: sortedKeys(refs),
	}
	switch {
	case !in.Panel.QuorumMet():
		c.Label = LabelWithheldBelowQuorum
	case k >= 2:
		c.Label = LabelCorroborated
	default:
		c.Label = LabelSingleSource
	}
	return c, nil
}

// declaredIndex maps each value's normalized form to its declared spelling.
func declaredIndex(values []string) map[string]string {
	out := make(map[string]string, len(values))
	for _, v := range values {
		out[normalizeName(v)] = v
	}
	return out
}

// normalizeName trims and lower-cases a name for matching. Matching is deliberately not fuzzy.
func normalizeName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// scoredDimensions returns the declared criteria that are scored axes, in declaration order.
func scoredDimensions(cs []schema.CompareCriterion) []schema.CompareCriterion {
	out := make([]schema.CompareCriterion, 0, len(cs))
	for _, c := range cs {
		if c.EffectiveRole() == schema.RoleDimension {
			out = append(out, c)
		}
	}
	return out
}

// distinct counts the distinct identities in a slice.
func distinct(ids []schema.ExplorerIdentity) int {
	seen := map[schema.ExplorerIdentity]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	return len(seen)
}

// medianOf returns the median of a sorted slice, averaging the two middle values for an even count.
func medianOf(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
