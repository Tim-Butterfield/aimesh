package mode

// This file implements the Challenge mode:
//
//	round 1 (blind)    each explorer attacks the supplied artifact with typed findings
//	canonicalize       two canonicalizers; only merges both propose hold
//	confirm            the confirmation round resolves typed challenges with a host rule
//	round 2 (mediated) the confirmed findings are shown back; explorers deepen or refute them
//	collate            a severity-triaged register, each entry pinned to a govern.Claim
//
// Corroboration counts the distinct blind round-1 sources behind each confirmed finding, so the
// cross-review round can add depth or refute a finding but cannot change a count. Findings raised by a
// single reviewer stay in the register, labeled single_source.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// ChallengeOutput is the Challenge mode's output: the severity-triaged register, the strengths that
// survived cross-review, and the model narrative. It is defined here rather than in package schema because
// it uses govern types.
type ChallengeOutput struct {
	// ArtifactDigest is the SHA-256 of the attacked artifact, or empty when none was supplied.
	ArtifactDigest string `json:"artifactDigest,omitempty"`
	// PartitionRevisionHash is the confirmed partition the counts were computed at.
	PartitionRevisionHash string `json:"partitionRevisionHash"`
	// Register lists the confirmed findings, most severe first, including single-source findings.
	Register []ChallengeEntry `json:"register"`
	// SurvivingStrengths are findings that cross-review refuted and no reviewer deepened.
	SurvivingStrengths []ChallengeStrength `json:"survivingStrengths,omitempty"`
	// CollatorNarrative holds model prose, such as coverage notes.
	CollatorNarrative []govern.Narrative `json:"collatorNarrative,omitempty"`
}

// ChallengeEntry is one confirmed finding in the register.
type ChallengeEntry struct {
	CanonicalID string `json:"canonicalId"`
	// Statement is the canonical finding's label; each reviewer's wording is in Findings.
	Statement string `json:"statement"`
	// Severity is the highest severity any blind round-1 source assigned, since severity describes a worst
	// case.
	Severity schema.Severity `json:"severity"`
	// Corroboration is the host's claim over blind round 1, with its label, denominators and any sensitivity
	// range.
	Corroboration govern.Claim `json:"corroboration"`
	// SingleSource marks a finding raised by only one reviewer.
	SingleSource bool `json:"singleSource"`
	// Findings are the attributed blind round-1 findings in this cluster.
	Findings []AttributedFinding `json:"findings"`
	// Deepening is the attributed cross-review record. It does not affect Corroboration.
	Deepening []AttributedAssessment `json:"deepening,omitempty"`
}

// AttributedFinding is one blind round-1 finding with its source.
type AttributedFinding struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Statement   string                  `json:"statement"`
	Severity    schema.Severity         `json:"severity"`
	// FailureScenario and Evidence are the reviewer's own words.
	FailureScenario string `json:"failureScenario,omitempty"`
	Evidence        string `json:"evidence,omitempty"`
}

// AttributedAssessment is one cross-review response with its source. Its severity does not affect the
// entry's severity.
type AttributedAssessment struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Stance      schema.Stance           `json:"stance"`
	Severity    schema.Severity         `json:"severity"`
	Depth       string                  `json:"depth,omitempty"`
	Evidence    string                  `json:"evidence,omitempty"`
}

// ChallengeStrength is a finding that cross-review refuted and no reviewer deepened.
type ChallengeStrength struct {
	CanonicalID string                    `json:"canonicalId"`
	Statement   string                    `json:"statement"`
	RefutedBy   []schema.ExplorerIdentity `json:"refutedBy"`
	Reason      string                    `json:"reason"`
}

