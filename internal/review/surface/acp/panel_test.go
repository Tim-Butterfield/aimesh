package acp

import (
	"context"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
)

// selectionReviewer captures the seats the Manager was actually called with.
type selectionReviewer struct {
	noRemediation // these prompts are report turns; none reaches a write
	called        bool
	panel         []review.SeatSpec
	roles         map[review.Role]review.SeatSpec
}

func (r *selectionReviewer) RunContext(ctx context.Context, req run.Request) (review.RunOutcome, error) {
	r.called, r.panel, r.roles = true, req.ReviewerPanel, req.ComposedRoles
	return review.RunOutcome{
		Status: "stable", Mode: req.Mode, RunDir: "/tmp/run-x",
		Panel: []review.SeatStatus{
			{SeatID: "reviewer", Index: 1, Adapter: "fake", Model: "m1", Status: "completed", Rounds: 1},
			{SeatID: "reviewer-2", Index: 2, Adapter: "claude-code", Model: "m2", Status: "completed", Rounds: 1},
		},
	}, nil
}

const composedPanel = `{"reviewers":[{"adapter":"fake","model":"m1"},{"adapter":"claude-code","model":"m2","effort":"high"}],` +
	`"author_remediator":{"adapter":"fake","model":"m1"},"cross_check":{"adapter":"claude-code","model":"cc"}}`

// TestPromptMeta_PanelReachesTheManager: `_meta.reviewmesh.panel` composes the turn's seats on both
// the ACP v1 `session/prompt` path and the compatibility `review` method. The reviewers arrive in the
// order the host wrote them, and each role seat reaches the Manager as a composed role.
func TestPromptMeta_PanelReachesTheManager(t *testing.T) {
	rec := &selectionReviewer{}
	r := serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"report","_meta":{"reviewmesh":{"panel":`+composedPanel+`}}}}`)
	res := result(t, r[0])
	if len(rec.panel) != 2 || rec.panel[0].Adapter != "fake" || rec.panel[1].Effort != "high" {
		t.Errorf("reviewer seats reached the Manager altered: %+v", rec.panel)
	}
	if got := rec.roles[review.RoleAuthorRemediator]; got.Adapter != "fake" || got.Model != "m1" {
		t.Errorf("author_remediator = %+v, want fake/m1", got)
	}
	if got := rec.roles[review.RoleCrossCheck]; got.Adapter != "claude-code" || got.Model != "cc" {
		t.Errorf("cross_check = %+v, want claude-code/cc", got)
	}
	if _, named := rec.roles[review.RoleVerifier]; named {
		t.Error("a verifier the turn did not name must not be composed")
	}
	// The executed roster is echoed back, so a host that composed N seats can verify N ran.
	if panel, ok := res["panel"].([]any); !ok || len(panel) != 2 {
		t.Errorf("the result must echo the executed panel roster, got %v", res["panel"])
	}

	sessionRec := &selectionReviewer{}
	serve(t, sessionRec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/ws"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report","_meta":{"reviewmesh":{"panel":`+composedPanel+`}}}}`)
	if len(sessionRec.panel) != 2 || sessionRec.roles[review.RoleCrossCheck].Adapter != "claude-code" {
		t.Errorf("session/prompt Manager received panel %+v roles %+v", sessionRec.panel, sessionRec.roles)
	}
}

// TestPromptMeta_SelectionFailsClosed: every panel error is -32602 with a machine reason code,
// BEFORE any spend.
func TestPromptMeta_SelectionFailsClosed(t *testing.T) {
	seat := `{"adapter":"fake","model":"m1"}`
	big := "[" + strings.TrimSuffix(strings.Repeat(seat+",", review.MaxReviewerSeats+1), ",") + "]"
	cases := []struct {
		name, meta, reason string
	}{
		{"no panel", `{"panel":null}`, ReasonPanelRequired},
		{"no reviewers", `{"panel":{"reviewers":[],"author_remediator":` + seat + `}}`, ReasonPanelReviewersEmpty},
		{"oversized panel", `{"panel":{"reviewers":` + big + `,"author_remediator":` + seat + `}}`, ReasonPanelTooLarge},
		{"incomplete seat", `{"panel":{"reviewers":[{"adapter":"fake"}],"author_remediator":` + seat + `}}`, ReasonPanelSeatIncomplete},
		{"no adjudicator", `{"panel":{"reviewers":[` + seat + `]}}`, ReasonPanelAdjudicatorNeeded},
		{"role seat effort", `{"panel":{"reviewers":[` + seat + `],"author_remediator":{"adapter":"fake","model":"m1","effort":"high"}}}`, ReasonPanelRoleEffort},
		{"adapter not launched", `{"panel":{"reviewers":[{"adapter":"codex-cli","model":"x"}],"author_remediator":` + seat + `}}`, ReasonPanelAdapterNotNamed},
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
			if rec.called {
				t.Error("the Manager must never be reached by an invalid panel (fail-closed, pre-spend)")
			}
		})
	}
}

// A turn on an agent launched with no adapter is refused, and the refusal names the launch flag.
func TestPromptMeta_NoLaunchedAdapterNamesTheFlag(t *testing.T) {
	rec := &selectionReviewer{}
	r := runServer(t, &Server{Manager: rec, Caps: review.SurfaceCaps{FileRead: true}},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"report"}}`)
	e := rpcErr(t, r[0])
	data, _ := e["data"].(map[string]any)
	if data["reasonCode"] != ReasonPanelNoAdapters {
		t.Errorf("data = %v, want reasonCode %q", e["data"], ReasonPanelNoAdapters)
	}
	if msg, _ := e["message"].(string); !strings.Contains(msg, "--adapter") || !strings.Contains(msg, launchflags.EnvVar) {
		t.Errorf("message = %q, want it to name --adapter and %s", msg, launchflags.EnvVar)
	}
	if rec.called {
		t.Error("the Manager must not run on an agent with no adapter")
	}
}

// TestPromptMeta_ShapeIsStrict: a panel is the role object, and nothing else decodes. A `profile`
// key, the bare-array form, and a seat carrying an adapter definition (`path`) are all malformed
// requests — a turn uses the adapters named at launch and never introduces one.
func TestPromptMeta_ShapeIsStrict(t *testing.T) {
	for name, meta := range map[string]string{
		"profile key":        `{"profile":"any"}`,
		"array panel":        `{"panel":[{"adapter":"fake","model":"m1"}]}`,
		"adapter definition": `{"panel":{"reviewers":[{"adapter":"fake","model":"m1","path":"/usr/bin/evil"}],"author_remediator":{"adapter":"fake","model":"m1"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &selectionReviewer{}
			r := serve(t, rec,
				`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"report","_meta":{"reviewmesh":`+meta+`}}}`)
			e := rpcErr(t, r[0])
			if e["code"] != float64(codeInvalidParams) {
				t.Fatalf("code = %v, want %d", e["code"], codeInvalidParams)
			}
			if rec.called {
				t.Error("a malformed panel must never reach the Manager")
			}
		})
	}
}
