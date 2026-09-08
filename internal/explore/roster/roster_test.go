package roster

import (
	"path/filepath"
	"strings"
	"testing"
)

func twoValid() Roster {
	return Roster{
		Explorers: []Explorer{
			{Adapter: "claude-code", Model: "opus", Effort: "high"},
			{Adapter: "codex-cli", Model: "gpt-5-codex", Effort: "medium"},
		},
		Collator: Collator{Adapter: "claude-code", Model: "opus", Effort: "high"},
	}
}

func TestValidate_AcceptsTwoUnique(t *testing.T) {
	if err := twoValid().Validate(); err != nil {
		t.Fatalf("valid roster rejected: %v", err)
	}
}

func TestValidate_RejectsFewerThanTwo(t *testing.T) {
	r := twoValid()
	r.Explorers = r.Explorers[:1]
	if err := r.Validate(); err == nil {
		t.Fatal("a single-explorer roster is not a panel — must be rejected")
	}
}

func TestValidate_RejectsDuplicateTriple(t *testing.T) {
	r := twoValid()
	r.Explorers = append(r.Explorers, Explorer{Adapter: "claude-code", Model: "opus", Effort: "high"}) // exact dup
	if err := r.Validate(); err == nil {
		t.Fatal("an identical (adapter,model,effort) triple must be rejected")
	}
}

func TestValidate_EffortDiffIsNotDuplicate(t *testing.T) {
	r := twoValid()
	// same adapter+model, different effort — a genuine second vantage (Opus-high vs Opus-low).
	r.Explorers = append(r.Explorers, Explorer{Adapter: "claude-code", Model: "opus", Effort: "low"})
	if err := r.Validate(); err != nil {
		t.Fatalf("explorers differing only in effort must be allowed: %v", err)
	}
}

func TestValidate_SameAdapterDifferentModelAllowed(t *testing.T) {
	r := Roster{
		Explorers: []Explorer{
			{Adapter: "claude-code", Model: "sonnet"},
			{Adapter: "claude-code", Model: "opus"},
		},
		Collator: Collator{Adapter: "codex-cli", Model: "gpt-5-codex"},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("same adapter with different models must be allowed: %v", err)
	}
}

func TestValidate_RejectsEmptyExplorerFields(t *testing.T) {
	r := twoValid()
	r.Explorers[0].Model = ""
	if err := r.Validate(); err == nil {
		t.Fatal("an explorer with an empty model must be rejected")
	}
}

func TestValidate_RejectsEmptyCollator(t *testing.T) {
	r := twoValid()
	r.Collator.Model = ""
	if err := r.Validate(); err == nil {
		t.Fatal("a collator with an empty model must be rejected")
	}
}

func TestPlan_StableExplorerOrder(t *testing.T) {
	r := Roster{
		Explorers: []Explorer{
			{Adapter: "z", Model: "m"},
			{Adapter: "a", Model: "m"},
		},
		Collator: Collator{Adapter: "a", Model: "m"},
	}
	p, err := r.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if p.Explorers[0].Adapter != "a" || p.Explorers[1].Adapter != "z" {
		t.Errorf("plan explorers not in stable identity order: %+v", p.Explorers)
	}
}

// four is a 4-explorer roster whose AUTHORED order (preference) is deliberately NOT the canonical
// attribution order, so the two orders can be told apart in the SelectTopN tests.
func four() Roster {
	return Roster{
		Explorers: []Explorer{
			{Adapter: "z", Model: "m"}, // preference 1
			{Adapter: "a", Model: "m"}, // preference 2
			{Adapter: "y", Model: "m"}, // preference 3
			{Adapter: "b", Model: "m"}, // preference 4
		},
		Collator: Collator{Adapter: "c", Model: "m"},
	}
}

// TestSelectTopN_TakesFirstNByAuthoredOrder: the SELECTION is the top-N by PREFERENCE (the authored
// slice order), while the returned plan is in canonical (triple-sorted) ATTRIBUTION order.
func TestSelectTopN_TakesFirstNByAuthoredOrder(t *testing.T) {
	p, err := four().SelectTopN(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Explorers) != 2 {
		t.Fatalf("SelectTopN(2) returned %d explorers", len(p.Explorers))
	}
	// The first TWO authored (z, a) were selected — not the two that sort first (a, b).
	got := map[string]bool{p.Explorers[0].Adapter: true, p.Explorers[1].Adapter: true}
	if !got["z"] || !got["a"] {
		t.Errorf("selected set = %+v, want the first 2 by authored order (z, a)", p.Explorers)
	}
	// …and they come back in canonical attribution order (a before z), not preference order.
	if p.Explorers[0].Adapter != "a" || p.Explorers[1].Adapter != "z" {
		t.Errorf("plan explorers not in canonical attribution order: %+v", p.Explorers)
	}
}

