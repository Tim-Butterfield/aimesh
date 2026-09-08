package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

type expectedIdentity struct {
	RequestedModel string `json:"requestedModel"`
	Evidence       string `json:"evidence"`
	ExpectedActual string `json:"expectedActual"`
	ExpectedStatus string `json:"expectedStatus"`
	FixtureKind    string `json:"fixtureKind"` // "real" | "synthetic"
}

func readExpected(t *testing.T, path string) expectedIdentity {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var e expectedIdentity
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return e
}

func TestClassifyIdentity(t *testing.T) {
	cases := []struct {
		name       string
		ev         core.IdentityEvidence
		requested  string
		actual     string
		adapter    string
		wantStatus string
		wantOK     bool
	}{
		{"envelope match → verified", core.EvidenceEnvelope, "opus", "opus", "", core.VerifVerified, true},
		{"trace match → verified", core.EvidenceTrace, "gpt-5.5", "gpt-5.5", "", core.VerifVerified, true},
		{"cli_status match → verified", core.EvidenceCLIStatus, "m", "M", "", core.VerifVerified, true}, // case-insensitive
		{"invocation_tag match → verified", core.EvidenceInvocationTag, "qwen", "qwen", "", core.VerifVerified, true},
		{"self_report match → self_reported (NOT verified)", core.EvidenceSelfReport, "opus", "opus", "", core.VerifSelfReported, true},
		{"self_report non-match → unknown (weak, uncertain — caveat not halt)", core.EvidenceSelfReport, "opus", "gpt-5", "", core.VerifUnknown, false},
		{"envelope mismatch → mismatch", core.EvidenceEnvelope, "opus", "gpt-5", "", core.VerifMismatch, false},
		{"empty actual → unknown (missing, not a mismatch)", core.EvidenceTrace, "opus", "", "", core.VerifUnknown, false},
		{"none evidence + match → unknown (no implicit trust)", core.EvidenceNone, "opus", "opus", "", core.VerifUnknown, false},
		{"unset evidence + match → unknown (no implicit trust)", core.IdentityEvidence(""), "opus", "opus", "", core.VerifUnknown, false},
		{"unknown tier + match → unknown", core.IdentityEvidence("bogus"), "opus", "opus", "", core.VerifUnknown, false},
		// claude-code adapter alias/family equivalence (envelope tier)
		{"claude alias opus vs claude-opus-4-8[1m] → verified", core.EvidenceEnvelope, "opus", "claude-opus-4-8[1m]", "claude-code", core.VerifVerified, true},
		{"claude short id vs full → verified", core.EvidenceEnvelope, "claude-opus-4-8", "claude-opus-4-8[1m]", "claude-code", core.VerifVerified, true},
		{"claude sonnet vs opus → mismatch (Class E)", core.EvidenceEnvelope, "sonnet", "claude-opus-4-8[1m]", "claude-code", core.VerifMismatch, false},
		{"claude family match only for claude-code adapter", core.EvidenceEnvelope, "opus", "claude-opus-4-8[1m]", "", core.VerifMismatch, false},
		// over-acceptance guards: the family must be a real token, preceded only by "claude"
		{"claude notopus must NOT match opus", core.EvidenceEnvelope, "notopus", "claude-opus-4-8[1m]", "claude-code", core.VerifMismatch, false},
		{"claude gpt-opus must NOT match opus", core.EvidenceEnvelope, "gpt-opus", "claude-opus-4-8[1m]", "claude-code", core.VerifMismatch, false},
		{"claude bare family matches generation-numbered real name", core.EvidenceEnvelope, "sonnet", "claude-3-5-sonnet-20241022", "claude-code", core.VerifVerified, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, ok := ClassifyIdentity(c.ev, c.requested, c.actual, c.adapter)
			if status != c.wantStatus || ok != c.wantOK {
				t.Errorf("ClassifyIdentity(%q,%q,%q,%q) = (%q,%v), want (%q,%v)", c.ev, c.requested, c.actual, c.adapter, status, ok, c.wantStatus, c.wantOK)
			}
		})
	}
}

