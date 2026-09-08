package govern

// This file is the HOST-TALLIED DECISION (design §3 Shortlist row / §4): the criterion vocabulary with
// its ORIGIN + AGGREGATION METHOD, the FROZEN + HASHED decision inputs, and the ballot tally itself. Like the
// rest of this package it calls no model — every value here is a deterministic host rule over persisted
// artifacts (§0 F-C).
//
// Two properties are load-bearing and are enforced by construction rather than by convention:
//
//   - THE FREEZE PRECEDES THE JUDGMENT. FreezeDecision hashes the candidate-universe revision, the presented
//     option order, the criterion set, the tally method, the shortlist size and the quorum / tie /
//     missing-response policy into one InputsHash. The pipeline calls it BEFORE the ballot round is
//     dispatched, and the host renders that hash INTO the ballot prompt — so "the criteria predate the votes"
//     is a property of the persisted prompt bytes, not a claim about the code (§4: "else a criterion is
//     introduced after seeing which candidate it favors"). Tally REFUSES an unfrozen decision.
//   - THE HOST TALLIES; THE MODEL ONLY VOTES. Tally is arithmetic over the recorded ballots and nothing else.
//     No model ever asserts a ranking, and Decision.Rendering states what a ballot actually is — an INFORMED
//     PREFERENCE UNDER SHARED FRAMING (§4). It is never consensus, and it is never `emergent`: emergent is
//     host-counted blind round-1 salience, a different measurement of a different thing.

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

// DecisionRuleVersion is the VERSIONED host rule that turns recorded ballots into a ranking. It is persisted
// on the decision and on every claim it emits, so a ranking can always be re-derived from the recorded
// ballots by the exact rule that produced it.
const DecisionRuleVersion = "host-ballot-tally@v1"

// BallotQuery is the versioned query id recorded on a ballot-placement claim (§9). It is deliberately
// DIFFERENT from CorroborationQuery: `voted` and `emergent` are two different measurements and a reader must
// never have to infer which one a claim reports.
const BallotQuery = "ballot-over-confirmed-universe@v1"

// DecisionMethod is the frozen tally method (§4: the method is frozen + hashed before judgments are solicited).
type DecisionMethod string

// MethodPositionalBallot is the v1 host tally: each ballot's RANKING awards positional (Borda) points — an
// option a voter placed at index i of a universe of N scores N−i, an option that voter did not place scores
// 0 — and APPROVALS are tallied separately. Approvals are deliberately not folded into the score: "which of
// these would you accept at all" is a different question from "in what order do you prefer them", and
// summing the two would be the host inventing a weighting nobody froze.
const MethodPositionalBallot DecisionMethod = "host-positional-ballot@v1"

// --- Criteria: origin + aggregation method (design §4) ---

// CriterionOrigin records WHERE a decision criterion came from (§4). It matters because a criterion the
// collator introduced carries a different epistemic weight from one the user stated, and a result that hides
// the difference invites the collator to author the standard it is then judged against.
type CriterionOrigin string

const (
	OriginUser                       CriterionOrigin = "user"
	OriginExplorerProposed           CriterionOrigin = "explorer_proposed"
	OriginCollatorProposedAuthorized CriterionOrigin = "collator_proposed_authorized"
)

// AggregationMethod records HOW a criterion's evidence is combined (§4) — so a reader can tell a ballot from
// a mention count from a collator judgment without inferring it from the prose.
type AggregationMethod string

const (
	AggregationNone              AggregationMethod = "none"
	AggregationEvidenceSynthesis AggregationMethod = "evidence_synthesis"
	AggregationMentionFrequency  AggregationMethod = "mention_frequency"
	AggregationBallot            AggregationMethod = "ballot"
	AggregationCollatorJudgment  AggregationMethod = "collator_judgment"
)

// Authorization is the EXPLICIT PERSISTED EVENT §4 requires before a `collator_proposed_authorized` criterion
// may be used: who authorized it, when, which criterion version, and over what scope. Without it the origin
// is not usable — that is the whole point of the origin existing.
type Authorization struct {
	Actor            string `json:"actor"`
	Timestamp        string `json:"timestamp"`
	CriterionVersion string `json:"criterionVersion"`
	Scope            string `json:"scope"`
}

