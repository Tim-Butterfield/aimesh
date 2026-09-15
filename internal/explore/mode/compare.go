package mode

// This file implements the Compare mode, a fixed-space mode:
//
//	declare          the user fixes the options and criteria
//	round 1 (blind)  every explorer evaluates the same options against the same criteria
//	host             computes the matrix, per-cell agreement, filter gate and Pareto frontier
//	collate          the collator adds narrative
//
// There is one round and no canonicalization: the options are declared, so matching them is a string
// comparison rather than entity resolution.
//
// The output never averages a disagreed cell (every value stays on the cell and the cell is labeled
// split), never ranks without user-declared weights, and states in place of a partition hash that there is
// no partition.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// CompareOutput is the Compare mode's output: the options×criteria matrix with per-cell agreement, the
// Pareto frontier, filter exclusions, missing evidence and an optional weighted ranking. It is defined here
// rather than in package schema because it uses govern types.
type CompareOutput struct {
	// Space and SpaceNote state that this is a fixed-space comparison with no entity resolution.
	Space     string `json:"space"`
	SpaceNote string `json:"spaceNote"`
	// PartitionRevisionHash holds govern.FixedSpaceNoPartition.
	PartitionRevisionHash string                    `json:"partitionRevisionHash"`
	Options               []string                  `json:"options"`
	Criteria              []schema.CompareCriterion `json:"criteria"`
	// Cells holds every declared cell with every attributed value.
	Cells []govern.CompareCell `json:"cells"`
	// Pareto is the non-dominated set over the scored criteria, after the filter gate.
	Pareto           []govern.ParetoEntry        `json:"pareto"`
	Dominated        []govern.DominatedOption    `json:"dominated,omitempty"`
	ExcludedByFilter []govern.ExcludedOption     `json:"excludedByFilter,omitempty"`
	Incomparable     []govern.IncomparableOption `json:"incomparable,omitempty"`
	// MissingEvidence lists cells no explorer could judge; ReportedMissingEvidence holds the explorers' notes.
	MissingEvidence         []govern.MissingEvidence      `json:"missingEvidence,omitempty"`
	ReportedMissingEvidence []govern.AttributedNote       `json:"reportedMissingEvidence,omitempty"`
	Unrecognized            []govern.AttributedEvaluation `json:"unrecognized,omitempty"`
	// ScalarRanking is set only when every scored criterion has a weight; otherwise RankingWithheld says why.
	ScalarRanking   []govern.ScalarEntry `json:"scalarRanking,omitempty"`
	RankingWithheld string               `json:"rankingWithheld,omitempty"`
	// Rules are the rule versions the values were computed under.
	Rules CompareRules `json:"rules"`
	// PanelSize and Respondents are the per-cell denominators.
	PanelSize   int `json:"panelSize"`
	Respondents int `json:"respondents"`
	// CollatorNarrative holds the collator's prose.
	CollatorNarrative []govern.Narrative `json:"collatorNarrative,omitempty"`
}

// CompareRules records the rule versions a comparison was computed under.
type CompareRules struct {
	RulesVersion    string `json:"rulesVersion"`
	CellPointRule   string `json:"cellPointRule"`
	FilterGateRule  string `json:"filterGateRule"`
	ParetoRule      string `json:"paretoRule"`
	ScalarRankRule  string `json:"scalarRankingRule,omitempty"`
	GovernanceRules string `json:"governanceRulesVersion"`
}

// Summary returns a one-line summary of the frontier, disagreements and ranking status. It names no winner.
func (o CompareOutput) Summary() string {
	disagreed := 0
	for _, c := range o.Cells {
		if c.Disagrees() {
			disagreed++
		}
	}
	ranking := "no scalar ranking (no weights declared)"
	if len(o.ScalarRanking) > 0 {
		ranking = fmt.Sprintf("weighted ranking of %d option(s) under the user's declared weights", len(o.ScalarRanking))
	}
	return fmt.Sprintf("compare: %d option(s) × %d criterion(a) — %d on the host Pareto frontier, %d dominated, %d excluded by a filter gate, %d incomparable; %d cell(s) with cross-explorer DISAGREEMENT (surfaced, not averaged), %d cell(s) with no evidence; %s [fixed space: no partition]",
		len(o.Options), len(o.Criteria), len(o.Pareto), len(o.Dominated), len(o.ExcludedByFilter),
		len(o.Incomparable), disagreed, len(o.MissingEvidence), ranking)
}

// Validate requires the declared options and criteria and the fixed-space note. An empty matrix is valid and
// is reported as missing evidence.
func (o CompareOutput) Validate() error {
	if len(o.Options) == 0 || len(o.Criteria) == 0 {
		return fmt.Errorf("compare output carries no declared option set or criteria")
	}
	if o.SpaceNote == "" || o.PartitionRevisionHash == "" {
		return fmt.Errorf("compare output carries no space rendering — a fixed-space result must state that no entity resolution was performed")
	}
	return nil
}

