package govern

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

func ex(model string) schema.ExplorerIdentity {
	return schema.ExplorerIdentity{Adapter: "ad-" + model, Model: model, Effort: "high"}
}

// blindRound1 records a blind round 1 with n explorers (orders 0..n-1).
func blindRound1(n int) round.Round {
	var envs []schema.Envelope
	for i := range n {
		envs = append(envs, schema.Envelope{
			Identity: ex(string(rune('a' + i))), Order: i,
			IdentityStatus: schema.IdentityVerified, Response: map[string]any{"candidates": []any{"Postgres"}},
		})
	}
	return round.NewRound(1, true, "payload-hash", envs, nil)
}

// partition builds a canon.Result-shaped partition view for counting: one entity with the given members.
func partition(revision string, clusters ...canon.Cluster) canon.Result {
	return canon.Result{Clusters: clusters, PartitionRevisionHash: revision}
}

func member(canonicalID, raw string, src schema.ExplorerIdentity, ref string) canon.Member {
	return canon.Member{CanonicalID: canonicalID, RawNomination: raw, SourceExplorer: src, EnvelopeRef: ref}
}

func frozen(t *testing.T, n int) Panel {
	t.Helper()
	var members []schema.ExplorerIdentity
	for i := range n {
		members = append(members, ex(string(rune('a'+i))))
	}
	p, err := Freeze(members, DefaultCountingPolicy())
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	return p
}

// TestNewBlindBaseline_RefusesAnythingButBlindRound1 is the ANTI-ECHO invariant as a property of the TYPE
// (§0 F-A): a later round — or a round 1 recorded as non-blind — cannot become a counting baseline, so no call
// site can launder post-mediation echo into an independence count even deliberately.
func TestNewBlindBaseline_RefusesAnythingButBlindRound1(t *testing.T) {
	if _, err := NewBlindBaseline(blindRound1(2)); err != nil {
		t.Fatalf("a blind round 1 must be a valid baseline: %v", err)
	}
	later := round.NewRound(2, false, "ph2", []schema.Envelope{{Identity: ex("a"), Order: 0}}, nil)
	if _, e := NewBlindBaseline(later); e == nil {
		t.Fatal("round 2 must NEVER be usable as an independence-count baseline")
	} else if !strings.Contains(e.Error(), "round 2 is not round 1") {
		t.Errorf("the refusal should name the round, got: %v", e)
	}
	nonBlind := round.NewRound(1, false, "ph", []schema.Envelope{{Identity: ex("a"), Order: 0}}, nil)
	if _, e := NewBlindBaseline(nonBlind); e == nil {
		t.Error("a round 1 recorded as NON-blind must be refused")
	}
	if _, e := NewBlindBaseline(round.NewRound(1, true, "ph", nil, nil)); e == nil {
		t.Error("an empty round 1 must be refused (there is nothing to count)")
	}
	// The baseline knows exactly which refs are in it — that set IS the anti-echo filter.
	b, _ := NewBlindBaseline(blindRound1(2))
	if !b.Contains("envelope#0") || !b.Contains("envelope#1") {
		t.Error("the baseline must contain its round-1 refs")
	}
	if b.Contains("r2:envelope#0") {
		t.Error("a later round's ref must never be in the baseline")
	}
	if b.Size() != 2 || b.RoundID() != "round-1" {
		t.Errorf("baseline metadata wrong: size=%d id=%q", b.Size(), b.RoundID())
	}
}

