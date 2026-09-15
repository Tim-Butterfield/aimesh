package mcp_test

// These tests resolve review_remediate {fromRun} from disk against a real run.Manager, workspace,
// decision-set artifact and live write. The in-memory run registry is bounded and dies with the
// process, so a handle naming a run this server produced also resolves from the run's directory, as
// it does on ACP.
//
// A restart is staged as two mcp.Server values over one run.Manager: the first reports and the
// second, which never saw the run, remediates.
//
// Two tests are guards: a handle cannot point a governed write at an arbitrary directory, and a run
// that adjudicated nothing cannot be applied.

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

// silenceableReviewer answers as the deterministic fake until silence is called, then approves with
// no findings. That makes a write replaying the stored set distinguishable from one that re-reviewed.
// It mirrors the ACP suite's switchingReviewer.
type silenceableReviewer struct {
	valid  model.Adapter
	empty  model.Adapter
	silent atomic.Bool
}

func (s *silenceableReviewer) Name() string              { return "fake" }
func (s *silenceableReviewer) Available() (bool, string) { return true, "ok" }
func (s *silenceableReviewer) silence()                  { s.silent.Store(true) }

// Evidence implements the optional identity capability. Without it a seat is weak-identity and its
// findings are quarantined, leaving no accepted set.
func (s *silenceableReviewer) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}

func (s *silenceableReviewer) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	if s.silent.Load() {
		return s.empty.Invoke(ctx, c)
	}
	return s.valid.Invoke(ctx, c)
}

// hostAdapter is the adapter the author_remediator seat names. It is a built-in recipe so the launch
// set can hold it; the manager maps it to writingHost, so no CLI runs.
const hostAdapter = "claude-code"

// fromRunPanel returns the panel every report here composes: the fake reviewer and writingHost as
// adjudicator.
func fromRunPanel() map[string]any {
	return map[string]any{
		"reviewers":         []any{map[string]any{"adapter": "fake", "model": "fake-model"}},
		"author_remediator": map[string]any{"adapter": hostAdapter, "model": "write-model"},
	}
}

// writingHost is the author_remediator seat: it accepts the reviewer's finding and returns an anchored
// edit, so an apply changes a real file.
type writingHost struct{}

func (writingHost) Name() string              { return hostAdapter }
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

// fromRunFixture is a write-capable run.Manager over a real workspace. The run directory, decision
// set and base-hash pins are genuine, because the on-disk path is under test.
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

	// The configuration a launch-configured server builds: built-in defaults, no profiles, and apply
	// reachable on agent surfaces.
	cfg := config.Default()
	cfg.Adapters[hostAdapter] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.Surfaces.DefaultModeBySurface = map[string]string{"cli": "apply", "ci": "report", "acp": "apply", "mcp": "apply"}
	one := 1
	cfg.Review.MaxOuterCycles = &one

	rv := &silenceableReviewer{valid: fake.New(fake.Valid), empty: fake.New(fake.Empty)}
	art := t.TempDir()
	return &fromRunFixture{
		mgr: &run.Manager{
			Cfg:         cfg,
			Adapters:    map[string]model.Adapter{"fake": rv, hostAdapter: writingHost{}},
			ArtifactDir: art, TempBase: t.TempDir(),
		},
		rv: rv, art: art, ws: ws, file: file,
	}
}

// server returns a new server over the fixture's manager. A second call stages a restart: an empty
// registry over the same artifact directory.
func (f *fromRunFixture) server(t *testing.T) *client {
	t.Helper()
	return serve(t, newServer(t, f.mgr, func(s *mcp.Server) {
		s.Ceiling, s.AllowWrites = []string{f.ws}, true
	}))
}

