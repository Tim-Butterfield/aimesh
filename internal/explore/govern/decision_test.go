package govern

// Unit tests for the HOST-TALLIED DECISION (design §4). The end-to-end behavior is pinned in the pipeline's
// adjudicative tests; these pin the rules that are easiest to get quietly wrong in isolation — the criterion
// authorization event, the freeze's completeness, and the tally's refusal to run under an unfrozen framing.

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// userCriterion is a well-formed user criterion aggregated by ballot.
func userCriterion(name string) Criterion {
	return Criterion{Name: name, Origin: OriginUser, AggregationMethod: AggregationBallot}
}

// testPresentation is a 3-option presented order.
func testPresentation() canon.Presentation {
	return canon.Presentation{Order: []string{"c0", "c1", "c2"}, Seed: "seed", RuleVersion: "host-confirmation-rule@v1"}
}

// testPartition is a partition whose clusters match testPresentation.
func testPartition() canon.Result {
	return canon.Result{
		PartitionRevisionHash: "rev-1",
		Clusters: []canon.Cluster{
			{CanonicalID: "c0", Name: "Postgres"}, {CanonicalID: "c1", Name: "SQLite"}, {CanonicalID: "c2", Name: "MySQL"},
		},
	}
}

// testPanel is a frozen 3-member panel with everyone responding.
func testPanel(t *testing.T) Panel {
	t.Helper()
	p, err := Freeze([]schema.ExplorerIdentity{
		{Adapter: "a", Model: "m1"}, {Adapter: "b", Model: "m2"}, {Adapter: "c", Model: "m3"},
	}, DefaultCountingPolicy())
	if err != nil {
		t.Fatalf("freeze panel: %v", err)
	}
	return p.WithOutcome(Outcome{Dispatched: 3, Eligible: 3})
}

// TestCriterion_CollatorProposedRequiresAnAuthorizationEvent pins §4's rule: a criterion the COLLATOR proposed
// is only usable with an EXPLICIT PERSISTED authorization event (actor, timestamp, criterion version, scope).
// A missing one is an ERROR rather than a quiet downgrade to `explorer_proposed` — downgrading would launder
// exactly the provenance the field exists to record.
func TestCriterion_CollatorProposedRequiresAnAuthorizationEvent(t *testing.T) {
	bare := Criterion{Name: "operational cost", Origin: OriginCollatorProposedAuthorized, AggregationMethod: AggregationBallot}
	err := bare.Validate()
	if err == nil {
		t.Fatal("a collator-proposed criterion with no authorization event must be rejected")
	}
	if !strings.Contains(err.Error(), "authorization event") {
		t.Errorf("the error must name the missing authorization event: %v", err)
	}
	// An INCOMPLETE authorization is equally refused — a partial record looks like a commitment and is not one.
	partial := bare
	partial.Authorization = &Authorization{Actor: "owner"}
	if err := partial.Validate(); err == nil {
		t.Error("an incomplete authorization event must be rejected")
	}
	// A complete one is accepted.
	full := bare
	full.Authorization = &Authorization{Actor: "owner", Timestamp: "2026-07-24T00:00:00Z", CriterionVersion: "v1", Scope: "this exploration"}
	if err := full.Validate(); err != nil {
		t.Errorf("a fully-authorized criterion must be accepted: %v", err)
	}
	// An authorization event on a criterion that did NOT come from the collator is a category error.
	stray := userCriterion("latency")
	stray.Authorization = full.Authorization
	if err := stray.Validate(); err == nil {
		t.Error("an authorization event is only meaningful for a collator-proposed criterion")
	}
	// The closed vocabularies are closed.
	for _, bad := range []Criterion{
		{Name: "x", Origin: "invented", AggregationMethod: AggregationBallot},
		{Name: "x", Origin: OriginUser, AggregationMethod: "invented"},
		{Name: "", Origin: OriginUser, AggregationMethod: AggregationBallot},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("an ill-formed criterion must be rejected: %+v", bad)
		}
	}
}

