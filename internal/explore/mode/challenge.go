package mode

// This file is the CHALLENGE mode (design §3 Challenge row) — an ADJUDICATIVE mode, and the one
// that has to be honest about a count.
//
//	round 1 (BLIND)   each explorer attacks the supplied artifact: typed findings with a closed severity
//	canonicalize      TWO independent canonicalizers; only merges BOTH propose hold (canon.CanonicalizeDual)
//	confirm           the BINDING confirmation round: typed panel challenges → a versioned host rule
//	round 2 (MEDIATED) the pooled CONFIRMED-canonical digest is redistributed; explorers deepen or refute it
//	collate           a SEVERITY-TRIAGED register, every entry pinned to a HOST-computed govern.Claim
//
// Challenge is count-bearing, so it takes the full ranking-grade canonicalization policy (Dual + Confirm) —
// §3 is explicit that confirmation covers EVERY count-bearing mode, and giving Shortlist a guard Challenge
// lacked would be exactly the asymmetry the design warns about.
//
// The register's arithmetic is the part worth stating plainly. Corroboration is NOT "how many reviewers
// mentioned it in the whole run": it is the number of distinct BLIND round-1 sources behind the confirmed
// canonical entity, computed by internal/govern over a baseline the type system will not let a later round
// enter. So the round-2 cross-review can sharpen a finding, escalate its severity in the reviewer's own
// assessment, or refute it — and it can never move the count. Minority findings are never dropped: a
// single-source finding survives into the register with its `single_source` label, because de-dup may
// cluster and may not drop (§4).

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

// --- the terminal output (design §3 Challenge row) ---

// ChallengeOutput is the FIXED, exploremesh-owned terminal output of the Challenge mode: a SEVERITY-TRIAGED
// register of the confirmed canonical findings, the strengths that survived the attack, and the quarantined
// model narrative. It lives in this package rather than in internal/schema because every register entry
// embeds the host-computed govern.Claim, and govern sits above schema in the import graph.
type ChallengeOutput struct {
	// ArtifactDigest is the SHA-256 of the exact artifact bytes the panel attacked ("" for a composition that
	// supplies none) — so a register can be tied to the artifact revision it was produced against.
	ArtifactDigest string `json:"artifactDigest,omitempty"`
	// PartitionRevisionHash is the CONFIRMED partition every entry's count was computed at (§4: every count
	// pins the partition revision it rides on).
	PartitionRevisionHash string `json:"partitionRevisionHash"`
	// Register is the severity-triaged finding register, most severe first. Minority findings are present —
	// carried, labeled single_source, never dropped.
	Register []ChallengeEntry `json:"register"`
	// SurvivingStrengths are the register entries a round-2 reviewer REFUTED without any reviewer deepening
	// them — the honest inverse of the register, recorded so "the panel attacked this and it held" is visible
	// rather than inferred from an absence.
	SurvivingStrengths []ChallengeStrength `json:"survivingStrengths,omitempty"`
	// CollatorNarrative is the quarantined MODEL PROSE namespace (§0 F-C): coverage notes and the like. No
	// machine governance field above may carry model text, which is why it is all collected here.
	CollatorNarrative []govern.Narrative `json:"collatorNarrative,omitempty"`
}

// ChallengeEntry is ONE confirmed canonical finding in the register.
type ChallengeEntry struct {
	CanonicalID string `json:"canonicalId"`
	// Statement is the canonical entity's label — the host's stable name for the finding, not one reviewer's
	// wording (every reviewer's exact wording is carried below in Findings).
	Statement string `json:"statement"`
	// Severity is the HOST triage severity: the MAXIMUM severity any BLIND round-1 source assigned. Max rather
	// than mean because severity is a claim about a worst case, and averaging one reviewer's "critical" with
	// another's "low" would produce a number neither of them made.
	Severity schema.Severity `json:"severity"`
	// Corroboration is the HOST-computed claim over the BLIND round-1 baseline: the count, BOTH denominators,
	// the label (corroborated | single_source | withheld_contested_partition | withheld_below_quorum), the
	// pinned inputs, and a sensitivity range when the partition under it is contested.
	Corroboration govern.Claim `json:"corroboration"`
	// SingleSource marks a carried minority finding (salience, not corroboration).
	SingleSource bool `json:"singleSource"`
	// Findings are the attributed BLIND round-1 findings clustered into this entity — every reviewer's exact
	// statement, severity, failure scenario and evidence.
	Findings []AttributedFinding `json:"findings"`
	// Deepening is the round-2 CROSS-REVIEW record for this entity, attributed. It adds depth and stances; it
	// contributes NOTHING to Corroboration (§0 F-A).
	Deepening []AttributedAssessment `json:"deepening,omitempty"`
}