// Validate checks the authorization event is complete (every field carries a decision's audit weight).
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

// Criterion is one decision criterion with its origin + aggregation method (§4).
type Criterion struct {
	Name              string            `json:"name"`
	Origin            CriterionOrigin   `json:"origin"`
	AggregationMethod AggregationMethod `json:"aggregationMethod"`
	// Authorization is REQUIRED when Origin is collator_proposed_authorized and forbidden otherwise.
	Authorization *Authorization `json:"authorization,omitempty"`
}

// Validate checks the criterion is usable: a name, a known origin, a known aggregation method, and — for a
// collator-proposed criterion — the explicit authorization event §4 requires. A collator-proposed criterion
// WITHOUT an authorization is an error rather than a downgrade: silently demoting it to `explorer_proposed`
// would launder exactly the provenance the field exists to record.
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

// --- The frozen decision inputs (design §4) ---

// DecisionInputs is EVERYTHING a ranking depends on, fixed before any ranking judgment is solicited (§4).
// Every field is host material or frozen policy; none of it is model output.
type DecisionInputs struct {
	RulesVersion string         `json:"rulesVersion"`
	Method       DecisionMethod `json:"method"`
	// UniverseRevisionHash is the CONFIRMED partition revision the option set is taken from — the same value
	// every claim computed over that partition pins.
	UniverseRevisionHash string `json:"universeRevisionHash"`
	// Presentation is the persisted, randomized order the options were shown in (canon.Present), carried whole
	// so the exact ballot a voter saw is reconstructable — including the seed and the rule version that
	// produced the order.
	Presentation canon.Presentation `json:"presentation"`
	Criteria     []Criterion        `json:"criteria"`
	// ShortlistSize is the frozen cut: how many candidates the shortlist holds. Frozen for the same reason the
	// criteria are — a cut chosen after the tally is a cut chosen to include or exclude a specific candidate.
	ShortlistSize int `json:"shortlistSize"`
	// Quorum / TieRule / MissingResponse are READ OUT OF the panel govern.Freeze already froze at the start of
	// the run, never re-authored here — so they predate not merely the ballot but every explorer call.
	Quorum          int                   `json:"quorum"`
	TieRule         TieRule               `json:"tieRule"`
	MissingResponse MissingResponsePolicy `json:"missingResponse"`
	PolicyHash      string                `json:"policyHash"`
}

// Validate checks the inputs are complete and usable BEFORE they are hashed — an incomplete freeze is worse
// than no freeze, because it looks like a commitment and is not one.
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

