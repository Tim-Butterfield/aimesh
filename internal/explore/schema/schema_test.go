package schema

import (
	"bytes"
	"encoding/json"
	"testing"
)

// expandedOK returns the minimum schema plus one additive field (a valid collator expansion).
func expandedOK() Schema {
	s := MinimumSchema()
	s.Fields = append(s.Fields, Field{Name: "tradeoffs", Type: TypeString, Required: false, Repeated: true})
	return s
}

func TestMinimumSchema_ValidatesAndRoundTrips(t *testing.T) {
	m := MinimumSchema()
	if err := m.Validate(); err != nil {
		t.Fatalf("minimum schema invalid: %v", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Schema
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Fields) != len(m.Fields) {
		t.Fatalf("round-trip field count %d, want %d", len(back.Fields), len(m.Fields))
	}
	for i := range m.Fields {
		if back.Fields[i] != m.Fields[i] {
			t.Errorf("round-trip field %d = %+v, want %+v", i, back.Fields[i], m.Fields[i])
		}
	}
	// every reserved field must be recognized
	for _, f := range m.Fields {
		if !IsReserved(f.Name) {
			t.Errorf("minimum field %q not reported reserved", f.Name)
		}
	}
}

func TestGuard_AcceptsAdditive(t *testing.T) {
	if err := ExpandedSatisfiesMinimum(expandedOK()); err != nil {
		t.Fatalf("additive expansion should satisfy the guard: %v", err)
	}
}

func TestGuard_RejectsDrop(t *testing.T) {
	s := MinimumSchema()
	// drop "uncertainties"
	kept := s.Fields[:0]
	for _, f := range s.Fields {
		if f.Name != "uncertainties" {
			kept = append(kept, f)
		}
	}
	s.Fields = kept
	if err := ExpandedSatisfiesMinimum(s); err == nil {
		t.Fatal("dropping a reserved field must fail the guard")
	}
}

func TestGuard_RejectsRename(t *testing.T) {
	s := MinimumSchema()
	for i := range s.Fields {
		if s.Fields[i].Name == "claims" {
			s.Fields[i].Name = "assertions" // rename
		}
	}
	if err := ExpandedSatisfiesMinimum(s); err == nil {
		t.Fatal("renaming a reserved field must fail the guard")
	}
}

func TestGuard_RejectsRetype(t *testing.T) {
	s := MinimumSchema()
	for i := range s.Fields {
		if s.Fields[i].Name == "confidence" {
			s.Fields[i].Type = TypeString // was number
		}
	}
	if err := ExpandedSatisfiesMinimum(s); err == nil {
		t.Fatal("re-typing a reserved field must fail the guard")
	}
}

func TestGuard_RejectsRelaxRequired(t *testing.T) {
	s := MinimumSchema()
	for i := range s.Fields {
		if s.Fields[i].Name == "evidence" {
			s.Fields[i].Required = false // relax
		}
	}
	if err := ExpandedSatisfiesMinimum(s); err == nil {
		t.Fatal("relaxing a reserved field's required must fail the guard")
	}
}

func TestGuard_RejectsRepeatedChange(t *testing.T) {
	s := MinimumSchema()
	for i := range s.Fields {
		if s.Fields[i].Name == "claims" {
			s.Fields[i].Repeated = false // was repeated
		}
	}
	if err := ExpandedSatisfiesMinimum(s); err == nil {
		t.Fatal("changing a reserved field's repeated must fail the guard")
	}
}

func TestValidateResponse_AcceptsWellFormed(t *testing.T) {
	resp := map[string]any{
		"claims":        []any{"a", "b"},
		"evidence":      "because",
		"confidence":    0.8,
		"sources":       []any{"src1"},
		"assumptions":   []any{},
		"uncertainties": []any{"unknown x"},
		"tradeoffs":     []any{"t1"},
	}
	if err := ValidateResponse(expandedOK(), resp); err != nil {
		t.Fatalf("well-formed response rejected: %v", err)
	}
}

func TestValidateResponse_MissingRequiredFails(t *testing.T) {
	resp := map[string]any{
		"claims":     []any{"a"},
		"evidence":   "x",
		"confidence": 1.0,
		"sources":    []any{"s"},
		// assumptions + uncertainties missing
	}
	if err := ValidateResponse(MinimumSchema(), resp); err == nil {
		t.Fatal("missing required field must fail validation")
	}
}

func TestValidateResponse_WrongTypeFails(t *testing.T) {
	resp := map[string]any{
		"claims":        []any{"a"},
		"evidence":      "x",
		"confidence":    "high", // should be number
		"sources":       []any{"s"},
		"assumptions":   []any{},
		"uncertainties": []any{},
	}
	if err := ValidateResponse(MinimumSchema(), resp); err == nil {
		t.Fatal("wrong scalar type must fail validation")
	}
}

func TestValidateResponse_RepeatedMustBeList(t *testing.T) {
	resp := map[string]any{
		"claims":        "not-a-list", // repeated field given a scalar
		"evidence":      "x",
		"confidence":    1.0,
		"sources":       []any{"s"},
		"assumptions":   []any{},
		"uncertainties": []any{},
	}
	if err := ValidateResponse(MinimumSchema(), resp); err == nil {
		t.Fatal("a repeated field given a scalar must fail validation")
	}
}

func TestExplorerTaskPayload_CanonicalIsByteIdenticalAndHashes(t *testing.T) {
	p := ExplorerTaskPayload{FinalPrompt: "explore X", ExpandedSchema: expandedOK()}
	if err := p.Validate(); err != nil {
		t.Fatalf("payload should validate: %v", err)
	}
	a, err := p.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	// A separately-constructed identical payload must serialize byte-identically (the §6.2 invariant).
	q := ExplorerTaskPayload{FinalPrompt: "explore X", ExpandedSchema: expandedOK()}
	b, err := q.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("identical payloads not byte-identical:\n%s\n%s", a, b)
	}
	ha, _ := p.Hash()
	hb, _ := q.Hash()
	if ha == "" || ha != hb {
		t.Errorf("payload hashes differ: %q vs %q", ha, hb)
	}
}