// TestFreezeDecision_SealsEveryInputAndReadsThePanelPolicy pins the freeze: it fixes everything §4 lists,
// it takes the quorum / tie rule / missing-response policy from the panel govern.Freeze already froze (so
// they predate the ballot by an entire fan-out), and its hash covers all of it.
func TestFreezeDecision_SealsEveryInputAndReadsThePanelPolicy(t *testing.T) {
	pan := testPanel(t)
	frozen, err := FreezeDecision(pan, testPartition(), testPresentation(),
		[]Criterion{userCriterion("latency")}, MethodPositionalBallot, 2)
	if err != nil {
		t.Fatalf("freeze decision: %v", err)
	}
	in := frozen.Inputs
	if in.Quorum != pan.Policy.Quorum || in.TieRule != pan.Policy.TieRule || in.MissingResponse != pan.Policy.MissingResponse {
		t.Errorf("the decision must READ the already-frozen counting policy, not re-author it: %+v", in)
	}
	if in.PolicyHash != pan.PolicyHash {
		t.Errorf("the decision must pin the frozen counting-policy hash: %q vs %q", in.PolicyHash, pan.PolicyHash)
	}
	if in.UniverseRevisionHash != "rev-1" || len(in.Presentation.Order) != 3 || in.ShortlistSize != 2 {
		t.Errorf("the decision must fix the universe, the presented order and the cut: %+v", in)
	}
	if frozen.InputsHash == "" || frozen.InputsHash != in.Hash() {
		t.Error("the frozen inputs must be hashed")
	}
	// The hash COVERS every input: changing any one of them changes it.
	for name, mutate := range map[string]func(*DecisionInputs){
		"criteria":      func(d *DecisionInputs) { d.Criteria = append(d.Criteria, userCriterion("cost")) },
		"shortlistSize": func(d *DecisionInputs) { d.ShortlistSize = 1 },
		"universe":      func(d *DecisionInputs) { d.UniverseRevisionHash = "rev-2" },
		"order":         func(d *DecisionInputs) { d.Presentation.Order = []string{"c2", "c1", "c0"} },
		"quorum":        func(d *DecisionInputs) { d.Quorum = 3 },
	} {
		mutated := in
		mutated.Criteria = append([]Criterion(nil), in.Criteria...)
		mutate(&mutated)
		if mutated.Hash() == frozen.InputsHash {
			t.Errorf("the inputs hash must cover %s", name)
		}
	}
	// An incomplete freeze is refused: no criteria, an empty universe, an out-of-range cut.
	for name, args := range map[string]struct {
		criteria []Criterion
		size     int
		pres     canon.Presentation
	}{
		"no criteria":      {nil, 2, testPresentation()},
		"empty universe":   {[]Criterion{userCriterion("x")}, 1, canon.Presentation{}},
		"cut out of range": {[]Criterion{userCriterion("x")}, 9, testPresentation()},
	} {
		if _, err := FreezeDecision(pan, testPartition(), args.pres, args.criteria, MethodPositionalBallot, args.size); err == nil {
			t.Errorf("an incomplete freeze (%s) must be refused", name)
		}
	}
}

// TestTally_RefusesAnUnfrozenOrAlteredFraming is the structural half of "the freeze precedes the judgment":
// the tally will not run at all under inputs that were never frozen, or whose bytes no longer match the hash
// the panel voted under.
func TestTally_RefusesAnUnfrozenOrAlteredFraming(t *testing.T) {
	pan := testPanel(t)
	frozen, err := FreezeDecision(pan, testPartition(), testPresentation(), []Criterion{userCriterion("latency")}, MethodPositionalBallot, 2)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, err := Tally(TallyInput{Panel: pan}); err == nil || !strings.Contains(err.Error(), "not frozen") {
		t.Errorf("an unfrozen decision must be refused: %v", err)
	}
	altered := frozen
	altered.Inputs.ShortlistSize = 3
	if _, err := Tally(TallyInput{Frozen: altered, Panel: pan}); err == nil || !strings.Contains(err.Error(), "do not match their hash") {
		t.Errorf("a framing altered after the freeze must be refused: %v", err)
	}
	// A ballot naming a candidate outside the frozen universe fails the tally rather than being repaired.
	stray := []Ballot{{By: schema.ExplorerIdentity{Adapter: "a", Model: "m1"}, Ranking: []string{"c0", "not-a-candidate"}}}
	if _, err := Tally(TallyInput{Frozen: frozen, Ballots: stray, Partition: testPartition(), Panel: pan}); err == nil {
		t.Error("a ballot naming a candidate outside the confirmed universe must fail the tally")
	}
}

