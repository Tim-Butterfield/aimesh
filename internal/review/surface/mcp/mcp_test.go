package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// These tests drive the server with a hand-rolled JSON-RPC client and cover the reviewmesh contract:
// which tools exist, what they refuse, and what a write may do. conformance_test.go covers the
// protocol with the official SDK client.

// --- a minimal JSON-RPC driver ---

type client struct {
	f  proto.Framer
	id int
}

type response struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *rpcErr         `json:"error"`
}

type rpcErr struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

type notification struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

func (c *client) send(method string, id any, params any) {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		req["id"] = id
	}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	_ = c.f.WriteMessage(b)
}

func (c *client) call(t *testing.T, method string, params any) (*response, []notification) {
	t.Helper()
	c.id++
	id := c.id
	c.send(method, id, params)
	var notes []notification
	for {
		raw, err := c.f.ReadMessage()
		if err != nil {
			t.Fatalf("%s: read: %v", method, err)
		}
		var resp response
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		if len(resp.ID) == 0 {
			var n notification
			if json.Unmarshal(raw, &n) == nil && n.Method != "" {
				notes = append(notes, n)
			}
			continue
		}
		var got int
		if json.Unmarshal(resp.ID, &got) == nil && got == id {
			return &resp, notes
		}
	}
}

func (c *client) notify(method string, params any) { c.send(method, nil, params) }

// toolResult holds the CallToolResult fields a test inspects.
type toolResult struct {
	text       string
	structured map[string]any
	isError    bool
	rpc        *rpcErr
}

// defaultPanel returns the panel a review_report call carries when a test names none. Send
// "panel": nil to omit it.
func defaultPanel() map[string]any {
	seat := map[string]any{"adapter": "fake", "model": "m1"}
	return map[string]any{"reviewers": []any{seat}, "author_remediator": seat}
}

// withPanel adds the default panel to a review_report call that names none, and removes a "panel" key
// set to nil.
func withPanel(name string, args map[string]any) map[string]any {
	if name != "review_report" {
		return args
	}
	out := make(map[string]any, len(args)+1)
	maps.Copy(out, args)
	if p, has := out["panel"]; !has {
		out["panel"] = defaultPanel()
	} else if p == nil {
		delete(out, "panel")
	}
	return out
}

func (c *client) tool(t *testing.T, name string, args map[string]any) toolResult {
	t.Helper()
	resp, _ := c.call(t, "tools/call", map[string]any{"name": name, "arguments": withPanel(name, args)})
	if resp.Error != nil {
		return toolResult{rpc: resp.Error}
	}
	out := toolResult{}
	if v, ok := resp.Result["isError"].(bool); ok {
		out.isError = v
	}
	if blocks, ok := resp.Result["content"].([]any); ok {
		for _, b := range blocks {
			if m, ok := b.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					out.text += s
				}
			}
		}
	}
	out.structured, _ = resp.Result["structuredContent"].(map[string]any)
	return out
}

// --- fixtures ---

// fakeReviewer is a deterministic in-process manager stand-in. It spawns nothing: it records requests
// and returns canned outcomes, so tests assert what the surface decided.
type fakeReviewer struct {
	mu            sync.Mutex
	runs          int
	remediations  int
	lastRun       run.Request
	lastRemediate run.RemediateRequest
	runErr        error
	outcome       *review.RunOutcome
	// refusals, when set, makes Remediate return a partial refusal: the write committed and these findings
	// were refused for a protected path. The write path's own behavior is tested in the run package.
	refusals []review.ApplyRefusal
	// selection, when set, is the ApplySelection Remediate reports. Selector matching is tested in the run
	// package.
	selection *review.ApplySelection
}

func (f *fakeReviewer) RunContext(ctx context.Context, r run.Request) (review.RunOutcome, error) {
	f.mu.Lock()
	f.runs++
	f.lastRun = r
	f.mu.Unlock()
	if f.runErr != nil {
		return review.RunOutcome{Status: "halted", Mode: r.Mode}, f.runErr
	}
	if f.outcome != nil {
		o := *f.outcome
		o.Mode = r.Mode
		return o, nil
	}
	return cannedOutcome(r.Mode), nil
}

func (f *fakeReviewer) Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error) {
	f.mu.Lock()
	f.remediations++
	f.lastRemediate = r
	refusals := f.refusals
	selection := f.selection
	f.mu.Unlock()
	notApplied := make([]run.AppliedFinding, 0, len(refusals))
	for _, ref := range refusals {
		notApplied = append(notApplied, run.AppliedFinding{
			FindingID: ref.FindingID, File: ref.File,
			State: string(review.StateReportedValid), Reason: ref.Reason,
		})
	}
	return run.RemediateOutcome{
		RunID: "audit-run", RunDir: "/tmp/does-not-matter", SourceRunID: r.SourceRunID, Mode: r.Mode,
		Refusals: refusals, Selection: selection,
		Receipt: run.Receipt{
			SchemaVersion: 1, RunID: "audit-run", SourceRunID: r.SourceRunID, Mode: string(r.Mode),
			Status: "complete", BaseHashesVerified: len(r.BaseHashes),
			Intended:   []run.IntendedHunk{{FindingID: "F1", File: "sample.go", Bytes: 12, Origin: "marker"}},
			Applied:    []run.AppliedFinding{{FindingID: "F1", File: "sample.go", State: "applied"}},
			NotApplied: notApplied,
			Files:      []string{"sample.go"}, Committed: r.Mode == review.ModeApply,
			CommitAttempted: r.Mode == review.ModeApply,
			PatchArtifact:   "patches/changes.patch", PatchSHA256: "sha256:deadbeef",
		},
	}, nil
}

// ReadDecisionSetByID answers as an agent that produced no such run: tests here use the registry path,
// and the on-disk path is tested in fromrun_test.go against a real run.Manager.
func (f *fakeReviewer) ReadDecisionSetByID(workspace, runID string) (*run.StoredDecisionSet, string, error) {
	return nil, "", fault.New(fault.Usage, "no decision set: "+runID).WithReason(run.ReasonRunHandleUnknown)
}

func (f *fakeReviewer) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs, f.remediations
}

