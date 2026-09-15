// Package runview builds the protocol-neutral view of a finished exploration shared by the ACP and MCP
// surfaces: the governance summary, per-mode detail, identity caveats and halt breakdown. Keeping one
// implementation guarantees both surfaces report the same facts. Every function is pure over
// pipeline.Result.
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

// Governance returns the governance summary for a run that produced claims: counts by label, withheld and
// contested tallies, and the hashes the counts are pinned to. It returns nil when the run has no governance
// report.
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
		// Report that there is no partition rather than omitting the key.
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

// fixedSpace reports whether g contains fixed-space claims.
func fixedSpace(g *govern.Report) bool {
	for _, c := range g.Claims {
		if c.PartitionRevisionHash == govern.FixedSpaceNoPartition {
			return true
		}
	}
	return false
}

// Shape returns a dry run's shape as a map, or nil for a real run. It converts through pipeline.Shape's
// JSON tags so the fields are defined in one place.
func Shape(out pipeline.Result) map[string]any {
	if out.Shape == nil {
		return nil
	}
	b, err := json.Marshal(out.Shape)
	if err != nil {
		// Report the failure rather than return an empty map, which would read as a free run.
		return map[string]any{"error": "the run shape could not be projected: " + err.Error()}
	}
	var m map[string]any
	if uerr := json.Unmarshal(b, &m); uerr != nil {
		return map[string]any{"error": "the run shape could not be projected: " + uerr.Error()}
	}
	return m
}

// ModeDetail returns the terminal output's `summary` and per-mode detail, or nil when there is no output.
// Each result is reported with its qualifiers, such as a ballot's decision label, a comparison's disagreed
// cells and withheld ranking, and a forecast's pooling rule.
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

// paretoOptions returns the options on a comparison's frontier. The frontier is a set with no winner.
func paretoOptions(o mode.CompareOutput) []string {
	out := make([]string, 0, len(o.Pareto))
	for _, e := range o.Pareto {
		out = append(out, e.Option)
	}
	return out
}

// disagreedCells returns the number of split cells in a comparison.
func disagreedCells(o mode.CompareOutput) int {
	n := 0
	for _, c := range o.Cells {
		if c.Disagrees() {
			n++
		}
	}
	return n
}

// Caveat is one seat's identity verdict: its role and identity, the evidence tier, and the caveat text when
// the evidence is weak.
type Caveat struct {
	Role     string `json:"role"` // "explorer" | "collator" | "canonicalizer"
	Adapter  string `json:"adapter"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
	Status   string `json:"identityStatus"`
	Evidence string `json:"identityEvidence,omitempty"`
	Caveat   string `json:"caveat,omitempty"`
}

// IdentityCaveats returns every explorer, collator or canonicalizer whose identity was not verified. The
// slice is never nil: empty means every seat was verified. Seats with no status never ran and are skipped.
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

// Failure returns the structured breakdown of a halt: the purpose, halt class, formulation and collator
// state, and dropped explorers with their reasons.
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

// HaltClass returns a short halt-class label for err: the fault's halt class if set, else its code's name,
// else "internal".
func HaltClass(err error) string {
	if f, ok := errors.AsType[*fault.Fault](err); ok {
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

// EffectiveMode returns m, or mode.DefaultName when m is empty.
func EffectiveMode(m string) string {
	if m == "" {
		return mode.DefaultName
	}
	return m
}
