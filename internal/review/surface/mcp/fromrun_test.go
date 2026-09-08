package mcp_test

// FROM-RUN, RESOLVED FROM DISK — the MCP half of the surface-parity invariant, proven against a real
// run.Manager, a real workspace, a real decision-set artifact and a real live write.
//
// THE DEFECT THESE CLOSE. `review_remediate {fromRun}` resolved a handle from the in-memory run
// registry and nowhere else. The registry is bounded by count and TTL and dies with the process, so a
// handle naming a run this server really did produce — and whose decision set is sitting in that
// run's own directory — was answered `unknown_run_id`. ACP honours the same handle, from the same
// file, for the same run. Same handle, same recorded decisions, two answers depending on the surface.
//
// HOW A RESTART IS STAGED. A restarted server is a NEW registry over the SAME artifact directory, so
// each test drives two `mcp.Server` values over one `run.Manager`: the first reports, the second
// remediates. Nothing is deleted and no clock is moved — the second server simply never saw the run,
// which is exactly what a restarted one has not.
//
// AGAINST THE OLD CODE every test in this file FAILS, and six of the seven fail at the same place —
// the durable-handle assertion in `report`, which is the root of the defect: the run id a client was
// handed and the directory the decision set was recorded in were two independently minted names, so
// the only handle a client held named nothing on disk and the fallback could never have fired for a
// real client at all. `_AnInlineRunIsStillRefusedByNameFromDisk` gets past that (an inline run has no
// workspace to pin) and fails on the refusal itself: `unknown_run_id` where the caller must be told
// `inline_workspace_not_remediable`.
//
// Two of them are PRESERVATION guards over the new path rather than proofs of new behaviour —
// `_AHandleThisAgentDidNotProduceIsRefused` and `_AWriteRunIsNotAFromRunSource`. What they hold is
// that adding a reader added no way to point a governed write at an arbitrary directory, and no way
// to apply a run that never adjudicated anything. They would pass vacuously on the old code if the
// handle assertion did not run first; they are here for what they would catch in the new one.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// silenceableReviewer is the reviewer lane, with a switch: it answers as the deterministic fake until
// `silence()` is called, after which it approves with NO findings.
//
// That switch is the experiment. It makes "what a fresh panel would say now" differ from "what the
// panel said in the report run", so a write that replays the STORED set and a write that re-derived
// one produce visibly different outcomes. (It is the ACP suite's `switchingReviewer`; the two live in
// different packages, and a shared copy would couple two surfaces' fixtures for four lines.)
type silenceableReviewer struct {
	valid  model.Adapter
	empty  model.Adapter
	silent atomic.Bool
}

func (s *silenceableReviewer) Name() string              { return "fake" }
func (s *silenceableReviewer) Available() (bool, string) { return true, "ok" }
func (s *silenceableReviewer) silence()                  { s.silent.Store(true) }

// Evidence is the OPTIONAL identity capability the manager type-asserts on every lane's adapter. A
// seat whose adapter cannot state its evidence tier is WEAK-IDENTITY, and a finding supported only by
// weak-identity seats is quarantined — so a wrapper that forgot this would produce a run with
// findings and no accepted set, and every assertion below would fail for an unrelated reason.
func (s *silenceableReviewer) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}

func (s *silenceableReviewer) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	if s.silent.Load() {
		return s.empty.Invoke(ctx, c)
	}
	return s.valid.Invoke(ctx, c)
}

// writingHost is the author_remediator lane: it adjudicates the reviewer's finding as apply-worthy
// and returns a real anchored edit, so an apply produces an observable change to a real file.
type writingHost struct{}

func (writingHost) Name() string              { return "write-host" }
func (writingHost) Available() (bool, string) { return true, "ok" }

