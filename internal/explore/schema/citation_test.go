package schema

import (
	"strconv"
	"strings"
	"testing"
)

// envAt builds a primary envelope at panel position `order` carrying `claims` claims.
func envAt(order, claims int) Envelope {
	c := make([]any, claims)
	for i := range c {
		c[i] = "claim"
	}
	return Envelope{Order: order, IdentityStatus: IdentityVerified, Response: map[string]any{"claims": c}}
}

// TestCitationIndex_Grammar pins the FROZEN citation grammar and the k-membership rule. The cases that
// matter are the ones a plausible-looking ref fails on: padding, a claim index past the end, and — the
// one a range check would get wrong — an alias whose k belongs to an envelope that is not in the PRIMARY
// panel (a dropped or weak-identity explorer still consumes a panel Order, so primary k values are not
// contiguous).
func TestCitationIndex_Grammar(t *testing.T) {
	// Panel positions 0 and 2 are primary; position 1 was a weak/dropped explorer.
	ix := NewCitationIndex([]Envelope{envAt(0, 2), envAt(2, 0)})

	valid := []string{"envelope#0", "envelope#2", "envelope#0/claims/0", "envelope#0/claims/1"}
	for _, ref := range valid {
		if !ix.Valid(ref) {
			t.Errorf("%q must be a VALID citation for this panel", ref)
		}
	}
	// Surrounding whitespace is trimmed before matching — a model padding a token has not cited
	// something different.
	if !ix.Valid("  envelope#0 ") {
		t.Error("a padded alias must still be a valid citation after trimming")
	}
	invalid := map[string]string{
		"envelope#1":          "position 1 is not in the primary panel (weak/dropped) — membership is a set, not a range",
		"envelope#3":          "no such panel position",
		"envelope#00":         "leading-zero padding must not name slot 0 a second way",
		"envelope#0/claims/2": "claim index is out of bounds for a 2-claim response",
		"envelope#2/claims/0": "that response has no claims array at all",
		"envelope#-1":         "negative index",
		"envelope#":           "no index",
		"s1":                  "free text is not a citation",
		"":                    "empty",
		"see envelope#0":      "the grammar is ANCHORED — a ref embedded in prose is not a citation",
		"envelope#0/claims":   "an incomplete narrowing",
		"envelope#0/notes/0":  "an unknown narrowing axis",
		"ENVELOPE#0":          "the alias is case-sensitive",
	}
	for ref, why := range invalid {
		if ix.Valid(ref) {
			t.Errorf("%q must be INVALID: %s", ref, why)
		}
	}

	// The vocabulary the prompt shows is exactly the vocabulary the validator enforces.
	got := ix.Aliases()
	if len(got) != 2 || got[0] != "envelope#0" || got[1] != "envelope#2" {
		t.Errorf("Aliases() = %v, want [envelope#0 envelope#2]", got)
	}
}

