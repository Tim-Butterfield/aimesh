package canon

import (
	"context"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// stamp returns a proposal-builder that decorates the clusters produced by build with a distinct canonicalizer
// attribution, so a test can tell WHICH canonicalizer proposed what from the ledger alone.
func stamp(name string, build func([]Nomination) (Proposal, error)) func([]Nomination) (Proposal, error) {
	return func(noms []Nomination) (Proposal, error) {
		p, err := build(noms)
		if err != nil {
			return p, err
		}
		p.DecidedByCall = name + ":canonicalize"
		p.DecidedByIdentity = ex(name, "model-"+name)
		return p, nil
	}
}

// mergeAll is the AGGRESSIVE canonicalizer: one entity containing every nomination. It is the shape of the
// attack the dual rule exists to stop — an aggressive merger inflates corroboration if its proposal is
// authoritative.
func mergeAll(noms []Nomination) (Proposal, error) {
	members := make([]int, len(noms))
	for i := range noms {
		members[i] = i
	}
	return Proposal{Clusters: []ProposedCluster{{CanonicalID: "all", Name: "everything", Members: members}}}, nil
}

// TestCanonicalizeDual_BothAgree_Merges: when both canonicalizers propose the same partition, the agreed
// partition IS that partition — the duplicate merges, the singletons carry, and every row records BOTH
// canonicalizers in agreedBy.
func TestCanonicalizeDual_BothAgree_Merges(t *testing.T) {
	noms := overlappingNoms()
	res, err := CanonicalizeDual(context.Background(), noms,
		fakeCanonicalizer{propose: stamp("a", clusterByRaw)}, fakeCanonicalizer{propose: stamp("b", clusterByRaw)})
	if err != nil {
		t.Fatalf("dual canonicalization: %v", err)
	}
	byName := map[string]Cluster{}
	for _, c := range res.Clusters {
		byName[c.Name] = c
	}
	pg, ok := byName["Postgres"]
	if !ok || len(pg.Members) != 2 || pg.SingleSource {
		t.Fatalf("an AGREED merge must hold (2 members, multi-source): %+v", pg)
	}
	for _, name := range []string{"SQLite", "MySQL"} {
		c, ok := byName[name]
		if !ok || !c.SingleSource {
			t.Errorf("singleton %q must carry through tagged single-source: %+v", name, c)
		}
	}
	// Surjectivity still holds: one ledger row per nomination.
	if got := res.Ledger.Len(); got != len(noms) {
		t.Errorf("expected %d ledger rows (surjectivity), got %d", len(noms), got)
	}
	// agreedBy names BOTH proposing calls; the DECIDING authority is the versioned HOST rule, not a model.
	for _, row := range res.Ledger.Rows() {
		if len(row.AgreedBy) != 2 {
			t.Fatalf("every dual row must record both proposing canonicalizers: %+v", row)
		}
		if row.AgreedBy[0].Call != "a:canonicalize" || row.AgreedBy[1].Call != "b:canonicalize" {
			t.Errorf("agreedBy must name the two canonicalizer calls: %+v", row.AgreedBy)
		}
		if row.DecidedByCall != "host:"+DualRuleVersion {
			t.Errorf("the deciding call must be the versioned HOST rule, got %q", row.DecidedByCall)
		}
		if row.DecidedByIdentity != (schema.ExplorerIdentity{}) {
			t.Errorf("no MODEL decided a dual mapping — DecidedByIdentity must stay zero, got %+v", row.DecidedByIdentity)
		}
	}
	if res.AgreementRuleVersion != DualRuleVersion || len(res.Canonicalizers) != 2 {
		t.Errorf("result must record the agreement rule + both canonicalizers: %+v", res)
	}
	if len(res.Ledger.Contested()) != 0 {
		t.Errorf("full agreement must record NO contested merge: %+v", res.Ledger.Contested())
	}
}

// TestCanonicalizeDual_OneProposes_ContestedSplits is the headline invariant (§0 F-B): a merge only ONE
// canonicalizer proposes is CONTESTED and resolved by SPLITTING — the direction that can undercount
// corroboration but can never manufacture it — and the refused merge is recorded with its proposer.
func TestCanonicalizeDual_OneProposes_ContestedSplits(t *testing.T) {
	noms := overlappingNoms() // Postgres(a), SQLite(a), Postgres(b), MySQL(b)
	res, err := CanonicalizeDual(context.Background(), noms,
		fakeCanonicalizer{propose: stamp("a", clusterByRaw)}, // Postgres merges; SQLite/MySQL separate
		fakeCanonicalizer{propose: stamp("b", mergeAll)})     // everything is one thing
	if err != nil {
		t.Fatalf("dual canonicalization: %v", err)
	}
	// The AGREEMENT is A's partition (A ∩ B == A): B's extra merges are refused, so nothing B alone wanted
	// survives. Crucially the agreed Postgres merge — which BOTH proposed — is kept.
	byName := map[string]Cluster{}
	for _, c := range res.Clusters {
		byName[c.Name] = c
	}
	if pg := byName["Postgres"]; len(pg.Members) != 2 {
		t.Errorf("the merge BOTH proposed must survive: %+v", pg)
	}
	if len(res.Clusters) != 3 {
		t.Fatalf("B's unilateral merge must be SPLIT (3 entities expected), got %d: %+v", len(res.Clusters), res.Clusters)
	}
	// SQLite and MySQL are single-source singletons — corroboration was NOT manufactured for them.
	for _, name := range []string{"SQLite", "MySQL"} {
		if c := byName[name]; !c.SingleSource || len(c.Members) != 1 {
			t.Errorf("%q must stay a single-source singleton (splitting can undercount, never manufacture): %+v", name, c)
		}
	}
	// The refused merge is a recorded DECISION with its proposer + resolution + rule version.
	contested := res.Ledger.Contested()
	if len(contested) != 1 {
		t.Fatalf("expected exactly 1 contested merge (B's), got %d: %+v", len(contested), contested)
	}
	cm := contested[0]
	if cm.ProposedBy.Call != "b:canonicalize" || cm.Resolution != ResolutionSplit || cm.RuleVersion != DualRuleVersion {
		t.Errorf("contested record must name the proposer, the split resolution + the rule version: %+v", cm)
	}
	if len(cm.SplitInto) != 3 || len(cm.RawNominations) != 4 {
		t.Errorf("contested record must name every raw nomination + every held entity: %+v", cm)
	}
	// Surjectivity + minority carry-through hold over the AGREED partition.
	if got := res.Ledger.Len(); got != len(noms) {
		t.Errorf("expected %d ledger rows (surjectivity), got %d", len(noms), got)
	}
}

// TestCanonicalizeDual_PartialDisagreement_SplitsOnlyTheContestedMerge: an agreement narrower than either
// proposal splits ONLY the contested group, and the split entities' composite labels name both canonicalizers'
// entities (so the label itself records that the group is agreement-narrowed).
func TestCanonicalizeDual_PartialDisagreement_SplitsOnlyTheContestedMerge(t *testing.T) {
	noms := []Nomination{
		{Raw: "Postgres", SourceExplorer: ex("a", "m-a"), EnvelopeRef: "envelope#0"},
		{Raw: "Postgres+Citus", SourceExplorer: ex("b", "m-b"), EnvelopeRef: "envelope#1"},
		{Raw: "Redis", SourceExplorer: ex("a", "m-a"), EnvelopeRef: "envelope#0"},
		{Raw: "Redis", SourceExplorer: ex("b", "m-b"), EnvelopeRef: "envelope#1"},
	}
	// A merges the two Postgres variants (indices 0,1) and the two Redis (2,3). B keeps the Postgres variants
	// APART and agrees on Redis.
	aProp := func([]Nomination) (Proposal, error) {
		return Proposal{Clusters: []ProposedCluster{
			{CanonicalID: "pg", Name: "Postgres family", Members: []int{0, 1}},
			{CanonicalID: "redis", Name: "Redis", Members: []int{2, 3}},
		}}, nil
	}
	bProp := func([]Nomination) (Proposal, error) {
		return Proposal{Clusters: []ProposedCluster{
			{CanonicalID: "pg-plain", Name: "Postgres", Members: []int{0}},
			{CanonicalID: "pg-citus", Name: "Citus", Members: []int{1}},
			{CanonicalID: "redis", Name: "Redis", Members: []int{2, 3}},
		}}, nil
	}
	res, err := CanonicalizeDual(context.Background(), noms,
		fakeCanonicalizer{propose: stamp("a", aProp)}, fakeCanonicalizer{propose: stamp("b", bProp)})
	if err != nil {
		t.Fatalf("dual canonicalization: %v", err)
	}
	if len(res.Clusters) != 3 {
		t.Fatalf("expected 3 agreed entities (pg split, redis merged), got %d: %+v", len(res.Clusters), res.Clusters)
	}
	var redis *Cluster
	split := 0
	for i := range res.Clusters {
		c := res.Clusters[i]
		switch {
		case c.CanonicalID == "redis":
			redis = &res.Clusters[i]
		case strings.Contains(c.CanonicalID, "~"):
			split++
			if !strings.Contains(c.Name, "/") {
				t.Errorf("a split entity's label must name BOTH canonicalizers' entities: %q", c.Name)
			}
		}
	}
	if redis == nil || len(redis.Members) != 2 || redis.SingleSource {
		t.Errorf("the AGREED Redis merge must survive untouched: %+v", redis)
	}
	if split != 2 {
		t.Errorf("expected the contested Postgres merge to split into 2 agreement-narrowed entities, got %d", split)
	}
	// Only ONE contested record: A's refused merge. B proposed nothing the agreement refused.
	if c := res.Ledger.Contested(); len(c) != 1 || c[0].ProposedBy.Call != "a:canonicalize" {
		t.Errorf("expected exactly A's merge to be recorded as contested: %+v", c)
	}
}

// TestCanonicalizeDual_RejectsInvalidProposal: each proposal is validated for partition integrity BEFORE the
// intersection, and the error names WHICH canonicalizer failed — intersecting around a dropped nomination would
// hide that canonicalizer's failure behind the agreement rule.
func TestCanonicalizeDual_RejectsInvalidProposal(t *testing.T) {
	noms := overlappingNoms()
	dropLast := func(n []Nomination) (Proposal, error) {
		p, _ := clusterByRaw(n)
		p.Clusters = p.Clusters[:len(p.Clusters)-1] // drop the last cluster → its nomination is dropped
		return p, nil
	}
	_, err := CanonicalizeDual(context.Background(), noms,
		fakeCanonicalizer{propose: stamp("a", clusterByRaw)}, fakeCanonicalizer{propose: stamp("b", dropLast)})
	if err == nil {
		t.Fatal("a canonicalizer that drops a nomination must fail the surjectivity gate")
	}
	if !strings.Contains(err.Error(), "canonicalizer B") || !strings.Contains(err.Error(), "DROPPED") {
		t.Errorf("the error must name the failing canonicalizer + the dropped nomination, got: %v", err)
	}
	// A failing CALL (the shape an identity halt takes, raised inside Propose by the pipeline) propagates and
	// names which canonicalizer failed.
	boom := fakeCanonicalizer{propose: func([]Nomination) (Proposal, error) { return Proposal{}, context.Canceled }}
	_, err = CanonicalizeDual(context.Background(), noms, fakeCanonicalizer{propose: stamp("a", clusterByRaw)}, boom)
	if err == nil || !strings.Contains(err.Error(), "canonicalizer B") {
		t.Errorf("a failing canonicalizer call must surface, naming which one: %v", err)
	}
}

// TestCanonicalizeDual_MergesDimensionsAndAttributesNotes: the PROPOSED dimensions are unioned (they are axes
// for the human, never criteria) and the coverage notes stay ATTRIBUTED per canonicalizer rather than blended.
func TestCanonicalizeDual_MergesDimensionsAndAttributesNotes(t *testing.T) {
	noms := overlappingNoms()
	withNotes := func(dims []string, notes string) func([]Nomination) (Proposal, error) {
		return func(n []Nomination) (Proposal, error) {
			p, _ := clusterByRaw(n)
			p.ProposedDimensions, p.CoverageNotes = dims, notes
			return p, nil
		}
	}
	res, err := CanonicalizeDual(context.Background(), noms,
		fakeCanonicalizer{propose: stamp("a", withNotes([]string{"maturity", "cost"}, "A's view"))},
		fakeCanonicalizer{propose: stamp("b", withNotes([]string{"cost", "ops"}, "B's view"))})
	if err != nil {
		t.Fatalf("dual canonicalization: %v", err)
	}
	if got := res.ProposedDimensions; len(got) != 3 || got[0] != "maturity" || got[2] != "ops" {
		t.Errorf("proposed dimensions must be a de-duplicated union in A-then-B order: %v", got)
	}
	if !strings.Contains(res.CoverageNotes, "canonicalizer A: A's view") || !strings.Contains(res.CoverageNotes, "canonicalizer B: B's view") {
		t.Errorf("coverage notes must stay attributed per canonicalizer: %q", res.CoverageNotes)
	}
}
