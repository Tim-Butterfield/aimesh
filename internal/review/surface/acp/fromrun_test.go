package acp

// FROM-RUN EXECUTION, proven end to end: a real run.Manager behind real JSON-RPC frames, a real
// workspace, and a real live write.
//
// THE PROPERTY UNDER TEST is the one ACP could not make before: **the applied set is the inspected
// set**. Turn 1 reports and hands back a run handle plus a fingerprint per accepted finding; turn 2
// applies THAT run's decisions. The difference from a re-adjudicating apply is only visible when the
// two would disagree, so these tests make them disagree on purpose: after turn 1, the reviewer is
// switched to one that raises nothing, and a control report turn proves it. A turn 2 that re-reviewed
// would then find nothing to write.
//
// AGAINST THE OLD CODE:
//   - TestACPFromRun_TheAppliedSetIsTheInspectedSet fails outright. Turn 2 ran its own governed
//     cycle, the silenced panel raised no finding, the selector matched nothing, and the turn was
//     refused with `apply_selection_matched_nothing` having written nothing at all.
//   - TestACPFromRun_AHandleThisAgentDidNotProduceIsRefused fails: the handle was recorded and never
//     resolved, so an arbitrary path ran a full review-and-write cycle and wrote to the workspace.
//   - TestACPFromRun_AStaleTreeHaltsRatherThanWrites fails, and how it failed is the point: turn 2
//     re-reviewed the CHANGED tree and answered `stopReason: end_turn`, `outcome: nothing_applied`.
//     A clean-looking turn, with no signal anywhere that the tree had moved on from the decisions the
//     host actually read. A stored set pinned to the tree it was decided against halts instead.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// switchingReviewer is the reviewer lane, with a switch. It answers as the deterministic fake until
// `silence()` is called, after which it approves with NO findings.
//
// That switch is the whole experiment: it makes "what a fresh panel would say now" differ from "what
// the panel said in turn 1", so a write turn that replays the stored set and a write turn that
// re-reviews produce visibly different outcomes.
type switchingReviewer struct {
	valid  model.Adapter
	empty  model.Adapter
	silent atomic.Bool
}

func (s *switchingReviewer) Name() string              { return "fake" }
func (s *switchingReviewer) Available() (bool, string) { return true, "ok" }
func (s *switchingReviewer) silence()                  { s.silent.Store(true) }

// Evidence is the OPTIONAL identity capability (`interface{ Evidence() review.IdentityEvidence }`)
// the manager type-asserts on every lane's adapter. A seat whose adapter cannot state its tier is
// recorded as weak-identity, which no longer changes anything about its findings — but the tier is
// still projected, so declaring it here keeps this fixture's records honest.
func (s *switchingReviewer) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}

func (s *switchingReviewer) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	if s.silent.Load() {
		return s.empty.Invoke(ctx, c)
	}
	return s.valid.Invoke(ctx, c)
}

// applyHost is the author_remediator lane: it adjudicates the reviewer's finding as apply-worthy and
// returns a real anchored edit. Unlike gateHost (writepath_test.go) it never blocks — these tests are
// about WHICH set gets written, not about what can land mid-window.
type applyHost struct{}

func (applyHost) Name() string              { return "apply-host" }
func (applyHost) Available() (bool, string) { return true, "ok" }