// TestApplyCitations_KeepsValidDropsUnknown is the core host-authoritative rule: a valid alias survives,
// an unknown/malformed one is DROPPED (never rewritten into something that resolves), and a finding left
// with no surviving citation is labeled uncited — while the finding itself is retained.
func TestApplyCitations_KeepsValidDropsUnknown(t *testing.T) {
	primary := []Envelope{envAt(0, 1), envAt(1, 1)}
	out := CollatorOutput{
		SynthesisSummary: "s",
		Findings: []Finding{
			{Statement: "cited", Sources: []string{"envelope#0", "envelope#1/claims/0"}},
			{Statement: "partly cited", Sources: []string{"envelope#0", "envelope#9", "s1"}},
			{Statement: "uncited", Sources: []string{"s1", "explorer A said so"}},
			{Statement: "no sources at all"},
		},
	}
	rep := ApplyCitations(&out, primary)

	if len(out.Findings) != 4 {
		t.Fatalf("a citation defect must never drop a finding: got %d findings, want 4", len(out.Findings))
	}
	if got := out.Findings[0].Sources; len(got) != 2 {
		t.Errorf("valid citations must be kept verbatim, got %v", got)
	}
	if got := out.Findings[1].Sources; len(got) != 1 || got[0] != "envelope#0" {
		t.Errorf("unknown refs must be dropped and the valid one kept, got %v", got)
	}
	if out.Findings[1].Uncited {
		t.Error("a finding with ONE surviving citation is cited, not uncited")
	}
	if !out.Findings[2].Uncited || len(out.Findings[2].Sources) != 0 {
		t.Errorf("a finding whose every ref was unknown must be uncited with no sources, got %+v", out.Findings[2])
	}
	if !out.Findings[3].Uncited {
		t.Error("a finding with no sources at all must be uncited")
	}
	// No dropped ref may survive anywhere: the pass metric is zero invalid refs left in Sources.
	for _, f := range out.Findings {
		for _, s := range f.Sources {
			if !NewCitationIndex(primary).Valid(s) {
				t.Errorf("invalid ref %q survived in Sources", s)
			}
		}
	}
	// Only `envelope#9` is DROPPED: it is a ref to this run that the host checked and rejected. The three
	// free-text sources name things outside the run entirely, and those are now retained as unverified
	// references rather than discarded — a pointer the host never looked at is a different fact from one
	// it looked at and refused. See TestApplyCitations_ExternalReferencesAreLabeledNotDropped.
	want := CitationReport{Findings: 4, Cited: 2, Uncited: 2, RefsKept: 3, RefsDropped: 1, RefsUnverified: 3}
	if rep != want {
		t.Errorf("report = %+v, want %+v", rep, want)
	}
}

// TestApplyCitations_ExternalReferencesAreLabeledNotDropped.
//
// exploremesh reads no filesystem — a deliberate posture, not a gap — so it cannot tell whether
// `src/foo.rs` exists. The failure that posture used to permit was silent: the pointer was discarded,
// and a reader saw a finding with no indication that it had ever named anything. Three statements have
// to stay distinguishable, and each needs its own place:
//
//   - SOURCED — a ref that resolved to a response this panel produced (Sources).
//   - CHECKED AND REJECTED — a ref addressed to this run that names nothing in it (dropped; the host
//     knows every alias it made, so this is a definite no).
//   - NOT LOOKED AT — a reference to something outside the run (UnverifiedReferences).
//
// Collapsing the third into either of the others is the whole bug: into the first it would read as
// provenance, into the second it would claim a check that never happened.
func TestApplyCitations_ExternalReferencesAreLabeledNotDropped(t *testing.T) {
	primary := []Envelope{envAt(0, 1)}
	out := CollatorOutput{
		SynthesisSummary: "s",
		Findings: []Finding{
			{Statement: "points outside", Sources: []string{"envelope#0", "src/foo.rs", "https://example.test/spec"}},
			{Statement: "only points outside", Sources: []string{"RFC 9110 §8.3"}},
		},
	}
	rep := ApplyCitations(&out, primary)

	if got := out.Findings[0].UnverifiedReferences; len(got) != 2 || got[0] != "src/foo.rs" {
		t.Errorf("external references must be retained verbatim, got %v", got)
	}
	if got := out.Findings[0].Sources; len(got) != 1 || got[0] != "envelope#0" {
		t.Errorf("an external reference must never survive in Sources, where it would read as provenance: %v", got)
	}
	// The decisive one: an unverified pointer is not a citation, so it cannot make a finding cited.
	if !out.Findings[1].Uncited {
		t.Error("a finding whose only source is an unverified external reference is UNCITED — nothing was verified, so nothing was sourced")
	}
	if got := out.Findings[1].UnverifiedReferences; len(got) != 1 || got[0] != "RFC 9110 §8.3" {
		t.Errorf("the reference must still be reported alongside the uncited label, got %v", got)
	}
	if rep.RefsUnverified != 3 || rep.RefsDropped != 0 {
		t.Errorf("report = %+v: external refs are unverified, not dropped", rep)
	}
	if !strings.Contains(rep.String(), "UNVERIFIED") {
		t.Errorf("the one-line report must name them: %q", rep.String())
	}
}