func TestParseSelfReport(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-8":                "claude-opus-4-8",
		"  gpt-5.5  ":                    "gpt-5.5",
		"`Gemini 3.1 Pro (High)`":        "Gemini 3.1 Pro (High)",
		"model: claude-opus-4-8-medium":  "claude-opus-4-8-medium",
		"I am gpt-5-4-medium":            "gpt-5-4-medium",
		"\n\n  qwen2.5-coder:14b\nextra": "qwen2.5-coder:14b",
		"":                               "",
		"   ":                            "",
	}
	for in, want := range cases {
		if got := ParseSelfReport([]byte(in)); got != want {
			t.Errorf("ParseSelfReport(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIdentityFixtures parses each sanitized adapter fixture's stdout as a self-report
// and asserts the classification — proving the parser contract without any real CLI.
// Fixtures are synthetic (see each notes.md).
func TestIdentityFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "adapters")
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			if _, e := os.Stat(filepath.Join(p, "expected-identity.json")); e == nil {
				dirs = append(dirs, p)
			}
		}
		return nil
	})
	if len(dirs) == 0 {
		t.Fatalf("no identity fixtures found under %s", root)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(filepath.Dir(dir))+"/"+filepath.Base(dir), func(t *testing.T) {
			exp := readExpected(t, filepath.Join(dir, "expected-identity.json"))
			if exp.FixtureKind != "real" && exp.FixtureKind != "synthetic" {
				t.Errorf("fixtureKind = %q, want real|synthetic (must be explicit)", exp.FixtureKind)
			}
			// This test exercises the SELF-REPORT parser; adapter-specific evidence
			// (e.g. claude envelope) is parsed + tested in its adapter package.
			if exp.Evidence != string(core.EvidenceSelfReport) {
				return
			}
			stdout, _ := os.ReadFile(filepath.Join(dir, "stdout.txt"))
			actual := ParseSelfReport(stdout)
			if actual != exp.ExpectedActual {
				t.Errorf("parsed actual = %q, want %q", actual, exp.ExpectedActual)
			}
			status, _ := ClassifyIdentity(core.IdentityEvidence(exp.Evidence), exp.RequestedModel, actual, "")
			if status != exp.ExpectedStatus {
				t.Errorf("status = %q, want %q", status, exp.ExpectedStatus)
			}
			if status == core.VerifVerified {
				t.Error("self_report must never classify as verified")
			}
		})
	}
}

// TestCapEvidence proves the evidence-authority boundary: a produced tier is clamped to the adapter's
// declared ceiling, so a self_report/none adapter can never be classified on strong evidence and
// faked as verified — even if it returns a matching model with an inflated Evidence.
func TestCapEvidence(t *testing.T) {
	cases := []struct{ ceiling, produced, want core.IdentityEvidence }{
		{core.EvidenceSelfReport, core.EvidenceEnvelope, core.EvidenceSelfReport}, // self_report adapter cannot inflate to envelope
		{core.EvidenceNone, core.EvidenceCLIStatus, core.EvidenceNone},            // none adapter cannot inflate to cli_status
		{core.EvidenceEnvelope, core.EvidenceCLIStatus, core.EvidenceCLIStatus},   // produced below ceiling → unchanged
		{core.EvidenceSelfReport, core.EvidenceSelfReport, core.EvidenceSelfReport},
	}
	for _, c := range cases {
		if got := CapEvidence(c.ceiling, c.produced); got != c.want {
			t.Errorf("CapEvidence(%q,%q)=%q want %q", c.ceiling, c.produced, got, c.want)
		}
	}
	// The guardrail end-to-end: after capping to a self_report ceiling, a matching model can NEVER be
	// classified as verified even though the produced tier claimed a strong envelope.
	capped := CapEvidence(core.EvidenceSelfReport, core.EvidenceEnvelope)
	if st, ok := ClassifyIdentity(capped, "opus", "opus", ""); ok && st == core.VerifVerified {
		t.Errorf("a self_report-ceiling adapter must never classify verified, got %q,%v", st, ok)
	}
}

// TestParseSelfReportJSON proves the structured self-report parser: it extracts model+effort from a
// probe response (even wrapped in prose) but does NOT read an ordinary ReviewerResult (no model field)
// as a self-report — so ordinary review stays unknown with no extra spend.
func TestParseSelfReportJSON(t *testing.T) {
	sr, ok := ParseSelfReportJSON([]byte(`prose {"result":"hello","model":"Claude Opus 4.8","effort":"Medium"} tail`))
	if !ok || sr.Model != "Claude Opus 4.8" || sr.Effort != "Medium" {
		t.Fatalf("ParseSelfReportJSON = %+v,%v", sr, ok)
	}
	if _, ok := ParseSelfReportJSON([]byte(`{"schemaVersion":1,"role":"reviewer","verdict":"approve","findings":[]}`)); ok {
		t.Error("a ReviewerResult without a model field must not be read as a self-report")
	}
	if _, ok := ParseSelfReportJSON([]byte(`not json at all`)); ok {
		t.Error("non-JSON must not parse as a self-report")
	}
}

