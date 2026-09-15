package mode

// This file implements the Shortlist mode:
//
//	round 1 (blind)   each explorer enumerates candidate options
//	canonicalize      two canonicalizers; only merges both propose hold
//	confirm           the confirmation round fixes the candidate set
//	freeze            the host hashes the decision inputs before any ballot exists
//	round 2 (ballot)  each explorer ranks and approves the confirmed candidates, in a seeded random order
//	tally             the host computes the ranking with govern.Tally
//
// The panel can contest the candidate set before voting, the frozen framing's hash appears in the ballot
// prompt, and the ranking comes only from the host tally. The result is reported as an informed preference
// under shared framing, alongside each candidate's blind round-1 salience.

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// maxShortlistSize caps the shortlist size.
const maxShortlistSize = 5

// ShortlistOutput is the Shortlist mode's output: the ranked candidates with their claims and provenance,
// the rejected candidates with reasons, the frozen criteria, and the tie and quorum outcome. It is defined
// here rather than in package schema because it uses govern types.
type ShortlistOutput struct {
	// Decision is the host's description of the result.
	Decision string `json:"decision"`
	// DecisionInputsHash is the hash of the frozen decision inputs, as shown in the ballot prompt.
	DecisionInputsHash    string `json:"decisionInputsHash"`
	PartitionRevisionHash string `json:"partitionRevisionHash"`
	Method                string `json:"method"`
	// Ranked is the shortlist in tally order, including entries whose label was withheld.
	Ranked []ShortlistEntry `json:"ranked"`
	// Rejects are the candidates below the cut, with the host's reasons.
	Rejects []ShortlistReject `json:"rejects,omitempty"`
	// Criteria are the frozen criteria.
	Criteria []govern.Criterion `json:"criteria"`
	// BallotsCast, PanelSize and Respondents describe participation in the tally.
	BallotsCast int  `json:"ballotsCast"`
	PanelSize   int  `json:"panelSize"`
	Respondents int  `json:"respondents"`
	QuorumMet   bool `json:"quorumMet"`
	// TieOutcome describes a tie at the cut, or is empty.
	TieOutcome string `json:"tieOutcome,omitempty"`
	// CollatorNarrative holds model prose: voters' rationales and canonicalizer notes.
	CollatorNarrative []govern.Narrative `json:"collatorNarrative,omitempty"`
}

// ShortlistEntry is one ranked candidate.
type ShortlistEntry struct {
	Rank        int    `json:"rank"`
	CanonicalID string `json:"canonicalId"`
	Name        string `json:"name"`
	// Score, Approvals and Support come from the host tally: positional points, approvals, and the number
	// of ballots that placed the candidate.
	Score     int `json:"score"`
	Approvals int `json:"approvals"`
	Support   int `json:"support"`
	// Claim is the candidate's ballot claim, with its label, denominators and any sensitivity range.
	Claim govern.Claim `json:"claim"`
	// Voters are the explorers that placed the candidate.
	Voters []schema.ExplorerIdentity `json:"voters"`
	// Provenance lists who nominated the candidate in blind round 1, with their wording.
	Provenance []ShortlistNomination `json:"provenance"`
	// EmergentSalience is the candidate's blind round-1 corroboration claim, when one was emitted.
	EmergentSalience *govern.Claim `json:"emergentSalience,omitempty"`
}

// ShortlistNomination is one blind round-1 nomination behind a candidate.
type ShortlistNomination struct {
	Explorer      schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef   string                  `json:"envelopeRef"`
	RawNomination string                  `json:"rawNomination"`
}

// ShortlistReject is a candidate below the cut, with the host's reason.
type ShortlistReject struct {
	Rank        int                   `json:"rank"`
	CanonicalID string                `json:"canonicalId"`
	Name        string                `json:"name"`
	Score       int                   `json:"score"`
	Approvals   int                   `json:"approvals"`
	Support     int                   `json:"support"`
	Reason      string                `json:"reason"`
	Provenance  []ShortlistNomination `json:"provenance"`
}

// Summary returns a one-line summary of the ranking and how many labels were withheld.
func (o ShortlistOutput) Summary() string {
	withheld := 0
	for _, e := range o.Ranked {
		if !e.Claim.Label.Definitive() {
			withheld++
		}
	}
	return fmt.Sprintf("shortlist: %d of %d candidate(s) ranked by a host tally of %d ballot(s) (%d panel / %d respondents) — %d definitive, %d WITHHELD; informed preference under shared framing, not consensus [inputs %s]",
		len(o.Ranked), len(o.Ranked)+len(o.Rejects), o.BallotsCast, o.PanelSize, o.Respondents,
		len(o.Ranked)-withheld, withheld, shortHash(o.DecisionInputsHash))
}