// compareCollator is the Compare mode's FixedSpaceContract.
type compareCollator struct{}

// Aggregate extracts the attributed evaluations and notes from the blind round-1 envelopes and builds the
// matrix with govern.BuildMatrix.
func (compareCollator) Aggregate(in FixedSpaceInput) (FixedSpaceView, error) {
	var evals []govern.AttributedEvaluation
	var notes []govern.AttributedNote
	for _, env := range in.Primary {
		ref := schema.EnvelopeRef(1, env.Order)
		for _, ev := range schema.ParseEvaluations(env.Response) {
			evals = append(evals, govern.AttributedEvaluation{
				Explorer: env.Identity, EnvelopeRef: ref,
				Option: ev.Option, Criterion: ev.Criterion, Value: ev.Value,
				Score: ev.Score, HasScore: ev.HasScore, Verdict: ev.Verdict, Rationale: ev.Rationale,
			})
		}
		for _, n := range schema.ParseMissingEvidence(env.Response) {
			notes = append(notes, govern.AttributedNote{Explorer: env.Identity, EnvelopeRef: ref, Note: n})
		}
	}
	matrix, err := govern.BuildMatrix(govern.MatrixInput{
		Options: schema.NonBlank(in.Raw.Options), Criteria: in.Raw.CompareCriteria,
		Baseline: in.Baseline, Evaluations: evals, Notes: notes,
		Panel: in.Panel, FormulationHash: in.FormulationHash,
	})
	if err != nil {
		return FixedSpaceView{}, err
	}
	return FixedSpaceView{Value: matrix, Claims: matrix.Claims}, nil
}

// compareNarrativeWire is the collator's response shape. It has no numeric fields.
type compareNarrativeWire struct {
	Reading           string   `json:"reading"`
	DisagreementNotes []string `json:"disagreementNotes"`
	EvidenceGaps      []string `json:"evidenceGaps"`
	Cautions          []string `json:"cautions"`
}

// CollatorPrompt shows the collator the matrix as JSON and asks for narrative about split cells and missing
// evidence.
func (compareCollator) CollatorPrompt(in FixedSpaceInput, view FixedSpaceView) (string, error) {
	matrix, ok := view.Value.(govern.CompareMatrix)
	if !ok {
		return "", fmt.Errorf("compare: host view is %T, not a govern.CompareMatrix", view.Value)
	}
	b, err := json.Marshal(matrix)
	if err != nil {
		return "", fmt.Errorf("marshal compare matrix: %w", err)
	}
	var s strings.Builder
	s.WriteString(fixedSpaceCollatorPreamble)
	s.WriteString("\n\nThis is a FIXED-SPACE comparison: the option set and the criteria were declared by the ")
	s.WriteString("requester BEFORE any evaluator was asked, so no entity resolution was performed and there ")
	s.WriteString("is no partition. The frontier below is a Pareto set, not a ranking — do NOT name a winner, ")
	s.WriteString("and do NOT invent one from the criteria you personally find most important.\n\n")
	if len(matrix.ScalarRanking) == 0 {
		s.WriteString("NOTE: the requester declared no weights, so the host computed NO scalar ranking. Do not ")
		s.WriteString("produce one. Explain the trade-offs instead.\n\n")
	}
	s.WriteString("Pay particular attention to the cells whose agreement is \"split\": the evaluators DISAGREED ")
	s.WriteString("there, every one of their values is recorded, and that disagreement is the most useful thing ")
	s.WriteString("in this result. Explain what the disagreement is ABOUT and what it would take to settle it — ")
	s.WriteString("do not smooth it over, and do not pick a side on the numbers.\n\n")
	s.WriteString("Purpose of the comparison:\n")
	s.WriteString(in.Raw.Purpose)
	s.WriteString("\n\nOutput ONLY a SINGLE JSON object — no prose before or after it, and no markdown code ")
	s.WriteString("fences. It MUST have EXACTLY these fields:\n")
	s.WriteString("- \"reading\": string — what this comparison shows, in plain language.\n")
	s.WriteString("- \"disagreementNotes\": array of strings — one entry per split cell you can speak to: what ")
	s.WriteString("the evaluators disagree about and why it matters.\n")
	s.WriteString("- \"evidenceGaps\": array of strings — what is missing and what it would take to fill it.\n")
	s.WriteString("- \"cautions\": array of strings — what a reader should be careful about.\n\n")
	s.WriteString("host-computed comparison:\n")
	s.Write(b)
	s.WriteString("\n")
	return s.String(), nil
}