func (writingHost) Invoke(_ context.Context, c model.Call) (model.Result, error) {
	ok := func(js string) (model.Result, error) {
		return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg),
			Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
	}
	switch c.Phase {
	case string(review.PhaseAdjudicate):
		return ok(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"f1","validity":"valid","decisionState":"applied","reasoning":"apply the fix"}]}`)
	case string(review.PhaseRemediate):
		return ok(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","edits":[{"file":"main.go","anchor":"func main() {}","replacement":"func main() { _ = 0 }","occurrence":0}]}`)
	default:
		return ok(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_author_review","verdict":"approve","findings":[]}`)
	}
}

// fromRunFixture is a real, write-capable run.Manager over a real workspace. Everything the MCP
// tests elsewhere in this package fake — the run directory, the decision-set artifact, the base-hash
// pins — is genuine here, because the on-disk path is the thing under test and a fake that
// "resolved" a handle would assert what it is meant to prove.
type fromRunFixture struct {
	mgr  *run.Manager
	rv   *silenceableReviewer
	art  string // the artifact directory: every run's own directory lives directly under it
	ws   string
	file string
}

func newFromRunFixture(t *testing.T) *fromRunFixture {
	t.Helper()
	t.Setenv(fake.EnvVar, "1")

	ws := t.TempDir()
	file := filepath.Join(ws, "main.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Adapters["write-host"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.ModelCatalog["write-model"] = config.CatalogEntry{
		Provider: "x", CanonicalModel: "mm",
		Adapters: map[string]config.AdapterModel{"write-host": {ModelArg: "mm"}},
	}
	cfg.Profiles["mcp-fromrun"] = config.Profile{
		Description:       "mcp from-run harness",
		AdapterPreference: []string{"write-host", "fake"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "write-host", Model: "write-model"},
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		},
	}
	cfg.DefaultProfile = "mcp-fromrun"
	// The write-authority ceiling is RAISED by the capability, never bypassed — the same grant
	// `aimesh review mcp --allow-remediate` makes, so the Manager resolves exactly the policy the tool
	// list was built from.
	cfg = config.WithSurfaceCapability(cfg, "mcp", config.CapabilityAllowRemediate)
	one := 1
	cfg.Review.MaxOuterCycles = &one

	rv := &silenceableReviewer{valid: fake.New(fake.Valid), empty: fake.New(fake.Empty)}
	art := t.TempDir()
	return &fromRunFixture{
		mgr: &run.Manager{
			Cfg:         cfg,
			Adapters:    map[string]model.Adapter{"fake": rv, "write-host": writingHost{}},
			ArtifactDir: art, TempBase: t.TempDir(),
		},
		rv: rv, art: art, ws: ws, file: file,
	}
}

// server returns a NEW server over the fixture's manager. Calling it twice is how a restart is
// staged: the second server has an empty run registry and the same artifact directory on disk.
func (f *fromRunFixture) server(t *testing.T) *client {
	t.Helper()
	return serve(t, newServer(t, f.mgr, func(s *mcp.Server) {
		s.Roots, s.AllowRemediate = []string{f.ws}, true
		s.PolicyCeiling = review.ModeApply
	}))
}

// report runs one report turn and returns its runId — the only handle this surface ever hands out.
func (f *fromRunFixture) report(t *testing.T, c *client) string {
	t.Helper()
	res := c.tool(t, "review_report", map[string]any{"workspace": f.ws})
	if res.isError || res.rpc != nil {
		t.Fatalf("the report turn must succeed: %+v %+v", res.structured, res.rpc)
	}
	id, _ := res.structured["runId"].(string)
	if id == "" {
		t.Fatalf("the report must return a runId: %+v", res.structured)
	}
	// THE HANDLE IS DURABLE, and this is what makes it so: the id a client is handed NAMES the run's
	// own directory, so the decision set recorded there is findable from the only thing the client
	// holds. Without this the fallback below could never fire for a real client.
	if _, err := os.Stat(filepath.Join(f.art, id, filepath.FromSlash(run.DecisionSetArtifact))); err != nil {
		t.Fatalf("the runId a client is handed must name the run directory its decision set is recorded in: %v", err)
	}
	return id
}