// TestTally_PositionalScoreRanksAndLabelsHonestly pins the arithmetic and the labels: positional points from
// the rankings, approvals tallied separately, both denominators on every claim, and the honest rendering that
// a ballot is not consensus and not emergent salience.
func TestTally_PositionalScoreRanksAndLabelsHonestly(t *testing.T) {
	pan := testPanel(t)
	frozen, err := FreezeDecision(pan, testPartition(), testPresentation(), []Criterion{userCriterion("latency")}, MethodPositionalBallot, 2)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	a := schema.ExplorerIdentity{Adapter: "a", Model: "m1"}
	b := schema.ExplorerIdentity{Adapter: "b", Model: "m2"}
	c := schema.ExplorerIdentity{Adapter: "c", Model: "m3"}
	dec, err := Tally(TallyInput{
		Frozen: frozen, Partition: testPartition(), Panel: pan, BallotRoundID: "round-2", FormulationHash: "form",
		Ballots: []Ballot{
			// c0 first twice, c1 first once → c0 = 3+3+2 = 8, c1 = 2+2+3 = 7, c2 = 1+1+1 = 3.
			{By: a, EnvelopeRef: "r2:envelope#0", Ranking: []string{"c0", "c1", "c2"}, Approved: []string{"c0"}},
			{By: b, EnvelopeRef: "r2:envelope#1", Ranking: []string{"c0", "c1", "c2"}, Approved: []string{"c0", "c1"}},
			{By: c, EnvelopeRef: "r2:envelope#2", Ranking: []string{"c1", "c0", "c2"}, Approved: []string{"c1"}},
		},
	})
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if len(dec.Entries) != 3 || dec.Cast != 3 || !dec.QuorumMet {
		t.Fatalf("tally shape wrong: %d entries, %d cast, quorum=%v", len(dec.Entries), dec.Cast, dec.QuorumMet)
	}
	if dec.Entries[0].CanonicalID != "c0" || dec.Entries[0].Score != 8 || dec.Entries[0].Rank != 1 {
		t.Errorf("positional tally wrong for the leader: %+v", dec.Entries[0])
	}
	if dec.Entries[1].CanonicalID != "c1" || dec.Entries[1].Score != 7 {
		t.Errorf("positional tally wrong for the runner-up: %+v", dec.Entries[1])
	}
	// Approvals are a SEPARATE question and are never folded into the score.
	if dec.Entries[0].Approvals != 2 || dec.Entries[1].Approvals != 2 {
		t.Errorf("approvals must be tallied separately: %+v / %+v", dec.Entries[0], dec.Entries[1])
	}
	// The frozen cut of 2, and the reject carried with a HOST reason.
	shortlisted, rejected := dec.Shortlisted(), dec.Rejected()
	if len(shortlisted) != 2 || len(rejected) != 1 {
		t.Fatalf("the frozen cut takes 2 of 3: %d ranked / %d rejected", len(shortlisted), len(rejected))
	}
	if rejected[0].CanonicalID != "c2" || rejected[0].Reason == "" || rejected[0].Claim != nil {
		t.Errorf("a reject carries a host reason and NO ranked claim: %+v", rejected[0])
	}
	for _, e := range shortlisted {
		if e.Claim == nil || e.Claim.Label != LabelRanked {
			t.Fatalf("a settled quorate entry is `ranked`: %+v", e.Claim)
		}
		if e.Claim.Query != BallotQuery || e.Claim.BaselineRoundID != "round-2" {
			t.Errorf("a ballot claim names the ballot query + the ballot round: %+v", e.Claim)
		}
		if e.Claim.KOfPanel.M != 3 || e.Claim.KOfRespondents.M != 3 || e.Claim.Value != 3 {
			t.Errorf("a ballot claim carries BOTH denominators over the frozen panel: %+v", e.Claim)
		}
		if len(e.Claim.ContributingSourceIDs) != 3 {
			t.Errorf("a ballot claim names the exact ballots behind it: %+v", e.Claim.ContributingSourceIDs)
		}
		r := e.Claim.Rendering()
		if !strings.Contains(r, "placed on their ballot") || !strings.Contains(r, "not consensus") {
			t.Errorf("a ballot rendering must say what it is and what it is not: %q", r)
		}
		if strings.Contains(strings.ToLower(r), "independent consensus") {
			t.Errorf("no claim may ever be rendered as independent consensus: %q", r)
		}
	}
	if !strings.Contains(dec.Rendering, "INFORMED PREFERENCE UNDER SHARED FRAMING") ||
		!strings.Contains(dec.Rendering, "NOT `emergent`") {
		t.Errorf("the decision rendering must state what a ballot is and distinguish it from emergent salience: %q", dec.Rendering)
	}
}