// TestCorroboration_CountsDistinctBlindSourcesOnly pins the count itself: DISTINCT sources (not nominations),
// and nothing from outside the blind baseline — a later round's nomination for the same entity is ignored, which
// is the anti-echo invariant in its operative form.
func TestCorroboration_CountsDistinctBlindSourcesOnly(t *testing.T) {
	base, err := NewBlindBaseline(blindRound1(3))
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	pan := frozen(t, 3).WithOutcome(Outcome{Dispatched: 3, Eligible: 3})
	pg := canon.Cluster{CanonicalID: "pg", Name: "Postgres", Members: []canon.Member{
		member("pg", "Postgres", ex("a"), "envelope#0"),
		member("pg", "Postgres", ex("b"), "envelope#1"),
		member("pg", "Postgres", ex("a"), "envelope#0"),    // the SAME source again — k counts sources, not rows
		member("pg", "Postgres", ex("c"), "r2:envelope#2"), // a LATER round echo — must not count
	}}
	claim, cerr := Corroboration(CountInput{Baseline: base, Partition: partition("rev-1", pg), CanonicalID: "pg", Panel: pan, FormulationHash: "form-1"})
	if cerr != nil {
		t.Fatalf("corroboration: %v", cerr)
	}
	if claim.Value != 2 {
		t.Errorf("k must be 2 distinct BLIND sources (a repeated source and a later-round echo add nothing), got %d", claim.Value)
	}
	if len(claim.ContributingSourceIDs) != 2 || claim.ContributingSourceIDs[0] != "envelope#0" {
		t.Errorf("contributing source IDs must be the blind refs only: %v", claim.ContributingSourceIDs)
	}
	for _, ref := range claim.ContributingSourceIDs {
		if strings.HasPrefix(ref, "r2:") {
			t.Errorf("a later-round ref must never be a contributing source: %v", claim.ContributingSourceIDs)
		}
	}
	// Every claim is pinned to its inputs (§0 F-C / §9).
	if claim.FormulationHash != "form-1" || claim.PartitionRevisionHash != "rev-1" ||
		claim.RulesVersion != RulesVersion || claim.PolicyHash != pan.PolicyHash ||
		claim.Query != CorroborationQuery || claim.BaselineRoundID != "round-1" {
		t.Errorf("claim is not fully pinned to its inputs: %+v", claim)
	}
	if claim.Label != LabelCorroborated {
		t.Errorf("2 distinct blind sources with quorum met and no contest = corroborated, got %q", claim.Label)
	}
	// An unknown subject is an error, never a zero count.
	if _, err := Corroboration(CountInput{Baseline: base, Partition: partition("rev-1", pg), CanonicalID: "ghost", Panel: pan}); err == nil {
		t.Error("counting an entity absent from the partition must error")
	}
}

// TestDenominators_DualWithAbstention pins §1's dual denominators: the PANEL denominator never moves, the
// RESPONDENTS denominator excludes non-responses, and the distinct tallies stay separately visible.
func TestDenominators_DualWithAbstention(t *testing.T) {
	pan := frozen(t, 4).WithOutcome(Outcome{
		Dispatched: 4, Eligible: 2, TechnicalAbsence: 1, DeliberateAbstention: 1,
	})
	if pan.Selected != 4 || pan.Respondents() != 2 {
		t.Fatalf("panel=4 respondents=2 expected, got panel=%d respondents=%d", pan.Selected, pan.Respondents())
	}
	panelDen, respDen := pan.Denominators(2)
	if panelDen.M != 4 || panelDen.Basis != BasisPanel {
		t.Errorf("panel denominator wrong: %+v", panelDen)
	}
	if respDen.M != 2 || respDen.Basis != BasisRespondents {
		t.Errorf("respondents denominator wrong: %+v", respDen)
	}
	if panelDen.String() != "2 of 4 panel" || respDen.String() != "2 of 2 respondents" {
		t.Errorf("denominator rendering wrong: %q / %q", panelDen, respDen)
	}
	// The categories are NOT collapsed: a technical absence and a deliberate abstention remain distinguishable.
	if pan.Outcome.TechnicalAbsence != 1 || pan.Outcome.DeliberateAbstention != 1 {
		t.Errorf("absence categories must stay distinct: %+v", pan.Outcome)
	}
	// Below the frozen quorum the definitive label is WITHHELD even with a clean partition.
	thin := frozen(t, 3).WithOutcome(Outcome{Dispatched: 3, TechnicalAbsence: 2})
	if thin.QuorumMet() {
		t.Fatal("1 respondent must not meet the frozen quorum of 2")
	}
	base, _ := NewBlindBaseline(blindRound1(3))
	pg := canon.Cluster{CanonicalID: "pg", Name: "Postgres", Members: []canon.Member{member("pg", "Postgres", ex("a"), "envelope#0")}}
	claim, err := Corroboration(CountInput{Baseline: base, Partition: partition("rev", pg), CanonicalID: "pg", Panel: thin})
	if err != nil {
		t.Fatalf("corroboration: %v", err)
	}
	if claim.Label != LabelWithheldBelowQuorum || claim.Label.Definitive() {
		t.Errorf("below quorum must WITHHOLD the definitive label, got %q", claim.Label)
	}
}