// cannedOutcome returns one accepted finding with full panel provenance, one identity caveat, one
// withheld file and one authority document, so tests can check each reaches the wire.
func cannedOutcome(mode review.Mode) review.RunOutcome {
	return review.RunOutcome{
		Status: "single_pass", Mode: mode, RunID: "20260101T000000-0001", RunDir: "/private/tmp/artifacts/20260101T000000-0001",
		Findings: []review.Finding{{
			ID: "F1", Title: "sample returns a magic number", Kind: review.KindRisk,
			Severity: review.SeverityMedium, File: "sample.go", Source: "reviewer",
		}},
		Decisions: []review.Decision{{
			FindingID: "F1", Valid: true, State: review.StateReportedValid,
			SupportingSeats: []review.SeatRef{
				{SeatID: "reviewer", Adapter: "fake", Model: "m1", IdentityTier: review.SeatIdentityVerified},
				{SeatID: "reviewer-2", Adapter: "fake", Model: "m2", IdentityTier: review.SeatIdentityVerified},
			},
			AgreementCount: 2, DissentingSeats: []string{"reviewer-3"},
		}},
		Panel: []review.SeatStatus{
			{SeatID: "reviewer", Index: 1, Adapter: "fake", Model: "m1", Status: "completed", Rounds: 1, Findings: 1, IdentityTier: "verified"},
			{SeatID: "reviewer-2", Index: 2, Adapter: "fake", Model: "m2", Status: "completed", Rounds: 1, Findings: 1, IdentityTier: "verified"},
			{SeatID: "reviewer-3", Index: 3, Adapter: "fake", Model: "m3", Status: "completed", Rounds: 1, Findings: 0, IdentityTier: "self_reported"},
		},
		IdentityCaveats: []review.IdentityCaveat{{
			Role: "reviewer-3", Adapter: "fake", RequestedModel: "m3", Status: review.VerifSelfReported,
		}},
		Withheld: []review.WithheldFile{{Path: "vendor/x.go", Reason: "workspace_hardlink_denied", Stage: "copy"}},
		Authority: []review.AuthorityInclusion{{
			Name: "spec.md", Source: review.AuthoritySourcePath, FullHash: "sha256:aa", EmbeddedHash: "sha256:aa",
			BytesEmbedded: 10, BytesTotal: 10, Complete: true,
		}},
		ShownFiles: []string{"sample.go"},
	}
}

// fakeConfig supplies the list and doctor projections. Its readiness detail contains an absolute path,
// which the surface must strip.
type fakeConfig struct{}

func (fakeConfig) Adapters() []mcp.AdapterFact {
	return []mcp.AdapterFact{
		{Name: "fake", DisplayName: "Fake", Kind: "fake", Available: true, Source: "flag"},
		{Name: "claude-code", DisplayName: "Claude Code", Kind: "shell", Available: true, Source: "flag", IdentityEvidenceCapability: "envelope"},
	}
}

func (fakeConfig) Readiness() (bool, []mcp.ReadinessCheck) {
	return true, []mcp.ReadinessCheck{
		{Name: "adapter: claude-code", OK: true, Detail: "required — found at /usr/local/bin/claude (version 1.2.3)"},
	}
}

// workspaceFixture returns a real directory, so base hashes are computable.
func workspaceFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return dir
}

func newServer(t *testing.T, rv mcp.Reviewer, tune ...func(*mcp.Server)) *mcp.Server {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	t.Setenv(fake.EnvVar, "1")
	// Launch claude-code at an executable the test owns, so availability needs no installed CLI.
	cc := filepath.Join(t.TempDir(), "claude")
	if runtime.GOOS == "windows" {
		cc += ".exe"
	}
	if err := os.WriteFile(cc, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	s := &mcp.Server{
		Manager: rv, Config: fakeConfig{},
		Adapters: launchflags.NewSet(
			launchflags.Adapter{Name: "fake", Source: launchflags.SourceFlag},
			launchflags.Adapter{Name: "claude-code", Path: cc, Source: launchflags.SourceFlag},
		),
		Diagnostics: io.Discard,
	}
	for _, f := range tune {
		f(s)
	}
	return s
}

func serve(t *testing.T, s *mcp.Server) *client {
	t.Helper()
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	done := make(chan struct{})
	go func() { _ = s.Serve(sr, sw); close(done) }()
	t.Cleanup(func() {
		_ = cw.Close()
		<-done
		_ = sw.Close()
	})
	c := &client{f: proto.NewFramer(proto.FramingNewline, cr, cw)}
	resp, _ := c.call(t, "initialize", map[string]any{
		"protocolVersion": proto.LatestProtocolVersion,
		"clientInfo":      map[string]any{"name": "test-client", "version": "1"},
	})
	if resp.Error != nil {
		t.Fatalf("initialize: %+v", resp.Error)
	}
	c.notify("notifications/initialized", nil)
	return c
}

func toolNames(t *testing.T, c *client) map[string]map[string]any {
	t.Helper()
	resp, _ := c.call(t, "tools/list", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("tools/list: %+v", resp.Error)
	}
	out := map[string]map[string]any{}
	tools, _ := resp.Result["tools"].([]any)
	for _, tv := range tools {
		m, _ := tv.(map[string]any)
		name, _ := m["name"].(string)
		out[name] = m
	}
	return out
}

// --- write gating ---

// review_remediate is listed on every server; --allow-writes decides whether output=apply is accepted
// (see withheld_test.go).
func TestToolsList_RemediateIsAlwaysAdvertised(t *testing.T) {
	c := serve(t, newServer(t, &fakeReviewer{}))
	tools := toolNames(t, c)
	for _, want := range []string{"review_report", "review_remediate", "review_list", "review_doctor", "review_run_status", "review_run_result"} {
		if _, ok := tools[want]; !ok {
			t.Fatalf("tool %q is missing from tools/list", want)
		}
	}
}

func TestToolsList_AnnotationsMarkTheWriteToolDestructive(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) {
		s.Ceiling, s.AllowWrites = []string{ws}, true
	}))
	tools := toolNames(t, c)
	rem, ok := tools["review_remediate"]
	if !ok {
		t.Fatal("review_remediate must be listed once the capability is granted")
	}
	ann, _ := rem["annotations"].(map[string]any)
	if d, _ := ann["destructiveHint"].(bool); !d {
		t.Fatalf("review_remediate annotations = %+v, want destructiveHint true", ann)
	}
	if ro, _ := ann["readOnlyHint"].(bool); ro {
		t.Fatal("review_remediate must never carry readOnlyHint")
	}
	repAnn, _ := tools["review_report"]["annotations"].(map[string]any)
	if ro, _ := repAnn["readOnlyHint"].(bool); ro {
		t.Fatal("review_report SPENDS and must not carry readOnlyHint")
	}
	if ow, _ := repAnn["openWorldHint"].(bool); !ow {
		t.Fatal("review_report reaches external providers and must carry openWorldHint")
	}
	if idem, _ := repAnn["idempotentHint"].(bool); idem {
		t.Fatal("review_report must not claim idempotency")
	}
	// Only read-only tools claim to be read-only.
	for _, name := range []string{"review_list", "review_doctor", "review_run_status", "review_run_result"} {
		a, _ := tools[name]["annotations"].(map[string]any)
		if ro, _ := a["readOnlyHint"].(bool); !ro {
			t.Fatalf("%s must carry readOnlyHint", name)
		}
	}
	// A title must not contradict its hints: the host asks on the strength of the hint, and the user decides
	// on the strength of the title.
	for name, tool := range tools {
		a, _ := tool["annotations"].(map[string]any)
		title, _ := a["title"].(string)
		ro, _ := a["readOnlyHint"].(bool)
		if !ro && strings.Contains(strings.ToLower(title), "read-only") {
			t.Fatalf("%s: title %q claims read-only while readOnlyHint is false", name, title)
		}
		if ro && (strings.Contains(strings.ToLower(title), "writes") || strings.Contains(strings.ToLower(title), "spends")) {
			t.Fatalf("%s: title %q claims to write or spend while readOnlyHint is true", name, title)
		}
	}
	// Each write-related tool description names its sibling.
	if !strings.Contains(tools["review_report"]["description"].(string), "review_remediate") {
		t.Fatal("review_report's description must point at review_remediate")
	}
	if !strings.Contains(rem["description"].(string), "review_report") {
		t.Fatal("review_remediate's description must point at review_report")
	}
}

