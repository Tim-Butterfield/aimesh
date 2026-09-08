package acp

// The ACP half of the governed-write convergence, proven END TO END: a real run.Manager behind
// real JSON-RPC frames, a real workspace, and a real live write.
//
// `docs/acp.md` states no-write-after-cancel as a standing ACP guarantee. Before this convergence
// it was a race: the ACP apply went through `handleMode`, which read `ctx.Err()` and then called an
// unpinned `ws.Commit` — two acts, with a cancellation able to land between them — and which
// verified nothing about the destination's content. Both properties are now the same single
// function every surface uses, and these tests hold ACP to them over the wire rather than by
// assertion in a design document.
//
// Determinism comes from the author_remediator lane: `gateHost` blocks inside the remediation model
// call, which is the last thing that happens before the write window opens. That is a real place
// for a run to spend minutes, and it is where a human's editor save or a host's cancel actually
// lands.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// gateHost is the author_remediator lane. It adjudicates the reviewer's finding as apply-worthy and
// returns a real anchored edit — but on the remediation phase it first BLOCKS on `reached`/`release`
// so the test can act while the run is in flight and before any write window opens.
type gateHost struct {
	reached chan struct{}
	release chan struct{}
}

func (gateHost) Name() string              { return "gate-host" }
func (gateHost) Available() (bool, string) { return true, "ok" }

func (g gateHost) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	ok := func(js string) (model.Result, error) {
		return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg),
			Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
	}
	switch c.Phase {
	case string(review.PhaseAdjudicate):
		return ok(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"f1","validity":"valid","decisionState":"applied","reasoning":"apply the fix"}]}`)
	case string(review.PhaseRemediate):
		close(g.reached)
		<-g.release
		return ok(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","edits":[{"file":"main.go","anchor":"func main() {}","replacement":"func main() { _ = 0 }","occurrence":0}]}`)
	default:
		return ok(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_author_review","verdict":"approve","findings":[]}`)
	}
}

// acpApplyHarness builds an ACP server whose Manager really writes: the config ceiling for the acp
// surface is widened to `apply` (the documented, config-visible opt-in) and the host advertises
// write capability, so nothing between the request and the live tree is stubbed.
func acpApplyHarness(t *testing.T) (*Server, *gateHost, string, string) {
	t.Helper()
	t.Setenv(fake.EnvVar, "1")
	t.Setenv("AIMESH_HOME", t.TempDir())

	ws := t.TempDir()
	file := filepath.Join(ws, "main.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Adapters["gate-host"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.ModelCatalog["gate-model"] = config.CatalogEntry{
		Provider: "x", CanonicalModel: "mm",
		Adapters: map[string]config.AdapterModel{"gate-host": {ModelArg: "mm"}},
	}
	cfg.Profiles["acp-write"] = config.Profile{
		Description:       "acp governed-write harness",
		AdapterPreference: []string{"gate-host", "fake"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "gate-host", Model: "gate-model"},
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		},
	}
	cfg.DefaultProfile = "acp-write"
	// The documented opt-in: `surfaces: {defaultModeBySurface: {acp: apply}}`. Without it the
	// Manager's own ceiling caps this run to report and nothing is under test.
	cfg.Surfaces.DefaultModeBySurface["acp"] = "apply"
	one := 1
	cfg.Review.MaxOuterCycles = &one // one write window, so the assertions are unambiguous

	g := &gateHost{reached: make(chan struct{}), release: make(chan struct{})}
	mgr := &run.Manager{
		Cfg: cfg,
		Adapters: map[string]model.Adapter{
			"fake": fake.New(fake.Valid), "gate-host": g,
		},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
	srv := &Server{
		Manager:       mgr,
		Caps:          review.SurfaceCaps{FileRead: true, FileWrite: true},
		PolicyCeiling: review.ModeApply,
		Roots:         []string{os.TempDir(), ws},
	}
	return srv, g, ws, file
}

// acpSession drives a server over live pipes so the test can interleave requests with the run's
// progress. A writer plus a continuously-drained frame channel is what makes "cancel WHILE it runs"
// a sequence rather than a hope — and the draining is not optional: the server's frame writes are
// synchronous, so a test that only reads when it wants a specific answer deadlocks the server on
// the first progress notification.
type acpSession struct {
	t      *testing.T
	in     *io.PipeWriter
	frames chan map[string]any
	done   chan struct{}
}

