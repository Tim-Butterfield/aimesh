package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestValidate_ShapeRules pins the declaration contract: exactly one source, a unique
// non-empty name, coherent completeness, and a checkable hash. Each of these is a
// fail-closed refusal, never an accepted-and-ignored field.
func TestValidate_ShapeRules(t *testing.T) {
	cases := []struct {
		name       string
		doc        review.AuthorityDoc
		wantReason string
	}{
		{"no name", review.AuthorityDoc{Path: "/x/spec.md"}, ReasonDocInvalid},
		{"both sources", review.AuthorityDoc{Name: "spec", Path: "/x/spec.md", Content: "hi"}, ReasonDocInvalid},
		{"neither source", review.AuthorityDoc{Name: "spec"}, ReasonDocInvalid},
		{"unknown completeness", review.AuthorityDoc{Name: "spec", Path: "/x", Completeness: "partial"}, ReasonDocInvalid},
		{"requireFull with ranges", review.AuthorityDoc{Name: "spec", Path: "/x",
			Completeness: review.CompletenessRequireFull, Ranges: []review.AuthorityRange{{Start: 0, End: 1}}}, ReasonDocInvalid},
		{"ranges without ranges", review.AuthorityDoc{Name: "spec", Path: "/x",
			Completeness: review.CompletenessRanges}, ReasonDocInvalid},
		{"inverted range", review.AuthorityDoc{Name: "spec", Path: "/x",
			Completeness: review.CompletenessRanges, Ranges: []review.AuthorityRange{{Start: 5, End: 5}}}, ReasonDocInvalid},
		{"overlapping ranges", review.AuthorityDoc{Name: "spec", Path: "/x",
			Completeness: review.CompletenessRanges, Ranges: []review.AuthorityRange{{Start: 0, End: 10}, {Start: 5, End: 20}}}, ReasonDocInvalid},
		{"bad hash", review.AuthorityDoc{Name: "spec", Path: "/x", ExpectedHash: "sha256:nope"}, ReasonDocInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate([]review.AuthorityDoc{tc.doc}, review.ModeReport)
			if err == nil {
				t.Fatalf("expected a refusal for %+v", tc.doc)
			}
			if got := fault.ReasonOf(err); got != tc.wantReason {
				t.Errorf("reasonCode = %q, want %q (%v)", got, tc.wantReason, err)
			}
		})
	}
}

func TestValidate_DuplicateNamesAndCap(t *testing.T) {
	dup := []review.AuthorityDoc{{Name: "spec", Path: "/a"}, {Name: "SPEC", Path: "/b"}}
	if got := fault.ReasonOf(Validate(dup, review.ModeReport)); got != ReasonDocInvalid {
		t.Errorf("duplicate names reasonCode = %q, want %q", got, ReasonDocInvalid)
	}
	many := make([]review.AuthorityDoc, MaxDocs+1)
	for i := range many {
		many[i] = review.AuthorityDoc{Name: string(rune('a'+i)) + "-doc", Path: "/x"}
	}
	if got := fault.ReasonOf(Validate(many, review.ModeReport)); got != ReasonTooManyDocs {
		t.Errorf("over-cap reasonCode = %q, want %q", got, ReasonTooManyDocs)
	}
}

// TestValidate_InlineIsReportOnly pins the PROVENANCE SPLIT at its cheapest enforcement
// point: inline caller-supplied intent may never steer a run that can write.
func TestValidate_InlineIsReportOnly(t *testing.T) {
	doc := []review.AuthorityDoc{{Name: "inline-spec", Content: "the intent"}}
	if err := Validate(doc, review.ModeReport); err != nil {
		t.Fatalf("inline authority must be allowed in report mode: %v", err)
	}
	for _, m := range []review.Mode{review.ModePatch, review.ModeApply} {
		err := Validate(doc, m)
		if got := fault.ReasonOf(err); got != ReasonInlineModeInvalid {
			t.Errorf("mode %s: reasonCode = %q, want %q", m, got, ReasonInlineModeInvalid)
		}
		if fault.CodeOf(err) != fault.Config {
			t.Errorf("mode %s: exit code = %d, want %d", m, fault.CodeOf(err), fault.Config)
		}
	}
}

