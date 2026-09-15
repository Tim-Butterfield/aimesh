package govern

// This file holds the host-tallied decision: criteria with their origin and aggregation method, the
// frozen and hashed decision inputs, and the ballot tally. It calls no model.
//
//   - FreezeDecision hashes every input a ranking depends on. The pipeline freezes before the ballot
//     round and renders the hash into the ballot prompt, so the prompt bytes show the criteria predate
//     the votes. Tally refuses an unfrozen decision.
//   - Models only vote; Tally computes the ranking arithmetically from recorded ballots. A ballot result
//     is an informed preference under shared framing, distinct from consensus and from `emergent`
//     (host-counted blind round-1 salience).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// DecisionRuleVersion identifies the host rule that turns ballots into a ranking. It is recorded on the
// decision and its claims so a ranking can be re-derived.
const DecisionRuleVersion = "host-ballot-tally@v1"

// BallotQuery is the query id on a ballot-placement claim, distinct from CorroborationQuery so a claim
// states which measurement it reports.
const BallotQuery = "ballot-over-confirmed-universe@v1"

// DecisionMethod is a tally method, frozen with the decision inputs.
type DecisionMethod string

// MethodPositionalBallot awards Borda points from each ballot's ranking (index i of N scores N−i; unplaced
// scores 0) and tallies approvals separately, since acceptability and preference order are different
// questions.
const MethodPositionalBallot DecisionMethod = "host-positional-ballot@v1"

// CriterionOrigin records where a decision criterion came from, so a collator-introduced criterion is
// distinguishable from a user-stated one.
type CriterionOrigin string

// Criterion origins.
const (
	OriginUser                       CriterionOrigin = "user"
	OriginExplorerProposed           CriterionOrigin = "explorer_proposed"
	OriginCollatorProposedAuthorized CriterionOrigin = "collator_proposed_authorized"
)

// AggregationMethod records how a criterion's evidence is combined.
type AggregationMethod string

// Aggregation methods.
const (
	AggregationNone              AggregationMethod = "none"
	AggregationEvidenceSynthesis AggregationMethod = "evidence_synthesis"
	AggregationMentionFrequency  AggregationMethod = "mention_frequency"
	AggregationBallot            AggregationMethod = "ballot"
	AggregationCollatorJudgment  AggregationMethod = "collator_judgment"
)

// Authorization is the recorded approval a `collator_proposed_authorized` criterion requires: who
// authorized it, when, which criterion version, and over what scope.
type Authorization struct {
	Actor            string `json:"actor"`
	Timestamp        string `json:"timestamp"`
	CriterionVersion string `json:"criterionVersion"`
	Scope            string `json:"scope"`
}

// Validate reports an error if any field is blank.
func (a Authorization) Validate() error {
	for name, v := range map[string]string{
		"actor": a.Actor, "timestamp": a.Timestamp, "criterionVersion": a.CriterionVersion, "scope": a.Scope,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("criterion authorization: %s is required", name)
		}
	}
	return nil
}

// Criterion is one decision criterion with its origin and aggregation method.
type Criterion struct {
	Name              string            `json:"name"`
	Origin            CriterionOrigin   `json:"origin"`
	AggregationMethod AggregationMethod `json:"aggregationMethod"`
	// Authorization is required when Origin is OriginCollatorProposedAuthorized and forbidden otherwise.
	Authorization *Authorization `json:"authorization,omitempty"`
}

