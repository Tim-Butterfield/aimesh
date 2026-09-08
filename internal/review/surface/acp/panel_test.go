package acp

import (
	"context"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
)

// selectionReviewer captures the profile/panel selection the Manager was actually called with.
type selectionReviewer struct {
	noRemediation // these prompts are report turns; none reaches a write
	profile       string
	panel         []review.SeatSpec
}

func (r *selectionReviewer) RunContext(ctx context.Context, req run.Request) (review.RunOutcome, error) {
	r.profile, r.panel = req.Profile, req.ReviewerPanel
	return review.RunOutcome{
		Status: "stable", Mode: req.Mode, RunDir: "/tmp/run-x",
		Panel: []review.SeatStatus{
			{SeatID: "reviewer", Index: 1, Adapter: "codex-cli", Model: "codex-cli-default", Status: "completed", Rounds: 1},
			{SeatID: "reviewer-2", Index: 2, Adapter: "claude-code", Model: "claude-code-default", Status: "completed", Rounds: 1},
		},
	}, nil
}

// TestPromptMeta_PanelReachesTheManager: `_meta.reviewmesh.panel` composes a blind panel over ACP —
// the surface parity that gives reviewmesh ACP profile/panel selection. The seats arrive in the
// order the host wrote them.
func TestPromptMeta_PanelReachesTheManager(t *testing.T) {
	rec := &selectionReviewer{}
	r := serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"report","_meta":{"reviewmesh":{"panel":[{"adapter":"codex-cli","model":"codex-cli-default"},{"adapter":"claude-code","model":"claude-code-default","effort":"high"}]}}}}`)
	res := result(t, r[0])
	if len(rec.panel) != 2 {
		t.Fatalf("Manager received %d seats, want 2", len(rec.panel))
	}
	if rec.panel[0].Adapter != "codex-cli" || rec.panel[1].Effort != "high" {
		t.Errorf("seats reached the Manager altered: %+v", rec.panel)
	}
	// The executed roster is echoed back, so a host that composed N seats can verify N ran.
	panel, ok := res["panel"].([]any)
	if !ok || len(panel) != 2 {
		t.Errorf("result._meta must echo the executed panel roster, got %v", res["panel"])
	}
}

// TestPromptMeta_ProfileReachesTheManager: `_meta.reviewmesh.profile` selects a configured profile
// on BOTH the ACP v1 `session/prompt` path and the compatibility `review` method — a compatibility
// method that could not select would be the parity hole this closes.
func TestPromptMeta_ProfileReachesTheManager(t *testing.T) {
	rec := &selectionReviewer{}
	serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"report","_meta":{"reviewmesh":{"profile":"native-three-provider"}}}}`)
	if rec.profile != "native-three-provider" {
		t.Errorf("`review` Manager received profile %q, want native-three-provider", rec.profile)
	}

	sessionRec := &selectionReviewer{}
	resps := serve(t, sessionRec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/ws"}}`)
	sid, _ := result(t, resps[0])["sessionId"].(string)
	if sid == "" {
		t.Fatal("session/new returned no sessionId")
	}
	serve(t, sessionRec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/ws"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"`+sid+`","workspace":"/ws","mode":"report","_meta":{"reviewmesh":{"profile":"native-three-provider"}}}}`)
	if sessionRec.profile != "native-three-provider" {
		t.Errorf("session/prompt Manager received profile %q, want native-three-provider", sessionRec.profile)
	}
}

// TestPromptMeta_SelectionFailsClosed: every selection error is -32602 with a machine reason code,
// BEFORE any spend — and the mutual exclusion of `profile` and `panel` is one of them.
func TestPromptMeta_SelectionFailsClosed(t *testing.T) {
	seat := `{"adapter":"codex-cli","model":"codex-cli-default"}`
	big := "[" + strings.TrimSuffix(strings.Repeat(seat+",", review.MaxReviewerSeats+1), ",") + "]"
	cases := []struct {
		name, meta, reason string
	}{
		{"both selectors", `{"profile":"p","panel":[` + seat + `]}`, "panel_and_profile"},
		{"oversized panel", `{"panel":` + big + `}`, "panel_too_large"},
		{"incomplete seat", `{"panel":[{"adapter":"codex-cli"}]}`, "panel_seat_incomplete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &selectionReviewer{}
			r := serve(t, rec,
				`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"report","_meta":{"reviewmesh":`+tc.meta+`}}}`)
			e := rpcErr(t, r[0])
			if e["code"] != float64(codeInvalidParams) {
				t.Fatalf("code = %v, want %d (invalid params)", e["code"], codeInvalidParams)
			}
			data, _ := e["data"].(map[string]any)
			if data == nil || data["reasonCode"] != tc.reason {
				t.Errorf("data = %v, want reasonCode %q", e["data"], tc.reason)
			}
			if rec.profile != "" || rec.panel != nil {
				t.Error("the Manager must never be reached by an invalid selection (fail-closed, pre-spend)")
			}
		})
	}
}

// TestPromptMeta_CannotIntroduceAnAdapter: compose-not-configure is enforced by the ABSENCE of any
// field through which a prompt could define an adapter — a `path`/`args` key inside the reviewmesh
// namespace is a strict-decode error, not a silently ignored extension.
func TestPromptMeta_CannotIntroduceAnAdapter(t *testing.T) {
	rec := &selectionReviewer{}
	r := serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"report","_meta":{"reviewmesh":{"panel":[{"adapter":"x","model":"m","path":"/usr/bin/evil"}]}}}}`)
	e := rpcErr(t, r[0])
	if e["code"] != float64(codeInvalidParams) {
		t.Fatalf("code = %v, want %d — a request may compose from CONFIGURED adapters, never introduce one", e["code"], codeInvalidParams)
	}
	if rec.panel != nil {
		t.Error("a panel carrying an adapter definition must never reach the Manager")
	}
}
