package mode

// This file is the COMPARE mode (design §3 Compare row) — one of the two FIXED-SPACE modes, and
// the clean counterexample to everything the emergent-space modes have to do:
//
//	declare          the USER fixes the option set and the criteria (with direction + role) up front
//	round 1 (BLIND)  every explorer evaluates the SAME options against the SAME criteria
//	HOST             the matrix, the per-cell agreement, the filter gate, the Pareto frontier — all host
//	collate          the collator writes NARRATIVE about a result it cannot change
//
// ONE round. NO canonicalizer. NO confirmation round. Not as a simplification, but because there is nothing
// for them to do: the option set was handed to the explorers, so recognizing "Postgres" as the option named
// "Postgres" is a string comparison, not the entity-resolution judgment §0 F-B exists to keep visible and
// contestable. Running a canonicalizer here would manufacture a contestable partition where none exists —
// and then a confirmation round to adjudicate disputes about it — which is a governance ceremony over an
// invented problem. The whole value of Compare in the model is that it shows what host-deterministic
// governance looks like when the space really is fixed.
//
// Three things the output refuses to do, all of them §3:
//
//   - It never averages a disagreed cell into one score. Every attributed value stays on the cell, the
//     split is labeled, and the definitive label is WITHHELD. A cell where two explorers said 2 and 9 is
//     the most informative thing in a comparison, and the mean of 5.5 is a number nobody believes.
//   - It never emits a scalar ranking the user did not license with weights. It says so, in the result.
//   - It never emits a partitionRevisionHash. There is no partition; the field carries the sentence saying
//     so instead of an empty string that would imply one.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// --- the terminal output (design §3 Compare row) ---

// CompareOutput is the FIXED, exploremesh-owned terminal output of the Compare mode: the consolidated
// options×criteria matrix with per-cell agreement, the host-computed Pareto frontier, the filter-gate
// exclusions, the explicit missing evidence, and the optional weighted ranking. It lives in this package
// rather than internal/schema because it embeds host-computed govern values, and govern sits above schema
// in the import graph.
type CompareOutput struct {
	// Space + SpaceNote are the honest rendering of what this result IS (host-authored, persisted with the
	// value): a FIXED-space comparison over a declared universe, with no entity resolution behind it.
	Space     string `json:"space"`
	SpaceNote string `json:"spaceNote"`
	// PartitionRevisionHash carries govern.FixedSpaceNoPartition — the statement that there is no partition,
	// rather than an empty field that would imply a blank one (§0 F-C: say what is reconstructable).
	PartitionRevisionHash string                    `json:"partitionRevisionHash"`
	Options               []string                  `json:"options"`
	Criteria              []schema.CompareCriterion `json:"criteria"`
	// Cells is the consolidated matrix — every declared cell, with EVERY attributed value the panel gave it.
	Cells []govern.CompareCell `json:"cells"`
	// Pareto is the HOST-computed non-dominated set over the scored dimensions, after the filter gate.
	Pareto           []govern.ParetoEntry        `json:"pareto"`
	Dominated        []govern.DominatedOption    `json:"dominated,omitempty"`
	ExcludedByFilter []govern.ExcludedOption     `json:"excludedByFilter,omitempty"`
	Incomparable     []govern.IncomparableOption `json:"incomparable,omitempty"`
	// MissingEvidence is the host's per-cell "nobody could judge this"; ReportedMissingEvidence carries the
	// explorers' own words about it.
	MissingEvidence         []govern.MissingEvidence      `json:"missingEvidence,omitempty"`
	ReportedMissingEvidence []govern.AttributedNote       `json:"reportedMissingEvidence,omitempty"`
	Unrecognized            []govern.AttributedEvaluation `json:"unrecognized,omitempty"`
	// ScalarRanking is present ONLY when the user weighted every scored criterion; otherwise it is nil and
	// RankingWithheld states why in the result itself, where a reader will actually see it.
	ScalarRanking   []govern.ScalarEntry `json:"scalarRanking,omitempty"`
	RankingWithheld string               `json:"rankingWithheld,omitempty"`
	// Rules are the versioned HOST rules every derived value above was computed under (§9).
	Rules CompareRules `json:"rules"`
	// PanelSize / Respondents are the participation facts behind the per-cell denominators.
	PanelSize   int `json:"panelSize"`
	Respondents int `json:"respondents"`
	// CollatorNarrative is the quarantined MODEL PROSE namespace (§0 F-C) — the collator's reading of the
	// matrix lives here and nowhere else.
	CollatorNarrative []govern.Narrative `json:"collatorNarrative,omitempty"`
}