// --- allowWrite, checked before any spend ---

func TestRemediate_AllowWriteOmittedIsATeachingErrorBeforeAnySpend(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	res := c.tool(t, "review_remediate", map[string]any{"fromRun": "run-x", "output": "apply"})
	if res.rpc == nil {
		t.Fatalf("omitting allowWrite must be refused, got %+v", res)
	}
	if !strings.Contains(res.rpc.Message, "allowWrite") || !strings.Contains(res.rpc.Message, "allowWrite\": true") {
		t.Fatalf("the refusal must name the field AND show a corrected call, got %q", res.rpc.Message)
	}
	if runs, rem := rv.counts(); runs != 0 || rem != 0 {
		t.Fatalf("nothing may be spent: runs=%d remediations=%d", runs, rem)
	}
	// allowWrite: false is refused like an absent one.
	res = c.tool(t, "review_remediate", map[string]any{"fromRun": "run-x", "output": "apply", "allowWrite": false})
	if res.rpc == nil {
		t.Fatal("allowWrite: false must be refused")
	}
}

// --- per-call scope ---

func TestReport_PathOutsideTheCeilingIsRefusedPreSpend(t *testing.T) {
	ws := workspaceFixture(t)
	outside := t.TempDir()
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	res := c.tool(t, "review_report", map[string]any{"workspace": outside})
	if !res.isError {
		t.Fatalf("a path outside the --root ceiling must be an isError result, got %+v", res)
	}
	if got, _ := res.structured["reasonCode"].(string); got != acp.ReasonOutsideCeiling {
		t.Fatalf("reasonCode = %q, want %s", got, acp.ReasonOutsideCeiling)
	}
	if hc, _ := res.structured["haltClass"].(string); hc != "M6" {
		t.Fatalf("haltClass = %q, want M6", hc)
	}
	if runs, _ := rv.counts(); runs != 0 {
		t.Fatalf("the refusal must happen BEFORE any spend, got %d run(s)", runs)
	}
	// The refusal must not echo the operator's roots.
	if strings.Contains(res.text, ws) {
		t.Fatalf("the refusal leaked a trusted-root path: %q", res.text)
	}
}

// With no --root ceiling, a call reviews the absolute workspace it declares, and the filesystem root is
// still refused.
func TestReport_WithNoCeilingACallReviewsTheWorkspaceItDeclares(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv)) // deliberately no ceiling
	if res := c.tool(t, "review_report", map[string]any{"workspace": ws}); res.isError || res.rpc != nil {
		t.Fatalf("a declared absolute workspace must be reviewable with no ceiling: %+v %+v", res.structured, res.rpc)
	}
	fsRoot := filepath.VolumeName(ws) + string(filepath.Separator)
	if res := c.tool(t, "review_report", map[string]any{"workspace": fsRoot}); !res.isError {
		t.Fatalf("the filesystem root must be refused as a workspace, got %+v", res)
	}
	if runs, _ := rv.counts(); runs != 1 {
		t.Fatalf("only the declared workspace may have spent: %d run(s)", runs)
	}
}

// --- governance ---

