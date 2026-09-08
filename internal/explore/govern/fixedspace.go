package govern

// This file is the HOST ARITHMETIC of the FIXED-SPACE modes (design §0 F-B, §3 Compare + Forecast rows,
// §4): the options×criteria matrix with PER-CELL agreement, the filter gate, the Pareto frontier, the
// optional weighted scalar ranking, and the governance claims all four pin themselves to.
//
// It lives in this package for the same reason the ballot tally does: every number here is a deterministic
// host rule over persisted artifacts, and none of it is anything a model asserted. What is different from
// the emergent-space arithmetic next door is what is ABSENT:
//
//   - THERE IS NO PARTITION. The option set and the criteria were declared by the user before any explorer
//     spoke, so matching "Postgres" to "Postgres" is a string comparison over a universe the host handed
//     out — not the entity-resolution judgment §0 F-B keeps visible and contestable. Emitting an empty
//     partitionRevisionHash would imply there was a partition and it happened to be blank, so a fixed-space
//     claim carries FixedSpaceNoPartition instead: a sentence saying plainly that no entity resolution was
//     performed, and why it was not needed.
//   - DISAGREEMENT IS NOT RESOLVED, IT IS REPORTED. Two explorers who score the same cell differently are
//     not a problem to be averaged away — they are the finding. Every attributed value stays on the cell,
//     the agreement verdict is host-computed, and the definitive label is WITHHELD for a split cell. The
//     cell's point estimate exists only so the Pareto rule has something to compare, and it is stamped with
//     the rule that produced it.
//   - THE ORDER OF OPERATIONS IS FIXED. A `filter` criterion excludes an option BEFORE dominance is
//     computed. A gate that could be traded off against a soft criterion is not a gate.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// The versioned HOST rules of the fixed-space path. Every one of them is stamped onto the value it
// produced, so a matrix read later is interpretable under the rules that built it rather than under
// whatever this file does today (§4/§9).
const (
	// FixedSpaceRulesVersion versions the fixed-space arithmetic as a whole.
	FixedSpaceRulesVersion = "exploremesh-fixed-space-rules@v1"
	// AgreementQuery is the versioned query id of a per-cell agreement claim.
	AgreementQuery = "cell-agreement-over-blind-round-1@v1"
	// EstimateQuery is the versioned query id of a pooled-estimate claim (Forecast).
	EstimateQuery = "pooled-estimate-over-blind-round-1@v1"
	// CellPointRule is how a cell's single comparable value is derived from the panel's blind values. It is
	// a DERIVED VIEW for the dominance rule only: the cell keeps every attributed value alongside it.
	CellPointRule = "median-of-blind-round-1-values@v1"
	// FilterGateRuleVersion is the gate rule: an option is EXCLUDED when at least one blind evaluation says
	// `fail` and NO blind evaluation says `pass`. Exclusion therefore requires no dissent — a split gate
	// leaves the option in the comparison and is surfaced as contested, because throwing an option out on a
	// disputed requirement is a decision the panel did not make.
	FilterGateRuleVersion = "host-filter-gate-unanimous-among-respondents@v1"
	// ParetoRuleVersion is the frontier rule: over the SCORED dimensions only, after the filter gate, option
	// A dominates B when A is at least as good as B on every dimension (under that dimension's declared
	// direction) and strictly better on at least one. The Pareto set is the non-dominated remainder.
	ParetoRuleVersion = "host-pareto-over-declared-dimensions@v1"
	// ScalarRankingRuleVersion is the OPTIONAL weighted ranking, computed ONLY when the user declared a
	// weight for every scored dimension: per-dimension min-max normalization oriented by direction (best =
	// 1), then a weight-weighted sum normalized by the total weight.
	ScalarRankingRuleVersion = "host-weighted-scalar@v1"
	// FixedSpaceNoPartition is what a fixed-space claim carries WHERE an emergent-space claim carries a
	// partitionRevisionHash. It is a sentence rather than an empty string on purpose (see the file comment).
	FixedSpaceNoPartition = "none — fixed space: the option set and criteria were declared by the user before any explorer spoke, so no entity resolution was performed and there is no partition to pin"
)

