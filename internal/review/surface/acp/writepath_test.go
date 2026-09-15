package acp

// These tests run the ACP governed write end to end: a real run.Manager behind JSON-RPC frames, a
// real workspace and a live write. They hold ACP to no-write-after-cancel and to verifying each
// destination's content before commit.
//
// gateHost blocks inside the remediation model call, the last step before the write window opens,
// which is where an editor save or a host cancel lands in practice.

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
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// gateHost is the author_remediator seat. It accepts the reviewer's finding and returns an anchored
// edit, but in the remediation phase it first blocks on reached and release so the test can act
// before the write window opens.
type gateHost struct {
	reached chan struct{}
	release chan struct{}
}

func (gateHost) Name() string              { return hostAdapter }
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

// acpApplyHarness builds an ACP server whose manager really writes: launched with --allow-writes over
// a write-capable host, with nothing stubbed between request and tree.
func acpApplyHarness(t *testing.T) (*Server, *gateHost, string, string) {
	t.Helper()
	t.Setenv(fake.EnvVar, "1")
	t.Setenv("AIMESH_HOME", t.TempDir())

	ws := t.TempDir()
	file := filepath.Join(ws, "main.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := &gateHost{reached: make(chan struct{}), release: make(chan struct{})}
	mgr := &run.Manager{
		Cfg: launchConfig(),
		Adapters: map[string]model.Adapter{
			"fake": fake.New(fake.Valid), hostAdapter: g,
		},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
	srv := &Server{
		Manager:     mgr,
		Caps:        review.SurfaceCaps{FileRead: true, FileWrite: true},
		Adapters:    harnessAdapters(t),
		AllowWrites: true,
	}
	return srv, g, ws, file
}

// acpSession drives a server over live pipes so a test can interleave requests with a run. Frames are
// drained continuously because server writes are synchronous; reading only on demand would deadlock
// on the first progress notification.
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

// promptReport runs turn one read-only and returns the runDir from its response.
func (s *acpSession) promptReport(ws string) string {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"mode":"report",` + hostPanelMeta + `}}`)
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
	// Turn one must offer something to apply, or the assertions below prove nothing.
	if accepted, _ := rm["accepted"].([]any); len(accepted) == 0 {
		s.t.Fatalf("the report turn produced no accepted finding, so there is nothing for the apply turn to write; got %v", rm)
	}
	return runDir
}

// promptApply sends turn two, applying the decision set turn one recorded, without waiting for the
// response.
func (s *acpSession) promptApply(ws, fromRun string) {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":` +
		strconv.Quote(ws) + `,"fromRun":` + strconv.Quote(fromRun) + `,"mode":"apply","permissions":{"allowWrite":true}}}`)
}

// The cancel is delivered and acknowledged while the run blocks in its remediation call, before the
// write window. The run must answer stopReason cancelled and leave the workspace untouched, and the
// receipt records it.
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

	// The cancellation is recorded: a receipt exists and says nothing was committed.
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

// The file is saved in place mid-remediation, leaving the inode unchanged, so only the content pin
// catches it and the write halts rather than replacing the edit.
func TestACPApply_ConcurrentInPlaceEditIsRefused(t *testing.T) {
	srv, g, ws, file := acpApplyHarness(t)
	const concurrent = "package main\n\nfunc main() { /* a human was here */ }\n"
	s := startACPSession(t, srv)

	s.newSession(ws)
	s.promptApply(ws, s.promptReport(ws))
	<-g.reached

	// An editor's save: the same path, rewritten in place.
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

// Control for the tests above: the same harness with nothing interfering writes.
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