func TestReport_GovernanceFieldsAreAlwaysInStructuredContent(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	res := c.tool(t, "review_report", map[string]any{"workspace": ws})
	if res.isError || res.rpc != nil {
		t.Fatalf("report failed: %+v", res)
	}
	for _, key := range []string{"runId", "state", "mode", "status", "findings", "counts", "identityCaveats", "authority", "withheld", "panel", "workspaceSource"} {
		if _, ok := res.structured[key]; !ok {
			t.Fatalf("structuredContent is missing the required field %q", key)
		}
	}
	// The run directory is a host path and must never be sent.
	blob, _ := json.Marshal(res.structured)
	if strings.Contains(string(blob), "/private/tmp/artifacts") {
		t.Fatalf("the payload leaked the run directory: %s", blob)
	}
	// Per-seat provenance.
	findings, _ := res.structured["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("findings = %v", findings)
	}
	f0, _ := findings[0].(map[string]any)
	seats, _ := f0["supportingSeats"].([]any)
	if len(seats) != 2 {
		t.Fatalf("supportingSeats = %v, want 2", seats)
	}
	if ac, _ := f0["agreementCount"].(float64); ac != 2 {
		t.Fatalf("agreementCount = %v, want 2 (host-computed, carried through)", f0["agreementCount"])
	}
	if ds, _ := f0["dissentingSeats"].([]any); len(ds) != 1 {
		t.Fatalf("dissentingSeats = %v, want 1", f0["dissentingSeats"])
	}
	// Requested and executed roster.
	panel, _ := res.structured["panel"].(map[string]any)
	if exec, _ := panel["executed"].([]any); len(exec) != 3 {
		t.Fatalf("executed roster = %v, want 3 seats", panel["executed"])
	}
	if req, _ := panel["requested"].(map[string]any); req["source"] != "adhoc" {
		t.Fatalf("requested = %v, want source adhoc", panel["requested"])
	}
	// The identity caveat and withheld file reach the caller, in structured and text form.
	if cav, _ := res.structured["identityCaveats"].([]any); len(cav) != 1 {
		t.Fatalf("identityCaveats = %v, want 1", res.structured["identityCaveats"])
	}
	if !strings.Contains(res.text, "Identity caveats") || !strings.Contains(res.text, "WITHHELD") {
		t.Fatalf("the text channel must surface caveats and withheld files, got:\n%s", res.text)
	}
	if !strings.Contains(res.text, "wrote NOTHING") {
		t.Fatalf("review_report's rendering must state that it wrote nothing, got:\n%s", res.text)
	}
}

func TestReport_HaltRidesIsErrorWithTheTaxonomy(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{runErr: fault.New(fault.Model, "adapter reported a different model").
		WithHalt("E").WithReason("model_identity_mismatch")}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	res := c.tool(t, "review_report", map[string]any{"workspace": ws})
	if res.rpc != nil {
		t.Fatalf("a domain halt must NOT be a JSON-RPC error: %+v", res.rpc)
	}
	if !res.isError {
		t.Fatal("a domain halt must ride isError")
	}
	if got, _ := res.structured["reasonCode"].(string); got != "model_identity_mismatch" {
		t.Fatalf("reasonCode = %q", got)
	}
	if got, _ := res.structured["exitCode"].(float64); int(got) != int(fault.Model) {
		t.Fatalf("exitCode = %v, want %d", res.structured["exitCode"], fault.Model)
	}
	if got, _ := res.structured["haltClass"].(string); got != "E" {
		t.Fatalf("haltClass = %q, want E", got)
	}
	if !strings.Contains(res.text, "reasonCode=model_identity_mismatch") {
		t.Fatalf("the text channel must carry the taxonomy too, got %q", res.text)
	}
}

// --- report → fromRun remediation ---

func reportRun(t *testing.T, c *client, ws string) string {
	t.Helper()
	res := c.tool(t, "review_report", map[string]any{"workspace": ws})
	if res.isError || res.rpc != nil {
		t.Fatalf("report failed: %+v", res)
	}
	id, _ := res.structured["runId"].(string)
	if id == "" {
		t.Fatal("the report must return a runId")
	}
	return id
}

func TestRemediate_FromRunAppliesTheAcceptedSetAndIsIdempotent(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	res := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "workspace": ws, "output": "apply", "allowWrite": true})
	if res.isError || res.rpc != nil {
		t.Fatalf("remediate failed: %+v %+v", res.structured, res.rpc)
	}
	receipt, _ := res.structured["receipt"].(map[string]any)
	if receipt == nil {
		t.Fatalf("the result must carry the receipt, got %+v", res.structured)
	}
	if committed, _ := receipt["committed"].(bool); !committed {
		t.Fatalf("apply must report a commit, receipt = %+v", receipt)
	}
	if intended, _ := receipt["intended"].([]any); len(intended) != 1 {
		t.Fatalf("the receipt must carry the journal, got %+v", receipt["intended"])
	}
	if art, _ := receipt["patchArtifact"].(string); art == "" {
		t.Fatal("the receipt must reference the complete patch")
	}
	if strings.Contains(res.text, "diff --git") {
		t.Fatal("the patch content must never be inlined in the result")
	}
	// The decision set reached the manager unchanged, with the shown-files gate and base hashes.
	rv.mu.Lock()
	got := rv.lastRemediate
	rv.mu.Unlock()
	if got.SourceRunID != runID {
		t.Fatalf("sourceRunId = %q, want %q", got.SourceRunID, runID)
	}
	if len(got.Findings) != 1 || len(got.Decisions) != 1 {
		t.Fatalf("the accepted set was not carried through: %+v", got)
	}
	if !got.Shown["sample.go"] {
		t.Fatal("the shown-files gate from the report run must ride the remediation")
	}
	if _, ok := got.BaseHashes["sample.go"]; !ok {
		t.Fatalf("base hashes must be captured at report time, got %+v", got.BaseHashes)
	}

	// Replaying the same source run returns the original receipt without a second application, even with no
	// idempotency key.
	res2 := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "workspace": ws, "output": "apply", "allowWrite": true})
	if res2.isError || res2.rpc != nil {
		t.Fatalf("replay failed: %+v", res2)
	}
	if _, rem := rv.counts(); rem != 1 {
		t.Fatalf("a replay must NOT write again: %d remediation(s)", rem)
	}
	r2, _ := res2.structured["receipt"].(map[string]any)
	if r2["runId"] != receipt["runId"] {
		t.Fatalf("the replay returned a different receipt: %v vs %v", r2["runId"], receipt["runId"])
	}
}

