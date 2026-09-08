package authority

import (
	"strings"
	"testing"
)

const outlineDoc = `# Alpha
one
# Beta
two
two
# Gamma
three
`

// TestOutline_RangesSelectExactlyTheSection is the property the whole remedy depends on: the numbers
// in the message must be usable verbatim. If a range is off by a byte, the user pastes it, gets the
// wrong text embedded, and the run is judged against something nobody intended — which is worse than
// the refusal that sent them there.
func TestOutline_RangesSelectExactlyTheSection(t *testing.T) {
	secs := outline(outlineDoc)
	if len(secs) != 3 {
		t.Fatalf("found %d sections, want 3: %+v", len(secs), secs)
	}
	want := []string{"Alpha", "Beta", "Gamma"}
	for i, s := range secs {
		if s.Title != want[i] {
			t.Errorf("section %d title = %q, want %q", i, s.Title, want[i])
		}
		// THE ROUND TRIP: slice the document with the reported range and the section comes back.
		got := outlineDoc[s.Start:s.End]
		if !strings.HasPrefix(got, "# "+want[i]) {
			t.Errorf("range %d..%d does not start at its own heading; got %q", s.Start, s.End, got)
		}
		if s.Bytes() != len(got) {
			t.Errorf("Bytes() = %d but the range spans %d", s.Bytes(), len(got))
		}
	}
	// The sections must PARTITION the document from the first heading on — a gap would mean the
	// offered ranges silently omit text.
	if secs[0].End != secs[1].Start || secs[1].End != secs[2].Start {
		t.Errorf("sections do not abut: %+v", secs)
	}
	if secs[2].End != len(outlineDoc) {
		t.Errorf("last section ends at %d, want the end of the document (%d)", secs[2].End, len(outlineDoc))
	}
}

// TestOutline_SectionsAtTheShallowestLevelPresent: a document whose headings all start at `##` is
// sectioned by its `##`s. "Top level" means the level its author used, not literally `#`.
func TestOutline_SectionsAtTheShallowestLevelPresent(t *testing.T) {
	doc := "## One\nx\n### One.a\ny\n## Two\nz\n"
	secs := outline(doc)
	if len(secs) != 2 {
		t.Fatalf("found %d sections, want 2 (the ##s): %+v", len(secs), secs)
	}
	if secs[0].Title != "One" || secs[1].Title != "Two" {
		t.Errorf("titles = %q, %q", secs[0].Title, secs[1].Title)
	}
	// The deeper heading belongs to its parent's span rather than starting one of its own.
	if !strings.Contains(doc[secs[0].Start:secs[0].End], "### One.a") {
		t.Error("a deeper heading must stay inside its parent section")
	}
}

// TestOutline_NoHeadingsMeansNoOutline. Inventing structure would defeat the point: the value of
// these numbers is that they are real.
func TestOutline_NoHeadingsMeansNoOutline(t *testing.T) {
	for _, doc := range []string{
		"just prose\nand more prose\n",
		"#nospace is not a heading\n",
		"    # indented four spaces is a code block\n",
		"####### seven hashes is not a heading\n",
		"",
	} {
		if secs := outline(doc); len(secs) != 0 {
			t.Errorf("outline(%q) = %+v, want none", doc, secs)
		}
	}
}