// AttributedFinding is one blind round-1 finding with its source.
type AttributedFinding struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Statement   string                  `json:"statement"`
	Severity    schema.Severity         `json:"severity"`
	// FailureScenario + Evidence are the reviewer's own words about this finding, kept attributed rather than
	// blended into a single host sentence.
	FailureScenario string `json:"failureScenario,omitempty"`
	Evidence        string `json:"evidence,omitempty"`
}

// AttributedAssessment is one round-2 cross-review reaction with its source. Its severity is that reviewer's
// own post-mediation assessment and is deliberately NOT folded into the entry's host triage severity, which
// is computed over blind round 1 only.
type AttributedAssessment struct {
	Explorer    schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Stance      schema.Stance           `json:"stance"`
	Severity    schema.Severity         `json:"severity"`
	Depth       string                  `json:"depth,omitempty"`
	Evidence    string                  `json:"evidence,omitempty"`
}

// ChallengeStrength records a finding the cross-review round REFUTED and nobody deepened — i.e. a part of the
// artifact that survived the attack, named by the finding that failed to stick.
type ChallengeStrength struct {
	CanonicalID string                    `json:"canonicalId"`
	Statement   string                    `json:"statement"`
	RefutedBy   []schema.ExplorerIdentity `json:"refutedBy"`
	Reason      string                    `json:"reason"`
}

// Summary returns the one-line human summary — counts by label, never a verdict of its own. It implements the
// ModeOutput contract.
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

// Validate checks the register is usable: at least one finding survived canonicalization. An attack that
// produced nothing is a legitimate outcome of a review but not of this collation — the pipeline would have
// halted at the surjectivity gate long before, so an empty register here means the assembly lost something.
func (o ChallengeOutput) Validate() error {
	if len(o.Register) == 0 {
		return fmt.Errorf("challenge output has an empty register")
	}
	return nil
}

// shortHash renders the first 12 hex chars of a hash for a human line.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// --- the round-2 (mediated cross-review) contract ---

// challengeReview is the Challenge mode's LaterRoundContract: it accepts ONLY the pooled confirmed-canonical
// uniques at the current artifact schema version, and it embeds the host's already-framed untrusted-data
// block VERBATIM — a contract may not re-frame or re-label it (§6).
type challengeReview struct{}

// Accepts declares the round→round edge this round will take. It names KindCanonicalUniques and nothing else,
// so feeding it raw blind envelopes is rejected before spend — which is the mechanical form of "explorers
// never see raw peer output" (§1).
func (challengeReview) Accepts() round.Accepts {
	return round.Accepts{Kinds: []round.Kind{round.KindCanonicalUniques}, SchemaVersion: round.SchemaVersion}
}

func (challengeReview) Prompt(raw schema.RawTask, untrustedDataBlock string) string {
	return schema.ChallengeReviewPrompt(raw, untrustedDataBlock)
}

func (challengeReview) ExplorerSchema() schema.Schema { return schema.ChallengeReviewSchema() }

// --- the canonicalizing + governed terminal contract ---

// challengeCollator is the Challenge mode's terminal contract. It is a CanonicalizingContract (the raw
// findings go through the dual canonicalizer + the append-only ledger + the surjectivity gate) AND a
// GovernedCollator (the register is assembled as a view over the confirmed partition plus the host's emitted
// claims and the recorded rounds).
type challengeCollator struct{}

// Nominations flattens the verified blind round-1 panel into raw nominations: each finding's STATEMENT is the
// nomination, attributed to its explorer + envelope ref. The statement is what gets canonicalized because it
// is what two reviewers can independently arrive at; the severity/scenario/evidence travel with it and are
// re-attached at collate time from the same recorded envelopes.
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

// challengeNominationWire is the per-nomination shape rendered into the canonicalizer prompt: the model
// clusters by INDEX, so the index is explicit alongside the raw statement + source.
type challengeNominationWire struct {
	Index          int                     `json:"index"`
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
}

// CanonicalizerPrompt renders the canonicalizer instruction for FINDINGS. It differs from Catalog's in the
// one way that matters: two findings are the same finding only when they name the same underlying DEFECT —
// two different defects in the same component are two findings, and merging them would silently manufacture
// corroboration for whichever wording survived.
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
	return "You are the CANONICALIZER — a role DISTINCT from the collator and from the reviewers (design §4). " +
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

// ParseProposal decodes the canonicalizer's raw output into a canon.Proposal, stamping the deciding call ref +
// the SEPARATELY-verified canonicalizer identity. The surjectivity GATE is canon's host-checked invariant, not
// this parser's — one authority, no second opinion about whether a finding was dropped.
func (challengeCollator) ParseProposal(raw []byte, _ []canon.Nomination, decidedByCall string, identity schema.ExplorerIdentity) (canon.Proposal, error) {
	return parseClusterProposal(raw, decidedByCall, identity)
}

