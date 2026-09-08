package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

const specDoc = `# Preamble
background prose
# The Relevant Part
the part that matters
# Appendix
trailing material
`

func writeSpec(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(p, []byte(specDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAuthorityRef_SectionResolvesToTheBytesItNames is the property the whole shorthand rests on: the
// range it produces must select exactly that section, because what gets embedded is what the
// reviewers are told is authoritative.
func TestAuthorityRef_SectionResolvesToTheBytesItNames(t *testing.T) {
	p := writeSpec(t)
	docs, err := parseAuthority([]string{p + "#The Relevant Part"}, nil, "")
	if err != nil {
		t.Fatalf("parseAuthority: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs", len(docs))
	}
	d := docs[0]
	// A SELECTOR MAKES IT A PARTIAL INCLUSION — which is what makes the manifest say complete:false
	// and the prompt mark the omitted spans. It must never arrive as requireFull.
	if d.Completeness != review.CompletenessRanges {
		t.Errorf("completeness = %q, want %q", d.Completeness, review.CompletenessRanges)
	}
	if len(d.Ranges) != 1 {
		t.Fatalf("ranges = %+v, want exactly one", d.Ranges)
	}
	got := specDoc[d.Ranges[0].Start:d.Ranges[0].End]
	if !strings.HasPrefix(got, "# The Relevant Part") {
		t.Errorf("the range does not start at the named section: %q", got)
	}
	if !strings.Contains(got, "the part that matters") {
		t.Errorf("the range omits the section's body: %q", got)
	}
	if strings.Contains(got, "# Appendix") {
		t.Errorf("the range spills into the next section: %q", got)
	}
	// The document's NAME is still its base name, so an --authority-hash pin refers to the same
	// thing it always did.
	if d.Name != "spec.md" {
		t.Errorf("name = %q, want the file base name", d.Name)
	}
}

// TestAuthorityRef_ExplicitByteRange: the form the budget refusal prints, pasted back verbatim.
func TestAuthorityRef_ExplicitByteRange(t *testing.T) {
	p := writeSpec(t)
	docs, err := parseAuthority([]string{p + ":10-30"}, nil, "")
	if err != nil {
		t.Fatalf("parseAuthority: %v", err)
	}
	d := docs[0]
	if d.Completeness != review.CompletenessRanges || len(d.Ranges) != 1 {
		t.Fatalf("declaration = %+v", d)
	}
	if d.Ranges[0].Start != 10 || d.Ranges[0].End != 30 {
		t.Errorf("range = %+v, want 10-30", d.Ranges[0])
	}
	if d.Path != p {
		t.Errorf("path = %q, want the selector stripped: %q", d.Path, p)
	}
}

// TestAuthorityRef_AWindowsPathKeepsItsDriveColon. The range syntax uses ':', and so does a drive
// letter — the parse must anchor on the digits at the END or it eats `C:\...` on the one platform
// this project explicitly supports but does not gate.
func TestAuthorityRef_AWindowsPathKeepsItsDriveColon(t *testing.T) {
	ref, err := parseAuthorityRef(`C:\specs\design.md`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ref.Path != `C:\specs\design.md` || ref.HasRange {
		t.Errorf("a drive letter was parsed as a range: %+v", ref)
	}
	ref, err = parseAuthorityRef(`C:\specs\design.md:100-200`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ref.Path != `C:\specs\design.md` || ref.Start != 100 || ref.End != 200 {
		t.Errorf("a Windows path with a range parsed wrong: %+v", ref)
	}
}

// TestAuthorityRef_NoSelectorIsUnchanged: the ordinary whole-document case must be byte-identical to
// what it was, or this convenience has changed the meaning of every existing invocation.
func TestAuthorityRef_NoSelectorIsUnchanged(t *testing.T) {
	p := writeSpec(t)
	docs, err := parseAuthority([]string{p}, nil, "")
	if err != nil {
		t.Fatalf("parseAuthority: %v", err)
	}
	d := docs[0]
	if d.Completeness != review.CompletenessRequireFull {
		t.Errorf("completeness = %q, want %q — a whole document is still whole", d.Completeness, review.CompletenessRequireFull)
	}
	if len(d.Ranges) != 0 {
		t.Errorf("ranges = %+v, want none", d.Ranges)
	}
}

// TestAuthorityRef_AnUnknownSectionNamesTheRealOnes. A mistyped heading should be one correction, not
// a guessing game — the refusal lists what the document actually has.
func TestAuthorityRef_AnUnknownSectionNamesTheRealOnes(t *testing.T) {
	p := writeSpec(t)
	_, err := parseAuthority([]string{p + "#Relevant"}, nil, "")
	if err == nil {
		t.Fatal("a section that does not exist must be refused")
	}
	msg := err.Error()
	for _, want := range []string{"Preamble", "The Relevant Part", "Appendix"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name the section %q: %v", want, err)
		}
	}
}

// TestAuthorityRef_AnAmbiguousSectionIsRefused. Two sections with one heading is exactly where a
// first-match would quietly choose which half of a document the review is judged against.
func TestAuthorityRef_AnAmbiguousSectionIsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dup.md")
	if err := os.WriteFile(p, []byte("# Notes\na\n# Notes\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := parseAuthority([]string{p + "#Notes"}, nil, "")
	if err == nil {
		t.Fatal("an ambiguous section name must be refused, not resolved to the first match")
	}
	if !strings.Contains(err.Error(), "appears 2 times") {
		t.Errorf("the refusal does not say why it is ambiguous: %v", err)
	}
}

// TestAuthorityRef_MalformedSelectors are refused at the flag boundary, where they cost nothing.
func TestAuthorityRef_MalformedSelectors(t *testing.T) {
	cases := map[string]string{
		"empty section":   "spec.md#",
		"empty path":      "#Heading",
		"inverted range":  "spec.md:300-100",
		"degenerate span": "spec.md:100-100",
	}
	for name, raw := range cases {
		if _, err := parseAuthorityRef(raw); err == nil {
			t.Errorf("%s: %q was accepted", name, raw)
		}
	}
}
