package mode

// This file is the SHORTLIST mode (design §3 Shortlist row / §4) — diverge, confirm the universe, then
// put it to an explicit ballot:
//
//	round 1 (BLIND)   each explorer enumerates candidate options broadly; nobody chooses yet
//	canonicalize      TWO independent canonicalizers; only merges BOTH propose hold
//	confirm           the BINDING confirmation round fixes the CANDIDATE UNIVERSE the ballot runs over
//	FREEZE            the host fixes + hashes the decision inputs — universe revision, presented order,
//	                  criteria (with origin + aggregation method), method, shortlist size, quorum, tie rule,
//	                  missing-response policy — BEFORE the ballot exists
//	round 2 (BALLOT)  each explorer ranks/approves the CONFIRMED canonical IDs in a randomized presented order
//	tally             the HOST computes the ranking (govern.Tally) and pins a claim to every shortlisted entry
//
// Three separations do the work, and none of them is a matter of style:
//
//   - The universe is fixed BEFORE anyone votes, by a process the voters themselves could contest. A ballot
//     over a universe one party assembled after seeing the options is a ballot that party has already won.
//   - The framing is fixed BEFORE anyone votes, and its hash is rendered into the ballot prompt. §4's rule is
//     that a criterion introduced after seeing which candidate it favors is not a criterion; the freeze is
//     what makes that checkable from the persisted bytes.
//   - The RANKING is computed by the host from the ballots. A mode contract here cannot produce an ordering —
//     BallotContract has no method that returns one — because a ranking a model could assert is a ranking the
//     artifacts do not support.
//
// And the label stays honest: a ballot is an INFORMED PREFERENCE UNDER SHARED FRAMING. It is not consensus,
// and `voted` is not `emergent` — emergent is host-counted blind round-1 salience (who NAMED a candidate),
// which the same run also reports, side by side, so the two can never be quietly substituted for each other.

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// maxShortlistSize caps the frozen cut. A "shortlist" that holds most of the universe has not decided
// anything, so the cut is the top half of the confirmed universe, never more than this.
const maxShortlistSize = 5

// --- the terminal output (design §3 Shortlist row) ---

// ShortlistOutput is the FIXED, exploremesh-owned terminal output of the Shortlist mode: the host-tallied
// ranked subset with each entry pinned to its govern.Claim, per-candidate provenance, the rejects WITH host
// reasons, the frozen criteria with their origin + method, the tie/quorum outcome, and any contested-partition
// sensitivity. It lives in this package rather than internal/schema because it embeds govern types, and
// govern sits above schema in the import graph.
type ShortlistOutput struct {
	// Decision is the honest rendering of what this result IS (host-authored, persisted with the value):
	// an informed preference under shared framing, tallied by the host — never consensus, never emergent.
	Decision string `json:"decision"`
	// DecisionInputsHash is the hash of the inputs frozen BEFORE the ballot was solicited; the same value the
	// voters saw rendered in their prompt.
	DecisionInputsHash    string `json:"decisionInputsHash"`
	PartitionRevisionHash string `json:"partitionRevisionHash"`
	Method                string `json:"method"`
	// Ranked is the shortlist in host-tallied order. An entry whose definitive `ranked` label was WITHHELD is
	// still here — withholding a label is not the same as removing a candidate.
	Ranked []ShortlistEntry `json:"ranked"`
	// Rejects are the candidates that did not make the frozen cut, carried with the HOST's reason. The panel
	// nominated them, so they are never silently dropped (§4's minority carry-through, applied to a decision).
	Rejects []ShortlistReject `json:"rejects,omitempty"`
	// Criteria are the FROZEN criteria with their origin + aggregationMethod (§4).
	Criteria []govern.Criterion `json:"criteria"`
	// BallotsCast / PanelSize / Respondents are the participation facts behind the tally.
	BallotsCast int  `json:"ballotsCast"`
	PanelSize   int  `json:"panelSize"`
	Respondents int  `json:"respondents"`
	QuorumMet   bool `json:"quorumMet"`
	// TieOutcome states the frozen tie rule's effect when a tie straddled the cut ("" when none did).
	TieOutcome string `json:"tieOutcome,omitempty"`
	// CollatorNarrative is the quarantined MODEL PROSE namespace (§0 F-C) — voters' rationales and the
	// canonicalizers' coverage notes live here and nowhere else.
	CollatorNarrative []govern.Narrative `json:"collatorNarrative,omitempty"`
}