// TestResolve_PathDocAndManifest is the happy path: a root-scoped path document with a
// matching hash pin is embedded whole, and the manifest describes exactly that.
func TestResolve_PathDocAndManifest(t *testing.T) {
	ws := t.TempDir()
	body := "# Spec\n\nThe system MUST fail closed.\n"
	p := writeFile(t, ws, "docs/spec.md", body)

	set, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport, Docs: []review.AuthorityDoc{{
		Name: "spec", Path: p, MediaType: "text/markdown", ExpectedHash: "sha256:" + sha256Of(body),
	}}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if set.Empty() {
		t.Fatal("set is empty")
	}
	m := set.Manifest()
	if len(m) != 1 {
		t.Fatalf("manifest entries = %d, want 1", len(m))
	}
	e := m[0]
	if e.Name != "spec" || e.Source != review.AuthoritySourcePath || e.MediaType != "text/markdown" {
		t.Errorf("manifest identity wrong: %+v", e)
	}
	if e.FullHash != "sha256:"+sha256Of(body) {
		t.Errorf("fullHash = %q", e.FullHash)
	}
	if e.EmbeddedHash != e.FullHash {
		t.Errorf("a complete inclusion must have embeddedHash == fullHash, got %q vs %q", e.EmbeddedHash, e.FullHash)
	}
	if !e.Complete || e.BytesEmbedded != len(body) || e.BytesTotal != len(body) {
		t.Errorf("byte accounting wrong: %+v (body is %d bytes)", e, len(body))
	}
	if !strings.Contains(set.ReviewerBlock(), "The system MUST fail closed.") {
		t.Error("the document text did not reach the rendered block")
	}
	if !strings.Contains(set.ReviewerBlock(), Header) {
		t.Error("the rendered block is missing the AUTHORITY CONTEXT header")
	}
}

// TestResolve_HashMismatchHalts pins the pin: a document whose content changed since the
// caller took the hash halts rather than being judged against silently.
func TestResolve_HashMismatchHalts(t *testing.T) {
	ws := t.TempDir()
	p := writeFile(t, ws, "spec.md", "current content\n")
	_, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport, Docs: []review.AuthorityDoc{{
		Name: "spec", Path: p, ExpectedHash: sha256Of("what the caller last saw\n"),
	}}})
	if err == nil {
		t.Fatal("a hash mismatch must refuse")
	}
	if got := fault.ReasonOf(err); got != ReasonHashMismatch {
		t.Errorf("reasonCode = %q, want %q", got, ReasonHashMismatch)
	}
	if !strings.Contains(err.Error(), "spec") {
		t.Errorf("the refusal must name the document: %v", err)
	}
}

// TestResolve_ALargeDocumentIsCarriedWhole is the no-silent-truncation rule, in its surviving
// form. There used to be a byte budget here and an oversized document was refused; the budget is
// gone, because how much context a model can take is the model's business and a real
// specification (this repo's own docs/mcp.md is 105 KiB) cleared the old 64 KiB ceiling easily.
//
// What must NOT change is that nothing is quietly shortened: the caller declared the document, so
// the document is what the seats get, byte for byte.
func TestResolve_ALargeDocumentIsCarriedWhole(t *testing.T) {
	ws := t.TempDir()
	const size = (64 << 10) + 5000 // comfortably past the ceiling that used to refuse
	p := writeFile(t, ws, "huge.md", strings.Repeat("A", size))
	set, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport,
		Docs: []review.AuthorityDoc{{Name: "huge", Path: p}}})
	if err != nil {
		t.Fatalf("a large document must be carried, not refused: %v", err)
	}
	man := set.Manifest()
	if len(man) != 1 {
		t.Fatalf("manifest has %d entries, want 1", len(man))
	}
	if man[0].BytesEmbedded != size {
		t.Errorf("embedded %d bytes of a %d-byte document — something truncated it", man[0].BytesEmbedded, size)
	}
	if !man[0].Complete {
		t.Error("the document is marked incomplete despite being embedded whole")
	}
}

// TestResolve_ManyLargeDocumentsAreCarried is the same rule across the whole set: there is no
// cross-document total either. MaxDocs still bounds the COUNT — see the next test.
func TestResolve_ManyLargeDocumentsAreCarried(t *testing.T) {
	ws := t.TempDir()
	var docs []review.AuthorityDoc
	for i := range 4 {
		p := writeFile(t, ws, filepath.Join("d", string(rune('a'+i))+".md"), strings.Repeat("B", 60<<10))
		docs = append(docs, review.AuthorityDoc{Name: string(rune('a' + i)), Path: p})
	}
	set, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport, Docs: docs})
	if err != nil {
		t.Fatalf("240 KiB across 4 documents must be carried: %v", err)
	}
	if got := len(set.All()); got != 4 {
		t.Errorf("resolved %d documents, want 4", got)
	}
}