// LabelWithheldDisagreement is the fixed-space counterpart of LabelWithheldContested: two or more
// blind round-1 sources evaluated this cell and they DISAGREE. The definitive label is withheld and the
// disagreement is reported per cell — averaging it into one number would delete the finding. It is a
// distinct label from LabelWithheldContested because the two mean different things: a contested PARTITION
// is a dispute about what the entities are, and this is a dispute about a value, in a space nobody disputes.
const LabelWithheldDisagreement Label = "withheld_cell_disagreement"

// Agreement is the host's per-cell verdict over the blind round-1 evaluations of ONE cell.
type Agreement string

const (
	// AgreementUnanimous: two or more sources evaluated the cell and every one of them reported the same
	// value (or the same gate verdict).
	AgreementUnanimous Agreement = "unanimous"
	// AgreementSplit: two or more sources evaluated the cell and they do NOT agree. This is SIGNAL.
	AgreementSplit Agreement = "split"
	// AgreementSingleSource: exactly one source evaluated the cell — salience, not corroboration.
	AgreementSingleSource Agreement = "single_source"
	// AgreementNoEvidence: nobody produced a usable value for the cell. It is a real, distinct outcome and
	// is reported as missing evidence, never as a zero.
	AgreementNoEvidence Agreement = "no_evidence"
)

// AttributedEvaluation is ONE explorer's evaluation of ONE cell, attributed to the blind round-1 envelope
// it came from. The raw value the explorer wrote is retained verbatim next to the host's numeric reading.
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

// AttributedNote is one explorer's own free-text note (a stated missing-evidence line), carried in the
// record with its author. It is MODEL PROSE and carries no governance value — it is kept because "nobody
// could judge this and here is why" is worth more to a reader than an empty cell.
type AttributedNote struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Note        string                  `json:"note"`
}

// CompareCell is ONE options×criteria cell as the host computed it: every attributed value the panel
// produced, the host's agreement verdict, the observed spread, the derived point estimate (dimensions) or
// gate verdict (filters), and the pinned governance claim.
type CompareCell struct {
	Option    string               `json:"option"`
	Criterion string               `json:"criterion"`
	Role      schema.CriterionRole `json:"role"`
	Direction schema.Direction     `json:"direction,omitempty"`
	Agreement Agreement            `json:"agreement"`
	// Sources is the number of DISTINCT blind round-1 explorers that produced a usable value for this cell.
	Sources int `json:"sources"`
	// Values are EVERY attributed evaluation of this cell, in panel order. They are the cell — the
	// aggregates below are views over them, and nothing here is ever collapsed into a single number that
	// hides a disagreement.
	Values []AttributedEvaluation `json:"values"`
	// PointEstimate is the cell's single comparable value under CellPointRule (scored dimensions only). It
	// exists so dominance has something to compare; HasPoint is false when no usable value was reported.
	PointEstimate float64 `json:"pointEstimate,omitempty"`
	HasPoint      bool    `json:"hasPoint,omitempty"`
	Low           float64 `json:"low,omitempty"`
	High          float64 `json:"high,omitempty"`
	Spread        float64 `json:"spread,omitempty"`
	// Verdict is the host's gate verdict under FilterGateRuleVersion (filter criteria only).
	Verdict schema.FilterVerdict `json:"verdict,omitempty"`
	Passes  int                  `json:"passes,omitempty"`
	Fails   int                  `json:"fails,omitempty"`
	// Contested marks a filter cell whose respondents split pass/fail — the option is NOT excluded on a
	// disputed requirement, and the dispute is surfaced instead.
	Contested bool `json:"contested,omitempty"`
	// Claim is the pinned governance claim for this cell (absent for a no-evidence cell, which is reported
	// as missing evidence — a claim about nothing would be a claim).
	Claim *Claim `json:"claim,omitempty"`
}

// Disagrees reports whether the cell's blind sources split — the property the renderers lead with.
func (c CompareCell) Disagrees() bool { return c.Agreement == AgreementSplit }