func (applyHost) Invoke(_ context.Context, c model.Call) (model.Result, error) {
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

// fromRunHarness is acpApplyHarness's sibling: the same documented config opt-in
// (`surfaces.defaultModeBySurface.acp: apply`) and the same write-capable host, with a reviewer that
// can be silenced and a remediation lane that does not block.
func fromRunHarness(t *testing.T) (*Server, *switchingReviewer, string, string) {
	t.Helper()
	t.Setenv(fake.EnvVar, "1")
	t.Setenv("AIMESH_HOME", t.TempDir())

	ws := t.TempDir()
	file := filepath.Join(ws, "main.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Adapters["apply-host"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.ModelCatalog["apply-model"] = config.CatalogEntry{
		Provider: "x", CanonicalModel: "mm",
		Adapters: map[string]config.AdapterModel{"apply-host": {ModelArg: "mm"}},
	}
	cfg.Profiles["acp-fromrun"] = config.Profile{
		Description:       "acp from-run harness",
		AdapterPreference: []string{"apply-host", "fake"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "apply-host", Model: "apply-model"},
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		},
	}
	cfg.DefaultProfile = "acp-fromrun"
	cfg.Surfaces.DefaultModeBySurface["acp"] = "apply"
	one := 1
	cfg.Review.MaxOuterCycles = &one

	rv := &switchingReviewer{valid: fake.New(fake.Valid), empty: fake.New(fake.Empty)}
	mgr := &run.Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": rv, "apply-host": applyHost{}},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
	srv := &Server{
		Manager:       mgr,
		Caps:          review.SurfaceCaps{FileRead: true, FileWrite: true},
		PolicyCeiling: review.ModeApply,
		Roots:         []string{os.TempDir(), ws},
	}
	return srv, rv, ws, file
}

// reportTurn runs one report turn at the given request id and returns its runDir plus the
// host-computed fingerprints of its accepted findings.
func (s *acpSession) reportTurn(id int, ws string) (string, []string) {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"mode":"report"}}`)
	resp := s.awaitID(float64(id))
	res, ok := resp["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("the report turn must succeed; got %v", resp)
	}
	meta, _ := res["_meta"].(map[string]any)
	rm, _ := meta["reviewmesh"].(map[string]any)
	runDir, _ := rm["runDir"].(string)
	var fps []string
	rows, _ := rm["accepted"].([]any)
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if fp, _ := row["fingerprint"].(string); fp != "" {
			fps = append(fps, fp)
		}
	}
	return runDir, fps
}

// applyTurnMeta sends one apply turn and returns its `_meta.reviewmesh`.
func (s *acpSession) applyTurnMeta(id int, ws, fromRun string, selection []string) map[string]any {
	s.t.Helper()
	sel := ""
	if selection != nil {
		quoted := make([]string, len(selection))
		for i, v := range selection {
			quoted[i] = strconv.Quote(v)
		}
		sel = `,"select":[` + strings.Join(quoted, ",") + `]`
	}
	s.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"fromRun":` + strconv.Quote(fromRun) + `,"mode":"apply","permissions":{"allowWrite":true}` + sel + `}}`)
	resp := s.awaitID(float64(id))
	res, ok := resp["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("the apply turn must succeed; got %v", resp)
	}
	meta, _ := res["_meta"].(map[string]any)
	rm, _ := meta["reviewmesh"].(map[string]any)
	return rm
}

