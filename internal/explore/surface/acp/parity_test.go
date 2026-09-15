package acp_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp/testhost"
)

// This file covers the ACP forms of two capabilities every surface carries:
//
//  1. PANEL COMPOSITION (`_meta.exploremesh.panel`) — the CLI's `--explorer/--collator` and MCP's `panel`.
//     A composition names only adapters the agent was launched with and can never introduce one.
//  2. RUN CAPTURE (`_meta.exploremesh.dumpRun`) — the CLI's `--dump-run`: the run record every governance
//     claim is checkable against.

// serveWith wires a server launched with exactly the named adapters.
func serveWith(t *testing.T, exp acp.Explorer, adapters []string) (*testhost.Client, func()) {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	srv := &acp.Server{Explorer: exp, Adapters: launchSet(t, adapters...), Framing: acp.FramingNewline}
	done := make(chan struct{})
	go func() { _ = srv.Serve(sr, sw); close(done) }()
	client := testhost.NewClient(acp.FramingNewline, cr, cw)
	return client, func() {
		_ = client.Notify("exit", nil)
		_ = cw.Close()
		<-done
		_ = sw.Close()
	}
}

// paritySession runs initialize + session/new against a server built by serveWith.
func paritySession(t *testing.T, c *testhost.Client) string {
	t.Helper()
	if init, err := c.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil || init.Error != nil {
		t.Fatalf("initialize: err=%v resp=%+v", err, init)
	}
	sn, err := c.Call("session/new", nil)
	if err != nil || sn.Error != nil {
		t.Fatalf("session/new: err=%v resp=%+v", err, sn)
	}
	sid, _ := sn.Result["sessionId"].(string)
	if sid == "" {
		t.Fatal("no sessionId")
	}
	return sid
}

// promptWith sends a prompt carrying em plus the required criteria. A prompt that names no panel gets the
// default test panel; a "panel" key set to nil sends none.
func promptWith(t *testing.T, c *testhost.Client, sid string, em map[string]any) *testhost.Response {
	t.Helper()
	em["criteria"] = []any{"cost", "latency"}
	em = withPanel(em)
	resp, err := c.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "choose a datastore"}},
		"_meta":     map[string]any{"exploremesh": em},
	})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	return resp
}

func TestParity_AdHocPanelComposition(t *testing.T) {
	exp := &fakeExplorer{}
	c, stop := serveWith(t, exp, []string{"claude-code", "fake"})
	defer stop()
	sid := paritySession(t, c)

	resp := promptWith(t, c, sid, map[string]any{"panel": map[string]any{
		"explorers": []any{
			map[string]any{"adapter": "fake", "model": "x1"},
			map[string]any{"adapter": "fake", "model": "x2", "effort": "high"},
		},
		"collator": map[string]any{"adapter": "fake", "model": "xc"},
	}})
	if resp.Error != nil {
		t.Fatalf("ad-hoc composition failed: %+v", resp.Error)
	}
	meta, _ := resp.Result["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	if em["panelSource"] != "adhoc" {
		t.Errorf("panelSource = %v, want adhoc", em["panelSource"])
	}
	if exp.gotPlan == nil || len(exp.gotPlan.Explorers) != 2 {
		t.Fatalf("executed plan = %+v", exp.gotPlan)
	}
	if exp.gotPlan.Collator.Model != "xc" {
		t.Errorf("collator = %+v, want the composed one", exp.gotPlan.Collator)
	}
	// The executed seats are echoed back, so the driver can compare what it asked for with what ran.
	seats, _ := em["explorers"].([]any)
	if len(seats) != 2 {
		t.Errorf("echoed explorers = %v, want the 2 composed seats", em["explorers"])
	}
}

func TestParity_AdHocPanelIsFailClosedAndComposeNotConfigure(t *testing.T) {
	c, stop := serveWith(t, &fakeExplorer{}, []string{"claude-code", "fake"})
	defer stop()
	sid := paritySession(t, c)

	// An adapter the agent was not launched with: refused, and the refusal names the launched set.
	resp := promptWith(t, c, sid, map[string]any{"panel": map[string]any{
		"explorers": []any{
			map[string]any{"adapter": "brand-new-cli", "model": "x1"},
			map[string]any{"adapter": "fake", "model": "x2"},
		},
		"collator": map[string]any{"adapter": "fake", "model": "xc"},
	}})
	if resp.Error == nil {
		t.Fatal("an adapter the agent was not launched with must be refused")
	}
	if want := "claude-code, fake"; !strings.Contains(resp.Error.Message, want) || !strings.Contains(resp.Error.Message, "not launched with") {
		t.Errorf("refusal must name the launched set, got %q", resp.Error.Message)
	}

	// A one-seat panel is refused (a panel needs something to be blind about).
	resp = promptWith(t, c, sid, map[string]any{"panel": map[string]any{
		"explorers": []any{map[string]any{"adapter": "fake", "model": "x1"}},
		"collator":  map[string]any{"adapter": "fake", "model": "xc"},
	}})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "at least 2") {
		t.Errorf("a 1-explorer panel must be refused, got %+v", resp.Error)
	}

	// A composition with no collator is refused.
	resp = promptWith(t, c, sid, map[string]any{"panel": map[string]any{
		"explorers": []any{map[string]any{"adapter": "fake", "model": "x1"}, map[string]any{"adapter": "fake", "model": "x2"}},
	}})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "collator") {
		t.Errorf("a composition with no collator must be refused, got %+v", resp.Error)
	}
}

