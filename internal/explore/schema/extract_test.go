package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExtractJSONObject_CleanIsByteIdentical is the zero-regression sentinel: an input that is already
// exactly one JSON object returns the SAME bytes with NO repairs (so the fake adapter + existing goldens
// are untouched).
func TestExtractJSONObject_CleanIsByteIdentical(t *testing.T) {
	in := []byte(`{"a":1,"b":["x","y"],"c":{"d":true}}`)
	obj, repairs, err := ExtractJSONObject(in)
	if err != nil {
		t.Fatalf("clean object errored: %v", err)
	}
	if len(repairs) != 0 {
		t.Errorf("clean object recorded repairs %v, want none", repairs)
	}
	if string(obj) != string(in) {
		t.Errorf("clean object not byte-identical:\n got %q\nwant %q", obj, in)
	}
}

// Whitespace padding is normalized without being counted as a repair (it never changes the JSON value).
func TestExtractJSONObject_WhitespacePaddedNoRepair(t *testing.T) {
	obj, repairs, err := ExtractJSONObject([]byte("  \n\t{\"a\":1}\n  "))
	if err != nil {
		t.Fatalf("padded object errored: %v", err)
	}
	if len(repairs) != 0 {
		t.Errorf("whitespace trim recorded a repair %v, want none", repairs)
	}
	if string(obj) != `{"a":1}` {
		t.Errorf("got %q, want %q", obj, `{"a":1}`)
	}
}

func TestExtractJSONObject_StripsCodeFence(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"json-tag", "```json\n{\"a\":1}\n```"},
		{"bare-fence", "```\n{\"a\":1}\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj, repairs, err := ExtractJSONObject([]byte(tc.in))
			if err != nil {
				t.Fatalf("fenced object errored: %v", err)
			}
			if got := strings.Join(repairs, ","); got != RepairStrippedCodeFence {
				t.Errorf("repairs = %q, want just %q", got, RepairStrippedCodeFence)
			}
			if string(obj) != `{"a":1}` {
				t.Errorf("got %q, want %q", obj, `{"a":1}`)
			}
		})
	}
}

func TestExtractJSONObject_TrimsSurroundingProse(t *testing.T) {
	obj, repairs, err := ExtractJSONObject([]byte(`Sure! Here is the result: {"a":1} — let me know if you need more.`))
	if err != nil {
		t.Fatalf("prose-wrapped object errored: %v", err)
	}
	if string(obj) != `{"a":1}` {
		t.Errorf("got %q, want %q", obj, `{"a":1}`)
	}
	want := map[string]bool{RepairTrimmedLeadingProse: true, RepairTrimmedTrailingProse: true}
	for _, r := range repairs {
		delete(want, r)
	}
	if len(want) != 0 {
		t.Errorf("missing expected repairs %v (got %v)", want, repairs)
	}
}

// A brace INSIDE a string literal must not falsely close the object (the string-aware matcher).
func TestExtractJSONObject_BraceInStringDoesNotFalseClose(t *testing.T) {
	in := `{"template":"a } that looks like a close","escaped":"a \" and a } too","n":1}`
	obj, repairs, err := ExtractJSONObject([]byte(in))
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if len(repairs) != 0 {
		t.Errorf("clean (brace-in-string) recorded repairs %v", repairs)
	}
	if string(obj) != in {
		t.Errorf("object truncated at the in-string brace:\n got %q\nwant %q", obj, in)
	}
	// And it must still be the exact valid JSON.
	var v map[string]any
	if err := json.Unmarshal(obj, &v); err != nil {
		t.Fatalf("extracted bytes are not valid JSON: %v", err)
	}
}

// Nested objects are ONE top-level object (nesting doesn't count as multiple).
func TestExtractJSONObject_NestedIsSingle(t *testing.T) {
	in := `{"outer":{"inner":{"x":1}},"arr":[{"y":2},{"z":3}]}`
	obj, _, err := ExtractJSONObject([]byte(in))
	if err != nil {
		t.Fatalf("nested object errored: %v", err)
	}
	if string(obj) != in {
		t.Errorf("got %q, want %q", obj, in)
	}
}

func TestExtractJSONObject_MultipleObjectsAmbiguous(t *testing.T) {
	_, _, err := ExtractJSONObject([]byte(`{"a":1} {"b":2}`))
	if err == nil {
		t.Fatal("two top-level objects must be an error (ambiguous), not a silent pick")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("error %q should mention 'multiple'", err)
	}
}

func TestExtractJSONObject_NoObject(t *testing.T) {
	for _, in := range []string{"", "   ", "just some prose, no json here", "[1,2,3]"} {
		if _, _, err := ExtractJSONObject([]byte(in)); err == nil {
			t.Errorf("input %q with no top-level object must error", in)
		}
	}
}

func TestExtractJSONObject_Malformed(t *testing.T) {
	for _, in := range []string{`{"a":1`, `{"a":"unterminated}`} {
		if _, _, err := ExtractJSONObject([]byte(in)); err == nil {
			t.Errorf("malformed input %q must error", in)
		}
	}
}

// A fenced block with two objects inside is still ambiguous: the fence strips, then the object scanner
// rejects the two-object body (it never silently picks one).
func TestExtractJSONObject_FenceThenAmbiguous(t *testing.T) {
	_, _, err := ExtractJSONObject([]byte("```json\n{\"a\":1}\n{\"b\":2}\n```"))
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("fenced two-object body should be ambiguous, got err=%v", err)
	}
}