// MissingEvidence is one cell the panel could not fill, with the host's reason and any explorer notes that
// named it. It is a first-class part of the result (§3 Compare row: "explicit missing-evidence").
type MissingEvidence struct {
	Option    string `json:"option"`
	Criterion string `json:"criterion"`
	Reason    string `json:"reason"`
}

// ExcludedOption is an option the FILTER GATE removed before dominance, with the gate that removed it.
type ExcludedOption struct {
	Option    string `json:"option"`
	Criterion string `json:"criterion"`
	Reason    string `json:"reason"`
}

// IncomparableOption is an option that survived the gate but cannot be placed on the frontier, because at
// least one scored dimension has no usable value for it. It is carried rather than dropped: an option
// nobody could score is not an option that lost.
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
	// Dominates names the options this one dominates (informative, not a ranking — dominance is a partial
	// order and the frontier is deliberately unordered).
	Dominates []string `json:"dominates,omitempty"`
	// Conditional marks a frontier position that rides on at least one SPLIT cell: the panel disagreed
	// about a value the dominance comparison used, so the position is conditional on which value holds.
	Conditional bool `json:"conditional,omitempty"`
	// DisagreedCells names the split cells behind Conditional.
	DisagreedCells []string `json:"disagreedCells,omitempty"`
}

// DominatedOption is an option the frontier excluded by dominance, carried with WHO dominates it — a
// dominated option is a real answer to "why not this one", and dropping it would lose that answer.
type DominatedOption struct {
	Option      string             `json:"option"`
	Scores      map[string]float64 `json:"scores"`
	DominatedBy []string           `json:"dominatedBy"`
	Reason      string             `json:"reason"`
}

// ScalarEntry is one option's position in the OPTIONAL weighted scalar ranking (present only when the user
// declared a weight for every scored dimension).
type ScalarEntry struct {
	Rank   int     `json:"rank"`
	Option string  `json:"option"`
	Score  float64 `json:"score"`
}

// CompareMatrix is the whole host-computed comparison (design §3 Compare row): the declared space, every
// cell, the gate outcome, the frontier, the optional weighted ranking, the explicit missing evidence, and
// the claims. It is assembled by BuildMatrix and rendered by the mode as a pure view — nothing downstream
// recomputes any of it.
type CompareMatrix struct {
	RulesVersion             string `json:"rulesVersion"`
	CellPointRule            string `json:"cellPointRule"`
	FilterGateRuleVersion    string `json:"filterGateRuleVersion"`
	ParetoRuleVersion        string `json:"paretoRuleVersion"`
	ScalarRankingRuleVersion string `json:"scalarRankingRuleVersion,omitempty"`
	// PartitionRevisionHash carries FixedSpaceNoPartition — the honest statement that there is no partition
	// (see the file comment). It is never an empty string.
	PartitionRevisionHash string                    `json:"partitionRevisionHash"`
	Options               []string                  `json:"options"`
	Criteria              []schema.CompareCriterion `json:"criteria"`
	Cells                 []CompareCell             `json:"cells"`
	Pareto                []ParetoEntry             `json:"pareto"`
	Dominated             []DominatedOption         `json:"dominated,omitempty"`
	ExcludedByFilter      []ExcludedOption          `json:"excludedByFilter,omitempty"`
	Incomparable          []IncomparableOption      `json:"incomparable,omitempty"`
	MissingEvidence       []MissingEvidence         `json:"missingEvidence,omitempty"`
	// ReportedMissingEvidence are the explorers' OWN "I could not judge this" notes (model prose, attributed).
	ReportedMissingEvidence []AttributedNote `json:"reportedMissingEvidence,omitempty"`
	// Unrecognized are evaluations naming an option or criterion OUTSIDE the declared space. They are
	// recorded, never re-attached to the nearest declared name: guessing what an explorer meant would be
	// exactly the entity resolution this mode does not do (§0 F-B).
	Unrecognized []AttributedEvaluation `json:"unrecognized,omitempty"`
	// ScalarRanking is nil unless the user weighted every scored dimension; RankingWithheld then says why.
	ScalarRanking   []ScalarEntry `json:"scalarRanking,omitempty"`
	RankingWithheld string        `json:"rankingWithheld,omitempty"`
	// Claims are the per-cell governance claims, in cell order.
	Claims []Claim `json:"claims"`
}

