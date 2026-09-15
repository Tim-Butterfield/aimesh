package acp

// These tests run fromRun writes end to end: a real run.Manager behind JSON-RPC frames, a real
// workspace and a live write.
//
// The property under test is that the applied set is the inspected set. After the report turn the
// reviewer is silenced, and a control report turn shows it now raises nothing, so an apply turn that
// re-reviewed would have nothing to write.

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

// hostAdapter is the adapter the author_remediator seat names. It is a built-in recipe so the
// launch set can hold it; the manager maps it to an in-process seat, so no CLI runs.
const hostAdapter = "claude-code"

// hostPanelMeta is the _meta a report turn carries: the fake reviewer and the in-process host as
// adjudicator.
const hostPanelMeta = `"_meta":{"reviewmesh":{"panel":{"reviewers":[{"adapter":"fake","model":"fake-model"}],` +
	`"author_remediator":{"adapter":"` + hostAdapter + `","model":"host-model"}}}}`

// launchConfig returns the configuration a launch-configured agent builds: built-in defaults, no
// profiles, and apply reachable on agent surfaces.
func launchConfig() config.Config {
	cfg := config.Default()
	cfg.Adapters[hostAdapter] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.Surfaces.DefaultModeBySurface = map[string]string{"cli": "apply", "ci": "report", "acp": "apply", "mcp": "apply"}
	one := 1
	cfg.Review.MaxOuterCycles = &one // one write window, so the assertions are unambiguous
	return cfg
}

// switchingReviewer answers as the deterministic fake until silence is called, then approves with
// no findings.
type switchingReviewer struct {
	valid  model.Adapter
	empty  model.Adapter
	silent atomic.Bool
}

func (s *switchingReviewer) Name() string              { return "fake" }
func (s *switchingReviewer) Available() (bool, string) { return true, "ok" }
func (s *switchingReviewer) silence()                  { s.silent.Store(true) }

// Evidence implements the optional identity capability the manager checks on every adapter.
func (s *switchingReviewer) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}

func (s *switchingReviewer) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	if s.silent.Load() {
		return s.empty.Invoke(ctx, c)
	}
	return s.valid.Invoke(ctx, c)
}

// applyHost is the author_remediator seat: it accepts the reviewer's finding and returns an anchored
// edit. Unlike gateHost it never blocks.
type applyHost struct{}

func (applyHost) Name() string              { return hostAdapter }
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

