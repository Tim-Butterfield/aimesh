package prompt

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// The shared Scripted prompter (the test double for the interactive stack) must return
// queued answers deterministically and exhaust to safe defaults — locking the contract
// the future setup wizard / guided repair tests will depend on.
func TestScripted_DeterministicAndExhausts(t *testing.T) {
	var s review.Prompter = &Scripted{
		Answers:  []string{"/bin/claude", "opus"},
		Choices:  []int{2},
		Confirms: []bool{true, false},
	}
	if got := s.AskPath("path?"); got != "/bin/claude" {
		t.Errorf("AskPath #1 = %q, want /bin/claude", got)
	}
	if got := s.Ask("model?"); got != "opus" {
		t.Errorf("Ask #2 = %q, want opus", got)
	}
	if got := s.Ask("more?"); got != "" {
		t.Errorf("exhausted Ask must return \"\", got %q", got)
	}
	if got := s.Choose("pick", []string{"a", "b", "c"}); got != 2 {
		t.Errorf("Choose = %d, want 2", got)
	}
	if got := s.Choose("pick again", []string{"a"}); got != 0 {
		t.Errorf("exhausted Choose must default to 0, got %d", got)
	}
	if !s.Confirm("first?") {
		t.Error("Confirm #1 should be true")
	}
	if s.Confirm("second?") {
		t.Error("Confirm #2 should be false")
	}
	if s.Confirm("exhausted?") {
		t.Error("exhausted Confirm must default to false (no accidental yes)")
	}
}

// The interactive Stdin prompter parses a yes/no confirm and is the non-fake half of the
// same contract.
func TestStdin_ConfirmParsesYes(t *testing.T) {
	p := NewStdin(strings.NewReader("yes\n"), &bytes.Buffer{})
	if !p.Confirm("ok?") {
		t.Error(`"yes" should confirm`)
	}
	p2 := NewStdin(strings.NewReader("\n"), &bytes.Buffer{})
	if p2.Confirm("ok?") {
		t.Error("empty input must default to no")
	}
}