// Validate checks that the criterion has a name, a known origin and aggregation method, and an
// authorization exactly when its origin requires one.
func (c Criterion) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("criterion: empty name")
	}
	switch c.Origin {
	case OriginUser, OriginExplorerProposed, OriginCollatorProposedAuthorized:
	default:
		return fmt.Errorf("criterion %q: unknown origin %q", c.Name, c.Origin)
	}
	switch c.AggregationMethod {
	case AggregationNone, AggregationEvidenceSynthesis, AggregationMentionFrequency, AggregationBallot, AggregationCollatorJudgment:
	default:
		return fmt.Errorf("criterion %q: unknown aggregationMethod %q", c.Name, c.AggregationMethod)
	}
	if c.Origin == OriginCollatorProposedAuthorized {
		if c.Authorization == nil {
			return fmt.Errorf("criterion %q: origin %q requires an explicit persisted authorization event (actor, timestamp, criterion version, scope)", c.Name, c.Origin)
		}
		if err := c.Authorization.Validate(); err != nil {
			return fmt.Errorf("criterion %q: %w", c.Name, err)
		}
	} else if c.Authorization != nil {
		return fmt.Errorf("criterion %q: an authorization event is only meaningful for origin %q", c.Name, OriginCollatorProposedAuthorized)
	}
	return nil
}

// DecisionInputs is everything a ranking depends on, fixed before the ballot round. None of it is model
// output.
type DecisionInputs struct {
	RulesVersion string         `json:"rulesVersion"`
	Method       DecisionMethod `json:"method"`
	// UniverseRevisionHash is the confirmed partition revision the options come from.
	UniverseRevisionHash string `json:"universeRevisionHash"`
	// Presentation is the order the options were shown in, with its seed and rule version.
	Presentation canon.Presentation `json:"presentation"`
	Criteria     []Criterion        `json:"criteria"`
	// ShortlistSize is the number of candidates the shortlist holds.
	ShortlistSize int `json:"shortlistSize"`
	// Quorum, TieRule and MissingResponse are copied from the panel frozen at the start of the run.
	Quorum          int                   `json:"quorum"`
	TieRule         TieRule               `json:"tieRule"`
	MissingResponse MissingResponsePolicy `json:"missingResponse"`
	PolicyHash      string                `json:"policyHash"`
}

