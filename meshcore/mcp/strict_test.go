package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// The OPT-IN send-time schema check.
//
// Both servers already validate their payloads in the BUILD, and that check exists because the drift was
// real: three payloads violated the very `oneOf` their own tool declared. What the build cannot cover is
// a payload no test constructs, so this is the runtime half — off by default, because it costs a schema
// walk on every call.
//
// The two tests below are a pair on purpose. One proves the check CATCHES a violation; the other proves
// that with the flag off the behavior is exactly what it was. A check that changed the default path would
// be a different feature.

// driftySchema declares a payload shape the handler below deliberately violates: `count` must be an
// integer and `state` must be the literal "complete".
const driftySchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["state", "count"],
  "properties": {"state": {"const": "complete"}, "count": {"type": "integer"}}
}`

func driftyServer(strict bool) *mcp.Server {
	return newServer(func(s *mcp.Server) {
		s.StrictSchema = strict
		s.Register(mcp.Tool{
			Name:         "drifty",
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(driftySchema),
		}, func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
			// `state` is not the declared const and `count` is not an integer: this payload matches
			// nothing its own tool advertises.
			return mcp.Result("ok", map[string]any{"state": "running", "count": "three"}), nil
		})
	})
}

func TestStrictSchema_On_CatchesAPayloadThatViolatesItsOwnDeclaration(t *testing.T) {
	c, stop := serve(t, driftyServer(true))
	defer stop()
	handshake(t, c)

	resp, _ := c.call(t, "tools/call", map[string]any{"name": "drifty"})
	if resp.Error != nil {
		t.Fatalf("a server-side schema violation must ride isError, not a protocol error: %+v", resp.Error)
	}
	isErr, _ := resp.Result["isError"].(bool)
	if !isErr {
		t.Fatalf("the violating payload was sent as a success: %+v", resp.Result)
	}
	if _, present := resp.Result["structuredContent"]; present {
		t.Fatalf("the refusal must carry NO structuredContent — the only payload available is the one just judged invalid: %+v", resp.Result)
	}
	text := contentText(resp.Result)
	if !strings.Contains(text, "drifty") {
		t.Errorf("the refusal must NAME the offending tool, got %q", text)
	}
	if !strings.Contains(text, "outputSchema") {
		t.Errorf("the refusal must say what was violated, got %q", text)
	}
}

func TestStrictSchema_Off_LeavesBehaviorExactlyAsItWas(t *testing.T) {
	c, stop := serve(t, driftyServer(false))
	defer stop()
	handshake(t, c)

	resp, _ := c.call(t, "tools/call", map[string]any{"name": "drifty"})
	if resp.Error != nil {
		t.Fatalf("tools/call: %+v", resp.Error)
	}
	if isErr, _ := resp.Result["isError"].(bool); isErr {
		t.Fatalf("with the check OFF the call must behave as before: %+v", resp.Result)
	}
	sc, _ := resp.Result["structuredContent"].(map[string]any)
	if sc["state"] != "running" || sc["count"] != "three" {
		t.Fatalf("with the check OFF the payload must go out untouched, got %+v", sc)
	}
}

// A conforming payload is not disturbed by the check being on — otherwise the flag would be unusable.
func TestStrictSchema_On_PassesAConformingPayloadThrough(t *testing.T) {
	s := newServer(func(s *mcp.Server) {
		s.StrictSchema = true
		s.Register(mcp.Tool{
			Name:         "tidy",
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(driftySchema),
		}, func(ctx context.Context, c *mcp.Call) (*mcp.CallToolResult, error) {
			return mcp.Result("ok", map[string]any{"state": "complete", "count": 3}), nil
		})
	})
	c, stop := serve(t, s)
	defer stop()
	handshake(t, c)
	resp, _ := c.call(t, "tools/call", map[string]any{"name": "tidy"})
	if resp.Error != nil {
		t.Fatalf("tools/call: %+v", resp.Error)
	}
	if isErr, _ := resp.Result["isError"].(bool); isErr {
		t.Fatalf("a CONFORMING payload was refused: %s", contentText(resp.Result))
	}
	sc, _ := resp.Result["structuredContent"].(map[string]any)
	if sc["state"] != "complete" {
		t.Fatalf("structuredContent = %+v", sc)
	}
}

// A tool that declares NO outputSchema has nothing to be checked against, and the flag must not invent a
// rule for it.
func TestStrictSchema_On_IgnoresAToolThatDeclaresNoOutputSchema(t *testing.T) {
	c, stop := serve(t, newServer(func(s *mcp.Server) { s.StrictSchema = true }))
	defer stop()
	handshake(t, c)
	resp, _ := c.call(t, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}})
	if resp.Error != nil {
		t.Fatalf("tools/call: %+v", resp.Error)
	}
	if isErr, _ := resp.Result["isError"].(bool); isErr {
		t.Fatalf("a tool with no declared outputSchema must be unaffected: %s", contentText(resp.Result))
	}
}

func contentText(result map[string]any) string {
	var b strings.Builder
	blocks, _ := result["content"].([]any)
	for _, blk := range blocks {
		if m, ok := blk.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}