// ParseNarrative converts the collator's output into narrative. Unusable output yields a host note rather
// than an error, since the result is already computed.
func (compareCollator) ParseNarrative(raw []byte, by schema.ExplorerIdentity) ([]govern.Narrative, error) {
	return fixedSpaceNarrative(raw, by, func(obj []byte) ([]string, error) {
		var w compareNarrativeWire
		if err := json.Unmarshal(obj, &w); err != nil {
			return nil, err
		}
		out := make([]string, 0, 4)
		if strings.TrimSpace(w.Reading) != "" {
			out = append(out, w.Reading)
		}
		for _, n := range w.DisagreementNotes {
			if strings.TrimSpace(n) != "" {
				out = append(out, "disagreement: "+n)
			}
		}
		for _, g := range w.EvidenceGaps {
			if strings.TrimSpace(g) != "" {
				out = append(out, "evidence gap: "+g)
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

// Collate builds the CompareOutput from the host matrix and narrative.
func (compareCollator) Collate(in FixedSpaceInput, view FixedSpaceView, narrative []govern.Narrative) (ModeOutput, error) {
	matrix, ok := view.Value.(govern.CompareMatrix)
	if !ok {
		return nil, fmt.Errorf("compare: host view is %T, not a govern.CompareMatrix", view.Value)
	}
	out := CompareOutput{
		Space: string(schema.FixedSpace),
		SpaceNote: fmt.Sprintf(
			"FIXED-SPACE comparison: the %d option(s) and %d criterion(a) were DECLARED by the requester before any explorer was asked, so no entity resolution was performed, no canonicalization ran, and there is no partition to pin. Every count below is a host count over the immutable blind round-1 responses, at cell level.",
			len(matrix.Options), len(matrix.Criteria)),
		PartitionRevisionHash:   matrix.PartitionRevisionHash,
		Options:                 matrix.Options,
		Criteria:                matrix.Criteria,
		Cells:                   matrix.Cells,
		Pareto:                  matrix.Pareto,
		Dominated:               matrix.Dominated,
		ExcludedByFilter:        matrix.ExcludedByFilter,
		Incomparable:            matrix.Incomparable,
		MissingEvidence:         matrix.MissingEvidence,
		ReportedMissingEvidence: matrix.ReportedMissingEvidence,
		Unrecognized:            matrix.Unrecognized,
		ScalarRanking:           matrix.ScalarRanking,
		RankingWithheld:         matrix.RankingWithheld,
		Rules: CompareRules{
			RulesVersion: matrix.RulesVersion, CellPointRule: matrix.CellPointRule,
			FilterGateRule: matrix.FilterGateRuleVersion, ParetoRule: matrix.ParetoRuleVersion,
			ScalarRankRule: matrix.ScalarRankingRuleVersion, GovernanceRules: govern.RulesVersion,
		},
		PanelSize:         in.Panel.Selected,
		Respondents:       in.Panel.Respondents(),
		CollatorNarrative: append([]govern.Narrative(nil), narrative...),
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// fixedSpaceNarrative extracts the collator's JSON object, reads its prose lines with read, and attributes
// them to by. Any failure yields a single host note instead of an error.
func fixedSpaceNarrative(raw []byte, by schema.ExplorerIdentity, read func([]byte) ([]string, error)) ([]govern.Narrative, error) {
	fail := func(reason string) []govern.Narrative {
		return []govern.Narrative{{
			Source: by, Phase: schema.PhaseSynthesize,
			Prose: "[host note] the collator's narrative was unusable (" + reason + "). Every value in this result was computed by the host before the collator was called, so the RESULT is unaffected — only its explanation is missing.",
		}}
	}
	obj, _, xerr := schema.ExtractJSONObject(raw)
	if xerr != nil {
		return fail(xerr.Error()), nil
	}
	lines, err := read(obj)
	if err != nil {
		return fail(err.Error()), nil
	}
	if len(lines) == 0 {
		return fail("it contained no narrative"), nil
	}
	out := make([]govern.Narrative, 0, len(lines))
	for _, l := range lines {
		out = append(out, govern.Narrative{Source: by, Phase: schema.PhaseSynthesize, Prose: l})
	}
	return out, nil
}

func init() {
	register(ModeSpec{
		Name:               Compare,
		FormulationFree:    true,
		Prompt:             schema.CompareExplorerPrompt,
		ExplorerSchema:     schema.CompareExplorerSchema,
		Objective:          ObjectiveHostAggregateCollate,
		FixedSpace:         compareCollator{},
		ValidateTask:       schema.ValidateCompareTask,
		Class:              schema.FixedSpace,
		FixedSpaceKeyField: "optionsEvaluated",
	})
}