// CompareRules names the versioned host rules a comparison was computed under, persisted with the value so
// a result read later is interpretable under the rules that produced it (§4/§9).
type CompareRules struct {
	RulesVersion    string `json:"rulesVersion"`
	CellPointRule   string `json:"cellPointRule"`
	FilterGateRule  string `json:"filterGateRule"`
	ParetoRule      string `json:"paretoRule"`
	ScalarRankRule  string `json:"scalarRankingRule,omitempty"`
	GovernanceRules string `json:"governanceRulesVersion"`
}

// Summary returns the one-line human summary. It leads with the frontier and the DISAGREED CELL COUNT, and
// it never says "the best option is" — a Pareto set is a set, and a comparison with no declared weights has
// no winner to name.
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

// Validate checks the comparison is usable: the declared space is present and the honest space rendering
// survived. A matrix with no cells is NOT an error — a panel that could judge nothing is a real outcome,
// reported as missing evidence — but a result that lost its "this is a fixed-space comparison with no
// partition" statement could be read as an emergent-space count, which it is not.
func (o CompareOutput) Validate() error {
	if len(o.Options) == 0 || len(o.Criteria) == 0 {
		return fmt.Errorf("compare output carries no declared option set or criteria")
	}
	if o.SpaceNote == "" || o.PartitionRevisionHash == "" {
		return fmt.Errorf("compare output carries no space rendering — a fixed-space result must state that no entity resolution was performed")
	}
	return nil
}

// --- the fixed-space contract ---

// compareCollator is the Compare mode's FixedSpaceContract: the host arithmetic, the narrative-only
// collator prompt, and the terminal view assembly.
type compareCollator struct{}

// Aggregate is the HOST step: it lifts every explorer's evaluations out of the immutable blind round-1
// envelopes and hands them to govern.BuildMatrix, which computes the cells, the per-cell agreement, the
// claims, the filter gate, the frontier and the optional weighted ranking. Nothing is computed here that
// govern does not compute — this function's whole job is projection + attribution.
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

// compareNarrativeWire is the SHAPE the collator is asked for: prose fields only. There is deliberately no
// numeric field in it, so a collator that tried to return a corrected score would have nowhere to put it.
type compareNarrativeWire struct {
	Reading           string   `json:"reading"`
	DisagreementNotes []string `json:"disagreementNotes"`
	EvidenceGaps      []string `json:"evidenceGaps"`
	Cautions          []string `json:"cautions"`
}

// CollatorPrompt shows the collator the finished matrix and asks for narrative. The matrix is rendered as
// JSON (the host's own bytes) so the collator reads exactly what the result carries, and the instruction
// names the two things a reader most needs explained: the SPLIT cells and the missing evidence.
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

// ParseNarrative reads the collator's output into the quarantined narrative namespace. An unparseable body
// is NOT an error: in a fixed-space mode the collator contributes prose only — every number is already
// computed — so a stumbling narrator degrades the PROSE, never the result. The failure is recorded as a
// host note in the same namespace, so the absence of narrative is visible rather than silent.
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

// Collate assembles the CompareOutput as a deterministic VIEW over the host matrix. Every number in it came
// from govern.BuildMatrix; this function copies and labels, and computes nothing.
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

// fixedSpaceNarrative is the shared narrative-parsing helper for both fixed-space modes: extract the single
// JSON object, hand it to the mode's own field reader, and turn the resulting prose into attributed
// Narrative entries. An extraction/decode failure yields ONE host note saying so — never an error (see
// ParseNarrative's comment), and never a fabricated narrative.
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
	// Compare (design §3 Compare row). Formulation-free like every registered mode, ONE round, and —
	// the whole point — no Canonicalization policy at all: the zero value means no canonicalizer and no
	// confirmation round, which is correct here rather than merely cheap (see the file comment).
	//
	// Class is FIXED-space, and unlike the emergent modes that is not the conservative default but a claim
	// the task input backs up: the key universe was handed to the explorers, so the degraded host register
	// keyed on `optionsEvaluated` is a genuine comparison rather than covert entity resolution (§1).
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