// TestACPFromRun_TheAppliedSetIsTheInspectedSet is THE test this whole change exists for.
//
// Turn 1 reports and the host reads one accepted fingerprint. The panel is then silenced, and a
// CONTROL report turn proves it: a fresh adjudication now raises nothing at all. Turn 2 then applies
// turn 1's run, selecting that same fingerprint — and it writes, because the set it applies is the
// set that was inspected, not one this turn re-derived.
func TestACPFromRun_TheAppliedSetIsTheInspectedSet(t *testing.T) {
	srv, rv, ws, file := fromRunHarness(t)
	s := startACPSession(t, srv)
	s.newSession(ws)

	sourceRun, fps := s.reportTurn(2, ws)
	if sourceRun == "" || len(fps) != 1 {
		t.Fatalf("the report turn must hand back a runDir and exactly one accepted fingerprint; got %q %v", sourceRun, fps)
	}
	// The decision set is DURABLE — a file in the run directory, not an entry in a registry that
	// dies with the process. That is what a later turn (or a later process) reads.
	if _, err := os.Stat(filepath.Join(sourceRun, filepath.FromSlash(run.DecisionSetArtifact))); err != nil {
		t.Fatalf("a completed report run must record its decision set on disk: %v", err)
	}

	// THE CONTROL. Silence the panel and prove a fresh adjudication would now produce nothing, so
	// the assertion below cannot pass by a re-review happening to agree.
	rv.silence()
	if _, freshFPs := s.reportTurn(3, ws); len(freshFPs) != 0 {
		t.Fatalf("the control turn must raise nothing once the panel is silenced; got %v", freshFPs)
	}

	rm := s.applyTurnMeta(4, ws, sourceRun, fps)
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), "func main() { _ = 0 }") {
		t.Fatalf("the apply turn did not write the finding the report turn showed — the applied set is not the inspected set; file=%q", got)
	}
	if rm["applied"] != float64(1) {
		t.Errorf("_meta.reviewmesh.applied = %v, want 1", rm["applied"])
	}
	if rm["outcome"] != review.OutcomeApplied {
		t.Errorf("outcome = %v, want %q", rm["outcome"], review.OutcomeApplied)
	}
	// The SELECTION matched against the STORED set. On the old surface this same selector landed in
	// `unmatched`, because the fingerprint was measured against a fresh adjudication that raised
	// nothing.
	sel, _ := rm["selection"].(map[string]any)
	matched, _ := sel["matched"].([]any)
	unmatched, _ := sel["unmatched"].([]any)
	if len(matched) != 1 || len(unmatched) != 0 {
		t.Errorf("selection = %v, want the selector matched against the stored set", sel)
	}
	// The receipt is the durable answer, and it is the REMEDIATION run's — a second run directory,
	// not the source run's.
	runDir, _ := rm["runDir"].(string)
	if runDir == "" || runDir == sourceRun {
		t.Fatalf("the write must have its own run directory; runDir=%q sourceRun=%q", runDir, sourceRun)
	}
	r := readACPReceipt(t, runDir)
	if !r.Committed || len(r.Files) != 1 || r.Files[0] != "main.go" {
		t.Fatalf("receipt = %+v, want a committed receipt naming main.go", r)
	}
	if r.SourceRunID != filepath.Base(sourceRun) {
		t.Errorf("receipt.sourceRunId = %q, want the run whose decisions were applied (%q)", r.SourceRunID, filepath.Base(sourceRun))
	}
}

