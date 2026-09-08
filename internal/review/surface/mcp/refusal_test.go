package mcp_test

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// The MCP half of the partial-refusal contract (design §13.4.3).
//
// A remediation that committed and refused one finding for a protected path must NOT read as a
// clean success. `state` deliberately stays "complete" — the write did complete, and adding a
// fifth value to one of the three run-status vocabularies already in this tree would make every
// consumer's exhaustive switch wrong. The coarse signal goes where a consumer reads it without
// parsing anything: `isError`.
//
// AGAINST THE OLD CODE EVERY TEST HERE FAILS: `isError` was false, and the payload carried no
// `outcome`, no `counts` and no `refusals` at all.

// oneRefusal is the protected-path refusal the fake remediation reports.
func oneRefusal() []review.ApplyRefusal {
	return []review.ApplyRefusal{{
		Fingerprint: "sha1:c0ffee", File: ".env",
		Reason: review.ApplyRefusalProtectedPath, FindingID: "F2",
	}}
}

func TestRemediate_PartialRefusal_IsErrorAndCarriesTheSplit(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{refusals: oneRefusal()}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "output": "apply", "allowWrite": true})
	if res.rpc != nil {
		t.Fatalf("a domain outcome must not be a protocol error: %+v", res.rpc)
	}
	// THE COARSE SIGNAL. A consumer that reads nothing else must not conclude this was clean.
	if !res.isError {
		t.Fatal("a partial refusal must ride isError: true — under-signalling makes a caller believe findings were applied that were not")
	}
	// …and `state` is still "complete", because the commit succeeded. "halted" would be
	// unambiguous about not-clean and catastrophically wrong about the facts.
	if got, _ := res.structured["state"].(string); got != "complete" {
		t.Errorf("state = %q, want complete — the write committed", got)
	}
	if got, _ := res.structured["outcome"].(string); got != review.OutcomePartialRefusal {
		t.Errorf("outcome = %q, want %q", got, review.OutcomePartialRefusal)
	}
	if got, _ := res.structured["reasonCode"].(string); got != run.ReasonApplyRefusedProtectedPath {
		t.Errorf("reasonCode = %q, want %q", got, run.ReasonApplyRefusedProtectedPath)
	}
	counts, _ := res.structured["counts"].(map[string]any)
	if counts == nil {
		t.Fatalf("the result must carry counts, got %+v", res.structured)
	}
	if refused, _ := counts["refused"].(float64); int(refused) != 1 {
		t.Errorf("counts.refused = %v, want 1", counts["refused"])
	}
	if applied, _ := counts["applied"].(float64); int(applied) != 1 {
		t.Errorf("counts.applied = %v, want 1 — the rest of the run must still have been written", counts["applied"])
	}
	// The refusals array, keyed on the HOST-COMPUTED fingerprint.
	refusals, _ := res.structured["refusals"].([]any)
	if len(refusals) != 1 {
		t.Fatalf("refusals = %+v, want one", res.structured["refusals"])
	}
	row, _ := refusals[0].(map[string]any)
	if row["fingerprint"] != "sha1:c0ffee" {
		t.Errorf("refusal fingerprint = %v, want the host-computed value — a model-authored id would let a model steer a follow-up selection", row["fingerprint"])
	}
	if row["file"] != ".env" || row["reason"] != review.ApplyRefusalProtectedPath {
		t.Errorf("refusal = %v, want .env/protected_path", row)
	}
	if _, leaked := row["findingId"]; leaked {
		t.Error("the model-authored finding id must not be offered as a key on this surface")
	}
	// The receipt names the refused finding and still says the write completed.
	receipt, _ := res.structured["receipt"].(map[string]any)
	if receipt == nil {
		t.Fatal("the result must carry the receipt")
	}
	if receipt["status"] != "complete" {
		t.Errorf("receipt.status = %v, want complete", receipt["status"])
	}
	if committed, _ := receipt["committed"].(bool); !committed {
		t.Error("receipt.committed = false; the write DID reach the live tree")
	}
	na, _ := receipt["notApplied"].([]any)
	if len(na) != 1 {
		t.Fatalf("receipt.notApplied = %+v, want the refused finding", receipt["notApplied"])
	}
	if r0, _ := na[0].(map[string]any); r0["reason"] != review.ApplyRefusalProtectedPath {
		t.Errorf("receipt.notApplied[0].reason = %v, want protected_path", na[0])
	}
	// THE TEXT CHANNEL LEADS WITH THE REFUSAL. Some clients show the model nothing else, and a
	// refusal buried under the applied summary is a refusal nobody reads.
	first := strings.SplitN(res.text, "\n", 2)[0]
	if !strings.Contains(first, "REFUSED") || !strings.Contains(first, ".env") {
		t.Errorf("the first content line must name the refusal and the path, got %q\nfull text:\n%s", first, res.text)
	}
}

