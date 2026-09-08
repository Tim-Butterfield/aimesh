package reviewprompt

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// TestRender_SelfReportWrapperGating proves the inline-identity instruction appears ONLY when the
// Manager sets RequestSelfReportIdentity (weak self-report adapters), never otherwise (strong-evidence
// adapters keep their unchanged prompt), and that a corrective retry restates the wrapper (guardrail G3).
func TestRender_SelfReportWrapperGating(t *testing.T) {
	on := Render(Input{RequestSelfReportIdentity: true, Files: []FileSnippet{{Path: "a", Content: "b"}}})
	if !strings.Contains(on, "reviewmeshIdentity") || !strings.Contains(on, "IDENTITY WRAPPER") {
		t.Errorf("a self-report adapter must get the wrapper instruction:\n%s", on)
	}
	off := Render(Input{Files: []FileSnippet{{Path: "a", Content: "b"}}})
	if strings.Contains(off, "reviewmeshIdentity") {
		t.Errorf("a strong-evidence adapter must NOT get the wrapper instruction:\n%s", off)
	}
	corr := Render(Input{RequestSelfReportIdentity: true, Corrective: true, ParserError: "bad"})
	if !strings.Contains(corr, "keep wrapping") {
		t.Errorf("a corrective retry must restate the wrapper:\n%s", corr)
	}
}

func TestRender_IncludesContractAndFiles(t *testing.T) {
	out := Render(Input{
		Phase: review.PhaseIterate,
		Files: []FileSnippet{{Path: "main.go", Content: "package main"}},
	})
	for _, want := range []string{
		`"schemaVersion": 1`,
		`"role": "reviewer"`,
		`"phase": "semantic_iterate"`,
		`"approve"`, `"request_changes"`,
		"pass | fail | ambiguity | inconsistency | gap | risk",
		"info | low | medium | high | critical",
		"workspace-relative",
		"=== main.go ===",
		"package main",
		"No Markdown code fences",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// A file reaches the reviewer WHOLE, and the prompt says nothing about truncation because nothing
// truncates. A stray marker would tell a reviewer to distrust a complete file.
func TestRender_CarriesFilesWholeWithNoTruncationMarker(t *testing.T) {
	body := strings.Repeat("x", 40<<10)
	out := Render(Input{Files: []FileSnippet{{Path: "big.txt", Content: body}}})
	if !strings.Contains(out, body) {
		t.Error("the prompt does not carry the whole file")
	}
	if strings.Contains(out, "[truncated]") {
		t.Error("the prompt claims a truncation that no longer happens")
	}
}

func TestRender_AddressedAndCorrective(t *testing.T) {
	out := Render(Input{
		Addressed:   []string{"applied: Use a constant (main.go)"},
		Reported:    []string{"Missing error check (main.go:12)"},
		Corrective:  true,
		ParserError: "bad json here",
	})
	if !strings.Contains(out, "applied: Use a constant") {
		t.Error("missing addressed-context")
	}
	if !strings.Contains(out, "PREVIOUSLY REPORTED THIS CYCLE") || !strings.Contains(out, "Missing error check") {
		t.Error("missing in-cycle reported-context")
	}
	if !strings.Contains(out, "REJECTED") || !strings.Contains(out, "bad json here") {
		t.Error("missing corrective section")
	}
}

// TestRender_PriorDispositionsGate pins the IN-RUN disposition feed: what THIS run's earlier cycles
// decided is shown to the reviewer under a disagreement-required gate, so a rejected finding is not
// blindly re-raised (budget crowding) while genuine disagreement is still allowed.
//
// It reaches one run only. Nothing a PREVIOUS run decided is ever in a prompt.
func TestRender_PriorDispositionsGate(t *testing.T) {
	out := Render(Input{Addressed: []string{
		"applied (fixed): Use a constant (main.go)",
		"rejected as invalid: Speculative NPE (main.go:5) — reason: not supported by the shown files",
	}})
	if !strings.Contains(out, "PRIOR DISPOSITIONS") {
		t.Error("missing prior-dispositions header")
	}
	if !strings.Contains(out, "specific evidence") {
		t.Error("gate must require specific evidence to re-raise a rejected finding")
	}
	if !strings.Contains(out, "rejected as invalid: Speculative NPE") {
		t.Error("rejected findings must be shown so they are not re-raised blindly")
	}
}

func TestRender_DefaultPhase(t *testing.T) {
	out := Render(Input{Files: []FileSnippet{{Path: "a", Content: "b"}}})
	if !strings.Contains(out, `"phase": "semantic_iterate"`) {
		t.Error("empty phase should default to semantic_iterate")
	}
}