// TestSelectTopN_EnvelopeIDStability is the load-bearing decoupling (design §7): reordering the FULL
// explorer list must NOT change the Plan for an UNCHANGED selected set, so envelope IDs / attribution
// stay reproducible across a preference reshuffle.
func TestSelectTopN_EnvelopeIDStability(t *testing.T) {
	base := four()
	p1, err := base.SelectTopN(2) // selects z, a
	if err != nil {
		t.Fatal(err)
	}
	// Swap the two selected explorers' PREFERENCE order (and reshuffle the unselected tail): the selected
	// SET is identical, so the plan must be identical.
	reordered := Roster{
		Explorers: []Explorer{
			{Adapter: "a", Model: "m"},
			{Adapter: "z", Model: "m"},
			{Adapter: "b", Model: "m"},
			{Adapter: "y", Model: "m"},
		},
		Collator: base.Collator,
	}
	p2, err := reordered.SelectTopN(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Explorers) != len(p2.Explorers) {
		t.Fatalf("plan sizes differ: %d vs %d", len(p1.Explorers), len(p2.Explorers))
	}
	for i := range p1.Explorers {
		if p1.Explorers[i] != p2.Explorers[i] {
			t.Fatalf("reordering the full list changed the plan for an unchanged selected set:\n %+v\n %+v", p1.Explorers, p2.Explorers)
		}
	}
	if p1.Collator != p2.Collator {
		t.Errorf("collator differs: %+v vs %+v", p1.Collator, p2.Collator)
	}
}

// TestSelectTopN_RangeIsNeverClamped: n<2 is not a panel, and n>len is a clear out-of-range error that
// says so — requested = executed (design §7).
func TestSelectTopN_RangeIsNeverClamped(t *testing.T) {
	r := four()
	for _, n := range []int{-1, 0, 1} {
		if _, err := r.SelectTopN(n); err == nil {
			t.Errorf("SelectTopN(%d) must be rejected — a panel needs 2+ explorers", n)
		}
	}
	_, err := r.SelectTopN(5)
	if err == nil {
		t.Fatal("a count above the roster size must be an error, never clamped")
	}
	if !strings.Contains(err.Error(), "never clamped") {
		t.Errorf("the out-of-range error should say the count is not clamped: %v", err)
	}
}

// TestSelectTopN_FullSetEqualsPlan: n == len(Explorers) selects everything and matches Plan() exactly
// (Plan IS SelectTopN over the whole roster).
func TestSelectTopN_FullSetEqualsPlan(t *testing.T) {
	r := four()
	sel, err := r.SelectTopN(len(r.Explorers))
	if err != nil {
		t.Fatal(err)
	}
	full, err := r.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Explorers) != 4 {
		t.Fatalf("SelectTopN(len) selected %d explorers, want all 4", len(sel.Explorers))
	}
	for i := range full.Explorers {
		if sel.Explorers[i] != full.Explorers[i] {
			t.Fatalf("SelectTopN(len) != Plan(): %+v vs %+v", sel.Explorers, full.Explorers)
		}
	}
}

// TestSelectTopN_ValidatesFirst: an invalid roster is rejected before any selection happens, so a
// --count can never "select around" a duplicate triple or an empty field.
func TestSelectTopN_ValidatesFirst(t *testing.T) {
	dup := four()
	dup.Explorers[1] = dup.Explorers[0] // exact duplicate triple
	if _, err := dup.SelectTopN(2); err == nil {
		t.Error("SelectTopN must validate the whole roster before selecting")
	}
}

func TestExplorer_Identity(t *testing.T) {
	e := Explorer{Adapter: "claude-code", Model: "opus", Effort: "high"}
	id := e.Identity()
	if id.Adapter != "claude-code" || id.Model != "opus" || id.Effort != "high" {
		t.Errorf("identity mismatch: %+v", id)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.yaml")
	orig := twoValid()
	if err := Save(path, orig); err != nil {
		t.Fatalf("save: %v", err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(back.Explorers) != 2 || back.Collator != orig.Collator {
		t.Errorf("round-trip lost data: %+v", back)
	}
	if back.Explorers[0] != orig.Explorers[0] {
		t.Errorf("explorer round-trip mismatch: %+v vs %+v", back.Explorers[0], orig.Explorers[0])
	}
}

func TestSave_RefusesInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.yaml")
	bad := twoValid()
	bad.Explorers = bad.Explorers[:1] // < 2
	if err := Save(path, bad); err == nil {
		t.Fatal("Save must refuse to persist an invalid roster")
	}
}

func TestDecode_RejectsUnknownField(t *testing.T) {
	// strict decode: an unknown struct field is a typo → rejected.
	_, err := Decode("roster.json", []byte(`{"explorers":[],"bogusField":true}`))
	if err == nil {
		t.Fatal("strict decode must reject an unknown field")
	}
}
