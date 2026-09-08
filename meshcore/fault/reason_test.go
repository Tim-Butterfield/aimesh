package fault

import (
	"errors"
	"strings"
	"testing"
)

// TestReason_ExplicitWins pins that an explicitly-classified fault reports its own code.
func TestReason_ExplicitWins(t *testing.T) {
	f := New(Adapter, "adapter \"x\" exited 2").WithReason("adapter_exited_nonzero")
	if got := f.Reason(); got != "adapter_exited_nonzero" {
		t.Errorf("Reason() = %q", got)
	}
	if got := ReasonOf(f); got != "adapter_exited_nonzero" {
		t.Errorf("ReasonOf() = %q", got)
	}
	// The human message is untouched — reason and message are separate channels.
	if f.Error() != "adapter \"x\" exited 2" {
		t.Errorf("Error() = %q", f.Error())
	}
}

// TestReason_FallbackIsCodeShaped is the core machine-readability guarantee: even an
// unclassified fault yields a code, never a sentence.
func TestReason_FallbackIsCodeShaped(t *testing.T) {
	for _, c := range []Code{OK, Findings, Usage, Config, Adapter, Model, Containment, Policy, Internal} {
		r := New(c, "some human sentence, with punctuation.").Reason()
		if r == "" {
			t.Errorf("code %d: empty reason", c)
			continue
		}
		if strings.ContainsAny(r, " .,:;\"'") || r != strings.ToLower(r) {
			t.Errorf("code %d: reason %q is not code-shaped", c, r)
		}
	}
}

// TestReasonOf_NonFault maps a plain error to the internal fallback, so a persisted
// reasonCode is code-shaped whatever the error turns out to be.
func TestReasonOf_NonFault(t *testing.T) {
	if got := ReasonOf(errors.New("boom")); got != "internal_error" {
		t.Errorf("ReasonOf(plain error) = %q, want internal_error", got)
	}
	if got := ReasonOf(nil); got != "" {
		t.Errorf("ReasonOf(nil) = %q, want \"\"", got)
	}
	var nilFault *Fault
	if got := nilFault.Reason(); got != "" {
		t.Errorf("(*Fault)(nil).Reason() = %q, want \"\"", got)
	}
}

// TestSignal pins the orthogonal actionability channel.
func TestSignal(t *testing.T) {
	f := Wrap(Adapter, "adapter failed", errors.New("not logged in")).
		WithHalt("A").WithReason("adapter_exited_nonzero").WithSignal("login_required")
	if got := SignalOf(f); got != "login_required" {
		t.Errorf("SignalOf() = %q", got)
	}
	if got := SignalOf(errors.New("x")); got != "" {
		t.Errorf("SignalOf(plain) = %q, want \"\"", got)
	}
	if f.Halt != "A" || f.Reason() != "adapter_exited_nonzero" {
		t.Errorf("halt/reason lost through the builder chain: %+v", f)
	}
}
