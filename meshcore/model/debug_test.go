package model

import (
	"bytes"
	"context"
	"testing"
)

// WithDebug/DebugWriter is the seam behind BOTH apps' shipped `--debug` flag: an app enables
// diagnostics once and EVERY adapter family (shell, acpagent, …) must read the same key, or
// `--debug` would silently produce nothing for some adapters. It is also off by default — a
// diagnostic writer must never be picked up accidentally.

func TestDebugWriter_OffByDefault(t *testing.T) {
	if w := DebugWriter(context.Background()); w != nil {
		t.Errorf("debug must be OFF for a plain context, got %#v", w)
	}
	// A context carrying unrelated values must not be mistaken for a debug context.
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, &bytes.Buffer{})
	if w := DebugWriter(ctx); w != nil {
		t.Error("an unrelated context value must not be read as the debug writer")
	}
}

func TestWithDebug_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithDebug(context.Background(), &buf)

	got := DebugWriter(ctx)
	if got == nil {
		t.Fatal("WithDebug must make the writer retrievable")
	}
	if got != &buf {
		t.Error("DebugWriter must return the SAME writer instance the app supplied")
	}
	// Prove it is usable as a sink (what adapters actually do with it).
	if _, err := got.Write([]byte("diagnostic")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if buf.String() != "diagnostic" {
		t.Errorf("writes must reach the app's buffer, got %q", buf.String())
	}
}

// A nil writer is an explicit no-op (an app may pass its writer unconditionally): it must return the
// ORIGINAL context, and must not install a typed-nil that later reads as "debug is on".
func TestWithDebug_NilWriterIsNoOp(t *testing.T) {
	base := context.Background()
	ctx := WithDebug(base, nil)
	if ctx != base {
		t.Error("WithDebug(nil) must return the original context unchanged")
	}
	if w := DebugWriter(ctx); w != nil {
		t.Errorf("a nil writer must leave debug OFF, got %#v", w)
	}
}

// The value must survive further context derivation (adapters wrap ctx with timeouts/cancels before
// invoking, so a diagnostic writer set at the top must still be visible at the call site).
func TestWithDebug_SurvivesDerivedContexts(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithDebug(context.Background(), &buf)
	derived, cancel := context.WithCancel(ctx)
	defer cancel()
	if DebugWriter(derived) != &buf {
		t.Error("the debug writer must survive context derivation (adapters add timeouts/cancels)")
	}
	// An inner WithDebug overrides for that subtree only.
	var inner bytes.Buffer
	if DebugWriter(WithDebug(derived, &inner)) != &inner {
		t.Error("a nested WithDebug must override within its subtree")
	}
	if DebugWriter(derived) != &buf {
		t.Error("the outer context must be unaffected by a nested override")
	}
}