// TestTally_BelowQuorumWithholdsTheRankedLabel: the FROZEN quorum is a precondition for a definitive label, so
// a panel that produced too few ballots still gets a real tally — and no `ranked` verdict.
func TestTally_BelowQuorumWithholdsTheRankedLabel(t *testing.T) {
	pan := testPanel(t)
	frozen, err := FreezeDecision(pan, testPartition(), testPresentation(), []Criterion{userCriterion("latency")}, MethodPositionalBallot, 2)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	dec, err := Tally(TallyInput{
		Frozen: frozen, Partition: testPartition(), Panel: pan, BallotRoundID: "round-2",
		Ballots: []Ballot{{By: schema.ExplorerIdentity{Adapter: "a", Model: "m1"}, EnvelopeRef: "r2:envelope#0", Ranking: []string{"c0", "c1"}}},
	})
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if dec.QuorumMet {
		t.Error("one ballot cannot meet a quorum of 2")
	}
	for _, e := range dec.Shortlisted() {
		if e.Claim.Label != LabelWithheldBelowQuorum {
			t.Errorf("below the frozen quorum the definitive label is WITHHELD, got %q", e.Claim.Label)
		}
		if e.Claim.Label.Definitive() {
			t.Errorf("a withheld label is not definitive: %q", e.Claim.Label)
		}
	}
}

// TestFrozenDecision_RenderMustNotOpenWithADash pins a property that is invisible inside this package and
// expensive in the field: Render() is prepended to the ballot prompt, so its first byte is the first byte
// of the prompt — and the argv-passing shell recipes (ollama, devin-cli, agy-cli, cursor-cli) hand that
// prompt to their CLI as a POSITIONAL ARGUMENT. A leading `-` is then parsed as an option.
//
// Measured 2026-08-30: with a `-----` header, a live shortlist run lost 3 of 5 seats — devin-cli exited 2
// printing its usage, cursor-cli exited 1 echoing prompt text — while the three PromptOnStdin recipes
// (codex-cli, claude-code, gemini-cli) were unaffected. One ballot was cast, quorum failed, and every
// ranked row came back withheld: a full panel spent for no usable ranking.
//
// This is a governance property rather than cosmetics. A header that silently disenfranchises every
// argv adapter biases the tally toward whichever providers happen to read their prompt from stdin.
func TestFrozenDecision_RenderMustNotOpenWithADash(t *testing.T) {
	pan := testPanel(t)
	frozen, err := FreezeDecision(pan, testPartition(), testPresentation(),
		[]Criterion{userCriterion("latency")}, MethodPositionalBallot, 2)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	rendered := frozen.Render()
	if rendered == "" {
		t.Fatal("Render produced nothing")
	}
	if strings.HasPrefix(rendered, "-") {
		t.Errorf("the ballot header must not begin with '-': an argv-passing adapter parses it as an "+
			"option and casts no ballot. Got: %.80q", rendered)
	}
	// It must still be identifiable: the pipeline's adjudicative tests and the fake model both match on
	// this substring, and a voter has to see the framing it is voting under.
	if !strings.Contains(rendered, "FROZEN DECISION INPUTS") {
		t.Error("the rendered header must still name the frozen decision inputs")
	}
}