// TestDefaultPrompt_PreservesIntentAndSatisfiesTheGuard pins the app-owned round-1 pair the Map mode
// ships: the prompt preserves every purpose + criterion verbatim (intent fidelity — add nothing, drop
// nothing), and the minimum schema it is paired with satisfies the minimum-schema guard. It replaces the
// old DefaultFormulation test, whose deterministic-FALLBACK constructor went away with the
// collator-formulate leg; the prompt + schema it exercised are still live.
func TestDefaultPrompt_PreservesIntentAndSatisfiesTheGuard(t *testing.T) {
	raw := RawTask{Purpose: "compare architectures", Criteria: []string{"scalability", "cost"}}
	prompt := DefaultPrompt(raw)
	for _, want := range []string{"compare architectures", "scalability", "cost"} {
		if !bytes.Contains([]byte(prompt), []byte(want)) {
			t.Errorf("default prompt dropped %q:\n%s", want, prompt)
		}
	}
	if err := ExpandedSatisfiesMinimum(MinimumSchema()); err != nil {
		t.Errorf("the minimum schema must satisfy its own guard: %v", err)
	}
}

// TestFormulationFree_RecordsProvenance pins the formulation-free constructor: it carries the
// app-owned prompt + fixed schema, records the formulation-free source, and does NOT mark a collator
// formulate attempt (the collator was never asked to formulate).
func TestFormulationFree_RecordsProvenance(t *testing.T) {
	f := FormulationFree(FormulationFreeMap, "app-owned prompt", MinimumSchema())
	if f.Source != FormulationFreeMap {
		t.Errorf("source = %q, want %q", f.Source, FormulationFreeMap)
	}
	if f.CollatorAttempted {
		t.Error("formulation-free must NOT record a collator formulate attempt")
	}
	if f.Payload.FinalPrompt != "app-owned prompt" {
		t.Errorf("payload prompt = %q", f.Payload.FinalPrompt)
	}
	if err := ExpandedSatisfiesMinimum(f.Payload.ExpandedSchema); err != nil {
		t.Errorf("formulation-free schema must satisfy the guard: %v", err)
	}
}

// IsWeak is the ONLY behavior an identity status drives: it selects the caveat text shown to a reader.
// Everything short of a strong-evidence match is weak, mismatch included.
func TestIdentityStatus_IsWeak(t *testing.T) {
	for _, s := range []IdentityStatus{IdentitySelfReported, IdentityUnknown, IdentityMismatch} {
		if !s.IsWeak() {
			t.Errorf("%s must be weak (it is not a strong-evidence match)", s)
		}
	}
	if IdentityVerified.IsWeak() {
		t.Error("verified must not be weak")
	}
}

// A collator output round-trips. There is deliberately NO weak-response appendix: nothing is ever
// relegated out of the primary synthesis on identity grounds, so there is nothing for one to hold.
func TestCollatorOutput_RoundTrip(t *testing.T) {
	out := CollatorOutput{
		SynthesisSummary: "summary",
		Findings:         []Finding{{Statement: "f1", Confidence: 0.9}},
		DisagreementRegister: []DisagreementEntry{{
			Subject:   "approach",
			Positions: []Position{{Explorer: ExplorerIdentity{Adapter: "claude-code", Model: "opus", Effort: "high"}, Stance: "A"}},
		}},
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var back CollatorOutput
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Findings) != 1 || back.Findings[0].Statement != "f1" {
		t.Errorf("findings not preserved: %+v", back.Findings)
	}
	if back.DisagreementRegister[0].Positions[0].Explorer.Effort != "high" {
		t.Error("disagreement position must key on full (adapter,model,effort) identity")
	}
}

func TestRawTaskValidate(t *testing.T) {
	cases := []struct {
		name    string
		task    RawTask
		wantErr bool
	}{
		{"ok", RawTask{Purpose: "explore X", Criteria: []string{"c1"}}, false},
		{"empty purpose", RawTask{Purpose: "  ", Criteria: []string{"c1"}}, true},
		{"nil criteria", RawTask{Purpose: "explore X"}, true},
		{"empty criteria", RawTask{Purpose: "explore X", Criteria: []string{}}, true},
		{"all-whitespace criteria", RawTask{Purpose: "explore X", Criteria: []string{"  ", "\t", ""}}, true},
		{"one non-blank among blanks", RawTask{Purpose: "explore X", Criteria: []string{"  ", "real", ""}}, false},
		// Validate accepts an empty or any mode string (the registry lives in internal/mode; the surfaces +
		// pipeline reject an UNKNOWN mode — Validate must not couple the schema package to the registry).
		{"with mode", RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: "map"}, false},
		{"arbitrary mode string accepted by Validate", RawTask{Purpose: "explore X", Criteria: []string{"c1"}, Mode: "whatever"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.task.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("expected an error for %+v", tc.task)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for %+v: %v", tc.task, err)
			}
		})
	}
}