// An agent launched with NO adapter refuses every composition and says how to name one.
func TestParity_AdHocPanelRefusedWithoutALaunchedAdapter(t *testing.T) {
	c, stop := serveWith(t, &fakeExplorer{}, nil)
	defer stop()
	sid := paritySession(t, c)
	resp := promptWith(t, c, sid, map[string]any{"panel": map[string]any{
		"explorers": []any{map[string]any{"adapter": "fake", "model": "x1"}, map[string]any{"adapter": "fake", "model": "x2"}},
		"collator":  map[string]any{"adapter": "fake", "model": "xc"},
	}})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "launched with no adapter") || !strings.Contains(resp.Error.Message, "--adapter") {
		t.Errorf("composition on an agent launched with no adapter must be refused naming --adapter, got %+v", resp.Error)
	}
}

// TestParity_DryRunFromACP is the third leg of the dry-run capability: the CLI has --dry-run, MCP has
// `dryRun`, and this is ACP's. It asserts the option reached the pipeline (not merely that it was accepted),
// that the turn reports `planned` rather than `complete` — it explored nothing, and an echo with a zero-size
// panel labelled `complete` reads as "the run happened and found nothing" — and that dumpRun is refused
// beside it, because there is no exploration to record.
func TestParity_DryRunFromACP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", dir)
	exp := &fakeExplorer{}
	c, stop := serveWith(t, exp, []string{"fake"})
	defer stop()
	sid := paritySession(t, c)

	resp := promptWith(t, c, sid, map[string]any{"dryRun": true})
	if resp.Error != nil {
		t.Fatalf("prompt: %+v", resp.Error)
	}
	if !exp.gotOpts.DryRun {
		t.Fatal("dryRun was accepted on the wire but not passed to the pipeline — the turn would have spent")
	}
	meta, _ := resp.Result["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	if em["status"] != "planned" {
		t.Errorf("status = %v, want planned", em["status"])
	}
	if em["dryRun"] != true {
		t.Errorf("dryRun = %v, want true", em["dryRun"])
	}
	sh, _ := em["shape"].(map[string]any)
	if sh == nil {
		t.Fatalf("no shape echo: %v", em)
	}
	if sh["modelCalls"] == nil || sh["payload"] == nil || sh["calls"] == nil {
		t.Errorf("the shape must carry modelCalls, the per-stage calls and the payload: %v", sh)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a dry run must write no run directory; found %d", len(entries))
	}

	// dumpRun beside it is refused rather than writing a record of an exploration nobody performed.
	resp = promptWith(t, c, sid, map[string]any{"dryRun": true, "dumpRun": true})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "cannot be combined with dumpRun") {
		t.Errorf("dryRun + dumpRun must be refused, got %+v", resp.Error)
	}
}

func TestParity_RunCaptureFromACP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", dir)
	c, stop := serveWith(t, &fakeExplorer{}, []string{"fake"})
	defer stop()
	sid := paritySession(t, c)

	// Without dumpRun: nothing is written, and the echo says nothing about capture.
	resp := promptWith(t, c, sid, map[string]any{})
	if resp.Error != nil {
		t.Fatalf("prompt: %+v", resp.Error)
	}
	meta, _ := resp.Result["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	if _, has := em["runCaptured"]; has {
		t.Errorf("a turn that did not ask for capture must not report on it: %v", em)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("capture is OPT-IN on ACP: %d run dir(s) written without dumpRun", len(entries))
	}

	// With dumpRun: a run directory is written and the run id is echoed — the id, not a host path.
	resp = promptWith(t, c, sid, map[string]any{"dumpRun": true})
	if resp.Error != nil {
		t.Fatalf("prompt: %+v", resp.Error)
	}
	meta, _ = resp.Result["_meta"].(map[string]any)
	em, _ = meta["exploremesh"].(map[string]any)
	if captured, _ := em["runCaptured"].(bool); !captured {
		t.Fatalf("dumpRun must record the run: %v", em)
	}
	runID, _ := em["runId"].(string)
	if runID == "" {
		t.Fatal("a captured run must echo its run id")
	}
	for _, v := range em {
		if s, ok := v.(string); ok && strings.Contains(s, dir) {
			t.Errorf("the echo must not carry a host path (%q)", s)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, runID, "manifest.json")); err != nil {
		t.Fatalf("no run record at the echoed run id: %v", err)
	}
}
