package adjudicationprompt

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

func TestRender_HostContractAndFindings(t *testing.T) {
	out := Render(Input{
		Mode:     review.ModeReport,
		Findings: []review.Finding{{ID: "F1", Title: "x", Kind: "fail", Severity: "high", File: "main.go"}},
		Files:    []FileSnippet{{Path: "main.go", Content: "package main"}},
	})
	for _, want := range []string{
		`"role": "author_remediator"`,
		`"phase": "semantic_adjudicate"`,
		`"adjudications"`,
		"valid | invalid",
		"info | low | medium | high | critical",
		"EXACTLY ONE adjudication per finding id",
		"Do NOT invent new findings",
		"INDEPENDENTLY",
		"\"id\": \"F1\"", // reviewer findings embedded as JSON
		"=== main.go ===",
		"untrusted CONTENT",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("host prompt missing %q", want)
		}
	}
}

func TestRender_HostAddressedCorrectiveWholeFiles(t *testing.T) {
	body := strings.Repeat("x", 40<<10)
	out := Render(Input{
		Findings:    []review.Finding{{ID: "F1", Title: "x", Kind: "fail", Severity: "low"}},
		Files:       []FileSnippet{{Path: "big", Content: body}},
		Addressed:   []string{"applied: Use a constant (main.go)"},
		Corrective:  true,
		ParserError: "bad json",
	})
	// The host adjudicates against the same COMPLETE files the reviewers saw; a truncation marker
	// here would invite it to invalidate a finding for evidence it was wrongly told it lacked.
	if !strings.Contains(out, body) {
		t.Error("the host prompt does not carry the whole file")
	}
	if strings.Contains(out, "[truncated]") {
		t.Error("the host prompt claims a truncation that no longer happens")
	}
	if !strings.Contains(out, "applied: Use a constant") {
		t.Error("missing prior dispositions")
	}
	if !strings.Contains(out, "REJECTED") || !strings.Contains(out, "bad json") {
		t.Error("missing corrective section")
	}
}
