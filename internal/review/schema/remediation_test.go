package schema

import "testing"

func TestParseRemediationResult_ValidEdit(t *testing.T) {
	in := []byte(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","edits":[{"file":"main.go","anchor":"func main() {}","replacement":"func main() { _ = 0 }","occurrence":0}]}`)
	r, err := ParseRemediationResult(in, "main.go")
	if err != nil {
		t.Fatalf("valid remediation should parse: %v", err)
	}
	if len(r.Edits) != 1 || r.Edits[0].Replacement != "func main() { _ = 0 }" {
		t.Errorf("edit not preserved: %+v", r.Edits)
	}
}

func TestParseRemediationResult_ToleratesUnknownField(t *testing.T) {
	in := []byte(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","confidence":0.9,"edits":[{"file":"a.go","anchor":"x","replacement":"y"}]}`)
	if _, err := ParseRemediationResult(in, "a.go"); err != nil {
		t.Errorf("a benign unknown field must be tolerated: %v", err)
	}
}

func TestParseRemediationResult_RejectsForeignFile(t *testing.T) {
	// A remediation may only edit the finding's OWN file — never another path.
	in := []byte(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","edits":[{"file":"other.go","anchor":"x","replacement":"y"}]}`)
	if _, err := ParseRemediationResult(in, "main.go"); err == nil {
		t.Error("an edit targeting a file other than the finding's must be rejected")
	}
}

func TestParseRemediationResult_RejectsEmptyAnchorAndNoop(t *testing.T) {
	empty := []byte(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","edits":[{"file":"main.go","anchor":"","replacement":"y"}]}`)
	if _, err := ParseRemediationResult(empty, "main.go"); err == nil {
		t.Error("an empty anchor (blind prepend) must be rejected")
	}
	noop := []byte(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","edits":[{"file":"main.go","anchor":"x","replacement":"x"}]}`)
	if _, err := ParseRemediationResult(noop, "main.go"); err == nil {
		t.Error("a no-op edit (replacement == anchor) must be rejected")
	}
}

func TestParseRemediationResult_NoEditIsValidDecline(t *testing.T) {
	// The model is allowed to safely decline; the caller falls back to the marker.
	in := []byte(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","noEdit":true,"reason":"fix needs context not shown"}`)
	r, err := ParseRemediationResult(in, "main.go")
	if err != nil {
		t.Fatalf("a decline must be valid: %v", err)
	}
	if !r.NoEdit {
		t.Error("NoEdit must be true for a decline")
	}
	// An empty edits array is also a decline.
	empty := []byte(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","edits":[]}`)
	if r2, err := ParseRemediationResult(empty, "main.go"); err != nil || !r2.NoEdit {
		t.Errorf("empty edits must degrade to a decline: r=%+v err=%v", r2, err)
	}
}

func TestParseRemediationResult_RejectsWrongRole(t *testing.T) {
	in := []byte(`{"schemaVersion":1,"role":"reviewer","phase":"semantic_remediate","edits":[{"file":"main.go","anchor":"x","replacement":"y"}]}`)
	if _, err := ParseRemediationResult(in, "main.go"); err == nil {
		t.Error("only author_remediator may produce remediation edits")
	}
}
