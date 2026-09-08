package mcp_test

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// TestRemediate_AnUngrantedToolSaysWhichGateIsMissing.
//
// The write primitive is protected by two gates: the server must be LAUNCHED with the grant, and
// every call must additionally pass allowWrite. Those failures need opposite responses — one is a
// message to hand a human, the other the model fixes itself — and until now the first answered
// "unknown tool", which reads as neither. A model told a tool does not exist goes looking for a
// different one; a model told the grant is missing reports it.
//
// The tool stays UNADVERTISED either way: non-advertisement is the stronger gate and this does not
// weaken it. TestList_* asserts the absence from tools/list separately.
func TestRemediate_AnUngrantedToolSaysWhichGateIsMissing(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Roots = []string{ws} }))
	res := c.tool(t, "review_remediate", map[string]any{"fromRun": "x", "output": "apply", "allowWrite": true})
	if res.rpc == nil {
		t.Fatalf("calling an ungranted write tool must be refused, got %+v", res)
	}
	msg := res.rpc.Message
	// It must name the OPERATOR ACT that would fix it. Naming the flag is the whole point: the caller
	// cannot grant it and must know what to ask for.
	if !strings.Contains(msg, "--allow-remediate") {
		t.Errorf("the refusal does not name the launch flag that would grant it: %q", msg)
	}
	// And it must not read as "you imagined this tool".
	if strings.Contains(strings.ToLower(msg), "unknown tool") {
		t.Errorf("the refusal still reads as a nonexistent tool rather than an ungranted one: %q", msg)
	}
	// It must point at what the caller CAN still do, so a refused turn is not a dead end.
	if !strings.Contains(msg, "review_report") {
		t.Errorf("the refusal names no usable alternative: %q", msg)
	}
}