// TestResolve_TheCountCapSurvives. Size stopped being this layer's business; SHAPE did not.
func TestResolve_TheCountCapSurvives(t *testing.T) {
	ws := t.TempDir()
	var docs []review.AuthorityDoc
	for i := 0; i <= MaxDocs; i++ {
		p := writeFile(t, ws, filepath.Join("d", string(rune('a'+i))+".md"), "tiny")
		docs = append(docs, review.AuthorityDoc{Name: string(rune('a' + i)), Path: p})
	}
	_, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport, Docs: docs})
	if got := fault.ReasonOf(err); got != ReasonTooManyDocs {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonTooManyDocs, err)
	}
}

// TestResolve_RangesAreDeclaredNotSilent pins partial inclusion: it happens only through
// caller-declared ranges, the manifest says `complete: false` with a DIFFERENT embedded
// hash, and the prompt marks the omitted spans explicitly.
func TestResolve_RangesAreDeclaredNotSilent(t *testing.T) {
	ws := t.TempDir()
	body := "AAAAABBBBBCCCCC"
	p := writeFile(t, ws, "spec.md", body)
	set, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport, Docs: []review.AuthorityDoc{{
		Name: "spec", Path: p, Completeness: review.CompletenessRanges,
		Ranges: []review.AuthorityRange{{Start: 0, End: 5}, {Start: 10, End: 15}},
	}}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	e := set.Manifest()[0]
	if e.Complete {
		t.Error("a ranged inclusion must be marked incomplete")
	}
	if e.BytesEmbedded != 10 || e.BytesTotal != 15 {
		t.Errorf("byte accounting = %d/%d, want 10/15", e.BytesEmbedded, e.BytesTotal)
	}
	if e.EmbeddedHash == e.FullHash {
		t.Error("an incomplete inclusion must hash differently from the full document")
	}
	if len(e.Ranges) != 2 {
		t.Errorf("manifest must record the declared ranges, got %+v", e.Ranges)
	}
	block := set.ReviewerBlock()
	if !strings.Contains(block, "omitted bytes 5-10") {
		t.Errorf("the omitted span must be marked in the prompt:\n%s", block)
	}
	if !strings.Contains(block, "complete=FALSE") {
		t.Errorf("the header must say the inclusion is incomplete:\n%s", block)
	}
	if strings.Contains(block, "BBBBB") {
		t.Errorf("bytes outside the declared ranges leaked into the prompt:\n%s", block)
	}
}

// TestResolve_RangeOutOfBoundsRefused: a range past the end is a refusal, not a clamp —
// clamping would be a silent truncation of the caller's stated intent.
func TestResolve_RangeOutOfBoundsRefused(t *testing.T) {
	ws := t.TempDir()
	p := writeFile(t, ws, "spec.md", "short")
	_, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport, Docs: []review.AuthorityDoc{{
		Name: "spec", Path: p, Completeness: review.CompletenessRanges,
		Ranges: []review.AuthorityRange{{Start: 0, End: 500}},
	}}})
	if got := fault.ReasonOf(err); got != ReasonRangeOutOfBounds {
		t.Errorf("reasonCode = %q, want %q (err=%v)", got, ReasonRangeOutOfBounds, err)
	}
}

// TestResolve_DenylistedPathRefused pins the read denylist through the authority door: a
// secret can never be admitted by calling it "authority", even though naming a path is
// itself the consent that makes it a root.
func TestResolve_DenylistedPathRefused(t *testing.T) {
	ws := t.TempDir()
	for _, name := range []string{".env", "id_rsa", "server.key"} {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, ws, name, "SECRET=super-secret-value\n")
			_, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport,
				Docs: []review.AuthorityDoc{{Name: "sneaky", Path: p}}})
			if err == nil {
				t.Fatalf("%s must never be readable as authority", name)
			}
			if got := fault.ReasonOf(err); got != string(scope.ReasonReadDenied) {
				t.Errorf("reasonCode = %q, want %q", got, scope.ReasonReadDenied)
			}
			if fault.CodeOf(err) != fault.Containment {
				t.Errorf("exit code = %d, want %d (containment)", fault.CodeOf(err), fault.Containment)
			}
		})
	}
}