// TestParseSelfReportEnvelope proves the wrapper-aware parser: it reads BOTH the review-path wrapper
// ({reviewmeshIdentity, result}) and the bare probe object, is fence-tolerant, and does NOT read a
// bare ReviewerResult (no identity) as a self-report.
func TestParseSelfReportEnvelope(t *testing.T) {
	sr, ok := ParseSelfReportEnvelope([]byte(`{"reviewmeshIdentity":{"model":"Gemini 3.1 Pro","effort":"High","source":"self_report"},"result":{"schemaVersion":1}}`))
	if !ok || sr.Model != "Gemini 3.1 Pro" || sr.Effort != "High" {
		t.Fatalf("wrapper: %+v,%v", sr, ok)
	}
	sr, ok = ParseSelfReportEnvelope([]byte(`prose {"result":"hello","model":"Claude Opus 4.8","effort":"Medium"} tail`))
	if !ok || sr.Model != "Claude Opus 4.8" {
		t.Fatalf("bare: %+v,%v", sr, ok)
	}
	sr, ok = ParseSelfReportEnvelope([]byte("```json\n{\"reviewmeshIdentity\":{\"model\":\"X\"},\"result\":{}}\n```"))
	if !ok || sr.Model != "X" {
		t.Fatalf("fenced wrapper: %+v,%v", sr, ok)
	}
	if _, ok := ParseSelfReportEnvelope([]byte(`{"schemaVersion":1,"role":"reviewer","verdict":"approve","findings":[]}`)); ok {
		t.Error("a bare ReviewerResult (no identity) must not read as a self-report")
	}
}

// TestExtractWrappedResult proves the payload strip: a wrapper yields the inner result bytes (so the
// strict schema parser never sees reviewmeshIdentity); a bare result yields nil (fall back to stdout).
func TestExtractWrappedResult(t *testing.T) {
	if got := ExtractWrappedResult([]byte(`{"reviewmeshIdentity":{"model":"X"},"result":{"schemaVersion":1,"verdict":"approve"}}`)); string(got) != `{"schemaVersion":1,"verdict":"approve"}` {
		t.Errorf("wrapped result = %q", got)
	}
	if got := ExtractWrappedResult([]byte("```\n{\"reviewmeshIdentity\":{\"model\":\"X\"},\"result\":{\"a\":1}}\n```")); string(got) != `{"a":1}` {
		t.Errorf("fenced wrapped result = %q", got)
	}
	if got := ExtractWrappedResult([]byte(`{"schemaVersion":1,"verdict":"approve"}`)); got != nil {
		t.Errorf("a bare result must not be treated as a wrapper, got %q", got)
	}
	// A SECOND top-level object must be rejected (held to the same single-object rule as a bare result).
	if got := ExtractWrappedResult([]byte(`{"reviewmeshIdentity":{"model":"X"},"result":{"a":1}} {"extra":true}`)); got != nil {
		t.Errorf("a second top-level object must be rejected, got %q", got)
	}
}

// TestAgySelfReportMatching proves the EXACT (non-fuzzy) Agy display-name+effort normalization: a
// display+effort self-report matches (self_reported), an effort synonym matches, but a slug non-match,
// a version drift, and a missing effort are all unknown (NOT self_reported, NOT a mismatch halt).
func TestAgySelfReportMatching(t *testing.T) {
	cases := []struct {
		name, requested string
		sr              SelfReport
		want            string
	}{
		{"display+effort match", "Gemini 3.1 Pro (High)", SelfReport{Model: "Gemini 3.1 Pro", Effort: "High"}, core.VerifSelfReported},
		{"effort synonym High Thinking == High", "Gemini 3.1 Pro (High)", SelfReport{Model: "Gemini 3.1 Pro", Effort: "High Thinking"}, core.VerifSelfReported},
		{"'Thinking' inside the model name is NOT dropped (non-match)", "Gemini 3.1 Pro (High)", SelfReport{Model: "Gemini Thinking Pro", Effort: "High"}, core.VerifUnknown},
		{"slug non-match", "gemini-3-pro", SelfReport{Model: "Gemini 3.5 Flash", Effort: "Medium"}, core.VerifUnknown},
		{"version drift non-match", "Gemini 3.1 Pro (High)", SelfReport{Model: "Gemini 3.5 Pro", Effort: "High"}, core.VerifUnknown},
		{"missing effort non-match", "Gemini 3.1 Pro (High)", SelfReport{Model: "Gemini 3.1 Pro", Effort: ""}, core.VerifUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actual := AgyModelString(c.sr)
			st, _ := ClassifyIdentity(core.EvidenceSelfReport, c.requested, actual, "agy-cli")
			if st != c.want {
				t.Errorf("ClassifyIdentity(agy, req=%q, actual=%q) = %q, want %q", c.requested, actual, st, c.want)
			}
		})
	}
}