// TestFreeze_HashesPolicyBeforeJudgments pins §4's freeze: membership is copied, the policy is hashed
// deterministically, and an invalid policy is refused. The hash is what proves the policy predates the counts.
func TestFreeze_HashesPolicyBeforeJudgments(t *testing.T) {
	members := []schema.ExplorerIdentity{ex("a"), ex("b")}
	p, err := Freeze(members, DefaultCountingPolicy())
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if p.PolicyHash == "" || p.PolicyHash != DefaultCountingPolicy().Hash() {
		t.Errorf("the frozen policy hash must be the policy's deterministic hash: %q", p.PolicyHash)
	}
	members[0] = ex("z") // mutating the caller's slice cannot re-open the freeze
	if p.Members[0] != ex("a") {
		t.Error("Freeze must copy panel membership")
	}
	// The outcome never re-opens the frozen fields.
	after := p.WithOutcome(Outcome{Dispatched: 2, DeliberateAbstention: 1})
	if after.Selected != p.Selected || after.PolicyHash != p.PolicyHash {
		t.Error("WithOutcome must carry the frozen fields through unchanged")
	}
	if _, err := Freeze(nil, DefaultCountingPolicy()); err == nil {
		t.Error("an empty panel must be refused")
	}
	for name, bad := range map[string]CountingPolicy{
		"no version": {Quorum: 2, TieRule: TieWithhold, MissingResponse: MissingExcludeFromRespondents},
		"quorum 1":   {RulesVersion: RulesVersion, Quorum: 1, TieRule: TieWithhold, MissingResponse: MissingExcludeFromRespondents},
		"tie rule":   {RulesVersion: RulesVersion, Quorum: 2, TieRule: "coin_flip", MissingResponse: MissingExcludeFromRespondents},
		"missing":    {RulesVersion: RulesVersion, Quorum: 2, TieRule: TieWithhold, MissingResponse: "ignore"},
	} {
		if _, err := Freeze([]schema.ExplorerIdentity{ex("a")}, bad); err == nil {
			t.Errorf("%s: an invalid policy must be refused", name)
		}
	}
	// A changed policy is a changed hash (so a claim cannot be re-interpreted under a different policy).
	other := DefaultCountingPolicy()
	other.Quorum = 3
	if other.Hash() == DefaultCountingPolicy().Hash() {
		t.Error("a different policy must hash differently")
	}
}