func TestRemediate_UnknownRunIsAnIsErrorResult(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	res := c.tool(t, "review_remediate", map[string]any{"fromRun": "run-nope", "workspace": ws, "output": "patch"})
	if res.rpc != nil {
		t.Fatalf("an unknown run must not be a protocol error: %+v", res.rpc)
	}
	if !res.isError {
		t.Fatal("an unknown run must ride isError")
	}
	if got, _ := res.structured["reasonCode"].(string); got != "unknown_run_id" {
		t.Fatalf("reasonCode = %q", got)
	}
}

// --- inline workspaces are never remediable ---

func TestInlineWorkspace_ReviewableButNeverRemediable(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	res := c.tool(t, "review_report", map[string]any{
		"inlineWorkspace": map[string]any{"sample.go": "package sample\n"},
	})
	if res.isError || res.rpc != nil {
		t.Fatalf("an inline review must work: %+v %+v", res.structured, res.rpc)
	}
	if src, _ := res.structured["workspaceSource"].(string); src != "inline" {
		t.Fatalf("workspaceSource = %q, want inline (model-authored input must be distinguishable)", src)
	}
	if rem, _ := res.structured["remediable"].(bool); rem {
		t.Fatal("an inline run must never be reported as remediable")
	}
	runID, _ := res.structured["runId"].(string)

	// Applying an inline run is refused, naming the reason.
	got := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "workspace": ws, "output": "apply", "allowWrite": true})
	if !got.isError {
		t.Fatalf("remediating an inline run must fail closed, got %+v", got)
	}
	if code, _ := got.structured["reasonCode"].(string); code != "inline_workspace_not_remediable" {
		t.Fatalf("reasonCode = %q", code)
	}
	if _, rem := rv.counts(); rem != 0 {
		t.Fatalf("nothing may be written: %d remediation(s)", rem)
	}
}

func TestRemediate_InlineWorkspaceParameterIsRefused(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	res := c.tool(t, "review_remediate", map[string]any{
		"inlineWorkspace": map[string]any{"a.go": "package a\n"}, "output": "apply", "allowWrite": true,
	})
	if res.rpc == nil || !strings.Contains(res.rpc.Message, "inlineWorkspace") {
		t.Fatalf("inlineWorkspace + a write must be a field-addressed refusal, got %+v", res)
	}
}

// --- panel composition ---

func TestPanel_TeachingErrors(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "an ad-hoc panel must name its adjudicator",
			args: map[string]any{"workspace": ws, "panel": map[string]any{
				"reviewers": []any{map[string]any{"adapter": "fake", "model": "m1"}},
			}},
			want: "author_remediator",
		},
		{
			name: "a panel is required",
			args: map[string]any{"workspace": ws, "panel": nil},
			want: "`panel` is required",
		},
		{
			name: "a panel needs a reviewer",
			args: map[string]any{"workspace": ws, "panel": map[string]any{
				"author_remediator": map[string]any{"adapter": "fake", "model": "m1"},
			}},
			want: "panel.reviewers",
		},
		{
			name: "an adapter the server was not launched with cannot be introduced",
			args: map[string]any{"workspace": ws, "panel": map[string]any{
				"reviewers":         []any{map[string]any{"adapter": "not-launched", "model": "m1"}},
				"author_remediator": map[string]any{"adapter": "fake", "model": "m1"},
			}},
			want: "was not launched with",
		},
		{
			name: "profile is not a parameter",
			args: map[string]any{"workspace": ws, "profile": "default"},
			want: "unknown field",
		},
		{
			name: "per-role effort is refused",
			args: map[string]any{"workspace": ws, "panel": map[string]any{
				"reviewers":         []any{map[string]any{"adapter": "fake", "model": "m1"}},
				"author_remediator": map[string]any{"adapter": "fake", "model": "m1", "effort": "deep"},
			}},
			want: "effort is not accepted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := c.tool(t, "review_report", tc.args)
			if res.rpc == nil {
				t.Fatalf("expected a teaching error, got %+v", res)
			}
			if !strings.Contains(res.rpc.Message, tc.want) {
				t.Fatalf("message %q does not contain %q", res.rpc.Message, tc.want)
			}
		})
	}
	if runs, _ := rv.counts(); runs != 0 {
		t.Fatalf("a malformed request must never spend: %d run(s)", runs)
	}
}

func TestPanel_AdHocSelectionReachesTheManagerAndIsEchoed(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	res := c.tool(t, "review_report", map[string]any{"workspace": ws, "panel": map[string]any{
		"reviewers": []any{
			map[string]any{"adapter": "fake", "model": "m1"},
			map[string]any{"adapter": "claude-code", "model": "m2", "effort": "deep"},
		},
		"author_remediator": map[string]any{"adapter": "fake", "model": "m1"},
		"cross_check":       map[string]any{"adapter": "claude-code", "model": "cc"},
	}})
	if res.isError || res.rpc != nil {
		t.Fatalf("ad-hoc panel failed: %+v %+v", res.structured, res.rpc)
	}
	rv.mu.Lock()
	got := rv.lastRun
	rv.mu.Unlock()
	if len(got.ReviewerPanel) != 2 || got.ReviewerPanel[1].Effort != "deep" {
		t.Fatalf("the composed panel did not reach the manager: %+v", got.ReviewerPanel)
	}
	if seat := got.ComposedRoles[review.RoleCrossCheck]; seat.Adapter != "claude-code" || seat.Model != "cc" {
		t.Fatalf("the cross_check seat did not reach the manager: %+v", got.ComposedRoles)
	}
	if got.Surface != "mcp" {
		t.Fatalf("surface = %q, want mcp (the policy ceiling is keyed on it)", got.Surface)
	}
	if len(got.TrustedRoots) == 0 {
		t.Fatal("the trusted roots must ride the request so the manager resolves paths against the same set")
	}
	panel, _ := res.structured["panel"].(map[string]any)
	req, _ := panel["requested"].(map[string]any)
	if req["source"] != "adhoc" {
		t.Fatalf("requested panel echo = %+v, want source adhoc", req)
	}
	if _, ok := req["author_remediator"]; !ok {
		t.Fatalf("the requested echo must name the adjudicator: %+v", req)
	}
}

// --- idempotency on the review path ---

