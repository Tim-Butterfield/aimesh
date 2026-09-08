package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Call.Log's two eras, tested by construction. The wire-level version of the same claim — zero
// notifications/message across a whole modern transcript, under every shape of `_meta.logLevel` — is
// in era_test.go, and the real-binary version is in each application's subprocess test.
//
// AGAINST A TREE WITH NO PER-REQUEST ERA THIS FAILS: Call.Log emitted unconditionally, because there
// was no era to ask.
func TestCallLog_TheEraDecidesWhetherAnythingIsEmitted(t *testing.T) {
	var out bytes.Buffer
	f := NewFramer(FramingNewline, strings.NewReader(""), &out)
	s := &Server{}
	s.markInitialized()

	legacy := &Call{srv: s, f: f, env: &RequestEnv{Era: EraLegacy, Trust: &scope.Resolver{}}}
	legacy.Log(LevelError, "probe", "a legacy line")
	if out.Len() == 0 {
		t.Fatal("the legacy era must still emit notifications/message — this phase changes nothing on the wire")
	}
	var note struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &note); err != nil || note.Method != "notifications/message" {
		t.Fatalf("legacy frame = %s, want a notifications/message", out.Bytes())
	}

	out.Reset()
	modern := &Call{srv: s, f: f, env: &RequestEnv{Era: EraModern, Trust: &scope.Resolver{}}}
	modern.Log(LevelError, "probe", "a modern line")
	if out.Len() != 0 {
		t.Fatalf("the modern era emitted %s — `server/utilities/logging` is deprecated in 2026-07-28 and its named stdio migration is stderr, so this server emits no notifications/message there at all", out.Bytes())
	}
}

// A Call built outside a dispatch has no protocol context. It must read as FAIL-CLOSED — no roots, a
// resolver that refuses every path, provenance `none` — rather than as an absent restriction. This is
// the one place a nil env can occur, so it is the one place the default matters.
func TestCallEnv_ANilContextIsFailClosedNotUnrestricted(t *testing.T) {
	var c *Call
	env := c.Env()
	if env == nil || env.Trust == nil {
		t.Fatal("Env() must never return a nil env or a nil resolver")
	}
	if env.RootSource != RootsNone {
		t.Errorf("RootSource = %q, want %q", env.RootSource, RootsNone)
	}
	if _, err := env.Trust.ResolveRead(t.TempDir()); err == nil {
		t.Fatal("the default env accepted a path — an absent context must refuse, never permit")
	}
	if c.HasProgressToken() {
		t.Error("a call with no context must not claim a progress token")
	}
}