// Validate checks that the inputs are complete before they are hashed.
func (d DecisionInputs) Validate() error {
	if d.Method != MethodPositionalBallot {
		return fmt.Errorf("decision inputs: unknown tally method %q", d.Method)
	}
	if strings.TrimSpace(d.UniverseRevisionHash) == "" {
		return fmt.Errorf("decision inputs: no candidate-universe revision hash")
	}
	if len(d.Presentation.Order) == 0 {
		return fmt.Errorf("decision inputs: the candidate universe is empty — there is nothing to put to a ballot")
	}
	if len(d.Criteria) == 0 {
		return fmt.Errorf("decision inputs: no criteria (a ranking with no stated criterion is a preference with a hidden standard)")
	}
	for _, c := range d.Criteria {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	if d.ShortlistSize < 1 || d.ShortlistSize > len(d.Presentation.Order) {
		return fmt.Errorf("decision inputs: shortlist size %d is outside the candidate universe of %d", d.ShortlistSize, len(d.Presentation.Order))
	}
	if d.Quorum < 2 {
		return fmt.Errorf("decision inputs: quorum %d is below 2 — a single ballot is not a panel", d.Quorum)
	}
	if strings.TrimSpace(d.PolicyHash) == "" {
		return fmt.Errorf("decision inputs: no frozen counting-policy hash (freeze the panel with govern.Freeze first)")
	}
	return nil
}

// Hash returns the hex SHA-256 of the inputs' JSON encoding.
func (d DecisionInputs) Hash() string {
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FrozenDecision is a set of validated decision inputs and their hash, as produced by FreezeDecision.
type FrozenDecision struct {
	Inputs     DecisionInputs `json:"inputs"`
	InputsHash string         `json:"inputsHash"`
}

// FreezeDecision validates and hashes the decision inputs. The quorum, tie rule and missing-response
// policy come from the already-frozen panel p.
func FreezeDecision(p Panel, universe canon.Result, pres canon.Presentation, criteria []Criterion, method DecisionMethod, shortlistSize int) (FrozenDecision, error) {
	in := DecisionInputs{
		RulesVersion:         RulesVersion,
		Method:               method,
		UniverseRevisionHash: universe.PartitionRevisionHash,
		Presentation:         pres,
		Criteria:             append([]Criterion(nil), criteria...),
		ShortlistSize:        shortlistSize,
		Quorum:               p.Policy.Quorum,
		TieRule:              p.Policy.TieRule,
		MissingResponse:      p.Policy.MissingResponse,
		PolicyHash:           p.PolicyHash,
	}
	if err := in.Validate(); err != nil {
		return FrozenDecision{}, err
	}
	return FrozenDecision{Inputs: in, InputsHash: in.Hash()}, nil
}

// Render returns the header the host prepends to the ballot prompt: the criteria, method, quorum, tie
// rule, cut and inputs hash the ballot is cast under.
//
// The header must not start with '-': adapters that pass the prompt as a command-line argument (ollama,
// devin-cli, agy-cli, cursor-cli) would parse it as an option.
func (f FrozenDecision) Render() string {
	var b strings.Builder
	b.WriteString("===== FROZEN DECISION INPUTS (host-authored; fixed and hashed BEFORE this ballot was requested) =====\n")
	fmt.Fprintf(&b, "inputsHash: %s\n", f.InputsHash)
	fmt.Fprintf(&b, "rules: %s; method: %s; tally: performed by the HOST, never by a model\n", f.Inputs.RulesVersion, f.Inputs.Method)
	fmt.Fprintf(&b, "candidate universe: %d option(s) at partition revision %s (presented order rule %s, seed %s)\n",
		len(f.Inputs.Presentation.Order), short(f.Inputs.UniverseRevisionHash), f.Inputs.Presentation.RuleVersion, short(f.Inputs.Presentation.Seed))
	fmt.Fprintf(&b, "shortlist size: %d; quorum: %d respondent(s); tie rule: %s; missing responses: %s; counting policy %s\n",
		f.Inputs.ShortlistSize, f.Inputs.Quorum, f.Inputs.TieRule, f.Inputs.MissingResponse, short(f.Inputs.PolicyHash))
	b.WriteString("criteria (with the origin and aggregation method each was frozen under):\n")
	for _, c := range f.Inputs.Criteria {
		fmt.Fprintf(&b, "  - %s [origin: %s; aggregation: %s]\n", c.Name, c.Origin, c.AggregationMethod)
	}
	b.WriteString("===== END FROZEN DECISION INPUTS =====\n")
	return b.String()
}

// Ballot is one explorer's recorded preference over the confirmed candidates. Ranking is best first and
// may be partial; Approved lists the candidates the voter finds acceptable.
type Ballot struct {
	By          schema.ExplorerIdentity `json:"by"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Ranking     []string                `json:"ranking"`
	Approved    []string                `json:"approved"`
	// Rationale is the voter's reasoning. It is excluded from JSON and moved into the narrative namespace.
	Rationale string `json:"-"`
}

// Validate checks that every ranked or approved id is in universe, that the ranking has no repeats, and
// that the ballot expresses some preference.
func (b Ballot) Validate(universe map[string]bool) error {
	seen := map[string]bool{}
	for _, id := range b.Ranking {
		if !universe[id] {
			return fmt.Errorf("ballot from %s/%s ranks %q, which is not in the confirmed candidate universe", b.By.Adapter, b.By.Model, id)
		}
		if seen[id] {
			return fmt.Errorf("ballot from %s/%s ranks %q more than once", b.By.Adapter, b.By.Model, id)
		}
		seen[id] = true
	}
	for _, id := range b.Approved {
		if !universe[id] {
			return fmt.Errorf("ballot from %s/%s approves %q, which is not in the confirmed candidate universe", b.By.Adapter, b.By.Model, id)
		}
	}
	if len(b.Ranking) == 0 && len(b.Approved) == 0 {
		return fmt.Errorf("ballot from %s/%s expresses no preference at all", b.By.Adapter, b.By.Model)
	}
	return nil
}

// DecisionEntry is one candidate's position in the tally. Claim is set only for a shortlisted candidate.
type DecisionEntry struct {
	Rank        int                       `json:"rank"`
	CanonicalID string                    `json:"canonicalId"`
	Name        string                    `json:"name"`
	Score       int                       `json:"score"`
	Approvals   int                       `json:"approvals"`
	Support     int                       `json:"support"` // distinct ballots that PLACED this candidate
	Voters      []schema.ExplorerIdentity `json:"voters"`
	Tied        bool                      `json:"tied"`
	Shortlisted bool                      `json:"shortlisted"`
	Claim       *Claim                    `json:"claim,omitempty"`
	// Reason is the host's explanation of why a candidate was not shortlisted.
	Reason string `json:"reason,omitempty"`
}

// Decision is the host tally: the frozen inputs, the recorded ballots and the ranked entries.
type Decision struct {
	Frozen      FrozenDecision  `json:"frozen"`
	Ballots     []Ballot        `json:"ballots"`
	Entries     []DecisionEntry `json:"entries"`
	Cast        int             `json:"cast"`
	QuorumMet   bool            `json:"quorumMet"`
	TieOutcome  string          `json:"tieOutcome,omitempty"`
	RuleVersion string          `json:"ruleVersion"`
	// Rendering is the human-readable description of the decision, persisted with it.
	Rendering string `json:"rendering"`
}

// Shortlisted returns the entries that made the frozen cut, in rank order.
func (d Decision) Shortlisted() []DecisionEntry {
	out := make([]DecisionEntry, 0, len(d.Entries))
	for _, e := range d.Entries {
		if e.Shortlisted {
			out = append(out, e)
		}
	}
	return out
}

// Rejected returns the entries that did not make the cut, in rank order, each with its Reason.
func (d Decision) Rejected() []DecisionEntry {
	out := make([]DecisionEntry, 0, len(d.Entries))
	for _, e := range d.Entries {
		if !e.Shortlisted {
			out = append(out, e)
		}
	}
	return out
}

// TallyInput is the input to Tally. Baseline is the blind first round, used to compute sensitivity for
// contested mappings on the same evidence as the corroboration claims.
type TallyInput struct {
	Frozen          FrozenDecision
	Ballots         []Ballot
	Partition       canon.Result
	Contested       []canon.ContestedMapping
	Baseline        BlindBaseline
	Panel           Panel
	FormulationHash string
	// BallotRoundID is the round the ballots were cast in, recorded on every ballot claim.
	BallotRoundID string
}

// Tally computes the ranking from the recorded ballots: positional points, separate approval counts,
// deterministic ordering, the frozen cut, and a claim for each shortlisted entry. The `ranked` label is
// withheld for a contested partition, a missed quorum, or a tie at the shortlist boundary.
func Tally(in TallyInput) (Decision, error) {
	if strings.TrimSpace(in.Frozen.InputsHash) == "" {
		return Decision{}, fmt.Errorf("tally: the decision inputs were not frozen — a ranking may only be computed under inputs fixed + hashed BEFORE the ballot was solicited")
	}
	if in.Frozen.InputsHash != in.Frozen.Inputs.Hash() {
		return Decision{}, fmt.Errorf("tally: the frozen decision inputs do not match their hash — the framing changed after the freeze")
	}
	order := in.Frozen.Inputs.Presentation.Order
	universe := map[string]bool{}
	for _, id := range order {
		universe[id] = true
	}
	names := map[string]string{}
	for _, c := range in.Partition.Clusters {
		names[c.CanonicalID] = c.Name
	}

	type agg struct {
		score, approvals int
		voters           []schema.ExplorerIdentity
		refs             map[string]bool
	}
	tallies := map[string]*agg{}
	for _, id := range order {
		tallies[id] = &agg{refs: map[string]bool{}}
	}
	n := len(order)
	for _, b := range in.Ballots {
		if err := b.Validate(universe); err != nil {
			return Decision{}, fmt.Errorf("tally: %w", err)
		}
		for i, id := range b.Ranking {
			// First place scores n; any placed candidate scores at least 1, more than an unplaced one.
			pts := max(n-i, 1)
			t := tallies[id]
			t.score += pts
			t.voters = append(t.voters, b.By)
			t.refs[b.EnvelopeRef] = true
		}
		for _, id := range b.Approved {
			tallies[id].approvals++
		}
	}

	// Order by score, support, approvals, then canonical ID; the last key only makes output deterministic.
	entries := make([]DecisionEntry, 0, n)
	for _, id := range order {
		t := tallies[id]
		entries = append(entries, DecisionEntry{
			CanonicalID: id, Name: names[id], Score: t.score, Approvals: t.approvals,
			Support: len(t.voters), Voters: t.voters,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		switch {
		case a.Score != b.Score:
			return a.Score > b.Score
		case a.Support != b.Support:
			return a.Support > b.Support
		case a.Approvals != b.Approvals:
			return a.Approvals > b.Approvals
		default:
			return a.CanonicalID < b.CanonicalID
		}
	})

	// Entries with equal score, support and approvals share a rank and are marked tied.
	rank := 0
	for i := range entries {
		if i == 0 || !sameStanding(entries[i-1], entries[i]) {
			rank = i + 1
		} else {
			entries[i-1].Tied, entries[i].Tied = true, true
		}
		entries[i].Rank = rank
	}

	// A tie group straddling the cut is carried into the shortlist whole, with the `ranked` label withheld.
	cut := in.Frozen.Inputs.ShortlistSize
	cutRank := entries[minInt(cut, len(entries))-1].Rank
	boundaryTie := ""
	for i := range entries {
		entries[i].Shortlisted = entries[i].Rank <= cutRank
	}
	straddling := map[int]bool{}
	if countShortlisted(entries) > cut {
		tiedAtCut := 0
		for _, e := range entries {
			if e.Rank == cutRank {
				tiedAtCut++
			}
		}
		straddling[cutRank] = true
		boundaryTie = fmt.Sprintf(
			"tie at the shortlist boundary: %d candidate(s) share rank %d, which straddles the frozen shortlist size of %d; under the frozen tie rule %q no winner is picked — every tied candidate is CARRIED into the subset and the definitive `ranked` label is WITHHELD for them",
			tiedAtCut, cutRank, cut, in.Frozen.Inputs.TieRule)
	}

	quorumMet := in.Panel.QuorumMet() && len(in.Ballots) >= in.Frozen.Inputs.Quorum
	for i := range entries {
		if !entries[i].Shortlisted {
			entries[i].Reason = rejectReason(entries[i], cutRank)
			continue
		}
		claim := ballotClaim(in, entries[i], straddling[entries[i].Rank], quorumMet)
		entries[i].Claim = &claim
	}

	return Decision{
		Frozen: in.Frozen, Ballots: append([]Ballot(nil), in.Ballots...), Entries: entries,
		Cast: len(in.Ballots), QuorumMet: quorumMet, TieOutcome: boundaryTie,
		RuleVersion: DecisionRuleVersion,
		Rendering: fmt.Sprintf(
			"%d ballot(s) cast by %d panel member(s) (%d respondents) over %d confirmed candidate(s) at partition %s, under decision inputs frozen as %s [%s]. "+
				"This is an INFORMED PREFERENCE UNDER SHARED FRAMING tallied by the host — it is NOT consensus, and it is NOT `emergent` "+
				"(which is host-counted blind round-1 salience: how many members independently NAMED a candidate, not how good it is).",
			len(in.Ballots), in.Panel.Selected, in.Panel.Respondents(), n,
			short(in.Frozen.Inputs.UniverseRevisionHash), short(in.Frozen.InputsHash), DecisionRuleVersion),
	}, nil
}

// sameStanding reports whether a and b have the same score, support and approvals.
func sameStanding(a, b DecisionEntry) bool {
	return a.Score == b.Score && a.Support == b.Support && a.Approvals == b.Approvals
}

// countShortlisted counts the entries currently inside the cut.
func countShortlisted(entries []DecisionEntry) int {
	n := 0
	for _, e := range entries {
		if e.Shortlisted {
			n++
		}
	}
	return n
}

// rejectReason explains why e fell outside the shortlist.
func rejectReason(e DecisionEntry, cutRank int) string {
	if e.Support == 0 {
		return "not placed on any ballot (score 0) — carried as a nominated candidate the panel did not rank"
	}
	return fmt.Sprintf("ranked %d (score %d, placed by %d ballot(s), %d approval(s)) — below the frozen shortlist cut at rank %d",
		e.Rank, e.Score, e.Support, e.Approvals, cutRank)
}

// ballotClaim builds the claim for a shortlisted candidate: the number of ballots that placed it, with
// both denominators.
func ballotClaim(in TallyInput, e DecisionEntry, tiedAtBoundary, quorumMet bool) Claim {
	panelDen, respDen := in.Panel.Denominators(e.Support)
	refs := map[string]bool{}
	for _, b := range in.Ballots {
		for _, id := range b.Ranking {
			if id == e.CanonicalID {
				refs[b.EnvelopeRef] = true
			}
		}
	}
	c := Claim{
		Query: BallotQuery, Subject: e.CanonicalID, SubjectLabel: e.Name, Value: e.Support,
		KOfPanel: panelDen, KOfRespondents: respDen,
		FormulationHash: in.FormulationHash, PartitionRevisionHash: in.Partition.PartitionRevisionHash,
		RulesVersion: RulesVersion, PolicyHash: in.Panel.PolicyHash,
		BaselineRoundID: in.BallotRoundID, ContributingSourceIDs: sortedKeys(refs),
	}
	c.Sensitivity = ballotSensitivity(in, e)
	switch {
	case c.Sensitivity != nil:
		c.Label = LabelWithheldContested
	case !quorumMet:
		c.Label = LabelWithheldBelowQuorum
	case tiedAtBoundary:
		c.Label = LabelWithheldTie
	default:
		c.Label = LabelRanked
	}
	return c
}

// ballotSensitivity returns e's support range across both plausible partitions when a contested mapping
// affects it, or nil. If the alternative joins entities, their supporters pool (the upper bound). If it
// separates this entity, the voted option would not exist, so the lower bound is 0.
func ballotSensitivity(in TallyInput, e DecisionEntry) *Sensitivity {
	support := map[string]int{}
	supporters := map[string]map[schema.ExplorerIdentity]bool{}
	for _, b := range in.Ballots {
		for _, id := range b.Ranking {
			if supporters[id] == nil {
				supporters[id] = map[schema.ExplorerIdentity]bool{}
			}
			supporters[id][b.By] = true
		}
	}
	for id, s := range supporters {
		support[id] = len(s)
	}

	var ids []string
	var notes []string
	direction := ""
	alt := e.Support
	affected := false
	for _, m := range in.Contested {
		if !m.Affects(e.CanonicalID) {
			continue
		}
		affected = true
		ids = append(ids, m.HeldCanonicalIDs...)
		direction = string(m.Direction)
		switch m.Direction {
		case canon.DirectionAlternativeJoins:
			pooled := map[schema.ExplorerIdentity]bool{}
			for _, held := range m.HeldCanonicalIDs {
				for who := range supporters[held] {
					pooled[who] = true
				}
			}
			if len(pooled) > alt {
				alt = len(pooled)
			}
		case canon.DirectionAlternativeSeparates:
			alt = 0
		}
		notes = append(notes, m.Reason)
	}
	if !affected {
		return nil
	}
	low, high := e.Support, alt
	if alt < e.Support {
		low, high = alt, e.Support
	}
	return &Sensitivity{
		Low: low, High: high, HeldValue: e.Support, AlternativeValue: alt, Direction: direction,
		ContestedCanonicalIDs: dedupe(ids),
		Note: "the entity resolution behind this candidate is contested, so the ballot's option set is itself " +
			"conditional; computed over both plausible partitions: " + strings.Join(notes, " | "),
	}
}

// minInt returns the smaller of a and b.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