// Validate requires at least one ranked candidate and a decision description.
func (o ShortlistOutput) Validate() error {
	if len(o.Ranked) == 0 {
		return fmt.Errorf("shortlist output ranked no candidates")
	}
	if o.Decision == "" {
		return fmt.Errorf("shortlist output carries no decision rendering — a ballot result must state what it is")
	}
	return nil
}

// shortlistBallot is the Shortlist mode's LaterRoundContract and BallotContract.
type shortlistBallot struct{}

// Accepts accepts only confirmed canonical entities, so the ballot always runs over the confirmed set.
func (shortlistBallot) Accepts() round.Accepts {
	return round.Accepts{Kinds: []round.Kind{round.KindCanonicalUniques}, SchemaVersion: round.SchemaVersion}
}

func (shortlistBallot) Prompt(raw schema.RawTask, untrustedDataBlock string) string {
	return schema.BallotPrompt(raw, untrustedDataBlock)
}

func (shortlistBallot) ExplorerSchema() schema.Schema { return schema.BallotSchema() }

// Criteria returns the task's criteria, each with origin `user` and aggregation method `ballot`.
func (shortlistBallot) Criteria(raw schema.RawTask) []govern.Criterion {
	out := make([]govern.Criterion, 0, len(raw.Criteria))
	for _, c := range raw.Criteria {
		if c == "" {
			continue
		}
		out = append(out, govern.Criterion{
			Name: c, Origin: govern.OriginUser, AggregationMethod: govern.AggregationBallot,
		})
	}
	return out
}

// Method returns the positional ballot tally method.
func (shortlistBallot) Method() govern.DecisionMethod { return govern.MethodPositionalBallot }

// ShortlistSize returns half the universe size rounded up, between 1 and maxShortlistSize.
func (shortlistBallot) ShortlistSize(universeSize int) int {
	n := max(min((universeSize+1)/2, maxShortlistSize), 1)
	return n
}

// ParseBallot extracts and validates an explorer's ballot. A candidate outside universe is an error, so
// the explorer is recorded as casting no usable ballot rather than having the entry silently removed.
func (shortlistBallot) ParseBallot(env schema.Envelope, universe map[string]bool) (govern.Ballot, error) {
	resp := schema.ParseBallotResponse(env.Response)
	b := govern.Ballot{
		By: env.Identity, Ranking: resp.Ranking, Approved: resp.Approved, Rationale: resp.Rationale,
	}
	if err := b.Validate(universe); err != nil {
		return govern.Ballot{}, err
	}
	return b, nil
}

// shortlistCollator is the Shortlist mode's CanonicalizingContract and GovernedCollator.
type shortlistCollator struct{}

// Nominations returns one nomination per candidate string in each primary envelope.
func (shortlistCollator) Nominations(primary []schema.Envelope) []canon.Nomination {
	var out []canon.Nomination
	for _, env := range primary {
		cands, _ := env.Response["candidates"].([]any)
		for _, c := range cands {
			s, ok := c.(string)
			if !ok || len(s) == 0 {
				continue
			}
			out = append(out, canon.Nomination{
				Raw: s, SourceExplorer: env.Identity, EnvelopeRef: schema.EnvelopeRef(1, env.Order),
			})
		}
	}
	return out
}

// shortlistNominationWire is one nomination as shown to the canonicalizer.
type shortlistNominationWire struct {
	Index          int                     `json:"index"`
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
}

// CanonicalizerPrompt builds the Shortlist canonicalizer prompt. Labels become ballot option names, so it
// asks for neutral labels.
func (shortlistCollator) CanonicalizerPrompt(noms []canon.Nomination) (string, error) {
	wire := make([]shortlistNominationWire, len(noms))
	for i, n := range noms {
		wire[i] = shortlistNominationWire{Index: i, Raw: n.Raw, SourceExplorer: n.SourceExplorer}
	}
	arr, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("marshal nominations: %w", err)
	}
	lastIdx := "0"
	if len(noms) > 0 {
		lastIdx = strconv.Itoa(len(noms) - 1)
	}
	return "You are the CANONICALIZER — a role DISTINCT from the collator and from the explorers. " +
		"Below are raw CANDIDATE OPTIONS nominated INDEPENDENTLY by separate explorers for the same decision. " +
		"CLUSTER the nominations that name the SAME underlying option (synonyms, variants, spellings) under one " +
		"canonical entity; keep genuinely distinct options separate. A variant that would be CHOSEN differently " +
		"is a different option, not a synonym. You may CLUSTER duplicates but MUST NEVER DROP a nomination: " +
		"every index 0.." + lastIdx + " MUST appear in EXACTLY one cluster (a singleton gets its own cluster).\n\n" +
		"These labels become the OPTION NAMES on a ballot the panel will vote over, so name each entity " +
		"NEUTRALLY and descriptively. Do not praise, disparage, rank, or recommend any option — a persuasive " +
		"label is a vote you were not asked to cast.\n\n" +
		"Output ONLY a SINGLE JSON object — no prose before or after it, and no markdown code fences. It MUST " +
		"have EXACTLY these fields, fully populated:\n" +
		"- \"clusters\": array of objects, each {\"canonicalId\": string (a stable slug for the option), " +
		"\"name\": string (the neutral human label), \"memberIndices\": array of integers (the indices of the " +
		"raw nominations clustered under it)}. Across ALL clusters, every nomination index appears exactly once.\n" +
		"- \"proposedDimensions\": array of strings — the distinguishing axes (PROPOSED for the human, NOT " +
		"decision criteria; the criteria are frozen separately by the host).\n" +
		"- \"coverageNotes\": string — notes on coverage/gaps across the nominations.\n\n" +
		"nominations:\n" + string(arr) + "\n", nil
}

