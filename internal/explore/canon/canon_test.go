package canon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// fakeCanonicalizer is a deterministic in-process CanonicalizerCall for the canon unit tests — no real
// model. Its propose func maps the nominations onto a Proposal so a test can exercise a specific
// partition (a valid merge, a dropped nomination, an out-of-range member, an empty cluster, …).
type fakeCanonicalizer struct {
	propose func(noms []Nomination) (Proposal, error)
}

func (f fakeCanonicalizer) Propose(_ context.Context, noms []Nomination) (Proposal, error) {
	return f.propose(noms)
}

// clusterByRaw is the reference deterministic canonicalizer: cluster nominations by EXACT raw text
// (synonyms merge; distinct names stay separate) — the honest, surjectivity-preserving partition.
func clusterByRaw(noms []Nomination) (Proposal, error) {
	order := []string{}
	byRaw := map[string][]int{}
	for i, n := range noms {
		if _, seen := byRaw[n.Raw]; !seen {
			order = append(order, n.Raw)
		}
		byRaw[n.Raw] = append(byRaw[n.Raw], i)
	}
	var clusters []ProposedCluster
	for _, raw := range order {
		clusters = append(clusters, ProposedCluster{
			CanonicalID: "canon-" + raw,
			Name:        raw,
			Members:     byRaw[raw],
		})
	}
	return Proposal{
		Clusters:           clusters,
		ProposedDimensions: []string{"maturity"},
		CoverageNotes:      "clustered by exact name",
		DecidedByCall:      "test:canonicalize",
		DecidedByIdentity:  schema.ExplorerIdentity{Adapter: "fake", Model: "canon-model", Effort: "high"},
	}, nil
}

func ex(adapter, model string) schema.ExplorerIdentity {
	return schema.ExplorerIdentity{Adapter: adapter, Model: model, Effort: "high"}
}

// overlappingNoms: A nominates Postgres+SQLite, B nominates Postgres+MySQL → Postgres merges (2 sources),
// SQLite + MySQL are carried singletons (1 source each).
func overlappingNoms() []Nomination {
	return []Nomination{
		{Raw: "Postgres", SourceExplorer: ex("a", "m-a"), EnvelopeRef: "envelope#0"},
		{Raw: "SQLite", SourceExplorer: ex("a", "m-a"), EnvelopeRef: "envelope#0"},
		{Raw: "Postgres", SourceExplorer: ex("b", "m-b"), EnvelopeRef: "envelope#1"},
		{Raw: "MySQL", SourceExplorer: ex("b", "m-b"), EnvelopeRef: "envelope#1"},
	}
}

// TestCanonicalize_MergesDuplicates_CarriesSingleton pins the happy path: the duplicate merges into a
// multi-source cluster, the singletons survive tagged single-source, and one ledger row exists per
// nomination (surjectivity holds).
func TestCanonicalize_MergesDuplicates_CarriesSingleton(t *testing.T) {
	noms := overlappingNoms()
	res, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: clusterByRaw})
	if err != nil {
		t.Fatalf("expected a valid canonicalization, got: %v", err)
	}
	if got := res.Ledger.Len(); got != len(noms) {
		t.Fatalf("ledger must have exactly one row per nomination: got %d, want %d", got, len(noms))
	}
	byName := map[string]Cluster{}
	for _, c := range res.Clusters {
		byName[c.Name] = c
	}
	pg, ok := byName["Postgres"]
	if !ok {
		t.Fatal("Postgres cluster missing")
	}
	if len(pg.Members) != 2 {
		t.Errorf("Postgres must merge both nominations: got %d members", len(pg.Members))
	}
	if pg.SingleSource {
		t.Error("Postgres is nominated by two explorers — must NOT be single-source")
	}
	for _, m := range pg.Members {
		if m.SingleSource {
			t.Error("a member of a multi-source cluster must not be tagged single-source")
		}
	}
	// The carried singletons survive, each tagged single-source (minority carry-through).
	for _, name := range []string{"SQLite", "MySQL"} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("singleton %q was DROPPED (minority carry-through violated)", name)
		}
		if !c.SingleSource || len(c.Members) != 1 || !c.Members[0].SingleSource {
			t.Errorf("singleton %q must be single-source with one member: %+v", name, c)
		}
	}
	if res.PartitionRevisionHash == "" {
		t.Error("a partition revision hash must be recorded")
	}
}

// TestCanonicalize_SurjectivityGate_DroppedNomination is the core gate test: a canonicalizer that DROPS a
// nomination (omits its index from every cluster) must ERROR — never a silent pass.
func TestCanonicalize_SurjectivityGate_DroppedNomination(t *testing.T) {
	noms := overlappingNoms()
	dropLast := func(n []Nomination) (Proposal, error) {
		p, _ := clusterByRaw(n)
		// Remove the MySQL nomination (index 3) from its cluster → it is dropped.
		var kept []ProposedCluster
		for _, c := range p.Clusters {
			var members []int
			for _, idx := range c.Members {
				if idx != 3 {
					members = append(members, idx)
				}
			}
			if len(members) > 0 {
				c.Members = members
				kept = append(kept, c)
			}
		}
		p.Clusters = kept
		return p, nil
	}
	_, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: dropLast})
	if err == nil {
		t.Fatal("a canonicalizer that DROPS a nomination must fail the surjectivity gate (not silently pass)")
	}
	if !strings.Contains(err.Error(), "surjectivity gate") || !strings.Contains(err.Error(), "DROPPED") {
		t.Errorf("error should name the surjectivity gate + the dropped nomination, got: %v", err)
	}
}