// DisagreedCells counts the cells whose blind sources split — the headline number of a comparison.
func (m CompareMatrix) DisagreedCells() int {
	n := 0
	for _, c := range m.Cells {
		if c.Disagrees() {
			n++
		}
	}
	return n
}

// cellKey identifies ONE options×criteria cell inside this file's computations (the declared option and
// criterion names, in their declared spelling).
type cellKey struct{ option, criterion string }

// MatrixInput is everything BuildMatrix needs. Every field is a host artifact or a user declaration; the
// Baseline can only be a blind round 1, so an evaluation from a later round cannot enter a cell.
type MatrixInput struct {
	Options         []string
	Criteria        []schema.CompareCriterion
	Baseline        BlindBaseline
	Evaluations     []AttributedEvaluation
	Notes           []AttributedNote
	Panel           Panel
	FormulationHash string
}

// BuildMatrix computes the whole comparison from the recorded blind round-1 evaluations (design §3 Compare
// row). The order of operations is the contract: place evaluations into DECLARED cells → compute per-cell
// agreement + claims → apply the FILTER GATE → compute the Pareto frontier over the scored dimensions →
// compute the weighted ranking ONLY if the user declared weights.
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

	// -- Place every evaluation into a DECLARED cell. Matching is exact on the trimmed, case-folded name:
	// the host handed these names out, so recognizing them back is arithmetic. An evaluation that names
	// something else is UNRECOGNIZED and is recorded as such — never mapped onto the nearest declared name.
	optionOf := declaredIndex(in.Options)
	criterionName := make(map[string]string, len(in.Criteria))
	for _, c := range in.Criteria {
		criterionName[normalizeName(c.Name)] = c.Name
	}
	byCell := map[cellKey][]AttributedEvaluation{}
	for _, ev := range in.Evaluations {
		// ANTI-ECHO (§0 F-A): only the immutable blind round-1 artifacts feed a cell. A fixed-space mode has
		// one round, so this filter is belt-and-braces — and it is exactly the kind of invariant that must
		// not depend on the mode contract remembering to be single-round.
		if !in.Baseline.Contains(ev.EnvelopeRef) {
			continue
		}
		opt, optOK := optionOf[normalizeName(ev.Option)]
		crit, critOK := criterionName[normalizeName(ev.Criterion)]
		if !optOK || !critOK {
			m.Unrecognized = append(m.Unrecognized, ev)
			continue
		}
		ev.Option, ev.Criterion = opt, crit // canonical DECLARED spelling, so the record reads consistently
		byCell[cellKey{opt, crit}] = append(byCell[cellKey{opt, crit}], ev)
	}

	// -- Per-cell agreement + claims, in declared (option, criterion) order so the matrix always serializes
	// identically for the same inputs.
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

	// -- The FILTER GATE, applied BEFORE dominance (see the file comment).
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

	// -- Comparability: an option missing a value on any scored dimension cannot be placed on the frontier.
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

	// -- The Pareto frontier over the scored dimensions, honoring each dimension's declared direction.
	m.Pareto, m.Dominated = paretoFrontier(comparable, dims, points, split)
	m.ScalarRanking, m.RankingWithheld = scalarRanking(comparable, dims, points)
	if m.ScalarRanking != nil {
		m.ScalarRankingRuleVersion = ScalarRankingRuleVersion
	}
	return m, nil
}

