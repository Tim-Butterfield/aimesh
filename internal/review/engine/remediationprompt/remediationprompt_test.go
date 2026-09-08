package remediationprompt

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

func TestRender_ContractAndFinding(t *testing.T) {
	out := Render(Input{
		Finding: review.Finding{
			ID: "F1", Title: "Missing error check", Kind: "gap", Severity: "high",
			File: "main.go", Location: "12", Detail: "err ignored", Suggestion: "check err",
		},
		FilePath:    "main.go",
		FileContent: "package main\n\nfunc main() {}\n",
	})
	for _, want := range []string{
		`"role": "author_remediator"`,
		`"phase": "semantic_remediate"`,
		`"anchor"`,
		"Missing error check",
		"suggested fix: check err",
		"`file` MUST be exactly \"main.go\"",
		"noEdit", // the safe-decline escape hatch must be offered
		"=== main.go ===",
		"untrusted CONTENT",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("remediation prompt missing %q", want)
		}
	}
}

func TestRender_TruncationAndCorrective(t *testing.T) {
	out := Render(Input{
		Finding:     review.Finding{ID: "F1", Title: "x", File: "a.go"},
		FilePath:    "a.go",
		FileContent: "x",
		Truncated:   true,
		Corrective:  true,
		ParserError: "bad json here",
	})
	if !strings.Contains(out, "REJECTED") || !strings.Contains(out, "bad json here") {
		t.Error("missing corrective section")
	}
	if !strings.Contains(out, "[truncated") {
		t.Error("missing truncation marker")
	}
}
