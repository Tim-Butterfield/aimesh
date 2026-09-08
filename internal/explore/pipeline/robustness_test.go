package pipeline

// These tests pin the CANONICALIZER SELECTION rule (design §4): which identity is asked for the second,
// independent canonicalization judgment on the dual path, where that choice comes from, and how it is
// recorded.
//
// The distinction the whole file exists to hold: a roster carries TWO orders over the same explorer set.
// PREFERENCE order is what the author ranked (best model first) and is what a SELECTION must read.
// ATTRIBUTION order is `roster.SelectTopN`'s sort by identity triple, which exists so envelope IDs are
// reproducible and is deliberately decoupled from preference. Deriving canonicalizer-b from the
// attribution order picks whichever explorer sorts first alphabetically — an ordering with no relationship
// to capability — for half of a governance rule.

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
)

// preferenceVsAlphabetRoster is a roster whose PREFERENCE-first explorer is NOT its alphabetically-first
// one, so the two orders name different explorers and a selection reading the wrong slice is observable.
// The collator is a third identity, so BOTH explorers are eligible to be canonicalizer-b and the choice
// between them is decided purely by which order is consulted.
func preferenceVsAlphabetRoster() roster.Roster {
	return roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "fake", Model: "zulu-strong", Effort: "high"}, // preference #1, sorts LAST
			{Adapter: "fake", Model: "alpha-weak", Effort: "low"},   // preference #2, sorts FIRST
		},
		Collator: roster.Collator{Adapter: "fake", Model: "collator-model"},
	}
}

// TestCanonicalizerBFollowsPreferenceNotAttribution is the headline: canonicalizer-b is the FIRST explorer
// by the author's PREFERENCE order that differs from the collator — never the first by attribution order.
func TestCanonicalizerBFollowsPreferenceNotAttribution(t *testing.T) {
	plan, err := preferenceVsAlphabetRoster().Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// The premise of the test: the two orders must genuinely disagree, or it proves nothing.
	if got := plan.Explorers[0].Model; got != "alpha-weak" {
		t.Fatalf("attribution order[0] = %q, want the alphabetically-first %q — this roster no longer distinguishes the two orders", got, "alpha-weak")
	}

	choice, err := canonicalizerIdentities(plan, true, "challenge", Options{})
	if err != nil {
		t.Fatalf("canonicalizerIdentities: %v", err)
	}
	ids, provenance := choice.ids, choice.provenance
	if len(ids) != 2 {
		t.Fatalf("dual path returned %d canonicalizer(s), want 2", len(ids))
	}
	if ids[0].role != "canonicalizer-a" || ids[1].role != "canonicalizer-b" {
		t.Fatalf("roles = %q/%q, want canonicalizer-a/canonicalizer-b", ids[0].role, ids[1].role)
	}
	if ids[0].explorer.Model != "collator-model" {
		t.Errorf("canonicalizer-a model = %q, want the collator's %q (slot a defaults to the collator identity)", ids[0].explorer.Model, "collator-model")
	}
	if got := ids[1].explorer.Model; got != "zulu-strong" {
		t.Errorf("canonicalizer-b model = %q, want %q — canonicalizer-b must track the author's PREFERENCE order, not the alphabetical attribution order", got, "zulu-strong")
	}
	if provenance != ProvenanceDerived {
		t.Errorf("provenance = %q, want %q", provenance, ProvenanceDerived)
	}
	if choice.independence != IndependenceDistinct {
		t.Errorf("independence = %q, want %q — the two slots run different models", choice.independence, IndependenceDistinct)
	}
}

// TestCanonicalizerDerivationSkipsCollatorSharers pins the rule the derivation walks under: an explorer
// sharing the collator's (adapter, model) is skipped however high its preference, because it is the same
// route to the same weights and adds nothing to the pair.
func TestCanonicalizerDerivationSkipsCollatorSharers(t *testing.T) {
	src := roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "fake", Model: "collator-model", Effort: "high"}, // preference #1 — but shares the collator
			{Adapter: "fake", Model: "second-choice", Effort: "low"},
		},
		Collator: roster.Collator{Adapter: "fake", Model: "collator-model"},
	}
	plan, err := src.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	choice, err := canonicalizerIdentities(plan, true, "challenge", Options{})
	if err != nil {
		t.Fatalf("canonicalizerIdentities: %v", err)
	}
	if got := choice.ids[1].explorer.Model; got != "second-choice" {
		t.Errorf("canonicalizer-b = %q, want %q — the seat sharing the collator's adapter AND model is the same route to the same weights", got, "second-choice")
	}
}