// Hash returns the hex SHA-256 of the canonical inputs JSON — the value rendered into the ballot prompt and
// pinned on the decision, proving the framing a ballot was cast under.
func (d DecisionInputs) Hash() string {
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FrozenDecision is the sealed record: the inputs plus their hash. It exists as a distinct type so a tally
// cannot be handed loose inputs — Tally takes a FrozenDecision, and the only way to obtain one is
// FreezeDecision, which validates and hashes.
type FrozenDecision struct {
	Inputs     DecisionInputs `json:"inputs"`
	InputsHash string         `json:"inputsHash"`
}

// FreezeDecision fixes + hashes every decision input BEFORE any ranking judgment is solicited (§4). The
// quorum, tie rule and missing-response policy are taken from the panel govern.Freeze already froze at the
// start of the run rather than re-authored here, so they are older than the ballot by an entire fan-out.
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

// Render is the HOST-owned header prepended to the ballot prompt. It is host material, so it lives OUTSIDE
// the untrusted-data block and is not something a mode contract can re-word: a voter sees the exact criteria,
// method, quorum, tie rule and cut it is voting under, plus the hash of all of them. That hash can only exist
// in the prompt bytes if the freeze already happened — which is what makes "frozen before the ballot" an
// auditable fact rather than an assurance.
// The delimiters are `=====`, deliberately NOT `-----`. This string is the FIRST bytes of the ballot
// prompt, and the argv-passing adapters (ollama, devin-cli, agy-cli, cursor-cli) hand the prompt to
// their CLI as a positional argument. A prompt opening with a dash is parsed by those CLIs as an
// option: measured 2026-08-30, a shortlist run lost 3 of 5 seats this way — devin exited 2 printing
// its usage, cursor exited 1 echoing prompt text — while the three PromptOnStdin recipes (codex-cli,
// claude-code, gemini-cli) were unaffected. Quorum failed and the ranking was withheld. Keep the first
// character of this header a letter or `=`.
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

// --- Ballots + the host tally (design §4) ---

// Ballot is ONE explorer's recorded preference over the CONFIRMED universe. Ranking is ordered (best first)
// and may be partial; Approved is a separate acceptability question.
type Ballot struct {
	By          schema.ExplorerIdentity `json:"by"`
	EnvelopeRef string                  `json:"envelopeRef"`
	Ranking     []string                `json:"ranking"`
	Approved    []string                `json:"approved"`
	// Rationale is the voter's stated reasoning — MODEL PROSE. It is carried on the value only so the host can
	// move it into the collatorNarrative namespace; `json:"-"` keeps it out of the machine record entirely, so
	// there is no serialized governance field a model's words can occupy (§0 F-C).
	Rationale string `json:"-"`
}

// Validate checks a ballot against the frozen universe: every ranked/approved entry must name a candidate in
// it, and the ranking must not repeat one. An unrecognized entry is an ERROR rather than a silent drop — a
// ballot naming a candidate that does not exist is a protocol failure worth surfacing, and quietly discarding
// it would change the voter's expressed preference without saying so.
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

// DecisionEntry is ONE candidate's position in the host tally. Every numeric field is host arithmetic over
// the recorded ballots; Claim is the pinned governance claim, present ONLY for a shortlisted candidate (a
// candidate that did not make the cut has no ranked claim to pin, and inventing one would be asserting a
// verdict the ballot did not produce).
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
	// Reason is the HOST's reason a candidate is not shortlisted (never a model's words): it names the
	// arithmetic that excluded it, so a reject is as reconstructable as a pick.
	Reason string `json:"reason,omitempty"`
}

