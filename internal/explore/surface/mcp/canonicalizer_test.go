package mcp_test

// MCP surface parity for the CANONICALIZER spec: the `canonicalizers` argument is the MCP
// analogue of the CLI's repeatable `--canonicalizer` and ACP's `_meta.exploremesh.canonicalizers`. Same
// rule on all three: 0 or 2 entries, never 1; two distinct identities; compose-not-configure.

import (
	"context"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCanonicalizers_HonoredAndEchoed(t *testing.T) {
	exp := &fakeExplorer{}
	s := connect(t, newServer(t, exp))

	res := call(t, s, "explore", exploreArgs(map[string]any{"canonicalizers": []any{
		map[string]any{"adapter": "fake", "model": "canon-one", "effort": "high"},
		map[string]any{"adapter": "fake", "model": "canon-two"},
	}}))
	if res.IsError {
		t.Fatalf("explicit canonicalizers refused: %s", textOf(res))
	}
	plan := exp.plan()
	if len(plan.Canonicalizers) != 2 || plan.Canonicalizers[0].Model != "canon-one" || plan.Canonicalizers[1].Model != "canon-two" {
		t.Fatalf("executed plan carried %+v, want the two named canonicalizers in order", plan.Canonicalizers)
	}
	out := structured(t, res)
	panel, _ := out["panel"].(map[string]any)
	exec, _ := panel["executed"].(map[string]any)
	if exec["canonicalizerSource"] != "explicit" {
		t.Errorf("executed.canonicalizerSource = %v, want explicit", exec["canonicalizerSource"])
	}
	if seats, _ := exec["canonicalizers"].([]any); len(seats) != 2 {
		t.Errorf("executed.canonicalizers = %v, want the two seats", exec["canonicalizers"])
	}
	// The REQUESTED half carries what the call asked for, so requested-vs-executed stays comparable.
	req, _ := panel["requested"].(map[string]any)
	if seats, _ := req["canonicalizers"].([]any); len(seats) != 2 {
		t.Errorf("requested.canonicalizers = %v, want the pair the call named", req["canonicalizers"])
	}
}

// A call that names none is told so explicitly: `derived` is a stated fact, not an inference from an
// empty array.
func TestCanonicalizers_DerivedIsStatedNotImplied(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	out := structured(t, call(t, s, "explore", exploreArgs(nil)))
	panel, _ := out["panel"].(map[string]any)
	exec, _ := panel["executed"].(map[string]any)
	if exec["canonicalizerSource"] != "derived" {
		t.Errorf("executed.canonicalizerSource = %v, want derived", exec["canonicalizerSource"])
	}
}

func TestCanonicalizers_FailClosedRefusals(t *testing.T) {
	s := connect(t, newServer(t, &fakeExplorer{}))
	cases := []struct {
		name string
		in   []any
		want string
	}{
		{"one entry", []any{map[string]any{"adapter": "fake", "model": "only-one"}}, "which slot it fills"},
		{"identical identities", []any{
			map[string]any{"adapter": "fake", "model": "twin", "effort": "high"},
			map[string]any{"adapter": "fake", "model": "twin", "effort": "low"},
		}, "independent"},
		{"adapter not launched", []any{
			map[string]any{"adapter": "brand-new-cli", "model": "a"},
			map[string]any{"adapter": "fake", "model": "b"},
		}, "was not launched with"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A malformed request is a PROTOCOL error (-32602), not an isError result: nothing was spent and
			// no run exists, which is exactly what the taxonomy reserves protocol errors for.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "explore",
				Arguments: exploreArgs(map[string]any{"canonicalizers": tc.in})})
			if err == nil {
				t.Fatal("a malformed canonicalizer spec must be refused as invalid-params")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not mention %q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), "canonicalizers") {
				t.Errorf("refusal %q must address the field", err.Error())
			}
		})
	}
}