// TestCanonicalizerSharedModelIsRecordedNotRefused pins the decision taken 2026-08-12 for the case the
// derivation does NOT skip: a seat that shares the collator's MODEL but reaches it through a DIFFERENT
// adapter is accepted as canonicalizer-b, and the run records that the pair shares a model.
//
// Accepted, because a panel is configured deliberately — a reader who wanted two different models would
// have named two different models, and the two proposals are not identical anyway (sampling makes two
// calls to one model differ). Recorded, because a merge held by that pair is weaker evidence than one held
// across two models: the errors correlate, since the priors are the same. Every corroboration count in the
// artifact rests on the difference, so it cannot be left out of the record.
func TestCanonicalizerSharedModelIsRecordedNotRefused(t *testing.T) {
	src := roster.Roster{
		Explorers: []roster.Explorer{
			// Same MODEL as the collator, different ADAPTER — two routes to one set of weights.
			{Adapter: "acp-claude", Model: "collator-model", Effort: "high"},
			{Adapter: "fake", Model: "genuinely-other", Effort: "low"},
		},
		Collator: roster.Collator{Adapter: "fake", Model: "collator-model"},
	}
	plan, err := src.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	choice, err := canonicalizerIdentities(plan, true, "challenge", Options{})
	if err != nil {
		t.Fatalf("a same-model pair behind two adapters must be ACCEPTED, not refused: %v", err)
	}
	if got := choice.ids[1].explorer.Adapter; got != "acp-claude" {
		t.Errorf("canonicalizer-b adapter = %q, want the preference-order winner %q", got, "acp-claude")
	}
	if choice.independence != IndependenceSharedModel {
		t.Errorf("independence = %q, want %q — both slots run %q", choice.independence, IndependenceSharedModel, "collator-model")
	}
}

// TestCanonicalizerDualHaltsWhenNoIndependentIdentity pins the FAIL-CLOSED halt: when every panel member
// shares the collator's model there is no independent second judgment to be had, and the dual path must
// refuse rather than manufacture one.
func TestCanonicalizerDualHaltsWhenNoIndependentIdentity(t *testing.T) {
	src := roster.Roster{
		Explorers: []roster.Explorer{
			{Adapter: "fake", Model: "same-model", Effort: "high"},
			{Adapter: "fake", Model: "same-model", Effort: "low"}, // a different effort is the same weights
		},
		Collator: roster.Collator{Adapter: "fake", Model: "same-model"},
	}
	plan, err := src.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := canonicalizerIdentities(plan, true, "challenge", Options{}); err == nil {
		t.Fatal("dual path accepted a panel with no distinct second canonicalizer — it must HALT")
	} else if !strings.Contains(err.Error(), "SECOND canonicalizer identity") {
		// The wording changed when a same-model pair behind two adapters stopped being refused: what is
		// missing here is a DISTINCT identity to name, not independence in the abstract.
		t.Errorf("halt message %q does not say what is missing", err.Error())
	}
	// IT MUST NAME THE MODE, and name what would work.
	//
	// The measured confusion: this halt reads as a problem with the roster, and the roster is fine —
	// the SAME roster runs the non-dual modes unchanged. Someone who is not told that goes and edits
	// their roster, because nothing pointed at the mode as the variable.
	msg := ""
	if _, err := canonicalizerIdentities(plan, true, "challenge", Options{}); err != nil {
		msg = err.Error()
	}
	if !strings.Contains(msg, `"challenge"`) {
		t.Errorf("the halt does not name the MODE that needs two identities: %q", msg)
	}
	if !strings.Contains(msg, "THE ROSTER IS NOT WRONG ON ITS OWN") {
		t.Errorf("the halt does not say the roster is not the problem: %q", msg)
	}
	// The alternatives are read off the mode registry, so they cannot rot as modes are added.
	for _, single := range singleCanonicalizerModes() {
		if !strings.Contains(msg, single) {
			t.Errorf("the halt does not offer %q, a mode this same roster can run: %q", single, msg)
		}
	}
	if len(singleCanonicalizerModes()) == 0 {
		t.Error("no single-canonicalizer mode is registered, so the offered alternative is empty")
	}

	// The SINGLE path is unaffected: it makes one call and claims no second opinion.
	if single, err := canonicalizerIdentities(plan, false, "catalog", Options{}); err != nil {
		t.Errorf("single path halted on a roster it can serve: %v", err)
	} else if single.provenance != ProvenanceDerived {
		t.Errorf("single-path provenance = %q, want %q", single.provenance, ProvenanceDerived)
	} else if single.independence != "" {
		t.Errorf("single-path independence = %q, want empty — there is no pair to characterize", single.independence)
	}
}