func startACPSession(t *testing.T, srv *Server) *acpSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &acpSession{t: t, in: inW, frames: make(chan map[string]any, 256), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		_ = srv.Serve(inR, outW)
		outW.Close()
	}()
	go func() {
		defer close(s.frames)
		dec := json.NewDecoder(bufio.NewReader(outR))
		for {
			var m map[string]any
			if err := dec.Decode(&m); err != nil {
				return
			}
			s.frames <- m
		}
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-s.done:
		case <-time.After(30 * time.Second):
			t.Error("the ACP server did not shut down")
		}
		outR.Close()
	})
	return s
}

func (s *acpSession) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		s.t.Fatalf("write frame: %v", err)
	}
}

// awaitID reads frames until the response for id arrives, skipping notifications.
func (s *acpSession) awaitID(id float64) map[string]any {
	s.t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case m, ok := <-s.frames:
			if !ok {
				s.t.Fatalf("the frame stream ended before a response for id %v", id)
			}
			if got, isNum := m["id"].(float64); isNum && got == id {
				return m
			}
		case <-deadline:
			s.t.Fatalf("timed out waiting for a response to id %v", id)
		}
	}
}

func (s *acpSession) newSession(ws string) {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":` + strconv.Quote(ws) + `}}`)
	res, _ := s.awaitID(1)["result"].(map[string]any)
	if res["sessionId"] != "s-0001" {
		s.t.Fatalf("sessionId = %v, want s-0001", res["sessionId"])
	}
}

// promptReport is TURN ONE. It runs the governed cycle read-only and returns the `runDir` from its
// RESPONSE — the one channel with a delivery property, and the handle turn two must carry.
func (s *acpSession) promptReport(ws string) string {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"mode":"report"}}`)
	resp := s.awaitID(2)
	res, ok := resp["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("the report turn must succeed; got %v", resp)
	}
	meta, _ := res["_meta"].(map[string]any)
	rm, _ := meta["reviewmesh"].(map[string]any)
	runDir, _ := rm["runDir"].(string)
	if runDir == "" {
		s.t.Fatalf("a report turn must hand back its runDir; got %v", rm)
	}
	// The turn must have offered something to apply, or turn two is vacuously safe and every
	// assertion below proves nothing.
	if accepted, _ := rm["accepted"].([]any); len(accepted) == 0 {
		s.t.Fatalf("the report turn produced no accepted finding, so there is nothing for the apply turn to write; got %v", rm)
	}
	return runDir
}

// promptApply is TURN TWO: apply the decision set turn one recorded. It does not wait — the tests
// interleave a cancel or a concurrent edit with the run.
func (s *acpSession) promptApply(ws, fromRun string) {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"fromRun":` + strconv.Quote(fromRun) + `,"mode":"apply","permissions":{"allowWrite":true}}}`)
}

// TestACPApply_CancelDuringTheRunNeverCommits is the standing ACP guarantee, held to the wire.
//
// The cancel is delivered — and ACKNOWLEDGED by the server — while the run is blocked inside its
// remediation model call, so it is in force before the write window is reached. The run must answer
// `stopReason: cancelled` AND leave the workspace untouched; those two facts are decided once,
// together, under the write window's lock.
//
// AGAINST THE OLD CODE THIS TEST FAILS: `handleMode` checked `ctx.Err()` and then called
// `ws.Commit`. Cancelling here (before the check) would still return "cancelled" — but there was no
// receipt at all, so the receipt assertion fails outright, and the guarantee was a two-act race
// rather than a gate. Reverting the delegation reproduces both.
func TestACPApply_CancelDuringTheRunNeverCommits(t *testing.T) {
	srv, g, ws, file := acpApplyHarness(t)
	before, _ := os.ReadFile(file)
	s := startACPSession(t, srv)

	s.newSession(ws)
	s.promptApply(ws, s.promptReport(ws))
	<-g.reached // the run is in flight, before any write window

	s.send(`{"jsonrpc":"2.0","id":4,"method":"session/cancel","params":{"sessionId":"s-0001"}}`)
	ack, _ := s.awaitID(4)["result"].(map[string]any)
	if ack["cancelled"] != true {
		t.Fatalf("session/cancel must find the in-flight run, got %v", ack)
	}
	close(g.release) // only now may the run proceed toward the window

	resp := s.awaitID(3)
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("a cancelled ACP prompt is answered with a normal result carrying the run handle, got %v", resp)
	}
	if res["stopReason"] != "cancelled" {
		t.Fatalf("stopReason = %v, want cancelled", res["stopReason"])
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("a cancelled run wrote to the live workspace:\nbefore: %q\nafter:  %q", before, after)
	}

	// The cancellation is RECORDED. A receipt exists on every path — including this one, which gets
	// no findings payload to carry it — and it says nothing was committed.
	meta, _ := res["_meta"].(map[string]any)
	rm, _ := meta["reviewmesh"].(map[string]any)
	runDir, _ := rm["runDir"].(string)
	if runDir == "" {
		t.Fatalf("a cancelled prompt must still carry runDir, got %v", res)
	}
	r := readACPReceipt(t, runDir)
	if r.Status != "cancelled" || r.Committed || r.CommitAttempted {
		t.Fatalf("receipt = %+v, want a cancelled receipt with no commit", r)
	}
}

