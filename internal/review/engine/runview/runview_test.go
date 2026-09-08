package runview

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

func sampleOutcome() review.RunOutcome {
	halt := review.HaltClass("E")
	return review.RunOutcome{
		Status: "stable", Mode: review.ModeReport, RunID: "20260727T120000-0001", RunDir: "/runs/20260727T120000-0001",
		Findings: []review.Finding{
			{ID: "f1", Title: "missing bound", Kind: review.KindFail, Severity: review.SeverityHigh, File: "a.go", Location: "12", Source: "reviewer"},
			{ID: "f2", Title: "unclear name", Kind: review.KindRisk, Severity: review.SeverityLow, File: "b.go", Source: "author_self_review"},
		},
		Decisions: []review.Decision{
			{FindingID: "f1", Valid: true, State: review.StateReportedValid},
			{FindingID: "f2", Valid: false, State: review.StateReportedInvalid, SeverityAdjusted: review.SeverityInfo},
		},
		IdentityCaveats: []review.IdentityCaveat{
			{Role: "reviewer", Adapter: "x-cli", RequestedModel: "m", Status: review.VerifUnknown},
		},
		Halt: &halt,
	}
}

// TestBuild_Success covers the shape a machine consumer integrates against.
func TestBuild_Success(t *testing.T) {
	o := sampleOutcome()
	o.Halt = nil
	v := Build(Input{Outcome: o, RequestedMode: review.ModeReport})

	if v.SchemaVersion != SchemaVersion || v.Status != "stable" || v.Mode != "report" {
		t.Fatalf("header wrong: %+v", v)
	}
	if v.RequestedMode != "" {
		t.Errorf("requestedMode = %q; it must appear only when the effective mode DIFFERS", v.RequestedMode)
	}
	if v.HaltRecord != nil {
		t.Error("no error ⇒ no haltRecord")
	}
	if len(v.Findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(v.Findings))
	}
	if v.Findings[0].Disposition != string(review.StateReportedValid) {
		t.Errorf("disposition = %q", v.Findings[0].Disposition)
	}
	if got := v.Findings[0].Sources; len(got) != 1 || got[0] != "reviewer" {
		t.Errorf("sources = %v, want [reviewer]", got)
	}
	// A host severity adjustment is authoritative — reporting the reviewer's original
	// would misstate the decision.
	if v.Findings[1].Severity != string(review.SeverityInfo) {
		t.Errorf("severity = %q, want the host-adjusted %q", v.Findings[1].Severity, review.SeverityInfo)
	}
	if v.Counts.Findings != 2 || v.Counts.IdentityCaveats != 1 {
		t.Errorf("counts = %+v", v.Counts)
	}
	if v.Counts.BySeverity["high"] != 1 || v.Counts.BySeverity["info"] != 1 {
		t.Errorf("bySeverity = %+v", v.Counts.BySeverity)
	}
	if v.Counts.ByDisposition["reported_valid"] != 1 || v.Counts.ByDisposition["reported_invalid"] != 1 {
		t.Errorf("byDisposition = %+v", v.Counts.ByDisposition)
	}
	if len(v.IdentityCaveats) != 1 || v.IdentityCaveats[0].Status != review.VerifUnknown {
		t.Errorf("identityCaveats = %+v", v.IdentityCaveats)
	}
}

// TestBuild_Halted covers the halted projection, including the sanitization rule: the
// captured adapter output never rides the projection (it is printed to stdout and may be
// logged), only the identification of the failing lane.
func TestBuild_Halted(t *testing.T) {
	o := sampleOutcome()
	o.Status = "halted"
	o.Failure = &review.LaneFailure{
		Role: "reviewer", Adapter: "x-cli", Model: "m", ExitCode: 1,
		HaltClass: "E", ReasonCode: "model_identity_mismatch", Signal: "login_required",
		StderrExcerpt: "API_KEY=sk-secret-value", StdoutExcerpt: "secret-too",
	}
	v := Build(Input{
		Outcome: o, Err: errors.New("model identity mismatch: requested \"a\", actual \"b\""),
		ExitCode: 5, ReasonCode: "model_identity_mismatch", Signal: "login_required",
	})
	if v.Status != "halted" || v.HaltRecord == nil {
		t.Fatalf("view = %+v", v)
	}
	h := v.HaltRecord
	if h.HaltClass != "E" || h.ReasonCode != "model_identity_mismatch" || h.ExitCode != 5 || h.Signal != "login_required" {
		t.Errorf("haltRecord = %+v", h)
	}
	if h.Failure == nil || h.Failure.Adapter != "x-cli" {
		t.Fatalf("haltRecord.failure = %+v", h.Failure)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-secret-value") || strings.Contains(string(b), "secret-too") {
		t.Errorf("captured adapter output must never ride the projection:\n%s", b)
	}
}

// TestBuild_DegradedMode pins that a downgrade is visible: requestedMode appears exactly
// when the effective mode differs, so a caller cannot miss a silent policy cap.
func TestBuild_DegradedMode(t *testing.T) {
	o := sampleOutcome()
	o.Halt = nil
	o.Mode = review.ModeReport
	v := Build(Input{Outcome: o, RequestedMode: review.ModeApply})
	if v.RequestedMode != "apply" || v.Mode != "report" {
		t.Errorf("mode=%q requestedMode=%q, want report/apply", v.Mode, v.RequestedMode)
	}
}

// TestBuild_Empty pins totality: a zero outcome yields well-formed EMPTY arrays, never
// nulls, so a consumer can iterate without null-checking.
func TestBuild_Empty(t *testing.T) {
	v := Build(Input{})
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, `"findings":null`) || strings.Contains(s, `"identityCaveats":null`) {
		t.Errorf("arrays must serialize as [] not null:\n%s", s)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schemaVersion", "status", "mode", "findings", "identityCaveats", "counts"} {
		if _, ok := back[k]; !ok {
			t.Errorf("required key %q missing from the projection", k)
		}
	}
}

// TestBuild_MissingDecisions pins the bounds-check: a Findings slice longer than Decisions
// must not panic (a caller can construct an outcome by hand).
func TestBuild_MissingDecisions(t *testing.T) {
	o := sampleOutcome()
	o.Halt = nil
	o.Decisions = nil
	v := Build(Input{Outcome: o})
	if len(v.Findings) != 2 {
		t.Fatalf("findings = %d", len(v.Findings))
	}
	if v.Findings[0].Disposition != "" {
		t.Errorf("disposition = %q, want empty when no decision exists", v.Findings[0].Disposition)
	}
}