// ShortlistEntry is one ranked candidate.
type ShortlistEntry struct {
	Rank        int    `json:"rank"`
	CanonicalID string `json:"canonicalId"`
	Name        string `json:"name"`
	// Score / Approvals / Support are HOST arithmetic over the recorded ballots (positional points; the
	// separate acceptability tally; the number of ballots that placed this candidate at all).
	Score     int `json:"score"`
	Approvals int `json:"approvals"`
	Support   int `json:"support"`
	// Claim is the pinned governance claim: the label (ranked | withheld_contested_partition |
	// withheld_below_quorum | withheld_tie), BOTH denominators, the frozen policy + partition + rules, and a
	// sensitivity range when a contested mapping reaches this candidate.
	Claim govern.Claim `json:"claim"`
	// Voters are the explorers that placed this candidate on their ballot (attribution, not a tally).
	Voters []schema.ExplorerIdentity `json:"voters"`
	// Provenance is who NOMINATED this candidate in the immutable BLIND round 1, with their exact wording —
	// deliberately a different measurement from the ballot support above, reported alongside it so `emergent`
	// and `voted` can be compared rather than confused (§4).
	Provenance []ShortlistNomination `json:"provenance"`
	// EmergentSalience is the host's BLIND round-1 corroboration claim for the same entity, when one was
	// emitted — the explicit side-by-side of salience and preference.
	EmergentSalience *govern.Claim `json:"emergentSalience,omitempty"`
}

// ShortlistNomination is one blind round-1 nomination behind a candidate.
type ShortlistNomination struct {
	Explorer      schema.ExplorerIdentity `json:"explorer"`
	EnvelopeRef   string                  `json:"envelopeRef"`
	RawNomination string                  `json:"rawNomination"`
}

// ShortlistReject is a candidate that did not make the frozen cut, with the HOST's arithmetic reason.
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

// Summary returns the one-line human summary. It names the ranked subset and how many labels were withheld —
// it never says "chosen" or "consensus".
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

// Validate checks the shortlist is usable: a non-empty ranked subset and a recorded decision rendering (the
// honest label is not optional — a result that lost it could be read as a consensus claim).
func (o ShortlistOutput) Validate() error {
	if len(o.Ranked) == 0 {
		return fmt.Errorf("shortlist output ranked no candidates")
	}
	if o.Decision == "" {
		return fmt.Errorf("shortlist output carries no decision rendering — a ballot result must state what it is")
	}
	return nil
}

// --- the round-2 (BALLOT) contract ---

// shortlistBallot is BOTH the Shortlist mode's LaterRoundContract (it renders the ballot round) and its
// BallotContract (it declares the frozen criteria and parses one ballot). Note again what it cannot do: no
// method here returns an ordering. The host freezes, solicits and tallies.
type shortlistBallot struct{}

// Accepts declares the round→round edge: only the pooled confirmed-canonical uniques, at the current artifact
// schema version. A ballot over anything else — raw peer output, an unconfirmed partition — is rejected before
// spend, which is what makes "the ballot ran over the CONFIRMED universe" mechanical.
func (shortlistBallot) Accepts() round.Accepts {
	return round.Accepts{Kinds: []round.Kind{round.KindCanonicalUniques}, SchemaVersion: round.SchemaVersion}
}

func (shortlistBallot) Prompt(raw schema.RawTask, untrustedDataBlock string) string {
	return schema.BallotPrompt(raw, untrustedDataBlock)
}

func (shortlistBallot) ExplorerSchema() schema.Schema { return schema.BallotSchema() }

// Criteria are the FROZEN decision criteria. They are derived from the USER's stated criteria and carry
// origin `user` + aggregationMethod `ballot`: the user said what matters, and the panel's ballots are how
// those criteria are aggregated. Nothing here is collator-proposed, so no authorization event is required —
// and a criterion this mode did NOT get from the user could not be smuggled in, because there is no code path
// that adds one.
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

// Method is the versioned HOST tally rule.
func (shortlistBallot) Method() govern.DecisionMethod { return govern.MethodPositionalBallot }

