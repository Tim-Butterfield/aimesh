// Package runview is the SURFACE-NEUTRAL projection of a finished exploration: the governance summary,
// the per-mode result detail, the identity caveats, and the halt breakdown — the facts every non-terminal
// surface has to hand back verbatim.
//
// It exists because of the surface-parity invariant (mcp-design.md §Surface-parity): ACP and MCP must
// report the SAME governance block for the same run. Two hand-written projections would agree on the day
// they were written and drift afterwards, and the drift would be invisible — a driver reading a claim
// count with no withheld tally beside it presents a contested result as a settled one. Computing it once
// makes that impossible rather than merely discouraged.
//
// Everything here is a pure function over pipeline.Result: no I/O, no formatting decisions beyond the
// map/key shape, and no surface-specific vocabulary (each surface decides where the maps are attached —
// ACP's `_meta.exploremesh`, MCP's `structuredContent`).
package runview

import (
	"encoding/json"
	"errors"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Governance is the GOVERNANCE SUMMARY a surface must hand back for every run that produced counts
// (design §4/§9): the claim tally by label, the hashes every count is pinned to, and any
// withheld/contested signal. It is nil for a mode that emits no counts, so a Map/Synthesize/Catalog
// response carries no governance key at all.
//
// The withheld/contested fields are here deliberately. A driver that reads only a claim COUNT would
// present a contested or below-quorum result as a settled one; surfacing the withheld tally next to it
// makes that take active effort rather than inattention.
func Governance(out pipeline.Result) map[string]any {
	if out.Governance == nil {
		return nil
	}
	g := out.Governance
	byLabel := map[string]int{}
	withheld, contested := 0, 0
	for _, c := range g.Claims {
		byLabel[string(c.Label)]++
		if !c.Label.Definitive() {
			withheld++
		}
		if c.Sensitivity != nil {
			contested++
		}
	}
	meta := map[string]any{
		"summary":            g.Summary(),
		"claims":             len(g.Claims),
		"claimsByLabel":      byLabel,
		"claimsWithheld":     withheld,
		"claimsContested":    contested,
		"claimsHash":         g.ClaimsHash,
		"rulesVersion":       g.RulesVersion,
		"countingPolicyHash": g.Panel.PolicyHash,
		"panelSize":          g.Panel.Selected,
		"respondents":        g.Panel.Respondents(),
		"quorumMet":          g.Panel.QuorumMet(),
		"collatorNarrative":  len(g.CollatorNarrative),
	}
	if out.Canonicalization != nil {
		meta["partitionRevisionHash"] = out.Canonicalization.PartitionRevisionHash
		meta["agreementRuleVersion"] = out.Canonicalization.AgreementRuleVersion
	} else if fixedSpace(g) {
		// A FIXED-SPACE run echoes the explicit no-partition statement rather than omitting the field: an
		// absent key and an honest "there is no partition" are different facts, and a driver must be able
		// to tell "no entity resolution was needed" from "the field was not reported" (design §0 F-C).
		meta["partitionRevisionHash"] = govern.FixedSpaceNoPartition
		meta["entityResolution"] = "none — the space was declared before any explorer spoke"
	}
	if out.Confirmation != nil {
		meta["confirmationRule"] = out.Confirmation.RuleVersion
		meta["challenges"] = len(out.Confirmation.Challenges)
		meta["contestedMappings"] = len(out.Confirmation.Contested)
		meta["settled"] = out.Confirmation.Settled()
	}
	if out.Decision != nil {
		meta["decisionRule"] = out.Decision.RuleVersion
		meta["decisionInputsHash"] = out.Decision.Frozen.InputsHash
		meta["ballotsCast"] = out.Decision.Cast
	}
	return meta
}

// fixedSpace reports whether a governance report's claims are FIXED-SPACE ones — read from the claims,
// not from the mode name, so the echo follows the governance record.
func fixedSpace(g *govern.Report) bool {
	for _, c := range g.Claims {
		if c.PartitionRevisionHash == govern.FixedSpaceNoPartition {
			return true
		}
	}
	return false
}

// Shape is the DRY RUN's pre-spend disclosure — what the exploration would do — or nil for a real run,
// whose shape stopped being a prediction the moment it started. A surface attaches it unconditionally and
// branches on nil.
//
// It round-trips pipeline.Shape through its own JSON tags rather than re-listing the fields: the wire form
// IS that type, and a hand-written copy per surface is a second and a third place to forget a field when
// the pipeline gains a stage. That is the drift this package exists to prevent.
func Shape(out pipeline.Result) map[string]any {
	if out.Shape == nil {
		return nil
	}
	b, err := json.Marshal(out.Shape)
	if err != nil {
		// Unreachable for a struct of strings, ints and bools — but a shape that cannot be projected must SAY
		// so rather than become an empty object, which reads exactly like a run that costs nothing.
		return map[string]any{"error": "the run shape could not be projected: " + err.Error()}
	}
	var m map[string]any
	if uerr := json.Unmarshal(b, &m); uerr != nil {
		return map[string]any{"error": "the run shape could not be projected: " + uerr.Error()}
	}
	return m
}

// ModeDetail is the terminal output's per-mode structured detail (the terminal output is per-mode),
// plus the generic one-line `summary` every mode has. It returns nil when the run produced no output (a
// halt before the collate step), so a surface can attach it unconditionally.
//
// The honesty pairs are non-negotiable and are the reason this lives in one place: a ballot's `decision`
// label travels with its counts, a comparison's `disagreedCells` and `rankingWithheld` travel with its
// frontier, and a forecast's `computedBy`/`poolingRule` travel with its aggregate. A driver must not be
// able to present a ballot as consensus, a disputed cell as a settled trade-off, or a host-pooled number
// as a model's answer without going out of its way.
func ModeDetail(out pipeline.Result) map[string]any {
	if out.Output == nil {
		return nil
	}
	em := map[string]any{"summary": out.Output.Summary()}
	switch o := out.Output.(type) {
	case schema.CollatorOutput:
		em["findings"] = len(o.Findings)
		em["disagreements"] = len(o.DisagreementRegister)
		em["synthesisSummary"] = o.SynthesisSummary
	case schema.SynthesizeOutput:
		em["artifact"] = o.Artifact
		em["componentProvenance"] = len(o.ComponentProvenance)
		em["minorityReport"] = len(o.MinorityReport)
	case mode.ChallengeOutput:
		em["registerEntries"] = len(o.Register)
		em["survivingStrengths"] = len(o.SurvivingStrengths)
		em["artifactDigest"] = o.ArtifactDigest
	case mode.ShortlistOutput:
		em["ranked"] = len(o.Ranked)
		em["rejected"] = len(o.Rejects)
		em["ballotsCast"] = o.BallotsCast
		em["quorumMet"] = o.QuorumMet
		em["decision"] = o.Decision
		em["decisionInputsHash"] = o.DecisionInputsHash
		if o.TieOutcome != "" {
			em["tieOutcome"] = o.TieOutcome
		}
	case mode.CompareOutput:
		em["options"] = len(o.Options)
		em["compareCriteria"] = len(o.Criteria)
		em["pareto"] = paretoOptions(o)
		em["dominated"] = len(o.Dominated)
		em["excludedByFilter"] = len(o.ExcludedByFilter)
		em["incomparable"] = len(o.Incomparable)
		em["disagreedCells"] = disagreedCells(o)
		em["missingEvidenceCells"] = len(o.MissingEvidence)
		em["scalarRanking"] = len(o.ScalarRanking)
		if o.RankingWithheld != "" {
			em["rankingWithheld"] = o.RankingWithheld
		}
		em["spaceNote"] = o.SpaceNote
	case mode.ForecastOutput:
		em["target"] = o.Target
		em["unit"] = o.Unit
		em["horizon"] = o.Horizon
		em["aggregate"] = o.Aggregate
		em["intervalLow"] = o.Interval.Low
		em["intervalHigh"] = o.Interval.High
		em["dispersionRange"] = o.Dispersion.Range
		em["dispersionIQR"] = o.Dispersion.IQR
		em["estimates"] = len(o.IndividualEstimates)
		em["outliers"] = len(o.Outliers)
		em["rejectedEstimates"] = len(o.Rejected)
		em["poolingRule"] = o.Method.Rule
		em["poolingRulesVersion"] = o.Method.RulesVersion
		em["computedBy"] = o.Method.ComputedBy
		em["spaceNote"] = o.SpaceNote
	}
	return em
}

// paretoOptions names the options on a comparison's frontier. A driver gets the SET, not a winner: a
// Pareto frontier has no first element, and echoing one would invent an ordering the host deliberately
// did not.
func paretoOptions(o mode.CompareOutput) []string {
	out := make([]string, 0, len(o.Pareto))
	for _, e := range o.Pareto {
		out = append(out, e.Option)
	}
	return out
}

// disagreedCells counts the cells whose blind sources split — the headline honesty number of a comparison.
func disagreedCells(o mode.CompareOutput) int {
	n := 0
	for _, c := range o.Cells {
		if c.Disagrees() {
			n++
		}
	}
	return n
}

// Caveat is one governed role's identity verdict for a run: which seat it was, the identity it ran under,
// the tier of evidence behind that identity, and the caveat text when the evidence was WEAK.
type Caveat struct {
	Role     string `json:"role"` // "explorer" | "collator" | "canonicalizer"
	Adapter  string `json:"adapter"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
	Status   string `json:"identityStatus"`
	Evidence string `json:"identityEvidence,omitempty"`
	Caveat   string `json:"caveat,omitempty"`
}

// IdentityCaveats lists every seat in a finished run whose model identity fell short of a strong-evidence
// match — self-reported, unknown, or a proven mismatch — for the explorers and for the
// collator/canonicalizer.
//
// It returns a non-nil (possibly empty) slice on purpose: an empty list is the positive statement "every
// seat's identity was verified", which is a different fact from "the surface did not report identity".
// Reporting is the WHOLE job here: the run already used every one of these seats, so a surface that
// dropped this list would let a synthesis produced by an unverifiable model read exactly like one
// produced by a verified one.
//
// A seat with no status at all is skipped — that is a role which never ran (a halted run), which is a
// technical absence rather than anything about identity.
func IdentityCaveats(out pipeline.Result) []Caveat {
	caveats := make([]Caveat, 0, 2)
	for _, env := range out.Envelopes {
		if env.IdentityStatus == "" || !env.IdentityStatus.IsWeak() {
			continue
		}
		caveats = append(caveats, Caveat{
			Role: "explorer", Adapter: env.Identity.Adapter, Model: env.Identity.Model, Effort: env.Identity.Effort,
			Status: string(env.IdentityStatus), Evidence: string(env.IdentityEvidence), Caveat: env.IdentityCaveat,
		})
	}
	if out.CollatorStatus != "" && out.CollatorStatus.IsWeak() {
		caveats = append(caveats, Caveat{Role: "collator", Status: string(out.CollatorStatus), Caveat: out.CollatorCaveat})
	}
	if out.CanonicalizerStatus != "" && out.CanonicalizerStatus.IsWeak() {
		caveats = append(caveats, Caveat{Role: "canonicalizer", Status: string(out.CanonicalizerStatus), Caveat: out.CanonicalizerCaveat})
	}
	return caveats
}

// Failure is the structured breakdown of a halt: the applied purpose, the halt class, the
// formulation/collator state, and the dropped explorers with their reasons. Every surface caps + sanitizes
// it before display; nothing here reads the filesystem or the environment.
func Failure(raw schema.RawTask, out pipeline.Result, err error) map[string]any {
	dropped := make([]map[string]any, 0, len(out.Dropped))
	for _, d := range out.Dropped {
		dropped = append(dropped, map[string]any{
			"adapter": d.Explorer.Adapter, "model": d.Explorer.Model, "effort": d.Explorer.Effort, "reason": d.Reason,
		})
	}
	return map[string]any{
		"purpose":           raw.Purpose,
		"message":           err.Error(),
		"haltClass":         HaltClass(err),
		"formulationSource": string(out.Formulation.Source),
		"collatorStatus":    string(out.CollatorStatus),
		"panelSize":         len(out.Envelopes),
		"dropped":           dropped,
	}
}

// HaltClass derives the short halt-class label from a pipeline halt: the fault's explicit halt class when
// set, else the fault code's name; a non-fault error degrades to "internal".
func HaltClass(err error) string {
	var f *fault.Fault
	if errors.As(err, &f) {
		if f.Halt != "" {
			return f.Halt
		}
		switch f.Code {
		case fault.Config:
			return "config"
		case fault.Usage:
			return "usage"
		case fault.Adapter:
			return "adapter"
		case fault.Model:
			return "model"
		case fault.Policy:
			return "policy"
		default:
			return "internal"
		}
	}
	return "internal"
}

// EffectiveMode returns the mode actually applied: the task's mode, or the default when it was omitted —
// so a surface reports which mode ran rather than a blank.
func EffectiveMode(m string) string {
	if m == "" {
		return mode.DefaultName
	}
	return m
}