func TestReport_IdempotencyKeyReturnsTheOriginalRun(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	first := c.tool(t, "review_report", map[string]any{"workspace": ws, "idempotencyKey": "k1"})
	second := c.tool(t, "review_report", map[string]any{"workspace": ws, "idempotencyKey": "k1"})
	if runs, _ := rv.counts(); runs != 1 {
		t.Fatalf("a repeated key must not spend twice: %d run(s)", runs)
	}
	if first.structured["runId"] != second.structured["runId"] {
		t.Fatalf("the repeat returned a different run: %v vs %v", first.structured["runId"], second.structured["runId"])
	}
}

// --- the sanitized projections ---

func TestList_ReportsNoPathsAndStatesWhoWrites(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	res := c.tool(t, "review_list", map[string]any{})
	if res.isError || res.rpc != nil {
		t.Fatalf("list failed: %+v", res)
	}
	rem, _ := res.structured["remediation"].(map[string]any)
	if rem["writes"] != "agent" {
		t.Fatalf("remediation.writes = %v, want agent on a server launched without --allow-writes", rem["writes"])
	}
	if d, _ := rem["diffAvailable"].(bool); !d {
		t.Fatalf("remediation.diffAvailable = %v, want true", rem["diffAvailable"])
	}
	adapters, _ := res.structured["adapters"].([]any)
	if len(adapters) != 2 {
		t.Fatalf("adapters = %v, want the two this server was launched with", adapters)
	}
	roots, _ := res.structured["roots"].(map[string]any)
	if cnt, _ := roots["ceiling"].(float64); int(cnt) != 1 {
		t.Fatalf("roots.ceiling = %v, want 1", roots["ceiling"])
	}
	blob, _ := json.Marshal(res.structured)
	if strings.Contains(string(blob), ws) {
		t.Fatalf("list leaked a trusted-root path: %s", blob)
	}
}

func TestDoctor_SanitizesPathsOutOfReadinessDetail(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	res := c.tool(t, "review_doctor", map[string]any{})
	checks, _ := res.structured["checks"].([]any)
	// One row: the adapter readiness check this test stages.
	if len(checks) != 1 {
		t.Fatalf("checks = %v", res.structured["checks"])
	}
	detail, _ := checks[0].(map[string]any)["detail"].(string)
	if strings.Contains(detail, "/usr/local/bin/claude") {
		t.Fatalf("doctor leaked a binary path: %q", detail)
	}
	// Redacted, not dropped: the check still says what it found, without saying where.
	if !strings.Contains(detail, "<path>") || !strings.Contains(detail, "version 1.2.3") {
		t.Fatalf("the path should have been redacted in place, got %q", detail)
	}
}

// --- run_status / run_result ---

func TestRunResult_ReReadsWithoutRerunning(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	runID := reportRun(t, c, ws)
	status := c.tool(t, "review_run_status", map[string]any{"runId": runID})
	if st, _ := status.structured["state"].(string); st != "complete" {
		t.Fatalf("state = %q, want complete", st)
	}
	again := c.tool(t, "review_run_result", map[string]any{"runId": runID})
	if again.structured["runId"] != runID {
		t.Fatalf("run_result returned %v", again.structured["runId"])
	}
	if runs, _ := rv.counts(); runs != 1 {
		t.Fatalf("re-reading a run must not re-run it: %d run(s)", runs)
	}
}

// --- client-declared roots narrow, never widen ---

func TestClientRoots_IntersectNeverUnion(t *testing.T) {
	parent := t.TempDir()
	sub := filepath.Join(parent, "sub")
	other := filepath.Join(parent, "other")
	unrelated := t.TempDir()
	for _, d := range []string{sub, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// The client narrows the server's root to a subdirectory.
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) {
		s.Ceiling, s.ClientRoots = []string{parent}, []string{sub}
	}))
	if res := c.tool(t, "review_report", map[string]any{"workspace": sub}); res.isError {
		t.Fatalf("a path inside the intersection must be accepted: %+v", res.structured)
	}
	if res := c.tool(t, "review_report", map[string]any{"workspace": other}); !res.isError {
		t.Fatal("a path inside the SERVER root but outside the client's must be refused (intersection, not union)")
	}

	// A client root outside the server's scope grants nothing: the intersection is empty.
	rv2 := &fakeReviewer{}
	c2 := serve(t, newServer(t, rv2, func(s *mcp.Server) {
		s.Ceiling, s.ClientRoots = []string{parent}, []string{unrelated}
	}))
	for _, p := range []string{parent, sub, unrelated} {
		if res := c2.tool(t, "review_report", map[string]any{"workspace": p}); !res.isError {
			t.Fatalf("a disjoint client root must leave NO effective root, but %q was accepted", p)
		}
	}
	if runs, _ := rv2.counts(); runs != 0 {
		t.Fatalf("nothing may be spent: %d run(s)", runs)
	}
}

// --- cancellation: the receipt survives the missing response ---

// blockingReviewer's remediation waits for cancellation, then reports a receipt saying nothing was
// committed.
type blockingReviewer struct {
	fakeReviewer
	entered chan struct{}
	once    sync.Once
}

func (b *blockingReviewer) Remediate(ctx context.Context, r run.RemediateRequest) (run.RemediateOutcome, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	out := run.RemediateOutcome{
		RunID: "audit-run", SourceRunID: r.SourceRunID, Mode: r.Mode, Cancelled: true,
		Receipt: run.Receipt{
			SchemaVersion: 1, RunID: "audit-run", SourceRunID: r.SourceRunID, Mode: string(r.Mode),
			Status: "cancelled", ReasonCode: run.ReasonRemediateCancelled,
			Intended: []run.IntendedHunk{{FindingID: "F1", File: "sample.go", Origin: "marker"}},
			Applied:  []run.AppliedFinding{}, Files: []string{}, Committed: false,
		},
	}
	return out, fault.Wrap(fault.Policy, "remediation cancelled before apply", ctx.Err()).
		WithHalt("F").WithReason(run.ReasonRemediateCancelled)
}

