package acp_test

// ACP surface parity for the CANONICALIZER spec (design §4): `_meta.exploremesh.canonicalizers` is the
// ACP analogue of the CLI's repeatable `--canonicalizer` and MCP's `canonicalizers` argument. Same rule
// on all three: 0 or 2 entries, never 1; two distinct identities; compose-not-configure.

import (
	"strings"
	"testing"
)

func TestCanonicalizers_HonoredAndEchoed(t *testing.T) {
	exp := &fakeExplorer{}
	c, stop := serveWith(t, exp, []string{"claude-code", "fake"})
	defer stop()
	sid := paritySession(t, c)

	resp := promptWith(t, c, sid, map[string]any{"canonicalizers": []any{
		map[string]any{"adapter": "fake", "model": "canon-one", "effort": "high"},
		map[string]any{"adapter": "fake", "model": "canon-two"},
	}})
	if resp.Error != nil {
		t.Fatalf("explicit canonicalizers refused: %+v", resp.Error)
	}
	if exp.gotPlan == nil || len(exp.gotPlan.Canonicalizers) != 2 {
		t.Fatalf("executed plan carried %+v, want the two named canonicalizers", exp.gotPlan)
	}
	if exp.gotPlan.Canonicalizers[0].Model != "canon-one" || exp.gotPlan.Canonicalizers[1].Model != "canon-two" {
		t.Errorf("canonicalizers = %+v, want them in the order supplied", exp.gotPlan.Canonicalizers)
	}
	meta, _ := resp.Result["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	if em["canonicalizerSource"] != "explicit" {
		t.Errorf("canonicalizerSource = %v, want explicit", em["canonicalizerSource"])
	}
	if seats, _ := em["canonicalizers"].([]any); len(seats) != 2 {
		t.Errorf("the echo must carry the canonicalizer seats: %v", em["canonicalizers"])
	}
}

// A turn that names none gets the honest `derived` label rather than silence — an empty list and "the host
// will choose" are different facts, and only one of them is checkable.
func TestCanonicalizers_DerivedIsStatedNotImplied(t *testing.T) {
	c, stop := serveWith(t, &fakeExplorer{}, []string{"fake"})
	defer stop()
	sid := paritySession(t, c)

	resp := promptWith(t, c, sid, map[string]any{})
	if resp.Error != nil {
		t.Fatalf("prompt: %+v", resp.Error)
	}
	meta, _ := resp.Result["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	if em["canonicalizerSource"] != "derived" {
		t.Errorf("canonicalizerSource = %v, want derived", em["canonicalizerSource"])
	}
}

func TestCanonicalizers_FailClosedRefusals(t *testing.T) {
	c, stop := serveWith(t, &fakeExplorer{}, []string{"claude-code", "fake"})
	defer stop()
	sid := paritySession(t, c)

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
		{"three entries", []any{
			map[string]any{"adapter": "fake", "model": "a"},
			map[string]any{"adapter": "fake", "model": "b"},
			map[string]any{"adapter": "fake", "model": "c"},
		}, "exactly 2"},
		{"unconfigured adapter", []any{
			map[string]any{"adapter": "brand-new-cli", "model": "a"},
			map[string]any{"adapter": "fake", "model": "b"},
		}, "not configured"},
		{"blank model", []any{
			map[string]any{"adapter": "fake", "model": ""},
			map[string]any{"adapter": "fake", "model": "b"},
		}, "non-empty adapter and model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := promptWith(t, c, sid, map[string]any{"canonicalizers": tc.in})
			if resp.Error == nil {
				t.Fatalf("a malformed canonicalizer spec must be invalid-params, got %+v", resp.Result)
			}
			if !strings.Contains(resp.Error.Message, tc.want) {
				t.Errorf("refusal %q does not mention %q", resp.Error.Message, tc.want)
			}
			if !strings.Contains(resp.Error.Message, "canonicalizers") {
				t.Errorf("refusal %q must address the field", resp.Error.Message)
			}
		})
	}
}