func (f *fromRunFixture) body(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// applied reports whether the workspace carries the edit the accepted finding asks for.
func (f *fromRunFixture) applied(t *testing.T) bool {
	t.Helper()
	return strings.Contains(f.body(t), "func main() { _ = 0 }")
}

func remediate(t *testing.T, c *client, fromRun string) toolResult {
	t.Helper()
	return c.tool(t, "review_remediate", map[string]any{
		"fromRun": fromRun, "output": "apply", "allowWrite": true,
	})
}

// TestMCPFromRun_AnEvictedHandleStillAppliesFromDisk is the parity defect itself. The run was
// produced by this agent and recorded its decision set; only the in-memory registry entry is gone.
// An ACP agent honours that handle, and now so does this one — from the same file.
func TestMCPFromRun_AnEvictedHandleStillAppliesFromDisk(t *testing.T) {
	f := newFromRunFixture(t)
	sourceRun := f.report(t, f.server(t))

	res := remediate(t, f.server(t), sourceRun)
	if res.rpc != nil {
		t.Fatalf("a from-disk apply must not be a protocol error: %+v", res.rpc)
	}
	if res.isError {
		t.Fatalf("a handle the registry no longer holds must still apply: %+v", res.structured)
	}
	if !f.applied(t) {
		t.Fatalf("nothing was written to the live workspace; file = %q", f.body(t))
	}
	receipt, _ := res.structured["receipt"].(map[string]any)
	if receipt == nil {
		t.Fatalf("the result must carry the receipt: %+v", res.structured)
	}
	if committed, _ := receipt["committed"].(bool); !committed {
		t.Fatalf("apply must report a commit: %+v", receipt)
	}
	// The receipt names the run whose decisions were applied — two runs, two handles, neither
	// standing in for the other.
	if receipt["sourceRunId"] != sourceRun {
		t.Errorf("receipt.sourceRunId = %v, want the source run %q", receipt["sourceRunId"], sourceRun)
	}
	if src, _ := res.structured["sourceRunId"].(string); src != sourceRun {
		t.Errorf("sourceRunId = %q, want %q", src, sourceRun)
	}
	// And the pins the SOURCE run captured were the ones verified — not pins re-derived now, which
	// would make a changed file look unchanged.
	if n, _ := receipt["baseHashesVerified"].(float64); n < 1 {
		t.Errorf("baseHashesVerified = %v, want the source run's pins to have been checked", receipt["baseHashesVerified"])
	}
}

// TestMCPFromRun_TheAppliedSetIsTheInspectedSet is the property the whole `fromRun` form exists for,
// asserted on the path that now also reaches disk: the write applies the set a human READ, never one
// re-derived at write time.
//
// The report turn raises one accepted finding. The panel is then SILENCED and a control turn proves
// a fresh adjudication would now produce nothing at all. The apply — on a server that never saw the
// source run — still writes, because what it applies came off the source run's own record.
func TestMCPFromRun_TheAppliedSetIsTheInspectedSet(t *testing.T) {
	f := newFromRunFixture(t)
	first := f.server(t)
	sourceRun := f.report(t, first)

	// THE CONTROL. Once silenced, a fresh run produces no accepted set, so the assertion below
	// cannot pass by a re-review happening to agree.
	f.rv.silence()
	control := c2Structured(t, first, f.ws)
	if n, _ := control["findings"].([]any); len(n) != 0 {
		t.Fatalf("the control turn must raise nothing once the panel is silenced; got %v", control["findings"])
	}

	if res := remediate(t, f.server(t), sourceRun); res.isError || res.rpc != nil {
		t.Fatalf("the apply must succeed from the stored set: %+v %+v", res.structured, res.rpc)
	}
	if !f.applied(t) {
		t.Fatalf("the apply did not write the finding the report turn showed — the applied set is not the inspected set; file = %q", f.body(t))
	}
}

// c2Structured runs a second report turn and returns its structured payload.
func c2Structured(t *testing.T, c *client, ws string) map[string]any {
	t.Helper()
	res := c.tool(t, "review_report", map[string]any{"workspace": ws})
	if res.isError || res.rpc != nil {
		t.Fatalf("the control report turn must succeed: %+v %+v", res.structured, res.rpc)
	}
	return res.structured
}

// TestMCPFromRun_AStaleTreeHaltsRatherThanWrites. A stored decision set describes a workspace; if
// that workspace has moved on, the set describes something that no longer exists. The pins the SOURCE
// run captured are re-verified before the write window opens, so the refusal costs a round trip
// rather than a run — and the human's edit survives.
func TestMCPFromRun_AStaleTreeHaltsRatherThanWrites(t *testing.T) {
	f := newFromRunFixture(t)
	sourceRun := f.report(t, f.server(t))

	const human = "package main\n\nfunc main() { /* a human was here */ }\n"
	if err := os.WriteFile(f.file, []byte(human), 0o644); err != nil {
		t.Fatal(err)
	}

	res := remediate(t, f.server(t), sourceRun)
	if res.rpc != nil {
		t.Fatalf("a halt must not be a protocol error: %+v", res.rpc)
	}
	if !res.isError {
		t.Fatalf("a stale decision set must halt the call: %+v", res.structured)
	}
	if code, _ := res.structured["reasonCode"].(string); code != run.ReasonStaleDecisionSet {
		t.Fatalf("reasonCode = %q, want %q — the caller must learn the tree moved on, not that the run is unknown",
			res.structured["reasonCode"], run.ReasonStaleDecisionSet)
	}
	if got := f.body(t); got != human {
		t.Fatalf("the human's edit was overwritten:\nwant: %q\ngot:  %q", human, got)
	}
	// The refusal is RECORDED: a receipt exists on every path, and it says nothing was committed.
	receipt, _ := res.structured["receipt"].(map[string]any)
	if committed, _ := receipt["committed"].(bool); committed {
		t.Fatalf("a halted write must record no commit: %+v", receipt)
	}
}

// TestMCPFromRun_ASecondApplyAfterARestartHalts is the RESTART SEAM, asserted rather than assumed.
//
// In-process, a second `review_remediate {fromRun: X}` returns the ORIGINAL receipt — the source-run
// guard, which lives in the registry (proved by TestRemediate_FromRunAppliesTheAcceptedSetAndIsIdempotent).
// That guard does not survive a restart, and this is what takes its place: the first apply changed
// the very files the stored set pins, so the second call HALTS on stale pins having written nothing.
// Both are refusals to double-write; a client must be able to tell them apart, so the codes differ
// and neither is dressed up as the other.
func TestMCPFromRun_ASecondApplyAfterARestartHalts(t *testing.T) {
	f := newFromRunFixture(t)
	sourceRun := f.report(t, f.server(t))

	if res := remediate(t, f.server(t), sourceRun); res.isError || res.rpc != nil {
		t.Fatalf("the first apply must succeed: %+v %+v", res.structured, res.rpc)
	}
	applied := f.body(t)

	res := remediate(t, f.server(t), sourceRun)
	if !res.isError {
		t.Fatalf("a second apply across a restart must not read as a clean write: %+v", res.structured)
	}
	if code, _ := res.structured["reasonCode"].(string); code != run.ReasonStaleDecisionSet {
		t.Errorf("reasonCode = %q, want %q", res.structured["reasonCode"], run.ReasonStaleDecisionSet)
	}
	if got := f.body(t); got != applied {
		t.Fatalf("the second apply wrote again:\nafter first:  %q\nafter second: %q", applied, got)
	}
}

// TestMCPFromRun_AHandleThisAgentDidNotProduceIsRefused. The handle arrives from a peer, so it is
// VERIFIED against this agent's own artifact directory rather than trusted — and every way of failing
// answers alike, so a peer gains no existence oracle over paths outside it.
//
// This is a PRESERVATION guard, and it is the one that matters most: on the old code nothing resolved
// at all, so it passed vacuously. What it now holds is that adding a reader did not add a way to
// point a governed write at an arbitrary directory.
func TestMCPFromRun_AHandleThisAgentDidNotProduceIsRefused(t *testing.T) {
	f := newFromRunFixture(t)
	f.report(t, f.server(t)) // a real run exists, so the artifact directory is populated
	before := f.body(t)

	elsewhere := t.TempDir()
	c := f.server(t)
	for _, handle := range []string{
		elsewhere,                        // a real, readable directory this agent did not produce
		"/runs/prior-report-run",         // a plausible-looking fabrication
		filepath.Join(elsewhere, ".."),   // a traversal
		f.art,                            // the artifact directory itself — not a run
		"../" + filepath.Base(elsewhere), // a traversal spelled as a run id
		filepath.Join(f.art, "nope"),     // a well-formed handle naming nothing
	} {
		res := remediate(t, c, handle)
		if res.rpc != nil {
			t.Fatalf("handle %q: an unresolvable handle is a domain refusal, not a protocol error: %+v", handle, res.rpc)
		}
		if !res.isError {
			t.Fatalf("handle %q must be refused, got %+v", handle, res.structured)
		}
		if code, _ := res.structured["reasonCode"].(string); code != "unknown_run_id" {
			t.Errorf("handle %q: reasonCode = %q, want %q — one code for every way of failing, or the refusal is an oracle",
				handle, code, "unknown_run_id")
		}
	}
	if after := f.body(t); after != before {
		t.Fatalf("a refused handle wrote to the live workspace:\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestMCPFromRun_AnInlineRunIsStillRefusedByNameFromDisk. The refusal ladder must not change SHAPE
// just because the answer now comes from disk: an inline run's content was materialized into a
// directory this server has since deleted, and the caller learns exactly that — with the same
// reasonCode the registry path reports — rather than an obscure failure on a vanished path.
func TestMCPFromRun_AnInlineRunIsStillRefusedByNameFromDisk(t *testing.T) {
	f := newFromRunFixture(t)
	c := f.server(t)
	res := c.tool(t, "review_report", map[string]any{
		"inlineWorkspace": map[string]any{"main.go": "package main\n\nfunc main() {}\n"},
	})
	if res.isError || res.rpc != nil {
		t.Fatalf("an inline review must work: %+v %+v", res.structured, res.rpc)
	}
	runID, _ := res.structured["runId"].(string)

	got := remediate(t, f.server(t), runID)
	if !got.isError {
		t.Fatalf("remediating an inline run must fail closed, got %+v", got.structured)
	}
	if code, _ := got.structured["reasonCode"].(string); code != run.ReasonInlineWorkspaceNotRemediable {
		t.Fatalf("reasonCode = %q, want %q — the on-disk path must report the same refusal the registry path does",
			code, run.ReasonInlineWorkspaceNotRemediable)
	}
}

// TestMCPFromRun_AWriteRunIsNotAFromRunSource. A patch/apply run has already applied its own accepted
// set; offering it as a source would be a second application of decisions nobody re-read. It records
// no decision set, so it resolves to nothing — the same answer ACP gives.
func TestMCPFromRun_AWriteRunIsNotAFromRunSource(t *testing.T) {
	f := newFromRunFixture(t)
	sourceRun := f.report(t, f.server(t))
	res := remediate(t, f.server(t), sourceRun)
	if res.isError || res.rpc != nil {
		t.Fatalf("the apply must succeed: %+v %+v", res.structured, res.rpc)
	}
	writeRun, _ := res.structured["runId"].(string)
	if writeRun == "" {
		t.Fatal("a remediation must carry its own runId")
	}
	again := remediate(t, f.server(t), writeRun)
	if !again.isError {
		t.Fatalf("a remediation run must never be a from-run source: %+v", again.structured)
	}
	if code, _ := again.structured["reasonCode"].(string); code != "unknown_run_id" {
		t.Errorf("reasonCode = %q, want unknown_run_id", code)
	}
}
