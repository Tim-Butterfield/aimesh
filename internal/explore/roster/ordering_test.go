package roster

// This file pins the TWO-ORDER contract of a Plan and the canonicalizer spec rule.
//
// A Plan carries the same selected explorer set twice: once in ATTRIBUTION order (sorted by identity
// triple, for reproducible envelope IDs) and once in PREFERENCE order (as authored, for selections). The
// invariant that makes that safe is that they are permutations of one another — the same SET, two ORDERS.
// If they could silently diverge, "the panel" would mean two different things depending on which field a
// reader happened to reach for, which is the class of confusion the named types exist to prevent.

import (
	"fmt"
	"strings"
	"testing"
)

// orderingRosters covers a set already in sorted order, one in reverse, one interleaved, and one where
// only the effort distinguishes two otherwise-identical entries (so the sort key's third field matters).
func orderingRosters() map[string]Roster {
	collator := Collator{Adapter: "claude-code", Model: "opus", Effort: "high"}
	return map[string]Roster{
		"already sorted": {Explorers: []Explorer{
			{Adapter: "a-adapter", Model: "m1"}, {Adapter: "b-adapter", Model: "m2"}, {Adapter: "c-adapter", Model: "m3"},
		}, Collator: collator},
		"reverse of sorted": {Explorers: []Explorer{
			{Adapter: "z-adapter", Model: "m1"}, {Adapter: "m-adapter", Model: "m2"}, {Adapter: "a-adapter", Model: "m3"},
		}, Collator: collator},
		"interleaved": {Explorers: []Explorer{
			{Adapter: "m-adapter", Model: "zeta"}, {Adapter: "a-adapter", Model: "omega"},
			{Adapter: "m-adapter", Model: "alpha"}, {Adapter: "z-adapter", Model: "beta"},
		}, Collator: collator},
		"effort is the only difference": {Explorers: []Explorer{
			{Adapter: "one", Model: "m", Effort: "medium"}, {Adapter: "one", Model: "m", Effort: "high"},
			{Adapter: "one", Model: "m", Effort: "low"},
		}, Collator: collator},
	}
}

// TestPlanOrdersArePermutations is the invariant: for every roster and every legal count, the attribution
// and preference orders hold exactly the same explorers.
func TestPlanOrdersArePermutations(t *testing.T) {
	for name, r := range orderingRosters() {
		for n := 2; n <= len(r.Explorers); n++ {
			t.Run(fmt.Sprintf("%s/n=%d", name, n), func(t *testing.T) {
				plan, err := r.SelectTopN(n)
				if err != nil {
					t.Fatalf("SelectTopN(%d): %v", n, err)
				}
				if len(plan.Explorers) != n || len(plan.Preferred) != n {
					t.Fatalf("lengths = %d/%d, want %d/%d", len(plan.Explorers), len(plan.Preferred), n, n)
				}
				if !plan.SameSet() {
					t.Fatalf("attribution order %+v and preference order %+v are not permutations of one another", plan.Explorers, plan.Preferred)
				}
			})
		}
	}
}

// TestPlanPreferenceIsTheAuthoredOrder pins WHICH order preference is: the authored slice, untouched, and
// the top-N of it — not the sorted one.
func TestPlanPreferenceIsTheAuthoredOrder(t *testing.T) {
	r := Roster{Explorers: []Explorer{
		{Adapter: "z-adapter", Model: "first-choice"},
		{Adapter: "a-adapter", Model: "second-choice"},
		{Adapter: "m-adapter", Model: "third-choice"},
	}, Collator: Collator{Adapter: "claude-code", Model: "opus"}}

	plan, err := r.SelectTopN(2)
	if err != nil {
		t.Fatalf("SelectTopN: %v", err)
	}
	if plan.Preferred[0].Model != "first-choice" || plan.Preferred[1].Model != "second-choice" {
		t.Errorf("preference order = %+v, want the authored top 2", plan.Preferred)
	}
	// And attribution is still sorted, decoupled from it.
	if plan.Explorers[0].Adapter != "a-adapter" || plan.Explorers[1].Adapter != "z-adapter" {
		t.Errorf("attribution order = %+v, want it sorted by identity triple", plan.Explorers)
	}
}

// TestSameSetDetectsDivergence guards the guard: SameSet must actually be able to fail, or the invariant
// test above proves nothing.
func TestSameSetDetectsDivergence(t *testing.T) {
	p := Plan{
		Explorers: AttributionOrdered{{Adapter: "a", Model: "1"}, {Adapter: "b", Model: "2"}},
		Preferred: PreferenceOrdered{{Adapter: "a", Model: "1"}, {Adapter: "c", Model: "3"}},
	}
	if p.SameSet() {
		t.Error("SameSet accepted two orders holding different explorers")
	}
	if (Plan{Explorers: AttributionOrdered{{Adapter: "a", Model: "1"}}}).SameSet() {
		t.Error("SameSet accepted a plan with no preference order at all")
	}
}

// TestValidateCanonicalizers is the shared 0-or-2 rule every surface and the profile schema defer to.
func TestValidateCanonicalizers(t *testing.T) {
	cases := []struct {
		name    string
		in      []Explorer
		wantErr string // "" = must be accepted
	}{
		{"none — the host derives them", nil, ""},
		{"two distinct", []Explorer{{Adapter: "a", Model: "m1"}, {Adapter: "b", Model: "m2"}}, ""},
		{"two on one adapter, different models", []Explorer{{Adapter: "a", Model: "m1"}, {Adapter: "a", Model: "m2"}}, ""},
		{"one entry", []Explorer{{Adapter: "a", Model: "m1"}}, "which slot it fills"},
		{"three entries", []Explorer{{Adapter: "a", Model: "1"}, {Adapter: "b", Model: "2"}, {Adapter: "c", Model: "3"}}, "exactly 2"},
		{"identical identities", []Explorer{{Adapter: "a", Model: "m"}, {Adapter: "a", Model: "m"}}, "independent"},
		{"identical but for effort", []Explorer{{Adapter: "a", Model: "m", Effort: "high"}, {Adapter: "a", Model: "m", Effort: "low"}}, "independent"},
		{"empty model", []Explorer{{Adapter: "a"}, {Adapter: "b", Model: "m"}}, "non-empty adapter and model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateCanonicalizers(c.in)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("rejected a valid spec: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("accepted an invalid spec")
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("error %q does not mention %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestPlanCarriesCanonicalizers pins that an explicit spec survives validation + selection into the plan
// the pipeline consumes — the spec is configuration, not a per-count decision.
func TestPlanCarriesCanonicalizers(t *testing.T) {
	r := twoValid()
	r.Canonicalizers = []Explorer{{Adapter: "claude-code", Model: "opus"}, {Adapter: "codex-cli", Model: "gpt-5-codex"}}
	plan, err := r.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Canonicalizers) != 2 || plan.Canonicalizers[1].Model != "gpt-5-codex" {
		t.Fatalf("plan canonicalizers = %+v, want both in the order authored", plan.Canonicalizers)
	}
	// A malformed spec is refused at Validate, before anything can be selected or spent.
	r.Canonicalizers = r.Canonicalizers[:1]
	if _, err := r.Plan(); err == nil {
		t.Error("a one-entry canonicalizer spec reached a plan — it must be refused at validation")
	}
}