// TestApplyCitations_RetainedReferencesAreBounded. `sources` is a model-authored array, so it is a way
// into the result. A reference is a pointer, not a payload: a model must not be able to move a
// paragraph into the artifact under a label implying the host handled it.
func TestApplyCitations_RetainedReferencesAreBounded(t *testing.T) {
	primary := []Envelope{envAt(0, 1)}
	long := strings.Repeat("x", maxUnverifiedRefBytes*2)
	srcs := []string{long, "a\nb\nc"}
	for i := 0; i < maxUnverifiedRefs+5; i++ {
		srcs = append(srcs, "ref"+strconv.Itoa(i))
	}
	out := CollatorOutput{SynthesisSummary: "s", Findings: []Finding{{Statement: "f", Sources: srcs}}}
	rep := ApplyCitations(&out, primary)

	refs := out.Findings[0].UnverifiedReferences
	if len(refs) != maxUnverifiedRefs {
		t.Errorf("retained %d reference(s), want the %d bound", len(refs), maxUnverifiedRefs)
	}
	if !strings.Contains(refs[0], "(clipped)") {
		t.Error("an over-long reference must be MARKED as clipped, or a shortened pointer reads like a complete one")
	}
	for _, r := range refs {
		if strings.ContainsAny(r, "\n\r") {
			t.Errorf("a retained reference must be one line, got %q", r)
		}
	}
	// The COUNT is not bounded: what was retained is capped, what was seen is reported in full.
	if rep.RefsUnverified != len(srcs) {
		t.Errorf("RefsUnverified = %d, want all %d — the cap limits what is carried, never what is counted", rep.RefsUnverified, len(srcs))
	}
}

// TestApplyCitations_HostOverwritesModelClaim proves the field is HOST-owned in both directions: a
// collator cannot certify its own unsourced finding as sourced, and cannot disown a sourced one. A model
// is a witness about the world, never an authority about its own provenance.
func TestApplyCitations_HostOverwritesModelClaim(t *testing.T) {
	primary := []Envelope{envAt(0, 1)}
	out := CollatorOutput{
		SynthesisSummary: "s",
		Findings: []Finding{
			{Statement: "really cited, claims otherwise", Sources: []string{"envelope#0"}, Uncited: true},
			{Statement: "really uncited, claims otherwise", Sources: []string{"s1"}, Uncited: false},
		},
	}
	ApplyCitations(&out, primary)
	if out.Findings[0].Uncited {
		t.Error("the host must overwrite a model-supplied uncited=true on a finding that IS cited")
	}
	if !out.Findings[1].Uncited {
		t.Error("the host must overwrite a model-supplied uncited=false on a finding whose refs did not resolve")
	}
}

// TestApplyCitations_AllCited proves a fully-cited collation carries no uncited finding at all.
func TestApplyCitations_AllCited(t *testing.T) {
	primary := []Envelope{envAt(0, 1), envAt(1, 1)}
	out := CollatorOutput{
		SynthesisSummary: "s",
		Findings: []Finding{
			{Statement: "a", Sources: []string{"envelope#0"}},
			{Statement: "b", Sources: []string{"envelope#1", "envelope#0/claims/0"}},
		},
	}
	rep := ApplyCitations(&out, primary)
	for _, f := range out.Findings {
		if f.Uncited {
			t.Errorf("finding %q must not be uncited in an all-cited collation", f.Statement)
		}
	}
	if rep.Uncited != 0 || rep.Cited != 2 || rep.RefsDropped != 0 {
		t.Errorf("report = %+v, want 2 cited / 0 uncited / 0 dropped", rep)
	}
}
