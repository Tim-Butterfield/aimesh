package model

import (
	"strings"
	"testing"
)

func TestRenderDevinModelArg(t *testing.T) {
	cases := []struct {
		model, effort, want string
		wantErr             bool
	}{
		{"Claude Opus 4.8", "medium", "claude-opus-4-8-medium", false},
		{"GPT-5.4", "Medium Thinking", "gpt-5-4-medium", false},
		{"Gemini 3.1 Pro", "High Thinking", "gemini-3-1-pro-high", false},
		{"Claude Opus 4.8", "", "claude-opus-4-8", false}, // effort optional
		{"Opus 4.8", "medium", "", true},                  // ambiguous: no brand prefix, never auto-add "Claude"
		{"", "medium", "", true},                          // empty
		{"Claude Opus 4.8", "ultra", "", true},            // bad effort
		{"-", "medium", "", true},                         // separator-only must error, not panic
		{"---", "", "", true},                             // separator-only must error, not panic
	}
	for _, c := range cases {
		got, err := RenderDevinModelArg(c.model, c.effort)
		if c.wantErr {
			if err == nil {
				t.Errorf("RenderDevinModelArg(%q,%q) = %q, want error", c.model, c.effort, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("RenderDevinModelArg(%q,%q) unexpected error: %v", c.model, c.effort, err)
			continue
		}
		if got != c.want {
			t.Errorf("RenderDevinModelArg(%q,%q) = %q, want %q", c.model, c.effort, got, c.want)
		}
		if strings.ContainsAny(got, " .") {
			t.Errorf("rendered Devin modelArg %q must contain no spaces or periods", got)
		}
	}
}

func TestRenderDevinModelArg_AmbiguousErrorMentionsFullName(t *testing.T) {
	_, err := RenderDevinModelArg("Opus 4.8", "medium")
	if err == nil || !strings.Contains(err.Error(), "full display model name") {
		t.Errorf("ambiguous error should guide to the full display name, got: %v", err)
	}
}