// TestRunStatus_PartialRefusal_CannotReadAsClean is the polling path. A caller that never fetches
// `run_result` sees only `run_status`, whose `state` is "complete" and always will be — so the
// two facts that distinguish a partially-refused run are repeated there.
func TestRunStatus_PartialRefusal_CannotReadAsClean(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{refusals: oneRefusal()}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	rem := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "output": "apply", "allowWrite": true})
	remID, _ := rem.structured["runId"].(string)
	if remID == "" {
		t.Fatalf("the remediation must return a runId: %+v", rem.structured)
	}

	st := c.tool(t, "review_run_status", map[string]any{"runId": remID})
	if st.rpc != nil {
		t.Fatalf("run_status failed: %+v", st.rpc)
	}
	if got, _ := st.structured["state"].(string); got != "complete" {
		t.Errorf("state = %q, want complete", got)
	}
	if got, _ := st.structured["outcome"].(string); got != review.OutcomePartialRefusal {
		t.Errorf("run_status.outcome = %q, want %q — a poller that never fetches run_result must not read this as clean", got, review.OutcomePartialRefusal)
	}
	if got, _ := st.structured["refusedCount"].(float64); int(got) != 1 {
		t.Errorf("run_status.refusedCount = %v, want 1", st.structured["refusedCount"])
	}
	if !strings.Contains(st.text, "REFUSED") {
		t.Errorf("the text channel must say so too, got %q", st.text)
	}
}

// TestRunResult_PartialRefusal_ReplaysIsError pins that COLLECTING the answer later is the same
// answer. The call that pays for a write is often not the call that collects it, and a replay
// that dropped `isError` would hand the collector a clean-looking success.
func TestRunResult_PartialRefusal_ReplaysIsError(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{refusals: oneRefusal()}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))
	runID := reportRun(t, c, ws)
	rem := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "output": "apply", "allowWrite": true})
	remID, _ := rem.structured["runId"].(string)

	got := c.tool(t, "review_run_result", map[string]any{"runId": remID})
	if !got.isError {
		t.Fatal("run_result must replay isError — a collector must not be told the run was clean")
	}
	if oc, _ := got.structured["outcome"].(string); oc != review.OutcomePartialRefusal {
		t.Errorf("outcome = %q, want %q", oc, review.OutcomePartialRefusal)
	}
}

// TestRemediate_NoRefusals_StaysClean is the blast radius: an ordinary remediation is untouched.
// `isError` has to mean something, which requires the clean case to stay clean.
func TestRemediate_NoRefusals_StaysClean(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Roots, s.AllowRemediate = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "output": "apply", "allowWrite": true})
	if res.isError {
		t.Fatalf("a clean remediation must not ride isError: %+v", res.structured)
	}
	if got, _ := res.structured["outcome"].(string); got != review.OutcomeApplied {
		t.Errorf("outcome = %q, want %q", got, review.OutcomeApplied)
	}
	if _, present := res.structured["refusals"]; present {
		t.Error("`refusals` must be absent when nothing was refused")
	}
	if strings.Contains(res.text, "REFUSED") {
		t.Errorf("the text channel must not manufacture a refusal:\n%s", res.text)
	}
}
