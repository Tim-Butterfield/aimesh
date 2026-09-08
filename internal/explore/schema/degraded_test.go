package schema

import (
	"strings"
	"testing"
)

func id(model string) ExplorerIdentity {
	return ExplorerIdentity{Adapter: "ad", Model: model, Effort: "high"}
}

func envs() []Envelope {
	return []Envelope{
		{Identity: id("m-a"), Order: 0, Response: map[string]any{
			"candidates": []any{"Postgres", "SQLite"}, "notes": "from A", "confidence": 0.8,
		}},
		{Identity: id("m-b"), Order: 1, Response: map[string]any{
			"candidates": []any{"Postgres", "MySQL"}, "notes": "from B", "confidence": 0.6,
		}},
	}
}

// TestDegrade_EmergentSpace_MechanicalIndexOnly pins the emergent-space degraded artifact (design §1): the raw
// attributed envelopes plus a MECHANICAL, ungrouped typed-claim index carrying the exact uncollated label — and
// NEVER a host register, which over explorer-authored values would be covert entity resolution.
func TestDegrade_EmergentSpace_MechanicalIndexOnly(t *testing.T) {
	d := Degrade(EmergentSpace, "map", DegradedCollatorUnavailable, "collator timed out", envs(), "")
	if d.Label != UncollatedLabel {
		t.Errorf("label = %q, want the exact uncollated label", d.Label)
	}
	if len(d.Register) != 0 {
		t.Error("an emergent-space degraded artifact must carry NO host register")
	}
	if len(d.Envelopes) != 2 {
		t.Fatalf("the raw envelopes must be carried, got %d", len(d.Envelopes))
	}
	// One entry per field value per envelope: 2 candidates + notes + confidence = 4 per envelope.
	if len(d.ClaimIndex) != 8 {
		t.Errorf("the index must be a mechanical transposition (8 entries expected), got %d: %+v", len(d.ClaimIndex), d.ClaimIndex)
	}
	// Nothing is grouped: "Postgres" appears twice, once per explorer, each attributed.
	pg := 0
	for _, c := range d.ClaimIndex {
		if c.Value == "Postgres" {
			pg++
			if c.Field != "candidates" || c.EnvelopeRef == "" || c.Explorer.Model == "" {
				t.Errorf("index entry must be attributed to a field + envelope + explorer: %+v", c)
			}
		}
	}
	if pg != 2 {
		t.Errorf("identical values from two explorers must stay SEPARATE entries (no grouping), got %d", pg)
	}
	// Determinism: fields are visited in sorted order, so two calls agree entry-for-entry.
	again := Degrade(EmergentSpace, "map", DegradedCollatorUnavailable, "collator timed out", envs(), "")
	for i := range d.ClaimIndex {
		if d.ClaimIndex[i] != again.ClaimIndex[i] {
			t.Fatalf("the claim index must be deterministic at %d: %+v vs %+v", i, d.ClaimIndex[i], again.ClaimIndex[i])
		}
	}
	if !strings.Contains(d.Summary(), UncollatedLabel) {
		t.Errorf("the summary must state the label: %q", d.Summary())
	}
}

// TestDegrade_FixedSpace_RealRegister pins the other half: a FIXED-space mode's degraded artifact is a REAL host
// register keyed on the declared field's exact values — legitimate because that universe was GIVEN to the
// explorers — with attributed positions and no uncollated label.
func TestDegrade_FixedSpace_RealRegister(t *testing.T) {
	d := Degrade(FixedSpace, "compare", DegradedIdentityHalt, "identity halt", envs(), "candidates")
	if len(d.ClaimIndex) != 0 || d.Label != "" {
		t.Error("a fixed-space degraded artifact is a register, not the uncollated emergent artifact")
	}
	byKey := map[string]RegisterEntry{}
	for _, e := range d.Register {
		byKey[e.Key] = e
	}
	if len(byKey["Postgres"].Positions) != 2 {
		t.Errorf("both explorers must appear under the shared declared key: %+v", byKey["Postgres"])
	}
	if len(byKey["SQLite"].Positions) != 1 || len(byKey["MySQL"].Positions) != 1 {
		t.Errorf("each distinct declared value gets its own row: %+v", d.Register)
	}
	for _, e := range d.Register {
		for _, p := range e.Positions {
			if p.Explorer.Model == "" || p.EnvelopeRef == "" {
				t.Errorf("a register position must be attributed: %+v", p)
			}
		}
	}
	// An envelope missing the declared key is recorded under the empty key — never silently dropped.
	missing := Degrade(FixedSpace, "compare", DegradedIdentityHalt, "", []Envelope{{Identity: id("m-c"), Order: 0, Response: map[string]any{"other": "x"}}}, "candidates")
	if len(missing.Register) != 1 || missing.Register[0].Key != "" {
		t.Errorf("an envelope without the key field must still appear: %+v", missing.Register)
	}
}

// TestEnvelopeRef_Round1Unqualified pins the ref scheme the anti-echo filter relies on: round 1 keeps the
// unqualified form and a later round is qualified, so refs never
// collide across rounds.
func TestEnvelopeRef_Round1Unqualified(t *testing.T) {
	if got := EnvelopeRef(1, 0); got != "envelope#0" {
		t.Errorf("round-1 refs must stay unqualified, got %q", got)
	}
	if got := EnvelopeRef(0, 3); got != "envelope#3" {
		t.Errorf("an unset round must render the round-1 form, got %q", got)
	}
	if got := EnvelopeRef(2, 0); got != "r2:envelope#0" {
		t.Errorf("a later round must be qualified, got %q", got)
	}
	if EnvelopeRef(2, 0) == EnvelopeRef(1, 0) {
		t.Error("refs must never collide across rounds (that is what keeps a count round-1 only)")
	}
}

// TestIsAbstention pins the DELIBERATE abstention channel (§1): only an explicit boolean true abstains, so a
// missing or non-boolean field is never read as a position.
func TestIsAbstention(t *testing.T) {
	if !IsAbstention(map[string]any{AbstentionField: true}) {
		t.Error("an explicit abstain:true must be recognized")
	}
	for name, resp := range map[string]map[string]any{
		"absent":     {"claims": []any{"x"}},
		"false":      {AbstentionField: false},
		"non-bool":   {AbstentionField: "yes"},
		"nil valued": {AbstentionField: nil},
	} {
		if IsAbstention(resp) {
			t.Errorf("%s: must NOT be read as an abstention", name)
		}
	}
}