// buildCell computes ONE cell: the agreement verdict, the derived point estimate or gate verdict, and the
// pinned claim. A cell with no usable value gets NO claim — it is reported as missing evidence instead,
// because a claim whose value is "nobody said anything" is a claim about the host, not the panel.
func buildCell(option string, crit schema.CompareCriterion, evals []AttributedEvaluation, panel Panel, baseline BlindBaseline, formulationHash string) CompareCell {
	cell := CompareCell{
		Option: option, Criterion: crit.Name, Role: crit.EffectiveRole(),
		Values: append([]AttributedEvaluation(nil), evals...),
	}
	if cell.Role == schema.RoleDimension {
		cell.Direction = crit.Direction
	}
	// k counts DISTINCT source explorers, never evaluations: an explorer that scored the same cell twice
	// has one opinion about it.
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
		return cell // no claim: see the function comment
	case cell.Sources == 1:
		cell.Agreement = AgreementSingleSource
	case len(groups) == 1:
		cell.Agreement = AgreementUnanimous
	default:
		cell.Agreement = AgreementSplit
	}

	if cell.Role == schema.RoleFilter {
		// The GATE rule: exclusion requires no dissent (FilterGateRuleVersion).
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

	// k = the size of the LARGEST AGREEING GROUP of distinct sources. For a unanimous cell that is every
	// respondent; for a split cell it is the modal position — reported so the split is visible as an
	// arithmetic fact, and labeled WITHHELD so it can never read as agreement.
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

// CellSubject is the stable subject id of a cell claim (`<option> / <criterion>`) — the same string the
// derived SQLite export keys a claim's subject on.
func CellSubject(option, criterion string) string { return option + " / " + criterion }

// paretoFrontier computes the non-dominated set over the scored dimensions, honoring each dimension's
// declared direction (ParetoRuleVersion). It returns the frontier and the dominated remainder, both with
// the option's per-dimension point estimates so a reader can check the arithmetic.
func paretoFrontier(options []string, dims []schema.CompareCriterion, points map[cellKey]float64, split map[cellKey]bool) ([]ParetoEntry, []DominatedOption) {
	scoresOf := func(opt string) map[string]float64 {
		out := make(map[string]float64, len(dims))
		for _, d := range dims {
			out[d.Name] = points[cellKey{opt, d.Name}]
		}
		return out
	}
	// dominates reports whether a is at least as good as b on EVERY dimension and strictly better on one.
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

// scalarRanking computes the OPTIONAL weighted ranking, and returns the honest refusal when it may not.
// §3 is explicit: no scalar ranking unless the user supplied weights. A single number over several criteria
// IS a weighting, so producing one from unweighted criteria would be the host quietly declaring what
// matters most — which is the user's call and nobody else's.
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
	// Per-dimension min-max normalization ORIENTED BY DIRECTION (best = 1). A dimension on which every
	// option is equal contributes 1 to all of them: it separates nothing, and normalizing it to 0 would
	// silently penalize every option for a criterion none of them lost on.
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
	// Deterministic ordering: score, then option name (the name is determinism, not standing).
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

// --- Forecast (design §3 Forecast row) ---

// EstimateClaimInput is everything a pooled-estimate claim needs. The pooled NUMBERS live on the mode's
// output (computed in internal/pool); the claim counts SOURCES, like every other claim in this package —
// so "how many independent forecasters is this aggregate built on" is answerable without reading the prose.
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

// EstimateClaim pins the pooled forecast to the evidence it was computed from: how many DISTINCT blind
// round-1 forecasters contributed an estimate, both denominators, the frozen policy, and the exact
// contributing envelope refs. Like every fixed-space claim it carries FixedSpaceNoPartition rather than an
// empty partition hash.
func EstimateClaim(in EstimateClaimInput) (Claim, error) {
	if in.Baseline.Size() == 0 {
		return Claim{}, fmt.Errorf("estimate claim: no blind round-1 baseline (build it with NewBlindBaseline)")
	}
	refs := map[string]bool{}
	for _, r := range in.EnvelopeRefs {
		// The anti-echo filter, applied here too: only a blind round-1 estimate may back the aggregate.
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

// --- small shared helpers ---

// declaredIndex maps each declared value's normalized form back to its DECLARED spelling.
func declaredIndex(values []string) map[string]string {
	out := make(map[string]string, len(values))
	for _, v := range values {
		out[normalizeName(v)] = v
	}
	return out
}

// normalizeName folds a declared name for matching: trimmed + lower-cased. Nothing else — no stemming, no
// fuzzy distance. A fuzzy match over a DECLARED universe would reintroduce exactly the judgment this mode
// is free of.
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

// medianOf returns the median of a SORTED slice (the mean of the two middle values for an even count —
// the same standard definition internal/pool states, applied to a cell's blind values).
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