// TestCanonicalize_SurjectivityGate_DoubleCounted: a nomination claimed by two clusters also violates
// "exactly one ledger row".
func TestCanonicalize_SurjectivityGate_DoubleCounted(t *testing.T) {
	noms := overlappingNoms()
	dup := func(n []Nomination) (Proposal, error) {
		p, _ := clusterByRaw(n)
		// Add index 0 to a second cluster as well → double-counted.
		p.Clusters = append(p.Clusters, ProposedCluster{CanonicalID: "canon-dup", Name: "dup", Members: []int{0}})
		return p, nil
	}
	_, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: dup})
	if err == nil || !strings.Contains(err.Error(), "surjectivity gate") {
		t.Fatalf("a double-counted nomination must fail the surjectivity gate, got: %v", err)
	}
}

// TestCanonicalize_RejectsOutOfRangeAndEmptyAndDuplicateID guards the other partition-integrity failures.
func TestCanonicalize_RejectsOutOfRangeAndEmptyAndDuplicateID(t *testing.T) {
	noms := overlappingNoms()
	cases := map[string]func([]Nomination) (Proposal, error){
		"out-of-range member": func(n []Nomination) (Proposal, error) {
			return Proposal{Clusters: []ProposedCluster{{CanonicalID: "c", Name: "c", Members: []int{99}}}}, nil
		},
		"empty canonical ID": func(n []Nomination) (Proposal, error) {
			return Proposal{Clusters: []ProposedCluster{{CanonicalID: "  ", Name: "c", Members: []int{0}}}}, nil
		},
		"unreachable (empty) cluster": func(n []Nomination) (Proposal, error) {
			p, _ := clusterByRaw(n)
			p.Clusters = append(p.Clusters, ProposedCluster{CanonicalID: "ghost", Name: "ghost", Members: nil})
			return p, nil
		},
		"duplicate canonical ID": func(n []Nomination) (Proposal, error) {
			return Proposal{Clusters: []ProposedCluster{
				{CanonicalID: "same", Name: "one", Members: []int{0, 1}},
				{CanonicalID: "same", Name: "two", Members: []int{2, 3}},
			}}, nil
		},
	}
	for name, propose := range cases {
		if _, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: propose}); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

// TestCanonicalize_PropagatesCanonicalizerError: a failing canonicalizer call (an identity halt in the
// real pipeline) surfaces as an error — canon never invents a partition.
func TestCanonicalize_PropagatesCanonicalizerError(t *testing.T) {
	noms := overlappingNoms()
	boom := fakeCanonicalizer{propose: func([]Nomination) (Proposal, error) {
		return Proposal{}, context.Canceled
	}}
	if _, err := Canonicalize(context.Background(), noms, boom); err == nil {
		t.Fatal("a failing canonicalizer call must surface an error")
	}
}

// TestLedger_AppendOnly_RevisionHashChanges pins the append-only + revision-hashed properties: the ledger
// has one row per nomination, Rows returns a COPY (mutating it cannot touch the ledger), and a different
// partition yields a different revision hash (the hash chain pins the exact partition).
func TestLedger_AppendOnly_RevisionHashChanges(t *testing.T) {
	noms := overlappingNoms()
	res, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: clusterByRaw})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	// Rows() is a defensive copy: mutating the returned slice must not change the ledger.
	rows := res.Ledger.Rows()
	if len(rows) != len(noms) {
		t.Fatalf("expected %d rows, got %d", len(noms), len(rows))
	}
	rows[0].CanonicalID = "TAMPERED"
	if res.Ledger.Rows()[0].CanonicalID == "TAMPERED" {
		t.Error("Rows() must return a copy — the append-only ledger must be immutable from outside")
	}
	// Every row carries the deciding call + identity (attributable partition).
	for _, r := range res.Ledger.Rows() {
		if r.DecidedByCall == "" || r.DecidedByIdentity.Model == "" {
			t.Errorf("ledger row missing deciding call/identity: %+v", r)
		}
	}
	// A DIFFERENT partition (all in one cluster) must produce a different revision hash.
	allOne := func(n []Nomination) (Proposal, error) {
		members := make([]int, len(n))
		for i := range n {
			members[i] = i
		}
		return Proposal{Clusters: []ProposedCluster{{CanonicalID: "all", Name: "all", Members: members}}, DecidedByIdentity: ex("fake", "m")}, nil
	}
	res2, err := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: allOne})
	if err != nil {
		t.Fatalf("canonicalize (all-one): %v", err)
	}
	if res.PartitionRevisionHash == res2.PartitionRevisionHash {
		t.Error("a different partition must yield a different partitionRevisionHash (the ledger is revision-hashed)")
	}
	// The hash is stable for the SAME partition (determinism).
	res3, _ := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: clusterByRaw})
	if res.PartitionRevisionHash != res3.PartitionRevisionHash {
		t.Error("the same partition must yield the same partitionRevisionHash (determinism)")
	}
}

// TestLedger_MarshalJSON_RoundTrips pins that a Result serializes fully despite the append-only rows being
// unexported (the --json / capture surface).
func TestLedger_MarshalJSON_RoundTrips(t *testing.T) {
	noms := overlappingNoms()
	res, _ := Canonicalize(context.Background(), noms, fakeCanonicalizer{propose: clusterByRaw})
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(b), "\"revisionHash\"") || !strings.Contains(string(b), "\"rows\"") {
		t.Errorf("serialized ledger must carry rows + revisionHash: %s", b)
	}
}