func TestRemediate_CancelledCallStillPublishesTheReceipt(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &blockingReviewer{entered: make(chan struct{})}
	s := newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true })
	c := serve(t, s)
	runID := reportRun(t, c, ws)

	// Send the remediation without waiting, then cancel it.
	c.id++
	callID := c.id
	c.send("tools/call", callID, map[string]any{
		"name":      "review_remediate",
		"arguments": map[string]any{"fromRun": runID, "workspace": ws, "output": "apply", "allowWrite": true, "waitSeconds": 60},
	})
	<-rv.entered
	c.notify("notifications/cancelled", map[string]any{"requestId": callID, "reason": "user stopped"})

	// The receipt arrives as a log notification, since a cancelled request gets no response.
	frames := make(chan []byte, 32)
	go func() {
		for {
			raw, err := c.f.ReadMessage()
			if err != nil {
				close(frames)
				return
			}
			frames <- raw
		}
	}()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case raw, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before the receipt notification arrived")
			}
			var resp response
			if json.Unmarshal(raw, &resp) == nil && len(resp.ID) > 0 {
				var got int
				if json.Unmarshal(resp.ID, &got) == nil && got == callID {
					t.Fatal("a cancelled request must receive NO response")
				}
				continue
			}
			var n notification
			if json.Unmarshal(raw, &n) != nil || n.Method != "notifications/message" {
				continue
			}
			data, _ := n.Params["data"].(map[string]any)
			if data["eventType"] != "remediation_receipt" {
				continue
			}
			receipt, _ := data["receipt"].(map[string]any)
			if receipt == nil {
				t.Fatalf("the receipt notification carries no receipt: %+v", data)
			}
			if receipt["status"] != "cancelled" || receipt["committed"] != false {
				t.Fatalf("receipt = %+v, want cancelled and uncommitted", receipt)
			}
			if intended, _ := receipt["intended"].([]any); len(intended) != 1 {
				t.Fatalf("the receipt must still say what it INTENDED to write: %+v", receipt["intended"])
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the receipt notification")
		}
	}
}

// --- declared outputSchema versus the wire ---

// slowReviewer blocks in RunContext until released, so a test can observe a running run: the running
// reply, a mid-flight run_result, and notifications emitted after the call was answered.
type slowReviewer struct {
	fakeReviewer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *slowReviewer) RunContext(ctx context.Context, r run.Request) (review.RunOutcome, error) {
	s.once.Do(func() { close(s.entered) })
	if r.OnEvent != nil {
		r.OnEvent(audit.EventLine{EventType: "panel_started", Level: "info", Message: "panel started"})
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return review.RunOutcome{Status: "halted", Mode: r.Mode}, ctx.Err()
	}
	// A phase event after the inline wait was answered; an armed progress sink would leak it.
	if r.OnEvent != nil {
		r.OnEvent(audit.EventLine{EventType: "panel_completed", Level: "info", Message: "panel completed"})
	}
	return cannedOutcome(r.Mode), nil
}

// declaredOutputSchemas compiles each tool's outputSchema as the client receives it, proving the
// constants are what a client gets.
func declaredOutputSchemas(t *testing.T, c *client) map[string]*jsonschema.Schema {
	t.Helper()
	out := map[string]*jsonschema.Schema{}
	for name, tool := range toolNames(t, c) {
		raw, ok := tool["outputSchema"]
		if !ok {
			t.Fatalf("tool %q declares no outputSchema — a structured result nothing describes is not machine-readable", name)
		}
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("tool %q: %v", name, err)
		}
		s, cerr := jsonschema.Compile(b)
		if cerr != nil {
			t.Fatalf("tool %q: its declared outputSchema does not compile: %v", name, cerr)
		}
		out[name] = s
	}
	return out
}

// Every structuredContent from real calls, including a cancelled call, run_result on a running run and
// run_result for a remediation, validates against its tool's declared outputSchema.
func TestWire_EveryStructuredContentValidatesAgainstTheDeclaredOutputSchema(t *testing.T) {
	ws := workspaceFixture(t)
	outside := t.TempDir()
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	schemas := declaredOutputSchemas(t, c)

	check := func(t *testing.T, tool string, res toolResult) {
		t.Helper()
		if res.rpc != nil {
			t.Fatalf("%s: unexpected protocol error %+v", tool, res.rpc)
		}
		if res.structured == nil {
			t.Fatalf("%s: no structuredContent — half of every client sees only this channel", tool)
		}
		if err := schemas[tool].Validate(res.structured); err != nil {
			t.Fatalf("%s emitted a payload its own declared outputSchema rejects: %v", tool, err)
		}
	}

	runID := reportRun(t, c, ws)
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"review_list", "review_list", map[string]any{}},
		{"review_doctor", "review_doctor", map[string]any{}},
		{"a completed review", "review_report", map[string]any{"workspace": ws}},
		{"a refusal outside the trusted roots", "review_report", map[string]any{"workspace": outside}},
		{"run_status on a finished run", "review_run_status", map[string]any{"runId": runID}},
		{"run_result on a finished review", "review_run_result", map[string]any{"runId": runID}},
		{"run_result on an unknown run", "review_run_result", map[string]any{"runId": "run-nope"}},
		{"a remediation receipt", "review_remediate", map[string]any{"fromRun": runID, "workspace": ws, "output": "patch", "allowWrite": true}},
	} {
		t.Run(tc.name, func(t *testing.T) { check(t, tc.tool, c.tool(t, tc.tool, tc.args)) })
	}

	// The remediation's own run via run_result, which must not be declared under the review schema.
	remID := func() string {
		res := c.tool(t, "review_remediate", map[string]any{"fromRun": runID, "workspace": ws, "output": "patch", "allowWrite": true})
		id, _ := res.structured["runId"].(string)
		return id
	}()
	t.Run("run_result on a remediation", func(t *testing.T) {
		check(t, "review_run_result", c.tool(t, "review_run_result", map[string]any{"runId": remID}))
	})

	// A running run: the running reply and a mid-flight run_result.
	slow := &slowReviewer{entered: make(chan struct{}), release: make(chan struct{})}
	c2 := serve(t, newServer(t, slow, func(s *mcp.Server) { s.Ceiling, s.WaitSeconds = []string{ws}, 1 }))
	schemas2 := declaredOutputSchemas(t, c2)
	running := c2.tool(t, "review_report", map[string]any{"workspace": ws})
	if st, _ := running.structured["state"].(string); st != "running" {
		t.Fatalf("state = %q, want running (the inline budget was 1s)", st)
	}
	if err := schemas2["review_report"].Validate(running.structured); err != nil {
		t.Fatalf("the `running` reply does not satisfy review_report's declared schema: %v", err)
	}
	midflight := c2.tool(t, "review_run_result", map[string]any{"runId": running.structured["runId"]})
	if err := schemas2["review_run_result"].Validate(midflight.structured); err != nil {
		t.Fatalf("run_result on a still-running review does not satisfy its declared schema: %v", err)
	}
	if _, ok := midflight.structured["panel"]; !ok {
		t.Fatalf("run_result on a still-running review dropped the panel echo: %+v", midflight.structured)
	}
	close(slow.release)
}

