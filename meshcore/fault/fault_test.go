package fault

import (
	"errors"
	"fmt"
	"testing"
)

// The exit-code table is a PUBLIC CLI contract (documented in the app READMEs + docs/architecture.md):
// scripts and CI gates branch on these numbers, so a silent renumbering would break callers with no
// compile error anywhere. These tests pin the numbers and the CodeOf mapping rules.

func TestCode_PublicExitCodeTable(t *testing.T) {
	for _, c := range []struct {
		got  Code
		want int
		name string
	}{
		{OK, 0, "OK"},
		{Findings, 1, "Findings"},
		{Usage, 2, "Usage"},
		{Config, 3, "Config"},
		{Adapter, 4, "Adapter"},
		{Model, 5, "Model"},
		{Containment, 6, "Containment"},
		{Policy, 7, "Policy"},
		{Internal, 8, "Internal"},
	} {
		if int(c.got) != c.want {
			t.Errorf("%s = %d, want %d — this is a PUBLIC exit code; changing it breaks scripts/CI", c.name, c.got, c.want)
		}
	}
}

func TestCodeOf(t *testing.T) {
	if got := CodeOf(nil); got != OK {
		t.Errorf("CodeOf(nil) = %d, want OK(0)", got)
	}
	// A plain error is NOT silently success — an unclassified failure is Internal.
	if got := CodeOf(errors.New("plain")); got != Internal {
		t.Errorf("CodeOf(plain error) = %d, want Internal(8)", got)
	}
	if got := CodeOf(New(Config, "bad config")); got != Config {
		t.Errorf("CodeOf(Fault) = %d, want Config(3)", got)
	}
	// CodeOf must find a Fault WRAPPED anywhere in the chain (errors.As), so a fault that bubbles
	// up through fmt.Errorf("%w") still exits with its own code rather than a generic 8.
	nested := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", New(Adapter, "binary missing")))
	if got := CodeOf(nested); got != Adapter {
		t.Errorf("CodeOf(deeply wrapped Fault) = %d, want Adapter(4)", got)
	}
}

func TestFault_ErrorUnwrapAndHalt(t *testing.T) {
	// A Fault with no cause renders just its message.
	if got := New(Usage, "bad flag").Error(); got != "bad flag" {
		t.Errorf("Error() = %q, want the bare message", got)
	}

	cause := errors.New("permission denied")
	f := Wrap(Config, "read config", cause)
	if got, want := f.Error(), "read config: permission denied"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// Unwrap keeps errors.Is working through the fault, so callers can still test sentinels.
	if !errors.Is(f, cause) {
		t.Error("a wrapped cause must remain discoverable via errors.Is")
	}
	if f.Unwrap() != cause {
		t.Error("Unwrap must return the cause")
	}

	// WithHalt annotates and returns the SAME fault (chainable), preserving code + message.
	h := New(Model, "identity mismatch").WithHalt("E")
	if h.Halt != "E" || h.Code != Model || h.Msg != "identity mismatch" {
		t.Errorf("WithHalt must annotate in place and stay chainable: %+v", h)
	}
	if CodeOf(h) != Model {
		t.Error("WithHalt must not disturb the exit code")
	}
}