// fromRunHarness returns an agent launched with --allow-writes over a write-capable host, with a
// reviewer that can be silenced and a non-blocking remediation seat.
func fromRunHarness(t *testing.T) (*Server, *switchingReviewer, string, string) {
	t.Helper()
	t.Setenv(fake.EnvVar, "1")
	t.Setenv("AIMESH_HOME", t.TempDir())

	ws := t.TempDir()
	file := filepath.Join(ws, "main.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rv := &switchingReviewer{valid: fake.New(fake.Valid), empty: fake.New(fake.Empty)}
	mgr := &run.Manager{
		Cfg:         launchConfig(),
		Adapters:    map[string]model.Adapter{"fake": rv, hostAdapter: applyHost{}},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
	srv := &Server{
		Manager:     mgr,
		Caps:        review.SurfaceCaps{FileRead: true, FileWrite: true},
		Adapters:    harnessAdapters(t),
		AllowWrites: true,
	}
	return srv, rv, ws, file
}

// reportTurn runs one report turn with request id and returns its runDir and the fingerprints of
// its accepted findings.
func (s *acpSession) reportTurn(id int, ws string) (string, []string) {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"mode":"report",` + hostPanelMeta + `}}`)
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

// applyTurnMeta sends one apply turn and returns its _meta.reviewmesh.
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

// Turn 1 reports one accepted fingerprint. After the panel is silenced and a control report raises
// nothing, turn 2 applies turn 1's run selecting that fingerprint, and it writes.
func TestACPFromRun_TheAppliedSetIsTheInspectedSet(t *testing.T) {
	srv, rv, ws, file := fromRunHarness(t)
	s := startACPSession(t, srv)
	s.newSession(ws)

	sourceRun, fps := s.reportTurn(2, ws)
	if sourceRun == "" || len(fps) != 1 {
		t.Fatalf("the report turn must hand back a runDir and exactly one accepted fingerprint; got %q %v", sourceRun, fps)
	}
	// The decision set is a file in the run directory, so it survives the process.
	if _, err := os.Stat(filepath.Join(sourceRun, filepath.FromSlash(run.DecisionSetArtifact))); err != nil {
		t.Fatalf("a completed report run must record its decision set on disk: %v", err)
	}

	// The control: a fresh adjudication now produces nothing.
	rv.silence()
	if _, freshFPs := s.reportTurn(3, ws); len(freshFPs) != 0 {
		t.Fatalf("the control turn must raise nothing once the panel is silenced; got %v", freshFPs)
	}

	rm := s.applyTurnMeta(4, ws, sourceRun, fps)
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), "func main() { _ = 0 }") {
		t.Fatalf("the apply turn did not write the finding the report turn showed; file=%q", got)
	}
	if rm["applied"] != float64(1) {
		t.Errorf("_meta.reviewmesh.applied = %v, want 1", rm["applied"])
	}
	if rm["outcome"] != review.OutcomeApplied {
		t.Errorf("outcome = %v, want %q", rm["outcome"], review.OutcomeApplied)
	}
	if rm["writes"] != "aimesh" || rm["diffAvailable"] != true {
		t.Errorf("a from-run turn must disclose writes=aimesh and diffAvailable=true, got %v", rm)
	}
	// The selection matched against the stored set.
	sel, _ := rm["selection"].(map[string]any)
	matched, _ := sel["matched"].([]any)
	unmatched, _ := sel["unmatched"].([]any)
	if len(matched) != 1 || len(unmatched) != 0 {
		t.Errorf("selection = %v, want the selector matched against the stored set", sel)
	}
	// The receipt belongs to the remediation run, not the source run.
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

// The handle is verified against the declared workspace's run-record locations, and every failure
// answers alike, revealing nothing about paths outside them.
func TestACPFromRun_AHandleThisAgentDidNotProduceIsRefused(t *testing.T) {
	srv, _, ws, file := fromRunHarness(t)
	before, _ := os.ReadFile(file)
	s := startACPSession(t, srv)
	s.newSession(ws)

	// A real directory this agent did not produce, a plausible fabrication, a traversal, and a relative
	// handle.
	elsewhere := t.TempDir()
	for i, handle := range []string{elsewhere, "/runs/prior-report-run", filepath.Join(elsewhere, ".."), "runs/prior-report-run"} {
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

// The source run's pins are re-verified before writing, so a changed tree halts and the human's
// edit survives.
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
	// The refusal is recorded: a receipt exists and says nothing was committed.
	runDir, _ := data["runDir"].(string)
	if runDir == "" {
		t.Fatalf("a halted from-run write must carry its own runDir, got %v", data)
	}
	r := readACPReceipt(t, runDir)
	if r.Status != "halted" || r.Committed {
		t.Fatalf("receipt = %+v, want halted with no commit", r)
	}
}

// Run-forming arguments on a fromRun turn are refused by name. MCP applies the same rule to
// review_remediate.
func TestACPFromRun_RunFormingArgumentsAreRefusedNotIgnored(t *testing.T) {
	srv, _, ws, _ := fromRunHarness(t)
	s := startACPSession(t, srv)
	s.newSession(ws)
	sourceRun, _ := s.reportTurn(2, ws)

	for i, meta := range []string{
		`{"reviewmesh":{"panel":{"reviewers":[{"adapter":"fake","model":"fake-model"}],"author_remediator":{"adapter":"fake","model":"fake-model"}}}}`,
		`{"reviewmesh":{"authority":[{"name":"spec","path":"spec.md"}]}}`,
		`{"reviewmesh":{"dryRun":true}}`,
		`{"reviewmesh":{"verifyReadiness":true}}`,
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

// A fromRun write naming a different workspace than the source run reviewed is refused.
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

// A turn naming no workspace declares its session cwd, which is checked against the source run's
// workspace the same way.
func TestACPFromRun_ASessionCwdMustBeTheOneTheSourceRunJudged(t *testing.T) {
	srv, _, ws, _ := fromRunHarness(t)
	s := startACPSession(t, srv)
	s.newSession(ws)
	sourceRun, _ := s.reportTurn(2, ws)

	other := t.TempDir()
	s.send(`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":` + strconv.Quote(other) + `}}`)
	res, _ := s.awaitID(3)["result"].(map[string]any)
	sid, _ := res["sessionId"].(string)
	if sid == "" {
		t.Fatalf("session/new returned no sessionId: %v", res)
	}
	s.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":` + strconv.Quote(sid) +
		`,"fromRun":` + strconv.Quote(sourceRun) + `,"mode":"apply","permissions":{"allowWrite":true}}}`)
	e, ok := s.awaitID(4)["error"].(map[string]any)
	if !ok {
		t.Fatal("a session cwd that is not the source run's workspace must be refused")
	}
	data, _ := e["data"].(map[string]any)
	if data["reasonCode"] != reasonFromRunWorkspaceMismatch {
		t.Errorf("reasonCode = %v, want %q", data["reasonCode"], reasonFromRunWorkspaceMismatch)
	}
}

// A host sends turn 2 as soon as turn 1's response arrives, so the session must no longer count
// turn 1 as in flight. ServeFramed releases the slot before writing the response; this test samples
// that ordering.
func TestACPSession_ATurnAnsweredIsATurnFinished(t *testing.T) {
	srv := &Server{
		Manager:  &fakeRemediator{},
		Caps:     review.SurfaceCaps{FileRead: true, FileWrite: true},
		Adapters: harnessAdapters(t),
	}
	ws := t.TempDir()
	s := startACPSession(t, srv)
	s.newSession(ws)
	for id := 2; id < 60; id++ {
		s.send(withDefaultPanel(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
			strconv.Quote(ws) + `,"mode":"report"}}`))
		resp := s.awaitID(float64(id))
		if e, ok := resp["error"].(map[string]any); ok {
			t.Fatalf("turn %d was refused after the previous turn had already ANSWERED: %v", id, e)
		}
	}
}

// A patch or apply run has already applied its own accepted set, so only report runs record a
// decision set that fromRun can apply.
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
	s.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"fromRun":` + strconv.Quote(writeRun) + `,"mode":"apply","permissions":{"allowWrite":true}}}`)
	e, _ := s.awaitID(4)["error"].(map[string]any)
	data, _ := e["data"].(map[string]any)
	if data["reasonCode"] != run.ReasonRunHandleUnknown {
		t.Errorf("reasonCode = %v, want %q", data["reasonCode"], run.ReasonRunHandleUnknown)
	}
}