// ShortlistSize is the frozen cut: the top half of the confirmed universe, at least 1 and at most
// maxShortlistSize. It is a pure function of the UNIVERSE SIZE — it cannot depend on the tally, because it is
// computed and hashed before any ballot exists.
func (shortlistBallot) ShortlistSize(universeSize int) int {
	n := (universeSize + 1) / 2
	if n > maxShortlistSize {
		n = maxShortlistSize
	}
	if n < 1 {
		n = 1
	}
	return n
}

// ParseBallot lifts ONE explorer's ballot out of its recorded ballot-round envelope and validates it against
// the confirmed universe. An entry naming a candidate outside the universe is an ERROR: the pipeline records
// that explorer as having cast no usable ballot rather than silently deleting the entry, because deleting it
// would change the voter's expressed preference without saying so.
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

// --- the canonicalizing + governed terminal contract ---

// shortlistCollator is the Shortlist mode's terminal contract: a CanonicalizingContract over the raw candidate
// nominations, and a GovernedCollator that assembles the ranked subset as a view over the host tally.
type shortlistCollator struct{}

// Nominations flattens the verified blind round-1 panel into raw candidate nominations, one per candidate
// string per explorer.
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

// shortlistNominationWire is the per-nomination shape rendered into the canonicalizer prompt.
type shortlistNominationWire struct {
	Index          int                     `json:"index"`
	Raw            string                  `json:"raw"`
	SourceExplorer schema.ExplorerIdentity `json:"sourceExplorer"`
}

// CanonicalizerPrompt renders the canonicalizer instruction for a BALLOT universe. It carries one instruction
// Catalog's does not need: the labels become the ballot's option names, so a label that editorializes is a
// thumb on the scale — the canonicalizer is told to name entities neutrally and told why.
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
	return "You are the CANONICALIZER — a role DISTINCT from the collator and from the explorers (design §4). " +
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

// ParseProposal decodes the proposed partition via the package-shared decoder, stamping the deciding call +
// verified canonicalizer identity. Surjectivity remains canon's gate.
func (shortlistCollator) ParseProposal(raw []byte, _ []canon.Nomination, decidedByCall string, identity schema.ExplorerIdentity) (canon.Proposal, error) {
	return parseClusterProposal(raw, decidedByCall, identity)
}

// Collate is the PARTITION-ONLY fallback required by CanonicalizingContract. Shortlist always runs governed:
// without the host tally there is no ranking, and a "shortlist" assembled from a partition alone would be the
// host silently choosing.
func (shortlistCollator) Collate(canon.Result) (ModeOutput, error) {
	return nil, fmt.Errorf("shortlist: a ranking may only be assembled from the HOST TALLY of the recorded ballots; a partition-only collation would be the host choosing without a ballot")
}

// CollateGoverned assembles the ranked subset as a deterministic view over the host tally: every number comes
// from govern.Tally, every claim from the same place, and the provenance from the confirmed partition's
// attributed members. It computes no ordering of its own.
func (shortlistCollator) CollateGoverned(in CollateInput) (ModeOutput, error) {
	if in.Decision == nil {
		return nil, fmt.Errorf("shortlist: no host tally was recorded — a ranking asserted without one is not a ranking the artifacts support (design §4)")
	}
	provenance := map[string][]ShortlistNomination{}
	for _, cl := range in.Partition.Clusters {
		for _, m := range cl.Members {
			provenance[cl.CanonicalID] = append(provenance[cl.CanonicalID], ShortlistNomination{
				Explorer: m.SourceExplorer, EnvelopeRef: m.EnvelopeRef, RawNomination: m.RawNomination,
			})
		}
	}
	// The BLIND round-1 salience claims, reported alongside the ballot claims so `emergent` and `voted` sit
	// side by side and neither can be read as the other (§4).
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
			return nil, fmt.Errorf("shortlist: ranked candidate %q carries no governance claim — a ranked entry must be pinned to its inputs (design §0 F-C)", e.CanonicalID)
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
	// Shortlist (design §3). Formulation-free; ballot-bearing, so the full ranking-grade policy applies
	// (Dual + Confirm — the confirmed partition IS the ballot's option set) and the pipeline freezes + hashes
	// the decision inputs before dispatching the ballot round.
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
		// EMERGENT space: the candidate universe is explorer-authored, so grouping it is entity resolution and
		// may only happen through the recorded ledger (§0 F-B).
		Class: schema.EmergentSpace,
	})
}