// TestACPApply_ConcurrentInPlaceEditIsRefused is the same data-loss defect the CLI test stages,
// proven on ACP: the file is saved in place while the run is mid-remediation, so the staged bytes
// derive from content that no longer exists.
//
// AGAINST THE OLD CODE THIS TEST FAILS: the ACP path committed with an unpinned `ws.Commit`, the
// destination's inode was unchanged by an in-place save, and the human's edit was silently replaced
// by the model's fix. The run returned success.
func TestACPApply_ConcurrentInPlaceEditIsRefused(t *testing.T) {
	srv, g, ws, file := acpApplyHarness(t)
	const concurrent = "package main\n\nfunc main() { /* a human was here */ }\n"
	s := startACPSession(t, srv)

	s.newSession(ws)
	s.promptApply(ws, s.promptReport(ws))
	<-g.reached

	// An editor's save: same path, opened and rewritten in place.
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open for in-place edit: %v", err)
	}
	if _, err := f.WriteString(concurrent); err != nil {
		t.Fatalf("in-place edit: %v", err)
	}
	f.Close()
	close(g.release)

	resp := s.awaitID(3)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("a refused write must halt the prompt, got %v", resp)
	}
	data, _ := e["data"].(map[string]any)
	if data["reasonCode"] != run.ReasonStaleDecisionSet {
		t.Fatalf("reasonCode = %v, want %q", data["reasonCode"], run.ReasonStaleDecisionSet)
	}
	if after, _ := os.ReadFile(file); string(after) != concurrent {
		t.Fatalf("the concurrent edit was overwritten:\nwant: %q\ngot:  %q", concurrent, after)
	}
	runDir, _ := data["runDir"].(string)
	if runDir == "" {
		t.Fatalf("a halted prompt must carry runDir, got %v", data)
	}
	r := readACPReceipt(t, runDir)
	if r.Status != "halted" || r.Committed || !r.CommitAttempted {
		t.Fatalf("receipt = %+v, want halted with commitAttempted and no commit", r)
	}
	if len(r.Files) != 0 {
		t.Fatalf("receipt.files = %v, want empty — the commit rolled back", r.Files)
	}
}

// TestACPApply_UncancelledRunStillWrites is the non-vacuity control for the two tests above: the
// same harness, nothing interfering, really does write. Without it a broken harness would make both
// refusal tests pass for the wrong reason.
func TestACPApply_UncancelledRunStillWrites(t *testing.T) {
	srv, g, ws, file := acpApplyHarness(t)
	s := startACPSession(t, srv)

	s.newSession(ws)
	s.promptApply(ws, s.promptReport(ws))
	<-g.reached
	close(g.release)

	resp := s.awaitID(3)
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("the control run must succeed, got %v", resp)
	}
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), "func main() { _ = 0 }") {
		t.Fatalf("the control run did not write the model's fix; file=%q", got)
	}
	meta, _ := res["_meta"].(map[string]any)
	rm, _ := meta["reviewmesh"].(map[string]any)
	runDir, _ := rm["runDir"].(string)
	r := readACPReceipt(t, runDir)
	if !r.Committed || len(r.Files) != 1 || r.Files[0] != "main.go" {
		t.Fatalf("receipt = %+v, want a committed receipt naming main.go", r)
	}
}

func readACPReceipt(t *testing.T, runDir string) run.Receipt {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(runDir, "remediation", "receipt.json"))
	if err != nil {
		t.Fatalf("the receipt must be persisted on every path: %v", err)
	}
	var r run.Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("receipt is not valid JSON: %v", err)
	}
	return r
}
