package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The usage line has advertised `aimesh explore run [question]` since the command existed, and
// nothing ever read a positional. So the documented shorthand refused every single time it was
// used — and, worse, Go's parser stops at the first non-flag token, so the flags a user typed AFTER
// the question were silently discarded and the run was then refused for missing them.

// The assertions go through `--dry-run --json`, which carries the exact round-1 prompt. That is
// the only place the purpose and criteria are OBSERVABLE: the human dry-run summary reports the
// panel and the cost, not the task text, so asserting on it would prove nothing about either.
func TestExplore_ThePositionalIsThePurpose(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"explore", "how should we shard the write path?", "--criteria", "cost", "--dry-run", "--json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("the advertised positional form must work: exit %d\nstderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "how should we shard the write path?") {
		t.Errorf("the positional did not reach the explorers' prompt:\n%s", out.String())
	}
}

// TestExplore_FlagsAfterThePositionalSurvive is the silent half. `--criteria` here is typed AFTER
// the question; before the split it never reached the parser at all, and the refusal said the user
// had given no criteria — blaming them for the flag they had just typed.
func TestExplore_FlagsAfterThePositionalSurvive(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"explore", "a question", "--criteria", "latency,cost", "--mode", "map", "--dry-run", "--json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("flags after the positional were dropped: exit %d\nstderr: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "latency") || !strings.Contains(s, "cost") {
		t.Errorf("the criteria typed after the question did not reach the run:\n%s", s)
	}
}

// TestExplore_ThePurposeGivenTwiceIsRefused. Choosing between them silently would run — and charge
// for — an exploration the user did not ask for.
func TestExplore_ThePurposeGivenTwiceIsRefused(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"explore", "positional question", "--purpose", "flag question", "--criteria", "a"}, &out, &errb)
	if code == 0 {
		t.Fatal("two different purposes were accepted; one of them was silently discarded")
	}
	if !strings.Contains(errb.String(), "given twice") {
		t.Errorf("the refusal does not explain the conflict: %s", errb.String())
	}
}

// TestExplore_TheMissingInputsRefusalTeachesTheCommand. This is the first thing a new user meets,
// and "raw_task: empty purpose" alone leaves them to rebuild the invocation from the help text.
func TestExplore_TheMissingInputsRefusalTeachesTheCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p"}, &out, &errb); code == 0 {
		t.Fatal("a run with no criteria must be refused")
	}
	s := errb.String()
	if !strings.Contains(s, "--criteria") {
		t.Errorf("the refusal does not name the missing flag:\n%s", s)
	}
	if !strings.Contains(s, "aimesh explore run") {
		t.Errorf("the refusal does not show a runnable command:\n%s", s)
	}
	// And it says WHY the tool will not choose them, which is the question a user asks next.
	if !strings.Contains(s, "never chosen for you") {
		t.Errorf("the refusal does not explain why criteria are required:\n%s", s)
	}
}