// report runs one report and returns its runId.
func (f *fromRunFixture) report(t *testing.T, c *client) string {
	t.Helper()
	res := c.tool(t, "review_report", map[string]any{"workspace": f.ws, "panel": fromRunPanel()})
	if res.isError || res.rpc != nil {
		t.Fatalf("the report turn must succeed: %+v %+v", res.structured, res.rpc)
	}
	id, _ := res.structured["runId"].(string)
	if id == "" {
		t.Fatalf("the report must return a runId: %+v", res.structured)
	}
	// The run id names the run's directory, so the decision set is findable from the handle the client
	// holds.
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

func (f *fromRunFixture) remediate(t *testing.T, c *client, fromRun string) toolResult {
	t.Helper()
	return c.tool(t, "review_remediate", map[string]any{
		"fromRun": fromRun, "workspace": f.ws, "output": "apply", "allowWrite": true,
	})
}

// A run this agent produced, whose registry entry is gone, still applies from its recorded decision
// set.
func TestMCPFromRun_AnEvictedHandleStillAppliesFromDisk(t *testing.T) {
	f := newFromRunFixture(t)
	sourceRun := f.report(t, f.server(t))

	res := f.remediate(t, f.server(t), sourceRun)
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
	// The receipt names the run whose decisions were applied.
	if receipt["sourceRunId"] != sourceRun {
		t.Errorf("receipt.sourceRunId = %v, want the source run %q", receipt["sourceRunId"], sourceRun)
	}
	if src, _ := res.structured["sourceRunId"].(string); src != sourceRun {
		t.Errorf("sourceRunId = %q, want %q", src, sourceRun)
	}
	// The pins verified are the ones the source run captured.
	if n, _ := receipt["baseHashesVerified"].(float64); n < 1 {
		t.Errorf("baseHashesVerified = %v, want the source run's pins to have been checked", receipt["baseHashesVerified"])
	}
}

// The write applies the set that was read, not one re-derived at write time. After the report, the
// panel is silenced and a control run produces nothing; the apply, on a server that never saw the
// source run, still writes.
func TestMCPFromRun_TheAppliedSetIsTheInspectedSet(t *testing.T) {
	f := newFromRunFixture(t)
	first := f.server(t)
	sourceRun := f.report(t, first)

	// The control: a fresh run now produces no accepted set.
	f.rv.silence()
	control := c2Structured(t, first, f.ws)
	if n, _ := control["findings"].([]any); len(n) != 0 {
		t.Fatalf("the control turn must raise nothing once the panel is silenced; got %v", control["findings"])
	}

	if res := f.remediate(t, f.server(t), sourceRun); res.isError || res.rpc != nil {
		t.Fatalf("the apply must succeed from the stored set: %+v %+v", res.structured, res.rpc)
	}
	if !f.applied(t) {
		t.Fatalf("the apply did not write the finding the report turn showed — the applied set is not the inspected set; file = %q", f.body(t))
	}
}

// c2Structured runs a second report turn and returns its structured payload.
func c2Structured(t *testing.T, c *client, ws string) map[string]any {
	t.Helper()
	res := c.tool(t, "review_report", map[string]any{"workspace": ws, "panel": fromRunPanel()})
	if res.isError || res.rpc != nil {
		t.Fatalf("the control report turn must succeed: %+v %+v", res.structured, res.rpc)
	}
	return res.structured
}

// The source run's pins are re-verified before the write window opens, so a changed tree halts and
// the human's edit survives.
func TestMCPFromRun_AStaleTreeHaltsRatherThanWrites(t *testing.T) {
	f := newFromRunFixture(t)
	sourceRun := f.report(t, f.server(t))

	const human = "package main\n\nfunc main() { /* a human was here */ }\n"
	if err := os.WriteFile(f.file, []byte(human), 0o644); err != nil {
		t.Fatal(err)
	}

	res := f.remediate(t, f.server(t), sourceRun)
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
	// The refusal is recorded: a receipt exists and says nothing was committed.
	receipt, _ := res.structured["receipt"].(map[string]any)
	if committed, _ := receipt["committed"].(bool); committed {
		t.Fatalf("a halted write must record no commit: %+v", receipt)
	}
}

// In-process, a second remediation of the same run returns the original receipt through the
// registry's source-run guard. After a restart the guard is gone, but the first apply changed the
// pinned files, so the second call halts on stale pins without writing. The two outcomes carry
// different codes.
func TestMCPFromRun_ASecondApplyAfterARestartHalts(t *testing.T) {
	f := newFromRunFixture(t)
	sourceRun := f.report(t, f.server(t))

	if res := f.remediate(t, f.server(t), sourceRun); res.isError || res.rpc != nil {
		t.Fatalf("the first apply must succeed: %+v %+v", res.structured, res.rpc)
	}
	applied := f.body(t)

	res := f.remediate(t, f.server(t), sourceRun)
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

// The handle is verified against the workspace's run-record locations, and every failure answers
// alike, so a governed write cannot be pointed at an arbitrary directory.
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
		res := f.remediate(t, c, handle)
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

// An inline run is refused from disk with the same reason code as from the registry, since its
// materialized directory has been deleted.
func TestMCPFromRun_AnInlineRunIsStillRefusedByNameFromDisk(t *testing.T) {
	f := newFromRunFixture(t)
	c := f.server(t)
	res := c.tool(t, "review_report", map[string]any{
		"inlineWorkspace": map[string]any{"main.go": "package main\n\nfunc main() {}\n"},
		"panel":           fromRunPanel(),
	})
	if res.isError || res.rpc != nil {
		t.Fatalf("an inline review must work: %+v %+v", res.structured, res.rpc)
	}
	runID, _ := res.structured["runId"].(string)

	got := f.remediate(t, f.server(t), runID)
	if !got.isError {
		t.Fatalf("remediating an inline run must fail closed, got %+v", got.structured)
	}
	if code, _ := got.structured["reasonCode"].(string); code != run.ReasonInlineWorkspaceNotRemediable {
		t.Fatalf("reasonCode = %q, want %q — the on-disk path must report the same refusal the registry path does",
			code, run.ReasonInlineWorkspaceNotRemediable)
	}
}

// A patch or apply run records no decision set, so it resolves to nothing, as on ACP.
func TestMCPFromRun_AWriteRunIsNotAFromRunSource(t *testing.T) {
	f := newFromRunFixture(t)
	sourceRun := f.report(t, f.server(t))
	res := f.remediate(t, f.server(t), sourceRun)
	if res.isError || res.rpc != nil {
		t.Fatalf("the apply must succeed: %+v %+v", res.structured, res.rpc)
	}
	writeRun, _ := res.structured["runId"].(string)
	if writeRun == "" {
		t.Fatal("a remediation must carry its own runId")
	}
	again := f.remediate(t, f.server(t), writeRun)
	if !again.isError {
		t.Fatalf("a remediation run must never be a from-run source: %+v", again.structured)
	}
	if code, _ := again.structured["reasonCode"].(string); code != "unknown_run_id" {
		t.Errorf("reasonCode = %q, want unknown_run_id", code)
	}
}