// Collate is the PARTITION-ONLY fallback required by CanonicalizingContract. Challenge always runs governed
// (the pipeline prefers CollateGoverned), so reaching this would mean the register was assembled without the
// claims that give it its meaning — an error, not a degraded rendering.
func (challengeCollator) Collate(canon.Result) (ModeOutput, error) {
	return nil, fmt.Errorf("challenge: the register may only be assembled from the governed record (claims + rounds); a partition-only collation would report counts with nothing pinning them")
}

// CollateGoverned assembles the severity-triaged register as a deterministic HOST VIEW: the confirmed
// partition supplies the entities, the recorded BLIND round-1 envelopes supply each finding's severity and
// evidence, the emitted governance claims supply every count + label + sensitivity, and the recorded round-2
// envelopes supply the attributed depth. Nothing here counts anything — the counting already happened in
// internal/govern over a baseline this function could not widen if it tried.
func (challengeCollator) CollateGoverned(in CollateInput) (ModeOutput, error) {
	if in.Governance == nil {
		return nil, fmt.Errorf("challenge: no governance claims were emitted — every register entry must be pinned to a host-computed claim (design §0 F-C)")
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
				// The member came from a round the register does not read details for (it cannot: only blind
				// round 1 feeds the partition). Carry the attribution anyway — losing a member here would be a
				// silent drop, which the surjectivity gate exists to make impossible.
				f = schema.ChallengeFinding{Statement: m.RawNomination, Severity: schema.SeverityUnspecified}
			}
			entry.Findings = append(entry.Findings, AttributedFinding{
				Explorer: m.SourceExplorer, EnvelopeRef: m.EnvelopeRef, Statement: f.Statement,
				Severity: f.Severity, FailureScenario: f.FailureScenario, Evidence: f.Evidence,
			})
			// HOST triage: the maximum severity any blind source assigned (see the Severity field's comment).
			if f.Severity.Rank() > entry.Severity.Rank() {
				entry.Severity = f.Severity
			}
		}
		entry.Deepening = append(entry.Deepening, assessments[cl.CanonicalID]...)
		out.Register = append(out.Register, entry)
	}

	// TRIAGE (a deterministic host sort, never a model's ordering): severity first, then the corroboration
	// count, then the canonical ID so identical standing always serializes identically.
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

// findingKey identifies one blind round-1 finding by the pair the partition records for it (envelope ref +
// raw statement) — the same key canon uses, so a member always resolves to the finding it came from.
type findingKey struct {
	envelopeRef string
	statement   string
}

// blindFindings indexes every BLIND round-1 finding by (envelope ref, statement). Only round 1 is read: a
// later round's findings are not in the partition and must not acquire a register position by being detailed.
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

// reviewAssessments indexes every round-2+ cross-review assessment by the canonical ID it referenced. An
// assessment naming a ref that is not in the partition is simply not indexed — it attaches to nothing, and a
// reviewer's mis-cited ref must never create an entity (that would be the round-2 injection the mediated
// design forbids).
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

// survivingStrengths names the entries the cross-review REFUTED with nobody deepening them. It is a strict
// mechanical read of the recorded stances — the host asserts nothing about whether the refutation was right,
// only that the panel's second look pushed back and no reviewer pushed the other way.
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

// artifactDigest is the SHA-256 of the exact artifact bytes attacked ("" when none was supplied). It ties a
// register to an artifact revision without copying the artifact into the machine surface.
func artifactDigest(artifact string) string {
	body := strings.TrimSpace(artifact)
	if body == "" {
		return ""
	}
	return sha256Hex([]byte(body))
}

// requireArtifact is Challenge's ModeSpec.ValidateTask: the mode exists to attack a supplied artifact, so a
// missing one is a task error caught by every surface BEFORE any spend, with one message.
func requireArtifact(raw schema.RawTask) error {
	if strings.TrimSpace(raw.Artifact) == "" {
		return fmt.Errorf("mode %q requires an ARTIFACT to attack: supply the thing under review (CLI: --artifact <path|->; ACP: _meta.exploremesh.artifact)", Challenge)
	}
	return nil
}

func init() {
	// Challenge (design §3). Formulation-free like every registered mode: the app owns BOTH the
	// round-1 prompt and schema, so the collator authors no explorer schema (§5). Count-bearing ⇒ the full
	// ranking-grade policy (Dual + Confirm). Two FIXED rounds: blind attack, then the collator-mediated
	// cross-review over the pooled confirmed-canonical digest.
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
		// EMERGENT space: the findings are authored by the reviewers, so grouping them across reviewers is
		// entity resolution and may only happen through the recorded canonicalization ledger (§0 F-B).
		Class: schema.EmergentSpace,
	})
}