// Summary returns a one-line summary of the register by severity and label.
func (o ChallengeOutput) Summary() string {
	bySeverity := map[schema.Severity]int{}
	corroborated, single, withheld := 0, 0, 0
	for _, e := range o.Register {
		bySeverity[e.Severity]++
		switch {
		case !e.Corroboration.Label.Definitive():
			withheld++
		case e.Corroboration.Label == govern.LabelCorroborated:
			corroborated++
		default:
			single++
		}
	}
	return fmt.Sprintf("challenge register: %d finding(s) (%d critical, %d high, %d medium, %d low) — %d corroborated, %d single-source (carried), %d withheld; partition %s",
		len(o.Register), bySeverity[schema.SeverityCritical], bySeverity[schema.SeverityHigh],
		bySeverity[schema.SeverityMedium], bySeverity[schema.SeverityLow],
		corroborated, single, withheld, shortHash(o.PartitionRevisionHash))
}

// Validate requires a non-empty register. An empty one means findings were lost during assembly, since
// canonicalization would already have failed.
func (o ChallengeOutput) Validate() error {
	if len(o.Register) == 0 {
		return fmt.Errorf("challenge output has an empty register")
	}
	return nil
}

// shortHash returns the first 12 characters of a hash.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// challengeReview is the Challenge mode's cross-review round contract.
type challengeReview struct{}

// Accepts accepts only confirmed canonical entities, so explorers never receive raw peer output.
func (challengeReview) Accepts() round.Accepts {
	return round.Accepts{Kinds: []round.Kind{round.KindCanonicalUniques}, SchemaVersion: round.SchemaVersion}
}

func (challengeReview) Prompt(raw schema.RawTask, untrustedDataBlock string) string {
	return schema.ChallengeReviewPrompt(raw, untrustedDataBlock)
}

func (challengeReview) ExplorerSchema() schema.Schema { return schema.ChallengeReviewSchema() }

// challengeCollator is the Challenge mode's CanonicalizingContract and GovernedCollator.
type challengeCollator struct{}

// Nominations returns one nomination per finding statement. Severity and evidence are reattached from the
// recorded envelopes at collation.
func (challengeCollator) Nominations(primary []schema.Envelope) []canon.Nomination {
	var out []canon.Nomination
	for _, env := range primary {
		for _, f := range schema.ParseFindings(env.Response) {
			out = append(out, canon.Nomination{
				Raw:            f.Statement,
				SourceExplorer: env.Identity,
				EnvelopeRef:    schema.EnvelopeRef(1, env.Order),
			})
		}
	}
	return out
}

// challengeNominationWire is one finding as shown to the canonicalizer, which clusters by index.
type challengeNominationWire struct {
	Index          int                     `json:"index"`
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
}

// CanonicalizerPrompt builds the Challenge canonicalizer prompt, which clusters findings only when they
// describe the same defect.
func (challengeCollator) CanonicalizerPrompt(noms []canon.Nomination) (string, error) {
	wire := make([]challengeNominationWire, len(noms))
	for i, n := range noms {
		wire[i] = challengeNominationWire{Index: i, Raw: n.Raw, SourceExplorer: n.SourceExplorer}
	}
	arr, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("marshal nominations: %w", err)
	}
	lastIdx := "0"
	if len(noms) > 0 {
		lastIdx = strconv.Itoa(len(noms) - 1)
	}
	return "You are the CANONICALIZER — a role DISTINCT from the collator and from the reviewers. " +
		"Below are raw FINDINGS produced INDEPENDENTLY by separate reviewers attacking the same artifact. " +
		"CLUSTER the findings that identify the SAME UNDERLYING DEFECT (the same failure, described differently) " +
		"under one canonical entity. Two DIFFERENT defects are two entities even when they touch the same " +
		"component, and two findings that merely share a topic are NOT the same finding — merging them would " +
		"turn one reviewer's observation into two reviewers' agreement, which is the specific error this step " +
		"exists to avoid. You may CLUSTER duplicates but MUST NEVER DROP a finding: every index 0.." + lastIdx +
		" MUST appear in EXACTLY one cluster (a finding only one reviewer raised gets its own cluster). Also " +
		"extract the distinguishing DIMENSIONS that separate the findings — PROPOSED axes for the human, NOT " +
		"decision criteria. Output ONLY a SINGLE JSON object — no prose before or after it, and no markdown " +
		"code fences.\n\n" +
		"The JSON object MUST have EXACTLY these fields, fully populated:\n" +
		"- \"clusters\": array of objects, each {\"canonicalId\": string (a stable slug for the defect), " +
		"\"name\": string (a neutral human label for the defect — do NOT editorialize its severity), " +
		"\"memberIndices\": array of integers (the indices of the raw findings clustered under it)}. Across ALL " +
		"clusters, every finding index appears exactly once.\n" +
		"- \"proposedDimensions\": array of strings — the distinguishing axes (PROPOSED, not criteria).\n" +
		"- \"coverageNotes\": string — notes on coverage/gaps across the findings.\n\n" +
		"nominations:\n" + string(arr) + "\n", nil
}