// TestCorroboration_ContestedPartition_RangeAndWithheld pins §4's conditional result: over a contested mapping
// the count is computed for BOTH plausible partitions, emitted as a range, and the definitive label is WITHHELD.
func TestCorroboration_ContestedPartition_RangeAndWithheld(t *testing.T) {
	base, _ := NewBlindBaseline(blindRound1(3))
	pan := frozen(t, 3).WithOutcome(Outcome{Dispatched: 3, Eligible: 3})
	// Held: two SEPARATE single-source entities. Alternative (a refused wrong_split): they are one entity with
	// two sources — so the count is 1 held, 2 under the alternative.
	pgA := canon.Cluster{CanonicalID: "pg-a", Name: "Postgres", SingleSource: true, Members: []canon.Member{member("pg-a", "Postgres", ex("a"), "envelope#0")}}
	pgB := canon.Cluster{CanonicalID: "pg-b", Name: "Postgres (Citus)", SingleSource: true, Members: []canon.Member{member("pg-b", "Postgres+Citus", ex("b"), "envelope#1")}}
	contested := []canon.ContestedMapping{{
		HeldCanonicalIDs: []string{"pg-a", "pg-b"},
		AlternativeEntities: []canon.AlternativeEntity{{
			Label: "pg-a+pg-b",
			Contributions: []canon.Contribution{
				{SourceExplorer: ex("a"), EnvelopeRef: "envelope#0"},
				{SourceExplorer: ex("b"), EnvelopeRef: "envelope#1"},
			},
		}},
		Direction: canon.DirectionAlternativeJoins, Source: canon.SourceWrongSplitChallenge,
		Reason: "an explorer says these are the same product", RuleVersion: canon.HostConfirmationRuleVersion,
	}}
	claim, err := Corroboration(CountInput{
		Baseline: base, Partition: partition("rev-2", pgA, pgB), Contested: contested,
		CanonicalID: "pg-a", Panel: pan, FormulationHash: "form-1",
	})
	if err != nil {
		t.Fatalf("corroboration: %v", err)
	}
	if claim.Sensitivity == nil {
		t.Fatal("a contested mapping must yield a sensitivity range")
	}
	if claim.Sensitivity.Low != 1 || claim.Sensitivity.High != 2 {
		t.Errorf("range must span both partitions (1 held .. 2 joined), got %d..%d", claim.Sensitivity.Low, claim.Sensitivity.High)
	}
	if claim.Label != LabelWithheldContested || claim.Label.Definitive() {
		t.Errorf("a contested mapping must WITHHOLD the definitive label, got %q", claim.Label)
	}
	if len(claim.Sensitivity.ContestedCanonicalIDs) != 2 {
		t.Errorf("the range must name the contested entities: %+v", claim.Sensitivity)
	}
	// The withheld rendering states the range + that the label is withheld, and never claims corroboration.
	r := claim.Rendering()
	if !strings.Contains(r, "CONTESTED PARTITION") || !strings.Contains(r, "WITHHELD") {
		t.Errorf("a withheld claim's rendering must say so: %q", r)
	}
	// An UNCONTESTED entity in the same partition is unaffected — withholding is per-subject, not global.
	other, _ := Corroboration(CountInput{
		Baseline: base, Partition: partition("rev-2", pgA, pgB), Contested: contested,
		CanonicalID: "pg-b", Panel: pan,
	})
	if !other.Label.Definitive() && other.Sensitivity == nil {
		t.Error("a subject named by the contested mapping is conditional; one that is not must be unaffected")
	}
	// A separating alternative lowers the count instead of raising it.
	merged := canon.Cluster{CanonicalID: "pg", Name: "Postgres", Members: []canon.Member{
		member("pg", "Postgres", ex("a"), "envelope#0"), member("pg", "Postgres", ex("b"), "envelope#1"),
	}}
	sep := []canon.ContestedMapping{{
		HeldCanonicalIDs: []string{"pg"},
		AlternativeEntities: []canon.AlternativeEntity{
			{Label: "pg@a", Contributions: []canon.Contribution{{SourceExplorer: ex("a"), EnvelopeRef: "envelope#0"}}},
			{Label: "pg@b", Contributions: []canon.Contribution{{SourceExplorer: ex("b"), EnvelopeRef: "envelope#1"}}},
		},
		Direction: canon.DirectionAlternativeSeparates, Source: canon.SourceUnsplittableMerge,
		Reason: "unsplittable wrong_merge", RuleVersion: canon.HostConfirmationRuleVersion,
	}}
	low, err := Corroboration(CountInput{Baseline: base, Partition: partition("rev-3", merged), Contested: sep, CanonicalID: "pg", Panel: pan})
	if err != nil {
		t.Fatalf("corroboration: %v", err)
	}
	if low.Sensitivity == nil || low.Sensitivity.Low != 1 || low.Sensitivity.High != 2 {
		t.Errorf("a separating alternative must lower the range's floor to 1: %+v", low.Sensitivity)
	}
}