// TestCanonicalizerExplicitFromPlan pins the SPECIFIABLE path: identities the surfaces put on the plan
// (from a profile, a --canonicalizer flag, `_meta.exploremesh.canonicalizers` or the MCP argument) are
// used verbatim, in the order given, and are recorded as `explicit`.
func TestCanonicalizerExplicitFromPlan(t *testing.T) {
	src := preferenceVsAlphabetRoster()
	src.Canonicalizers = []roster.Explorer{
		{Adapter: "fake", Model: "chosen-a", Effort: "high"},
		{Adapter: "fake", Model: "chosen-b"},
	}
	plan, err := src.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	choice, err := canonicalizerIdentities(plan, true, "challenge", Options{})
	if err != nil {
		t.Fatalf("canonicalizerIdentities: %v", err)
	}
	ids, prov := choice.ids, choice.provenance
	if ids[0].explorer.Model != "chosen-a" || ids[1].explorer.Model != "chosen-b" {
		t.Errorf("explicit canonicalizers = %q/%q, want chosen-a/chosen-b in the order supplied", ids[0].explorer.Model, ids[1].explorer.Model)
	}
	if prov != ProvenanceExplicit {
		t.Errorf("provenance = %q, want %q", prov, ProvenanceExplicit)
	}
	// The SINGLE path takes the first named identity rather than silently ignoring the spec.
	singleChoice, err := canonicalizerIdentities(plan, false, "catalog", Options{})
	if err != nil {
		t.Fatalf("single path: %v", err)
	}
	single, sprov := singleChoice.ids, singleChoice.provenance
	if len(single) != 1 || single[0].explorer.Model != "chosen-a" || single[0].role != "canonicalizer" {
		t.Errorf("single path = %+v, want one `canonicalizer` call on chosen-a", single)
	}
	if sprov != ProvenanceExplicit {
		t.Errorf("single-path provenance = %q, want %q", sprov, ProvenanceExplicit)
	}
}

// TestCanonicalizerOptionsOverridePlan pins the host override's precedence: Options wins over the plan's
// own spec (a caller driving RunSpec directly is the most specific voice in the room).
func TestCanonicalizerOptionsOverridePlan(t *testing.T) {
	src := preferenceVsAlphabetRoster()
	src.Canonicalizers = []roster.Explorer{{Adapter: "fake", Model: "from-plan-a"}, {Adapter: "fake", Model: "from-plan-b"}}
	plan, err := src.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	opts := Options{Canonicalizers: []roster.Explorer{{Adapter: "fake", Model: "from-opts-a"}, {Adapter: "fake", Model: "from-opts-b"}}}
	choice, err := canonicalizerIdentities(plan, true, "challenge", opts)
	if err != nil {
		t.Fatalf("canonicalizerIdentities: %v", err)
	}
	ids := choice.ids
	if ids[0].explorer.Model != "from-opts-a" || ids[1].explorer.Model != "from-opts-b" {
		t.Errorf("got %q/%q, want the Options override to win over the plan's spec", ids[0].explorer.Model, ids[1].explorer.Model)
	}
}

// TestCanonicalizerMalformedSpecRefused pins the backstop: even reaching the pipeline directly, a spec of
// one entry or of two identical identities is refused rather than half-applied.
func TestCanonicalizerMalformedSpecRefused(t *testing.T) {
	plan, err := preferenceVsAlphabetRoster().Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	cases := map[string][]roster.Explorer{
		"one entry":      {{Adapter: "fake", Model: "lonely"}},
		"identical pair": {{Adapter: "fake", Model: "twin"}, {Adapter: "fake", Model: "twin"}},
		"identical but effort": {{Adapter: "fake", Model: "twin", Effort: "high"},
			{Adapter: "fake", Model: "twin", Effort: "low"}},
		"three entries": {{Adapter: "fake", Model: "a"}, {Adapter: "fake", Model: "b"}, {Adapter: "fake", Model: "c"}},
	}
	for name, cs := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := canonicalizerIdentities(plan, true, "challenge", Options{Canonicalizers: cs}); err == nil {
				t.Fatalf("accepted a malformed canonicalizer spec (%s)", name)
			}
		})
	}
}

// TestCanonicalizerDerivationRefusesPlanWithoutPreference pins the guard that keeps the fix from being
// undone by a hand-built plan: without the preference order the only order left is the attribution order,
// and quietly falling back to it is exactly the defect. It must refuse instead.
func TestCanonicalizerDerivationRefusesPlanWithoutPreference(t *testing.T) {
	handBuilt := roster.Plan{
		Explorers: roster.AttributionOrdered{{Adapter: "fake", Model: "alpha"}, {Adapter: "fake", Model: "zulu"}},
		Collator:  roster.Collator{Adapter: "fake", Model: "collator-model"},
	}
	if _, err := canonicalizerIdentities(handBuilt, true, "challenge", Options{}); err == nil {
		t.Fatal("derived canonicalizer-b from a plan that recorded no preference order — it must refuse")
	}
	// An EXPLICIT spec needs no preference order, so the same plan is serviceable when the identities are named.
	handBuilt.Canonicalizers = []roster.Explorer{{Adapter: "fake", Model: "x"}, {Adapter: "fake", Model: "y"}}
	if explicit, err := canonicalizerIdentities(handBuilt, true, "challenge", Options{}); err != nil {
		t.Errorf("explicit spec refused on a plan with no preference order: %v", err)
	} else if explicit.provenance != ProvenanceExplicit {
		t.Errorf("provenance = %q, want %q", explicit.provenance, ProvenanceExplicit)
	}
}