// ParseProposal decodes the canonicalizer's output with parseClusterProposal.
func (challengeCollator) ParseProposal(raw []byte, _ []canon.Nomination, decidedByCall string, identity schema.ExplorerIdentity) (canon.Proposal, error) {
	return parseClusterProposal(raw, decidedByCall, identity)
}

// Collate always returns an error: the register needs the governance claims, so Challenge uses
// CollateGoverned.
func (challengeCollator) Collate(canon.Result) (ModeOutput, error) {
	return nil, fmt.Errorf("challenge: the register may only be assembled from the governed record (claims + rounds); a partition-only collation would report counts with nothing pinning them")
}

// CollateGoverned builds the register from the confirmed partition, the blind round-1 findings, the
// emitted claims and the cross-review envelopes. It counts nothing itself.
func (challengeCollator) CollateGoverned(in CollateInput) (ModeOutput, error) {
	if in.Governance == nil {
		return nil, fmt.Errorf("challenge: no governance claims were emitted — every register entry must be pinned to a host-computed claim")
	}
	claims := map[string]govern.Claim{}
	for _, c := range in.Governance.Claims {
		if c.Query == govern.CorroborationQuery {
			claims[c.Subject] = c
		}
	}
	findings := blindFindings(in.Rounds)
	assessments := reviewAssessments(in.Rounds)

	out := ChallengeOutput{
		ArtifactDigest:        artifactDigest(in.Raw.Artifact),
		PartitionRevisionHash: in.Partition.PartitionRevisionHash,
		CollatorNarrative:     append([]govern.Narrative(nil), in.Governance.CollatorNarrative...),
	}
	for _, cl := range in.Partition.Clusters {
		claim, ok := claims[cl.CanonicalID]
		if !ok {
			return nil, fmt.Errorf("challenge: confirmed entity %q carries no governance claim — a register entry may never report an unpinned count", cl.CanonicalID)
		}
		entry := ChallengeEntry{
			CanonicalID: cl.CanonicalID, Statement: cl.Name,
			Severity: schema.SeverityUnspecified, Corroboration: claim, SingleSource: cl.SingleSource,
		}
		for _, m := range cl.Members {
			f, ok := findings[findingKey{m.EnvelopeRef, m.RawNomination}]
			if !ok {
				// Keep the member even without details, so no finding is dropped.
				f = schema.ChallengeFinding{Statement: m.RawNomination, Severity: schema.SeverityUnspecified}
			}
			entry.Findings = append(entry.Findings, AttributedFinding{
				Explorer: m.SourceExplorer, EnvelopeRef: m.EnvelopeRef, Statement: f.Statement,
				Severity: f.Severity, FailureScenario: f.FailureScenario, Evidence: f.Evidence,
			})
			if f.Severity.Rank() > entry.Severity.Rank() {
				entry.Severity = f.Severity
			}
		}
		entry.Deepening = append(entry.Deepening, assessments[cl.CanonicalID]...)
		out.Register = append(out.Register, entry)
	}

	// Order by severity, then corroboration count, then canonical ID for determinism.
	sort.SliceStable(out.Register, func(i, j int) bool {
		a, b := out.Register[i], out.Register[j]
		switch {
		case a.Severity.Rank() != b.Severity.Rank():
			return a.Severity.Rank() > b.Severity.Rank()
		case a.Corroboration.Value != b.Corroboration.Value:
			return a.Corroboration.Value > b.Corroboration.Value
		default:
			return a.CanonicalID < b.CanonicalID
		}
	})
	out.SurvivingStrengths = survivingStrengths(out.Register)
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// findingKey identifies a blind round-1 finding by envelope ref and statement, as the partition does.
type findingKey struct {
	envelopeRef string
	statement   string
}

// blindFindings indexes the round-1 findings by envelope ref and statement.
func blindFindings(rounds []round.Round) map[findingKey]schema.ChallengeFinding {
	out := map[findingKey]schema.ChallengeFinding{}
	if len(rounds) == 0 {
		return out
	}
	for _, env := range rounds[0].Envelopes() {
		ref := schema.EnvelopeRef(1, env.Order)
		for _, f := range schema.ParseFindings(env.Response) {
			out[findingKey{ref, f.Statement}] = f
		}
	}
	return out
}

// reviewAssessments indexes cross-review assessments by the canonical ID they reference. An assessment
// citing an unknown ID attaches to nothing and never creates an entry.
func reviewAssessments(rounds []round.Round) map[string][]AttributedAssessment {
	out := map[string][]AttributedAssessment{}
	for k := 1; k < len(rounds); k++ {
		r := rounds[k]
		for _, env := range r.Envelopes() {
			ref := schema.EnvelopeRef(r.Index(), env.Order)
			for _, a := range schema.ParseAssessments(env.Response) {
				out[a.Ref] = append(out[a.Ref], AttributedAssessment{
					Explorer: env.Identity, EnvelopeRef: ref, Stance: a.Stance,
					Severity: a.Severity, Depth: a.Depth, Evidence: a.Evidence,
				})
			}
		}
	}
	return out
}

// survivingStrengths returns the entries at least one reviewer refuted and none deepened. It reads the
// recorded stances only and does not judge the refutations.
func survivingStrengths(register []ChallengeEntry) []ChallengeStrength {
	var out []ChallengeStrength
	for _, e := range register {
		var refuters []schema.ExplorerIdentity
		deepened := false
		for _, a := range e.Deepening {
			switch a.Stance {
			case schema.StanceRefutes:
				refuters = append(refuters, a.Explorer)
			case schema.StanceDeepens:
				deepened = true
			}
		}
		if len(refuters) == 0 || deepened {
			continue
		}
		out = append(out, ChallengeStrength{
			CanonicalID: e.CanonicalID, Statement: e.Statement, RefutedBy: refuters,
			Reason: fmt.Sprintf("%d cross-review reviewer(s) refuted this finding and none deepened it — the artifact held here (the finding itself is still carried in the register with its count)", len(refuters)),
		})
	}
	return out
}

// artifactDigest returns the SHA-256 of the trimmed artifact, or "" when there is none.
func artifactDigest(artifact string) string {
	body := strings.TrimSpace(artifact)
	if body == "" {
		return ""
	}
	return sha256Hex([]byte(body))
}

// requireArtifact is Challenge's ValidateTask: the task must supply an artifact.
func requireArtifact(raw schema.RawTask) error {
	if strings.TrimSpace(raw.Artifact) == "" {
		return fmt.Errorf("mode %q requires an ARTIFACT to attack: supply the thing under review (CLI: --artifact <path|->; ACP: _meta.exploremesh.artifact)", Challenge)
	}
	return nil
}

func init() {
	register(ModeSpec{
		Name:             Challenge,
		FormulationFree:  true,
		Prompt:           schema.ChallengeExplorerPrompt,
		ExplorerSchema:   schema.ChallengeExplorerSchema,
		Objective:        ObjectiveCanonicalizeMediateCollate,
		Canonicalizing:   challengeCollator{},
		Canonicalization: CanonicalizationPolicy{Dual: true, Confirm: true},
		Rounds:           2,
		LaterRound:       challengeReview{},
		ValidateTask:     requireArtifact,
		Class:            schema.EmergentSpace,
	})
}