// TestClaim_HonestLabelingAndNarrativeSplit pins §0 F-B/F-C: a claim's rendering never asserts consensus, a
// single-source count is labeled salience, and ALL model prose lives in the collatorNarrative namespace — a
// Claim has no field that can carry model text.
func TestClaim_HonestLabelingAndNarrativeSplit(t *testing.T) {
	base, _ := NewBlindBaseline(blindRound1(3))
	pan := frozen(t, 3).WithOutcome(Outcome{Dispatched: 3, Eligible: 3})
	solo := canon.Cluster{CanonicalID: "duck", Name: "DuckDB", SingleSource: true, Members: []canon.Member{member("duck", "DuckDB", ex("a"), "envelope#0")}}
	multi := canon.Cluster{CanonicalID: "pg", Name: "Postgres", Members: []canon.Member{
		member("pg", "Postgres", ex("a"), "envelope#0"), member("pg", "Postgres", ex("b"), "envelope#1"),
	}}
	part := partition("rev-1", solo, multi)
	ledger := &ClaimLedger{}
	for _, id := range []string{"duck", "pg"} {
		c, err := Corroboration(CountInput{Baseline: base, Partition: part, CanonicalID: id, Panel: pan, FormulationHash: "form"})
		if err != nil {
			t.Fatalf("corroboration %s: %v", id, err)
		}
		ledger.Emit(c)
	}
	claims := ledger.Claims()
	if claims[0].Label != LabelSingleSource {
		t.Errorf("a single-source count is SALIENCE, not support: %q", claims[0].Label)
	}
	for _, c := range claims {
		r := c.Rendering()
		if strings.Contains(strings.ToLower(r), "independent consensus") {
			t.Errorf("no count may ever be rendered as independent consensus: %q", r)
		}
		if !strings.Contains(r, "panel") || !strings.Contains(r, "respondents") {
			t.Errorf("a rendering must state BOTH denominators: %q", r)
		}
		if !strings.Contains(r, "formulationHash") || !strings.Contains(r, "partition") {
			t.Errorf("a rendering must pin the formulation + partition: %q", r)
		}
	}
	// The claim ledger is append-only + hash-chained, and Claims() copies.
	if ledger.Len() != 2 || ledger.RevisionHash() == "" {
		t.Fatalf("claim ledger must chain its emissions: len=%d hash=%q", ledger.Len(), ledger.RevisionHash())
	}
	claims[0].Value = 99
	if ledger.Claims()[0].Value == 99 {
		t.Error("Claims() must return a copy (the claim record is append-only)")
	}
	before := ledger.RevisionHash()
	ledger.Emit(claims[1])
	if ledger.RevisionHash() == before {
		t.Error("emitting a claim must extend the hash chain")
	}

	// Model prose is quarantined: it appears under collatorNarrative and nowhere inside a claim.
	report := NewReport(pan, ledger, []Narrative{{Source: ex("c"), Phase: schema.PhaseCanonicalize, Prose: "MODEL PROSE HERE"}})
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var probe struct {
		Claims            []map[string]any `json:"claims"`
		CollatorNarrative []map[string]any `json:"collatorNarrative"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if len(probe.CollatorNarrative) != 1 || probe.CollatorNarrative[0]["prose"] != "MODEL PROSE HERE" {
		t.Errorf("model prose must live in the collatorNarrative namespace: %+v", probe.CollatorNarrative)
	}
	for _, c := range probe.Claims {
		for k, v := range c {
			if s, ok := v.(string); ok && strings.Contains(s, "MODEL PROSE") {
				t.Errorf("claim field %q carries model prose — machine governance fields are host-produced only", k)
			}
		}
	}
	if !strings.Contains(report.Summary(), RulesVersion) {
		t.Errorf("the report summary must pin the rules version: %q", report.Summary())
	}
}