// TestResolve_OutsideWorkspaceIsAllowedWhenNamed pins the consent model: a design doc kept
// OUTSIDE the workspace is the normal case, and naming it on a surface is the consent that
// makes it readable. The denylist still applies (previous test).
func TestResolve_OutsideWorkspaceIsAllowedWhenNamed(t *testing.T) {
	ws := t.TempDir()
	elsewhere := t.TempDir()
	p := writeFile(t, elsewhere, "design.md", "the design\n")
	set, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport,
		Docs: []review.AuthorityDoc{{Name: "design", Path: p}}})
	if err != nil {
		t.Fatalf("a human-named authority path outside the workspace must resolve: %v", err)
	}
	if !strings.Contains(set.ReviewerBlock(), "the design") {
		t.Error("the document did not reach the block")
	}
}

// TestProvenanceSplit_AdjudicatorBlockExcludesInline pins the split at the rendering seam:
// the analysis lanes see everything; the host adjudicator sees PATH documents only.
func TestProvenanceSplit_AdjudicatorBlockExcludesInline(t *testing.T) {
	ws := t.TempDir()
	p := writeFile(t, ws, "spec.md", "PATH-AUTHORITY-TEXT\n")
	set, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport, Docs: []review.AuthorityDoc{
		{Name: "spec", Path: p},
		{Name: "inline", Content: "INLINE-AUTHORITY-TEXT\n"},
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rev := set.ReviewerBlock()
	if !strings.Contains(rev, "PATH-AUTHORITY-TEXT") || !strings.Contains(rev, "INLINE-AUTHORITY-TEXT") {
		t.Errorf("the analysis lanes must see both documents:\n%s", rev)
	}
	adj := set.AdjudicatorBlock()
	if !strings.Contains(adj, "PATH-AUTHORITY-TEXT") {
		t.Errorf("path authority must reach the adjudicator:\n%s", adj)
	}
	if strings.Contains(adj, "INLINE-AUTHORITY-TEXT") {
		t.Errorf("inline authority must NEVER reach the adjudicator:\n%s", adj)
	}
}

// TestRender_EmptyIsEmpty pins the blast radius: with no authority the rendered block is the
// empty string, so a prompt is byte-identical to what it was before this feature existed.
func TestRender_EmptyIsEmpty(t *testing.T) {
	var zero Set
	if zero.ReviewerBlock() != "" || zero.AdjudicatorBlock() != "" {
		t.Error("the zero Set must render nothing")
	}
	if !zero.Empty() || len(zero.Manifest()) != 0 {
		t.Error("the zero Set must be empty")
	}
	if zero.ApplyRefusal("main.go", nil) != "" {
		t.Error("the write-path rule must be inert without authority")
	}
}

// TestRender_InstructionHierarchy pins the injection posture: the block always restates the
// hierarchy and the not-under-review framing, in every phase that renders it.
func TestRender_InstructionHierarchy(t *testing.T) {
	ws := t.TempDir()
	p := writeFile(t, ws, "spec.md", "x\n")
	set, _ := Resolve(Input{Workspace: ws, Mode: review.ModeReport,
		Docs: []review.AuthorityDoc{{Name: "spec", Path: p}}})
	block := set.ReviewerBlock()
	for _, want := range []string{
		Header,
		"INSTRUCTION HIERARCHY",
		"NEVER report a finding against an authority document",
		"never applied",
		"<<< AUTHORITY spec",
		">>> END AUTHORITY spec <<<",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("rendered block is missing %q:\n%s", want, block)
		}
	}
}

// TestApplyRefusal is the WRITE-PATH RULE in isolation: a finding that names an authority
// document, or a file the workspace copy never showed, cannot be applied.
func TestApplyRefusal(t *testing.T) {
	ws := t.TempDir()
	p := writeFile(t, ws, "docs/spec.md", "x\n")
	set, err := Resolve(Input{Workspace: ws, Mode: review.ModeReport,
		Docs: []review.AuthorityDoc{{Name: "spec.md", Path: p}}})
	if err != nil {
		t.Fatal(err)
	}
	// `docs/spec.md` is deliberately in `shown`: an authority document kept INSIDE the repo is
	// collected into the containment copy like any other file, so the rule must recognize it by
	// its workspace-relative path or it would be an editable target.
	shown := map[string]bool{"main.go": true, "docs/spec.md": true}
	cases := []struct{ file, want string }{
		{"main.go", ""},
		{"spec.md", RefusalAuthorityOnly},
		{"docs/spec.md", RefusalAuthorityOnly},
		{p, RefusalAuthorityOnly},
		{"other.go", RefusalNoWorkspaceEvidence},
		{"", ""},
	}
	for _, tc := range cases {
		if got := set.ApplyRefusal(tc.file, shown); got != tc.want {
			t.Errorf("ApplyRefusal(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}