// TestACPFromRun_AHandleThisAgentDidNotProduceIsRefused. The handle arrives from a peer, so it is
// verified against this agent's own artifact directory rather than trusted as a path — and every way
// of failing answers alike, so a peer gains no existence oracle over paths outside it.
func TestACPFromRun_AHandleThisAgentDidNotProduceIsRefused(t *testing.T) {
	srv, _, ws, file := fromRunHarness(t)
	before, _ := os.ReadFile(file)
	s := startACPSession(t, srv)
	s.newSession(ws)

	// A real, existing, readable directory that this agent did not produce; a plausible-looking
	// fabrication; and a traversal out of the artifact directory.
	elsewhere := t.TempDir()
	for i, handle := range []string{elsewhere, "/runs/prior-report-run", filepath.Join(elsewhere, "..")} {
		id := 10 + i
		s.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
			strconv.Quote(ws) + `,"fromRun":` + strconv.Quote(handle) + `,"mode":"apply","permissions":{"allowWrite":true}}}`)
		resp := s.awaitID(float64(id))
		e, ok := resp["error"].(map[string]any)
		if !ok {
			t.Fatalf("handle %q must be refused, got %v", handle, resp)
		}
		if e["code"].(float64) != codeInvalidParams {
			t.Errorf("handle %q: code = %v, want %d", handle, e["code"], codeInvalidParams)
		}
		data, _ := e["data"].(map[string]any)
		if data["reasonCode"] != run.ReasonRunHandleUnknown {
			t.Errorf("handle %q: reasonCode = %v, want %q — one code for every way of failing, or the refusal is an oracle",
				handle, data["reasonCode"], run.ReasonRunHandleUnknown)
		}
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("a refused handle wrote to the live workspace:\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestACPFromRun_AStaleTreeHaltsRatherThanWrites. A stored decision set describes a workspace; if
// that workspace has moved on, the set describes something that no longer exists. The pins the
// SOURCE run captured are re-verified before the write window opens, so the refusal costs a round
// trip rather than a run — and the human's edit survives.
func TestACPFromRun_AStaleTreeHaltsRatherThanWrites(t *testing.T) {
	srv, _, ws, file := fromRunHarness(t)
	s := startACPSession(t, srv)
	s.newSession(ws)

	sourceRun, _ := s.reportTurn(2, ws)
	if sourceRun == "" {
		t.Fatal("the report turn must hand back its runDir")
	}
	const human = "package main\n\nfunc main() { /* a human was here */ }\n"
	if err := os.WriteFile(file, []byte(human), 0o644); err != nil {
		t.Fatal(err)
	}

	s.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"fromRun":` + strconv.Quote(sourceRun) + `,"mode":"apply","permissions":{"allowWrite":true}}}`)
	resp := s.awaitID(3)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("a stale decision set must halt the turn, got %v", resp)
	}
	data, _ := e["data"].(map[string]any)
	if data["reasonCode"] != run.ReasonStaleDecisionSet {
		t.Fatalf("reasonCode = %v, want %q", data["reasonCode"], run.ReasonStaleDecisionSet)
	}
	if after, _ := os.ReadFile(file); string(after) != human {
		t.Fatalf("the human's edit was overwritten:\nwant: %q\ngot:  %q", human, after)
	}
	// The refusal is RECORDED: a receipt exists on every path, and it says nothing was committed.
	runDir, _ := data["runDir"].(string)
	if runDir == "" {
		t.Fatalf("a halted from-run write must carry its own runDir, got %v", data)
	}
	r := readACPReceipt(t, runDir)
	if r.Status != "halted" || r.Committed {
		t.Fatalf("receipt = %+v, want halted with no commit", r)
	}
}

// TestACPFromRun_RunFormingArgumentsAreRefusedNotIgnored. Each of `profile`, `panel` and
// `authority` names a governance input to an adjudication that has already happened. Silently
// ignoring one would tell a host its panel or its declared intent had been honoured when nothing was
// re-reviewed at all. This is the same rule MCP applies to `review_remediate {fromRun}`.
func TestACPFromRun_RunFormingArgumentsAreRefusedNotIgnored(t *testing.T) {
	srv, _, ws, _ := fromRunHarness(t)
	s := startACPSession(t, srv)
	s.newSession(ws)
	sourceRun, _ := s.reportTurn(2, ws)

	for i, meta := range []string{
		`{"reviewmesh":{"profile":"acp-fromrun"}}`,
		`{"reviewmesh":{"panel":[{"adapter":"fake","model":"fake-model"}]}}`,
		`{"reviewmesh":{"authority":[{"name":"spec","path":"spec.md"}]}}`,
	} {
		id := 20 + i
		s.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
			strconv.Quote(ws) + `,"fromRun":` + strconv.Quote(sourceRun) + `,"mode":"apply","permissions":{"allowWrite":true},"_meta":` + meta + `}}`)
		resp := s.awaitID(float64(id))
		e, ok := resp["error"].(map[string]any)
		if !ok {
			t.Fatalf("%s must be refused on a fromRun turn, got %v", meta, resp)
		}
		data, _ := e["data"].(map[string]any)
		if data["reasonCode"] != reasonFromRunArgsRefused {
			t.Errorf("%s: reasonCode = %v, want %q", meta, data["reasonCode"], reasonFromRunArgsRefused)
		}
	}
}

// TestACPFromRun_ANamedWorkspaceMustBeTheOneTheSourceRunJudged. A `fromRun` write is applied to the
// tree its decisions were made against. A turn that NAMED a different one is told so rather than
// having its request silently redirected — the write would otherwise land somewhere the caller did
// not ask for and would still report success.
func TestACPFromRun_ANamedWorkspaceMustBeTheOneTheSourceRunJudged(t *testing.T) {
	srv, _, ws, _ := fromRunHarness(t)
	s := startACPSession(t, srv)
	s.newSession(ws)
	sourceRun, _ := s.reportTurn(2, ws)

	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(other) + `,"fromRun":` + strconv.Quote(sourceRun) + `,"mode":"apply","permissions":{"allowWrite":true}}}`)
	resp := s.awaitID(3)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("naming a different workspace must be refused, got %v", resp)
	}
	data, _ := e["data"].(map[string]any)
	if data["reasonCode"] != reasonFromRunWorkspaceMismatch {
		t.Errorf("reasonCode = %v, want %q", data["reasonCode"], reasonFromRunWorkspaceMismatch)
	}
	if b, _ := os.ReadFile(filepath.Join(other, "main.go")); string(b) != "package main\n" {
		t.Errorf("nothing may be written to a workspace the source run never judged; got %q", b)
	}
}

// TestACPSession_ATurnAnsweredIsATurnFinished. The two-turn write contract has a host send turn 2
// the moment turn 1's response arrives — that is the whole point of riding the run handle on a
// response. So the response must not arrive while the session still counts the turn as in flight.
//
// It used to. The session slot was released in a `defer`, which runs AFTER the response is written,
// so a host that sequenced its turns exactly as documented raced a cleanup it cannot observe and
// intermittently got `-32600 session busy` for a correct sequence. This test found it, and the fix
// is the ordering in ServeFramed's session/prompt goroutine.
//
// WHAT THIS TEST CAN AND CANNOT PROVE, stated because a reader will otherwise assume the stronger
// thing: the defect is a RACE, so this samples it rather than deciding it. Against the old code it
// fails intermittently — it did, which is how the defect was found — and a green run is weaker
// evidence than a red one. The property itself is guaranteed by construction, not by this test.
func TestACPSession_ATurnAnsweredIsATurnFinished(t *testing.T) {
	srv := &Server{
		Manager: &fakeRemediator{},
		Caps:    review.SurfaceCaps{FileRead: true, FileWrite: true},
		Roots:   harnessRoots(t),
	}
	ws := t.TempDir()
	s := startACPSession(t, srv)
	s.newSession(ws)
	for id := 2; id < 60; id++ {
		s.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
			strconv.Quote(ws) + `,"mode":"report"}}`)
		resp := s.awaitID(float64(id))
		if e, ok := resp["error"].(map[string]any); ok {
			t.Fatalf("turn %d was refused after the previous turn had already ANSWERED: %v", id, e)
		}
	}
}

// TestACPFromRun_DecisionSetIsNotRecordedForAWriteRun. A patch/apply run has already applied its own
// accepted set; recording it as a from-run source would offer a second application of decisions
// nobody re-read. Report runs, and only report runs, are remediable.
func TestACPFromRun_DecisionSetIsNotRecordedForAWriteRun(t *testing.T) {
	srv, _, ws, _ := fromRunHarness(t)
	s := startACPSession(t, srv)
	s.newSession(ws)

	sourceRun, fps := s.reportTurn(2, ws)
	rm := s.applyTurnMeta(3, ws, sourceRun, fps)
	writeRun, _ := rm["runDir"].(string)
	if writeRun == "" {
		t.Fatal("the apply turn must carry its own runDir")
	}
	if _, err := os.Stat(filepath.Join(writeRun, filepath.FromSlash(run.DecisionSetArtifact))); err == nil {
		t.Error("a write run must not record a decision set: applying it again would re-apply decisions nobody re-read")
	}
	// And the surface says so by name rather than failing obscurely.
	s.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"fromRun":` + strconv.Quote(writeRun) + `,"mode":"apply","permissions":{"allowWrite":true}}}`)
	e, _ := s.awaitID(4)["error"].(map[string]any)
	data, _ := e["data"].(map[string]any)
	if data["reasonCode"] != run.ReasonRunHandleUnknown {
		t.Errorf("reasonCode = %v, want %q", data["reasonCode"], run.ReasonRunHandleUnknown)
	}
}