// ParseProposal decodes the canonicalizer's output with parseClusterProposal.
func (shortlistCollator) ParseProposal(raw []byte, _ []canon.Nomination, decidedByCall string, identity schema.ExplorerIdentity) (canon.Proposal, error) {
	return parseClusterProposal(raw, decidedByCall, identity)
}

// Collate always returns an error: a ranking requires the host tally, so Shortlist uses CollateGoverned.
func (shortlistCollator) Collate(canon.Result) (ModeOutput, error) {
	return nil, fmt.Errorf("shortlist: a ranking may only be assembled from the HOST TALLY of the recorded ballots; a partition-only collation would be the host choosing without a ballot")
}

// CollateGoverned builds the output from the host tally and the confirmed partition's attributions. It
// computes no ordering of its own.
func (shortlistCollator) CollateGoverned(in CollateInput) (ModeOutput, error) {
	if in.Decision == nil {
		return nil, fmt.Errorf("shortlist: no host tally was recorded — a ranking asserted without one is not a ranking the artifacts support")
	}
	provenance := map[string][]ShortlistNomination{}
	for _, cl := range in.Partition.Clusters {
		for _, m := range cl.Members {
			provenance[cl.CanonicalID] = append(provenance[cl.CanonicalID], ShortlistNomination{
				Explorer: m.SourceExplorer, EnvelopeRef: m.EnvelopeRef, RawNomination: m.RawNomination,
			})
		}
	}
	// Blind round-1 corroboration claims, reported beside the ballot claims.
	salience := map[string]govern.Claim{}
	if in.Governance != nil {
		for _, c := range in.Governance.Claims {
			if c.Query == govern.CorroborationQuery {
				salience[c.Subject] = c
			}
		}
	}

	out := ShortlistOutput{
		Decision:              in.Decision.Rendering,
		DecisionInputsHash:    in.Decision.Frozen.InputsHash,
		PartitionRevisionHash: in.Partition.PartitionRevisionHash,
		Method:                string(in.Decision.Frozen.Inputs.Method),
		Criteria:              append([]govern.Criterion(nil), in.Decision.Frozen.Inputs.Criteria...),
		BallotsCast:           in.Decision.Cast,
		PanelSize:             in.Panel.Selected,
		Respondents:           in.Panel.Respondents(),
		QuorumMet:             in.Decision.QuorumMet,
		TieOutcome:            in.Decision.TieOutcome,
	}
	if in.Governance != nil {
		out.CollatorNarrative = append([]govern.Narrative(nil), in.Governance.CollatorNarrative...)
	}
	for _, e := range in.Decision.Entries {
		if !e.Shortlisted {
			out.Rejects = append(out.Rejects, ShortlistReject{
				Rank: e.Rank, CanonicalID: e.CanonicalID, Name: e.Name, Score: e.Score,
				Approvals: e.Approvals, Support: e.Support, Reason: e.Reason,
				Provenance: provenance[e.CanonicalID],
			})
			continue
		}
		if e.Claim == nil {
			return nil, fmt.Errorf("shortlist: ranked candidate %q carries no governance claim — a ranked entry must be pinned to its inputs", e.CanonicalID)
		}
		entry := ShortlistEntry{
			Rank: e.Rank, CanonicalID: e.CanonicalID, Name: e.Name, Score: e.Score,
			Approvals: e.Approvals, Support: e.Support, Claim: *e.Claim, Voters: e.Voters,
			Provenance: provenance[e.CanonicalID],
		}
		if s, ok := salience[e.CanonicalID]; ok {
			entry.EmergentSalience = &s
		}
		out.Ranked = append(out.Ranked, entry)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

func init() {
	register(ModeSpec{
		Name:             Shortlist,
		FormulationFree:  true,
		Prompt:           schema.ShortlistExplorerPrompt,
		ExplorerSchema:   schema.ShortlistExplorerSchema,
		Objective:        ObjectiveCanonicalizeBallotCollate,
		Canonicalizing:   shortlistCollator{},
		Canonicalization: CanonicalizationPolicy{Dual: true, Confirm: true},
		Rounds:           2,
		LaterRound:       shortlistBallot{},
		Ballot:           shortlistBallot{},
		Class:            schema.EmergentSpace,
	})
}