// --- progress stops when the call is answered ---

// Once the request that supplied the progress token has been answered with state "running", the run
// stops sending progress for it.
func TestProgress_StopsWhenTheCallHasBeenAnswered(t *testing.T) {
	ws := workspaceFixture(t)
	slow := &slowReviewer{entered: make(chan struct{}), release: make(chan struct{})}
	c := serve(t, newServer(t, slow, func(s *mcp.Server) { s.Ceiling, s.WaitSeconds = []string{ws}, 1 }))

	c.id++
	callID := c.id
	c.send("tools/call", callID, map[string]any{
		"name":      "review_report",
		"arguments": map[string]any{"workspace": ws, "panel": defaultPanel()},
		"_meta":     map[string]any{"progressToken": "p1"},
	})

	// Drain to the response, keeping notifications sent while the call was live.
	var before []notification
	for {
		raw, err := c.f.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var resp response
		if json.Unmarshal(raw, &resp) == nil && len(resp.ID) > 0 {
			var got int
			if json.Unmarshal(resp.ID, &got) == nil && got == callID {
				break
			}
			continue
		}
		var n notification
		if json.Unmarshal(raw, &n) == nil && n.Method != "" {
			before = append(before, n)
		}
	}
	progressCount := func(ns []notification) int {
		n := 0
		for _, note := range ns {
			if note.Method == "notifications/progress" {
				n++
			}
		}
		return n
	}
	if progressCount(before) == 0 {
		t.Fatal("no progress arrived while the call was live — the fixture proves nothing about disarming")
	}

	// The run emits another phase event, which must not become progress.
	close(slow.release)
	frames := make(chan []byte, 64)
	go func() {
		for {
			raw, err := c.f.ReadMessage()
			if err != nil {
				close(frames)
				return
			}
			frames <- raw
		}
	}()
	deadline := time.After(3 * time.Second)
	sawLater := false
	for done := false; !done; {
		select {
		case raw, ok := <-frames:
			if !ok {
				done = true
				break
			}
			var n notification
			if json.Unmarshal(raw, &n) == nil && n.Method == "notifications/progress" {
				sawLater = true
				done = true
			}
		case <-deadline:
			done = true
		}
	}
	if sawLater {
		t.Fatal("a progress notification arrived AFTER the call was answered — the sink was never disarmed")
	}
}

// --- review_remediate refuses fromRun arguments it cannot honor ---

// review_remediate refuses panel, authority, dryRun and verifyReadiness by name: the decoder embeds
// runArgs, and those inputs belong to the source run or to a review.
func TestRemediate_FromRunRefusesRunFormingParameters(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling, s.AllowWrites = []string{ws}, true }))
	runID := reportRun(t, c, ws)

	cases := []struct {
		name string
		add  map[string]any
		want string
	}{
		{"panel", map[string]any{"panel": map[string]any{
			"reviewers":         []any{map[string]any{"adapter": "fake", "model": "m1"}},
			"author_remediator": map[string]any{"adapter": "fake", "model": "m1"},
		}}, "`panel`"},
		{"authority", map[string]any{"authority": []any{map[string]any{"name": "spec.md", "path": "spec.md"}}}, "`authority`"},
		{"dryRun", map[string]any{"dryRun": true}, "`dryRun`"},
		{"verifyReadiness", map[string]any{"verifyReadiness": true}, "`verifyReadiness`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"fromRun": runID, "workspace": ws, "output": "patch", "allowWrite": true}
			maps.Copy(args, tc.add)
			res := c.tool(t, "review_remediate", args)
			if res.rpc == nil {
				t.Fatalf("%s on the fromRun branch must be REFUSED, not ignored; got %+v", tc.name, res)
			}
			if !strings.Contains(res.rpc.Message, tc.want) || !strings.Contains(res.rpc.Message, "fromRun") {
				t.Fatalf("the refusal must name the parameter AND the branch, got %q", res.rpc.Message)
			}
			if _, rem := rv.counts(); rem != 0 {
				t.Fatalf("nothing may be written: %d remediation(s)", rem)
			}
		})
	}

	// There is no one-call review-and-write; the refusal teaches the two-step path.
	gone := c.tool(t, "review_remediate", map[string]any{
		"workspace": ws, "output": "patch",
	})
	if gone.rpc == nil {
		t.Fatalf("the full-cycle `workspace` branch must be refused on MCP, got %+v", gone)
	}
	if !strings.Contains(gone.rpc.Message, "review_report") || !strings.Contains(gone.rpc.Message, "fromRun") {
		t.Fatalf("the refusal must teach the two-step path (review_report → review_remediate {fromRun}), got %q", gone.rpc.Message)
	}
	if _, rem := rv.counts(); rem != 0 {
		t.Fatalf("nothing may be written: %d remediation(s)", rem)
	}
}

// --- admission ---

// There is no lifetime run cap: a server that has run still accepts another run.
func TestAdmission_ThereIsNoLifetimeRunCap(t *testing.T) {
	ws := workspaceFixture(t)
	rv := &fakeReviewer{}
	c := serve(t, newServer(t, rv, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	reportRun(t, c, ws)
	res := c.tool(t, "review_report", map[string]any{"workspace": ws})
	if res.isError {
		t.Fatalf("a second run must not be refused by any lifetime bound: %+v", res)
	}
}