// Decision is the terminal host tally (design §4): the frozen inputs, every recorded ballot, the ranked
// entries, and the honest rendering of what a ballot IS.
type Decision struct {
	Frozen      FrozenDecision  `json:"frozen"`
	Ballots     []Ballot        `json:"ballots"`
	Entries     []DecisionEntry `json:"entries"`
	Cast        int             `json:"cast"`
	QuorumMet   bool            `json:"quorumMet"`
	TieOutcome  string          `json:"tieOutcome,omitempty"`
	RuleVersion string          `json:"ruleVersion"`
	// Rendering is the ONLY sanctioned human phrasing of the decision. It is a field rather than a method so
	// it is persisted with the decision: a result read later must carry its own honest label, not depend on a
	// reader calling the right function.
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

// Rejected returns the entries that did NOT make the cut, in rank order — carried with their host reason so
// a shortlist never silently loses a candidate the panel nominated (§4's minority carry-through applied to
// the decision).
func (d Decision) Rejected() []DecisionEntry {
	out := make([]DecisionEntry, 0, len(d.Entries))
	for _, e := range d.Entries {
		if !e.Shortlisted {
			out = append(out, e)
		}
	}
	return out
}

// TallyInput is everything the tally needs. Baseline is the immutable blind round-1 baseline — carried not
// because a ballot is counted over it (a ballot is its own evidence, cast in the ballot round) but because a
// CONTESTED mapping's alternative partition is described in blind round-1 contributions, so the sensitivity
// branch is computed over the same evidence base the corroboration claims use.
type TallyInput struct {
	Frozen          FrozenDecision
	Ballots         []Ballot
	Partition       canon.Result
	Contested       []canon.ContestedMapping
	Baseline        BlindBaseline
	Panel           Panel
	FormulationHash string
	// BallotRoundID is the recorded round the ballots were cast in — pinned on every ballot claim as its
	// evidence round.
	BallotRoundID string
}

// Tally computes the ranking from the recorded ballots (design §4). It is the ONLY place a Shortlist ranking
// is produced, and it is pure arithmetic: positional points from the rankings, approvals tallied separately,
// deterministic ordering, the frozen cut applied, and a claim pinned to every shortlisted entry.
//
// It withholds rather than guesses in three cases, all of them §4's rules rather than this function's taste:
// a contested partition under a ranked candidate, a respondents count below the frozen quorum, and a tie at
// the shortlist boundary under the frozen tie rule.
func Tally(in TallyInput) (Decision, error) {
	if strings.TrimSpace(in.Frozen.InputsHash) == "" {
		return Decision{}, fmt.Errorf("tally: the decision inputs were not frozen — a ranking may only be computed under inputs fixed + hashed BEFORE the ballot was solicited (design §4)")
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
			// Positional (Borda) points: first place scores n, and a placed candidate always scores at least 1,
			// so placing a candidate last is still a stronger statement than not placing it at all.
			pts := n - i
			if pts < 1 {
				pts = 1
			}
			t := tallies[id]
			t.score += pts
			t.voters = append(t.voters, b.By)
			t.refs[b.EnvelopeRef] = true
		}
		for _, id := range b.Approved {
			tallies[id].approvals++
		}
	}

	// Deterministic ordering: score, then support, then approvals, then canonical ID. The final key is not a
	// tie-break in any substantive sense — it exists so the same ballots always produce the same bytes.
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

	// Ranks: entries with an identical (score, support, approvals) key share a rank and are marked TIED — the
	// host does not break a tie it was not given a rule for.
	rank := 0
	for i := range entries {
		if i == 0 || !sameStanding(entries[i-1], entries[i]) {
			rank = i + 1
		} else {
			entries[i-1].Tied, entries[i].Tied = true, true
		}
		entries[i].Rank = rank
	}

	// The frozen cut. A tie group STRADDLING it carries every tied candidate into the shortlist rather than
	// picking one: picking would BE the judgment the frozen tie rule declines to make. The straddle is then
	// what withholds the definitive label for those candidates.
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

// sameStanding reports whether two entries are indistinguishable under the frozen tally — the definition of a
// tie here (the canonical-ID ordering is determinism, not standing).
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

// rejectReason states the arithmetic that excluded a candidate — a HOST reason, never a model's words.
func rejectReason(e DecisionEntry, cutRank int) string {
	if e.Support == 0 {
		return "not placed on any ballot (score 0) — carried as a nominated candidate the panel did not rank"
	}
	return fmt.Sprintf("ranked %d (score %d, placed by %d ballot(s), %d approval(s)) — below the frozen shortlist cut at rank %d",
		e.Rank, e.Score, e.Support, e.Approvals, cutRank)
}

// ballotClaim builds the pinned governance claim for ONE shortlisted candidate. The counted value is the
// number of DISTINCT ballots that PLACED the candidate — a count of expressed preferences, which is what a
// ballot actually measures — reported with both denominators like every other claim.
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

// ballotSensitivity computes a ranked candidate's support under BOTH plausible partitions when a contested
// mapping reaches it (§4), so the definitive `ranked` label can be withheld and a range emitted instead.
//
// The two directions are asymmetric and both are honest:
//   - the alternative JOINS the held entities: they would have been ONE option on the ballot, so their
//     supporters POOL — the upper bound;
//   - the alternative SEPARATES this entity: the option that was voted on does not exist in that partition at
//     all, and a preference expressed over one option set cannot be re-attributed to a different one without
//     a new ballot — so the honest lower bound is 0.
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

// minInt returns the smaller of two ints (the shortlist cut can exceed a small universe only through a
// contract bug, but clamping the INDEX is not the same as clamping the frozen policy — the frozen size stays
// exactly what was hashed).
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
