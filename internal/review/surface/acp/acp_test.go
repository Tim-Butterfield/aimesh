package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// noRemediation is the FROM-RUN half of Reviewer, stubbed to REFUSE.
//
// A write turn carrying `fromRun` no longer runs a review cycle: it resolves the handle and applies
// the decision set that run recorded. Most fakes here are about dispatch, mode gating, sessions or
// authority and never reach a write, so they embed this. Refusing rather than quietly succeeding is
// the fail-closed default — a stub that answered "sure, applied" would let a from-run assertion pass
// without a decision set ever being read.
type noRemediation struct{}

func (noRemediation) ReadDecisionSet(string) (*run.StoredDecisionSet, string, error) {
	return nil, "", fault.New(fault.Usage, "this fake resolves no run handles").
		WithReason(run.ReasonRunHandleUnknown)
}

func (noRemediation) Remediate(context.Context, run.RemediateRequest) (run.RemediateOutcome, error) {
	return run.RemediateOutcome{}, fault.New(fault.Internal, "this fake performs no remediation")
}

// stubDecisionSet is a minimal, always-remediable set for the fakes that DO answer a handle. `ws` is
// the directory the harness substituted for wsPlaceholder — the fake has to claim it, because a
// from-run turn that named a workspace is checked against the source run's.
func stubDecisionSet(handle, ws string) *run.StoredDecisionSet {
	return &run.StoredDecisionSet{
		SchemaVersion: 1, RunID: filepath.Base(handle),
		Workspace: ws, WorkspaceCanonical: canonicalForTest(ws),
		Accepted:  1,
		Findings:  []review.Finding{{ID: "F-001", Kind: "bug", File: "a.go", Title: "boom"}},
		Decisions: []review.Decision{{FindingID: "F-001", Valid: true, State: review.StateReportedValid}},
		Shown:     []string{"a.go"}, BaseHashes: map[string]string{"a.go": "sha256:x"},
	}
}

// canonicalForTest resolves symlinks the way the surface's workspace check does (on macOS every
// t.TempDir() is under a symlinked /var, so a test that skipped this would compare two spellings of
// one directory and call them different).
func canonicalForTest(p string) string {
	if c, err := filepath.EvalSymlinks(p); err == nil {
		return c
	}
	return p
}

// workspaceAware lets runServer tell a fake which directory it substituted for wsPlaceholder. The
// harness invents that path, so a fake that has to answer "which tree did the source run judge?"
// cannot know it any other way.
type workspaceAware interface{ setWorkspace(string) }

// fakeReviewer is a deterministic Manager stand-in for the ACP harness. It reaches no write.
type fakeReviewer struct {
	noRemediation
	out   review.RunOutcome
	err   error
	block bool // if set, RunContext blocks until ctx is cancelled (cancellation test)
}

func (f fakeReviewer) RunContext(ctx context.Context, r run.Request) (review.RunOutcome, error) {
	o := f.out
	o.Mode = r.Mode
	if o.RunDir == "" {
		o.RunDir = "/tmp/run-x"
	}
	if f.block {
		<-ctx.Done()
		return o, ctx.Err()
	}
	return o, f.err
}

func serve(t *testing.T, mgr Reviewer, lines ...string) []map[string]any {
	t.Helper()
	return serveStore(t, mgr, nil, lines...)
}

// harnessRoots are the trusted roots the GENERIC harness servers below are launched with.
// These tests are about dispatch, sessions and mode gating — not confinement — so the harness
// stands in for an operator who ran `aimesh review acp --root <os-temp> --root <package-dir>`:
// every workspace they name is a `t.TempDir()` under the OS temp dir, and one test exercises
// the process-cwd fallback. Confinement itself is proven in scope_test.go, where each server
// is built with ONE narrow root and the requested path is deliberately outside it.
func harnessRoots(t *testing.T) []string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return []string{os.TempDir(), wd}
}

// wsPlaceholder is the literal that tests write for "some workspace path". runServer swaps it
// for a real directory inside a trusted root, because a path is no longer self-authorizing:
// the server judges it against the roots it was launched with.
const wsPlaceholder = `"/ws"`

// runServer feeds lines to srv and decodes the response frames, substituting wsPlaceholder
// with a real directory first.
func runServer(t *testing.T, srv *Server, lines ...string) []map[string]any {
	t.Helper()
	dir := t.TempDir()
	// A from-run fake has to be able to claim this directory as the source run's workspace: the
	// harness invents the path, so the fake cannot know it any other way. See workspaceAware.
	if wa, ok := srv.Manager.(workspaceAware); ok {
		wa.setWorkspace(dir)
	}
	ws := strconv.Quote(dir)
	subst := make([]string, len(lines))
	for i, l := range lines {
		subst[i] = strings.ReplaceAll(l, wsPlaceholder, ws)
	}
	var out bytes.Buffer
	if err := srv.Serve(strings.NewReader(strings.Join(subst, "\n")+"\n"), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resps []map[string]any
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		resps = append(resps, m)
	}
	return resps
}

// serveStore runs a Server wired with a durable SessionStore (nil = none) — for cross-instance
// resume tests, where a fresh Server sharing the same store stands in for a restarted agent.
func serveStore(t *testing.T, mgr Reviewer, store SessionStore, lines ...string) []map[string]any {
	t.Helper()
	return runServer(t, &Server{Manager: mgr, Caps: review.SurfaceCaps{FileRead: true, FileWrite: true},
		Sessions: store, Roots: harnessRoots(t)}, lines...)
}

// serveCaps runs the server with explicit host caps + degrade policy (for mode-gating tests),
// and no config policy ceiling.
func serveCaps(t *testing.T, mgr Reviewer, caps review.SurfaceCaps, degrade bool, lines ...string) []map[string]any {
	t.Helper()
	return servePolicy(t, mgr, caps, "", degrade, lines...)
}

// servePolicy is serveCaps with an explicit CONFIG policy ceiling (Surfaces.DefaultModeBySurface
// for the ACP surface) — the ceiling that is independent of what the host can do.
func servePolicy(t *testing.T, mgr Reviewer, caps review.SurfaceCaps, policy review.Mode, degrade bool, lines ...string) []map[string]any {
	t.Helper()
	return runServer(t, &Server{Manager: mgr, Caps: caps, DegradeWhenModeUnavailable: degrade,
		PolicyCeiling: policy, Roots: harnessRoots(t)}, lines...)
}

// recordReviewer captures the mode + workspace the Manager was actually called with — on BOTH
// halves of Reviewer.
//
// The from-run half is not padding: a write turn carrying `fromRun` reaches Remediate and never
// RunContext, so a fake that recorded only the review call would make every mode-gating assertion
// about `apply` fail for a reason that has nothing to do with mode gating.
type recordReviewer struct {
	mode review.Mode
	ws   string
	// source is the workspace the harness substituted for wsPlaceholder, claimed as the source
	// run's so a from-run turn naming it is not refused as a mismatch.
	source string
}

func (r *recordReviewer) setWorkspace(ws string) { r.source = ws }

func (r *recordReviewer) RunContext(ctx context.Context, req run.Request) (review.RunOutcome, error) {
	r.mode = req.Mode
	r.ws = req.Workspace
	return review.RunOutcome{Status: "stable", Mode: req.Mode, RunDir: "/tmp/run-x"}, nil
}

func (r *recordReviewer) ReadDecisionSet(handle string) (*run.StoredDecisionSet, string, error) {
	return stubDecisionSet(handle, r.source), "/artifacts/" + filepath.Base(handle), nil
}

func (r *recordReviewer) Remediate(_ context.Context, req run.RemediateRequest) (run.RemediateOutcome, error) {
	r.mode, r.ws = req.Mode, req.Workspace
	return run.RemediateOutcome{RunID: "run-remediate", RunDir: "/tmp/run-remediate", Mode: req.Mode}, nil
}

// fakeRemediator is the stand-in for the OTHER half: a from-run write turn whose outcome the test
// dictates. It answers any handle with a remediable set and hands back the outcome it was given.
type fakeRemediator struct {
	out    run.RemediateOutcome
	err    error
	source string
}

func (f *fakeRemediator) setWorkspace(ws string) { f.source = ws }

func (f *fakeRemediator) RunContext(_ context.Context, req run.Request) (review.RunOutcome, error) {
	return review.RunOutcome{Status: "stable", Mode: req.Mode, RunDir: "/tmp/run-x"}, nil
}

func (f *fakeRemediator) ReadDecisionSet(handle string) (*run.StoredDecisionSet, string, error) {
	return stubDecisionSet(handle, f.source), "/artifacts/" + filepath.Base(handle), nil
}

func (f *fakeRemediator) Remediate(_ context.Context, req run.RemediateRequest) (run.RemediateOutcome, error) {
	out := f.out
	out.Mode = req.Mode
	if out.RunDir == "" {
		out.RunDir = "/tmp/run-remediate"
	}
	return out, f.err
}

// A read-only host (no FileWrite) requesting apply degrades to patch (it has diff), with a
// reason — and the Manager is invoked with the capped mode (so no live write can occur).
func TestModeGating_ReadOnlyHost_ApplyDegradesToPatch(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true, DiffContext: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"apply"}}`)
	res := result(t, r[0])
	if res["mode"] != "patch" {
		t.Errorf("effective mode = %v, want patch", res["mode"])
	}
	if res["modeDegraded"] != true || res["requestedMode"] != "apply" {
		t.Errorf("expected modeDegraded=true requestedMode=apply, got %v", res)
	}
	if rec.mode != review.ModePatch {
		t.Errorf("Manager received mode %q, want patch", rec.mode)
	}
}

// With degradation disabled, a too-high request fails with a usage error (before any run).
func TestModeGating_ReadOnlyHost_ApplyFailsWhenDegradeDisabled(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true, DiffContext: true}, false,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"apply"}}`)
	e := rpcErr(t, r[0])
	if e["code"] == nil {
		t.Errorf("expected an error code, got %v", e)
	}
	if rec.mode != "" {
		t.Errorf("Manager must not run when the mode is rejected; got %q", rec.mode)
	}
}

// An unknown mode is rejected with a usage error before any run (the resolver does not
// validate modes, so the surface must).
func TestModeGating_UnknownModeRejected(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"bogus"}}`)
	e := rpcErr(t, r[0])
	if e["code"] == nil {
		t.Errorf("expected an error for an unknown mode, got %v", e)
	}
	if rec.mode != "" {
		t.Errorf("Manager must not run for an invalid mode; got %q", rec.mode)
	}
}

// A host with neither write nor diff capability caps patch down to report.
func TestModeGating_NoDiffHost_PatchDegradesToReport(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"patch"}}`)
	res := result(t, r[0])
	if res["mode"] != "report" {
		t.Errorf("effective mode = %v, want report", res["mode"])
	}
	if rec.mode != review.ModeReport {
		t.Errorf("Manager received mode %q, want report", rec.mode)
	}
}

// A write-capable host may request apply unchanged (no degrade).
func TestModeGating_WriteCapableHost_ApplyAllowed(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","fromRun":"/runs/prior-report-run","mode":"apply"}}`)
	res := result(t, r[0])
	if res["mode"] != "apply" {
		t.Errorf("effective mode = %v, want apply", res["mode"])
	}
	if res["modeDegraded"] != nil {
		t.Errorf("apply within capability must not degrade: %v", res)
	}
	if rec.mode != review.ModeApply {
		t.Errorf("Manager received mode %q, want apply", rec.mode)
	}
}

// initialize narrows caps from host-advertised clientCapabilities (writeTextFile=false),
// which both reports a lower modeCeiling and gates a later apply request.
func TestModeGating_InitializeNarrowsCapsFromHost(t *testing.T) {
	rec := &recordReviewer{}
	full := review.SurfaceCaps{WorkspaceRoot: true, FileRead: true, FileWrite: true, DiffContext: true, ArtifactDir: true}
	r := serveCaps(t, rec, full, true,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":false}}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"review","params":{"workspace":"/ws","mode":"apply"}}`)
	// initialize narrows fileWrite (not echoed in the ACP v1 result); the narrowing is proven
	// behaviorally: the later apply request degrades to patch.
	if result(t, r[0])["protocolVersion"] != float64(1) {
		t.Errorf("initialize should return protocolVersion 1, got %v", result(t, r[0])["protocolVersion"])
	}
	res := result(t, r[len(r)-1])
	if res["mode"] != "patch" || rec.mode != review.ModePatch {
		t.Errorf("apply should degrade to patch after host narrowed write cap; result=%v managerMode=%q", res, rec.mode)
	}
}

// session/load is NOT implemented (loadSession:false; its ACP contract mandates replaying a
// conversation a stateless reviewer has none of) → method-not-found. Reconnect uses
// session/resume instead.
func TestSessionLoad_MethodNotFound(t *testing.T) {
	r := serve(t, fakeReviewer{},
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"s-0001"}}`)
	if rpcErr(t, r[len(r)-1])["code"].(float64) != codeMethodNotFound {
		t.Errorf("session/load should be method-not-found (not implemented), got %v", r[len(r)-1])
	}
}

// materializeInline writes safe inline content to an isolated temp workspace and rejects
// unsafe/excluded/empty maps without leaving files outside the temp dir.
func TestMaterializeInline(t *testing.T) {
	dir, err := materializeInline(map[string]string{"main.go": "package main\n", "sub/x.go": "package sub\n"})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer os.RemoveAll(dir)
	if b, _ := os.ReadFile(filepath.Join(dir, "main.go")); string(b) != "package main\n" {
		t.Error("main.go not materialized")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sub", "x.go")); string(b) != "package sub\n" {
		t.Error("sub/x.go not materialized")
	}
	// Rejected on EVERY platform (shared cross-platform path-safety invariant): traversal,
	// POSIX-rooted, backslash-rooted, UNC, drive-letter, excluded, empty-key, empty-map.
	for name, files := range map[string]map[string]string{
		"empty-map":   {},
		"traversal":   {"../escape.go": "x"},
		"posix-root":  {"/etc/passwd": "x"},
		"posix-root2": {"/abs/x": "x"},
		"backslash":   {`\abs\x`: "x"},
		"unc":         {`\\server\share\x`: "x"},
		"drive":       {`C:\abs\x`: "x"},
		"excluded":    {".git/config": "x"},
		"empty-key":   {"": "x"},
	} {
		if _, err := materializeInline(files); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// session/prompt rejects an ambiguous (workspace + inlineWorkspace) request and an unsafe
// inline path, before the Manager runs.
func TestInlineWorkspace_RejectsAmbiguousAndUnsafe(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"report","inlineWorkspace":{"main.go":"package main\n"}}}`)
	if rpcErr(t, r[len(r)-1])["code"] == nil {
		t.Error("workspace + inlineWorkspace together should error")
	}
	rec2 := &recordReviewer{}
	r2 := serveCaps(t, rec2, review.SurfaceCaps{FileRead: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report","inlineWorkspace":{"../escape.go":"x"}}}`)
	if rpcErr(t, r2[1])["code"] == nil {
		t.Error("unsafe inline path should error")
	}
	if rec2.mode != "" {
		t.Error("Manager must not run on a rejected inline prompt")
	}
}

// A host that denies write permission for THIS prompt degrades apply→patch (below the
// connection's write capability), with a reason; the Manager receives the capped mode.
func TestPermissionGate_DenyWriteDegradesApply(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"apply","permissions":{"allowWrite":false}}}`)
	if sr := stopReasonOf(t, r[len(r)-1]); sr != "end_turn" {
		t.Errorf("session/prompt stopReason = %q, want end_turn", sr)
	}
	rm := promptMeta(t, r[len(r)-1])
	if rm["mode"] != "patch" {
		t.Errorf("effective mode = %v, want patch", rm["mode"])
	}
	if rm["modeDegraded"] != true {
		t.Errorf("expected modeDegraded=true, got %v", rm)
	}
	if rec.mode != review.ModePatch {
		t.Errorf("Manager received mode %q, want patch", rec.mode)
	}
}

// Denying patch permission caps to report.
func TestPermissionGate_DenyPatchDegradesToReport(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"patch","permissions":{"allowPatch":false}}}`)
	if rm := promptMeta(t, r[len(r)-1]); rm["mode"] != "report" {
		t.Errorf("effective mode = %v, want report", rm["mode"])
	}
	if rec.mode != review.ModeReport {
		t.Errorf("Manager received mode %q, want report", rec.mode)
	}
}

// Explicit write permission lets apply proceed (no degrade).
func TestPermissionGate_AllowWriteApplyProceeds(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","fromRun":"/runs/prior-report-run","mode":"apply","permissions":{"allowWrite":true}}}`)
	rm := promptMeta(t, r[len(r)-1])
	if rm["mode"] != "apply" || rm["modeDegraded"] != nil {
		t.Errorf("apply with write permission should proceed unchanged, got %v", rm)
	}
	if rec.mode != review.ModeApply {
		t.Errorf("Manager received mode %q, want apply", rec.mode)
	}
}

// With degradation disabled, a write-denied apply fails before the Manager runs.
func TestPermissionGate_DenyWriteFailsWhenDegradeDisabled(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCaps(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true}, false,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"apply","permissions":{"allowWrite":false}}}`)
	if rpcErr(t, r[len(r)-1])["code"] == nil {
		t.Error("write-denied apply with degradation disabled should error")
	}
	if rec.mode != "" {
		t.Error("Manager must not run when a permission-denied mode is rejected")
	}
}

// The CONFIG policy ceiling for the ACP surface caps a fully write-capable connection: the
// shipped seed is `acp: report`, so a host that advertises write capability (or advertises no
// `fs` capabilities at all) and asks for `apply` runs in REPORT mode. The degradation is
// explicit — a `mode_degraded` warn plus modeDegraded/requestedMode in the result — never a
// silent downgrade, and the Manager is invoked with the capped mode so no live write can occur.
func TestPolicyCeiling_ReportCapsApplyOnWriteCapableHost(t *testing.T) {
	rec := &recordReviewer{}
	r := servePolicy(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true},
		review.ModeReport, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"apply"}}`)
	rm := promptMeta(t, r[len(r)-1])
	if rm["mode"] != "report" {
		t.Errorf("effective mode = %v, want report (config policy ceiling)", rm["mode"])
	}
	if rm["modeDegraded"] != true || rm["requestedMode"] != "apply" {
		t.Errorf("expected modeDegraded=true requestedMode=apply, got %v", rm)
	}
	if rec.mode != review.ModeReport {
		t.Errorf("Manager received mode %q, want report", rec.mode)
	}
	var warned bool
	for _, resp := range r {
		if resp["method"] != "session/update" {
			continue
		}
		upd, _ := resp["params"].(map[string]any)["update"].(map[string]any)
		meta, _ := upd["_meta"].(map[string]any)
		if em, _ := meta["reviewmesh"].(map[string]any); em["eventType"] == "mode_degraded" && em["level"] == "warn" {
			warned = true
		}
	}
	if !warned {
		t.Error("a policy-capped apply must emit a mode_degraded warn session/update")
	}
}

// With degradation disabled, the same policy-capped apply is refused with a usage error
// BEFORE any run (no spend), not silently downgraded.
func TestPolicyCeiling_ReportFailsApplyWhenDegradeDisabled(t *testing.T) {
	rec := &recordReviewer{}
	r := servePolicy(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true},
		review.ModeReport, false,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"apply"}}`)
	e := rpcErr(t, r[len(r)-1])
	if e["code"].(float64) != codeInvalidParams {
		t.Errorf("error code = %v, want %d (invalid params)", e["code"], codeInvalidParams)
	}
	if rec.mode != "" {
		t.Errorf("Manager must not run when the policy-capped mode is rejected; got %q", rec.mode)
	}
}

// A user who widens the policy (`surfaces.defaultModeBySurface.acp: apply`) gets apply — the
// ceiling is config-visible policy, not a hard-coded refusal.
func TestPolicyCeiling_WidenedToApplyIsHonored(t *testing.T) {
	rec := &recordReviewer{}
	r := servePolicy(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true},
		review.ModeApply, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","fromRun":"/runs/prior-report-run","mode":"apply"}}`)
	rm := promptMeta(t, r[len(r)-1])
	if rm["mode"] != "apply" || rm["modeDegraded"] != nil {
		t.Errorf("a widened policy must honor apply unchanged, got %v", rm)
	}
	if rec.mode != review.ModeApply {
		t.Errorf("Manager received mode %q, want apply", rec.mode)
	}
}

// A widened policy never WIDENS past what the connection can do: the narrowest of
// capability / permission / policy always wins.
func TestPolicyCeiling_NeverWidensBeyondHostCapability(t *testing.T) {
	rec := &recordReviewer{}
	r := servePolicy(t, rec, review.SurfaceCaps{FileRead: true, DiffContext: true},
		review.ModeApply, true,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"apply"}}`)
	if res := result(t, r[0]); res["mode"] != "patch" {
		t.Errorf("effective mode = %v, want patch (read-only host beats a widened policy)", res["mode"])
	}
	if rec.mode != review.ModePatch {
		t.Errorf("Manager received mode %q, want patch", rec.mode)
	}
}

func result(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got %v", resp)
	}
	return r
}

func rpcErr(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error, got %v", resp)
	}
	return e
}

// promptMeta extracts an ACP v1 PromptResponse's `_meta.reviewmesh` block (where reviewmesh
// result details — status/mode/findings/runDir — live, not at the ACP top level).
func promptMeta(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	meta, ok := result(t, resp)["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("PromptResponse missing _meta, got %v", resp)
	}
	rm, ok := meta["reviewmesh"].(map[string]any)
	if !ok {
		t.Fatalf("PromptResponse missing _meta.reviewmesh, got %v", meta)
	}
	return rm
}

// stopReasonOf returns the ACP v1 stopReason of a PromptResponse result.
func stopReasonOf(t *testing.T, resp map[string]any) string {
	t.Helper()
	sr, _ := result(t, resp)["stopReason"].(string)
	return sr
}

// initialize returns an ACP v1-shaped result: integer protocolVersion 1, agentCapabilities,
// authMethods, optional agentInfo — and NO reviewmesh-internal capabilities/serverInfo.
func TestInitialize(t *testing.T) {
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	res := result(t, r[0])
	if res["protocolVersion"] != float64(1) {
		t.Errorf("protocolVersion must be numeric 1, got %#v", res["protocolVersion"])
	}
	if _, ok := res["agentCapabilities"].(map[string]any); !ok {
		t.Errorf("initialize must include agentCapabilities, got %v", res)
	}
	if _, ok := res["authMethods"]; !ok {
		t.Errorf("initialize must include authMethods, got %v", res)
	}
	if ai, ok := res["agentInfo"].(map[string]any); !ok || ai["name"] != "reviewmesh" {
		t.Errorf("initialize agentInfo.name should be reviewmesh, got %v", res["agentInfo"])
	}
	if res["capabilities"] != nil || res["serverInfo"] != nil {
		t.Errorf("ACP v1 initialize must not carry top-level capabilities/serverInfo, got %v", res)
	}
}

// agentCapabilities is honest + minimal: durable session load unsupported; text-only
// prompts; no MCP (matching what is implemented, per the no-over-advertise rule).
func TestInitialize_AgentCapabilitiesHonest(t *testing.T) {
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	ac := result(t, r[0])["agentCapabilities"].(map[string]any)
	if ac["loadSession"] != false {
		t.Errorf("loadSession should be false (durable cross-process load unsupported), got %v", ac["loadSession"])
	}
	pc, ok := ac["promptCapabilities"].(map[string]any)
	if !ok || pc["image"] != false || pc["audio"] != false || pc["embeddedContext"] != false {
		t.Errorf("promptCapabilities should be all-false (text-only), got %v", ac["promptCapabilities"])
	}
}

// A missing, non-numeric, non-integer, or too-low protocolVersion is a deterministic error
// — never silently answered with a different version (e.g. the old "0.1").
func TestInitialize_RejectsBadProtocolVersion(t *testing.T) {
	for _, params := range []string{`{}`, `{"protocolVersion":"0.1"}`, `{"protocolVersion":0.1}`, `{"protocolVersion":0}`} {
		r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+params+`}`)
		if e := rpcErr(t, r[0]); e["code"] == nil {
			t.Errorf("initialize params %s should error on bad protocolVersion, got %v", params, r[0])
		}
	}
}

// A higher client protocol version negotiates DOWN to the agent's supported version (1).
func TestInitialize_HigherVersionNegotiatesDown(t *testing.T) {
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2}}`)
	if result(t, r[0])["protocolVersion"] != float64(1) {
		t.Errorf("a higher client version should negotiate down to 1, got %v", result(t, r[0])["protocolVersion"])
	}
}

// A request shaped like the captured Zed initialize (sanitized) is accepted and yields a
// valid ACP v1 result.
func TestInitialize_ZedSampleRequest(t *testing.T) {
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":"zed-1","method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true},"clientInfo":{"name":"zed","title":"Zed","version":"x.y.z"}}}`)
	res := result(t, r[0])
	if res["protocolVersion"] != float64(1) || res["agentCapabilities"] == nil {
		t.Errorf("Zed-shaped initialize should yield an ACP v1 result, got %v", res)
	}
}

func TestACP_SessionNewAndPrompt(t *testing.T) {
	mgr := fakeReviewer{out: review.RunOutcome{Status: "stable", Findings: []review.Finding{{ID: "F1"}}}}
	r := serve(t, mgr,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"report"}}`)
	if sid, _ := result(t, r[0])["sessionId"].(string); sid == "" {
		t.Errorf("session/new should return a sessionId, got %v", r[0])
	}
	// ACP v1 PromptResponse: top-level stopReason; reviewmesh details under _meta.reviewmesh.
	if sr := stopReasonOf(t, r[len(r)-1]); sr != "end_turn" {
		t.Errorf("session/prompt stopReason = %q, want end_turn", sr)
	}
	rm := promptMeta(t, r[len(r)-1])
	if rm["status"] != "stable" || rm["findings"].(float64) != 1 {
		t.Errorf("session/prompt _meta.reviewmesh = %v", rm)
	}
}

// session/update notifications are ACP v1 SessionNotification-shaped: params {sessionId,
// update} with the official `sessionUpdate` discriminator + a text content block; reviewmesh
// audit details live ONLY under update._meta.reviewmesh — never as top-level params siblings.
func TestSessionUpdate_ACPv1Shape(t *testing.T) {
	// A write-denied apply degrades → a `mode_degraded` session/update is emitted SYNCHRONOUSLY
	// (before the run, no ctx gate), so the notification shape is deterministically testable.
	r := serveCaps(t, &recordReviewer{}, review.SurfaceCaps{FileRead: true, FileWrite: true, DiffContext: true}, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"apply","permissions":{"allowWrite":false}}}`)
	var found bool
	for _, resp := range r {
		if resp["method"] != "session/update" {
			continue
		}
		found = true
		p, _ := resp["params"].(map[string]any)
		if p["sessionId"] != "s-0001" {
			t.Errorf("session/update missing top-level params.sessionId, got %v", p)
		}
		for _, leaked := range []string{"eventType", "level", "message", "timestamp"} {
			if _, ok := p[leaked]; ok {
				t.Errorf("session/update must not carry top-level params.%s (ACP v1), got %v", leaked, p)
			}
		}
		upd, ok := p["update"].(map[string]any)
		if !ok {
			t.Fatalf("session/update missing params.update, got %v", p)
		}
		if upd["sessionUpdate"] != "agent_message_chunk" {
			t.Errorf("update.sessionUpdate = %v, want agent_message_chunk", upd["sessionUpdate"])
		}
		if c, ok := upd["content"].(map[string]any); !ok || c["type"] != "text" || c["text"] == nil {
			t.Errorf("update.content should be a text ContentBlock, got %v", upd["content"])
		}
		meta, _ := upd["_meta"].(map[string]any)
		rm, _ := meta["reviewmesh"].(map[string]any)
		if rm == nil || rm["eventType"] == nil {
			t.Errorf("reviewmesh event details should be under update._meta.reviewmesh, got %v", upd["_meta"])
		}
	}
	if !found {
		t.Error("expected at least one session/update notification")
	}
}

// Full Zed-shaped flow regression: initialize → session/new with cwd → session/prompt WITHOUT
// workspace (+ prompt array) yields ACP v1-shaped session/update notifications and a
// PromptResponse final, run against the session cwd.
func TestACP_ZedFlow_SchemaShaped(t *testing.T) {
	rec := &recordReviewer{}
	cwd := t.TempDir()
	r := serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":`+strconv.Quote(cwd)+`,"mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s-0001","prompt":[{"type":"text","text":"Use ReviewMesh to run a report-only review of README.md."}]}}`)
	last := r[len(r)-1]
	if sr := stopReasonOf(t, last); sr != "end_turn" {
		t.Errorf("Zed flow prompt stopReason = %q, want end_turn", sr)
	}
	if rec.ws != cwd {
		t.Errorf("Zed flow should run against the session cwd %q, got %q (prompt text must NOT be parsed for scope)", cwd, rec.ws)
	}
	// Any session/update emitted must be ACP v1-shaped (carry params.update). Streamed-update
	// presence + ordering is asserted reliably by the real-manager subprocess test
	// (TestACP_SessionPromptProgress); the in-process serve() harness races EOF cancellation.
	for _, resp := range r {
		if resp["method"] == "session/update" {
			if _, ok := resp["params"].(map[string]any)["update"].(map[string]any); !ok {
				t.Errorf("Zed flow session/update must carry params.update, got %v", resp["params"])
			}
		}
	}
}

// WITHOUT a durable store, resume is unsupported: `sessionCapabilities.resume` is not
// advertised and `session/resume` returns method-not-found (capability advertised only when
// implemented). loadSession stays false on both paths (session/load mandates history replay).
func TestSessionResume_NotAdvertisedWithoutStore(t *testing.T) {
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"s-0001","cwd":"/ws"}}`)
	if rpcErr(t, r[0])["code"].(float64) != codeMethodNotFound {
		t.Errorf("session/resume without a store should be method-not-found, got %v", r[0])
	}
	ir := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	ac := result(t, ir[0])["agentCapabilities"].(map[string]any)
	if _, ok := ac["sessionCapabilities"]; ok {
		t.Errorf("must not advertise sessionCapabilities without a store, got %v", ac)
	}
	if ac["loadSession"] != false {
		t.Errorf("loadSession must stay false (session/load mandates history replay), got %v", ac["loadSession"])
	}
}

// With a durable store, initialize advertises sessionCapabilities.resume (empty object); a
// session/new-created session is persisted and a NEW Server instance sharing the same store
// resumes it and runs a subsequent session/prompt via the restored cwd — CROSS-INSTANCE
// (restart) durability, not just in-process. resume returns an empty ResumeSessionResponse
// and replays NO history.
func TestSessionResume_CrossInstanceDurable(t *testing.T) {
	store := NewFileSessionStore(t.TempDir())
	cwd := t.TempDir()

	rA := serveStore(t, &recordReviewer{}, store,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":`+strconv.Quote(cwd)+`,"mcpServers":[]}}`)
	ac := result(t, rA[0])["agentCapabilities"].(map[string]any)
	sc, ok := ac["sessionCapabilities"].(map[string]any)
	if !ok {
		t.Fatalf("initialize with a store must advertise sessionCapabilities, got %v", ac)
	}
	if _, ok := sc["resume"].(map[string]any); !ok {
		t.Errorf("sessionCapabilities.resume should be an (empty) object, got %v", sc["resume"])
	}
	sid, _ := result(t, rA[1])["sessionId"].(string)
	if sid == "" {
		t.Fatal("session/new returned no sessionId")
	}

	recB := &recordReviewer{}
	rB := serveStore(t, recB, store,
		`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"`+sid+`"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"`+sid+`","prompt":[{"type":"text","text":"review"}]}}`)
	if _, isErr := rB[0]["error"]; isErr {
		t.Fatalf("session/resume should succeed, got %v", rB[0])
	}
	if res := result(t, rB[0]); len(res) != 0 { // ResumeSessionResponse: empty object, no history
		t.Errorf("ResumeSessionResponse should be an empty object, got %v", res)
	}
	if recB.ws != cwd {
		t.Errorf("resumed prompt should run against the restored cwd %q, got %q", cwd, recB.ws)
	}
}

// Resuming an unknown / malformed persisted record is a structured error (never silently OK).
func TestSessionResume_UnknownAndMalformedError(t *testing.T) {
	r := serveStore(t, &recordReviewer{}, NewFileSessionStore(t.TempDir()),
		`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"s-9999"}}`)
	if rpcErr(t, r[0])["code"].(float64) != codeInvalidParams {
		t.Errorf("resuming an unknown session should be invalid params, got %v", r[0])
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s-0001.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := serveStore(t, &recordReviewer{}, NewFileSessionStore(dir),
		`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"s-0001"}}`)
	if rpcErr(t, r2[0])["code"] == nil {
		t.Errorf("a malformed session record should error, got %v", r2[0])
	}
}

// session/new with a store persists ONLY safe metadata (no prompt/model/file content/secrets).
func TestSessionNew_PersistsOnlySafeMetadata(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()
	r := serveStore(t, &recordReviewer{}, NewFileSessionStore(dir),
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+strconv.Quote(cwd)+`}}`)
	sid, _ := result(t, r[0])["sessionId"].(string)
	if sid == "" {
		t.Fatal("session/new returned no sessionId")
	}
	b, err := os.ReadFile(filepath.Join(dir, sid+".json"))
	if err != nil {
		t.Fatalf("session record not persisted: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("persisted record not valid JSON: %v", err)
	}
	allowed := map[string]bool{"schemaVersion": true, "sessionId": true, "cwd": true, "createdAt": true, "updatedAt": true}
	for k := range rec {
		if !allowed[k] {
			t.Errorf("persisted record leaks non-metadata field %q", k)
		}
	}
	if rec["cwd"] != cwd {
		t.Errorf("persisted cwd = %v, want %q", rec["cwd"], cwd)
	}
}

// Progress display text carries a trailing newline (so a host concatenating chunks renders
// each line separately) while _meta.reviewmesh.message stays clean for programmatic use.
func TestProgressUpdate_DisplayNewlineCleanMeta(t *testing.T) {
	upd := acpProgressUpdate("run_started", "info", "review run started", "<TS>")
	if got := upd["content"].(map[string]any)["text"]; got != "review run started\n" {
		t.Errorf("display text should end with a newline, got %q", got)
	}
	rm := upd["_meta"].(map[string]any)["reviewmesh"].(map[string]any)
	if rm["message"] != "review run started" {
		t.Errorf("_meta.reviewmesh.message should stay clean (no newline), got %q", rm["message"])
	}
}

func TestACP_SessionPromptUnknownSession(t *testing.T) {
	// session/prompt without a session/new-created id is rejected (authoritative lifecycle)
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"never-created","workspace":"/ws","mode":"report"}}`)
	if rpcErr(t, r[0])["code"].(float64) != codeInvalidParams {
		t.Errorf("session/prompt with an unknown sessionId should be invalid params, got %v", r[0])
	}
}

// Zed flow: session/new supplies the workspace in params.cwd; a later session/prompt that
// omits `workspace` (and `mode`) runs the Manager with Workspace == that cwd, mode report.
func TestACP_SessionPrompt_UsesCwdFallback(t *testing.T) {
	rec := &recordReviewer{}
	cwd := t.TempDir()
	serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+strconv.Quote(cwd)+`,"mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","prompt":[{"type":"text","text":"run a report-only review"}]}}`)
	if rec.ws != cwd {
		t.Errorf("session/prompt without workspace should use session cwd %q, got %q", cwd, rec.ws)
	}
	if rec.mode != review.ModeReport {
		t.Errorf("omitted session/prompt mode should default to report, got %q", rec.mode)
	}
}

// An explicit `workspace` on session/prompt overrides the stored session cwd.
func TestACP_SessionPrompt_ExplicitWorkspaceOverridesCwd(t *testing.T) {
	rec := &recordReviewer{}
	cwd, explicit := t.TempDir(), t.TempDir()
	serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+strconv.Quote(cwd)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":`+strconv.Quote(explicit)+`,"mode":"report"}}`)
	if rec.ws != explicit {
		t.Errorf("explicit workspace should override session cwd; got %q want %q", rec.ws, explicit)
	}
}

// With no cwd stored, a prompt that omits `workspace` falls back to the process working
// directory (not an error).
func TestACP_SessionPrompt_GetwdFallbackWhenNoCwd(t *testing.T) {
	rec := &recordReviewer{}
	serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report"}}`)
	wd, _ := os.Getwd()
	if rec.ws != wd {
		t.Errorf("a no-cwd prompt should fall back to os.Getwd() %q, got %q", wd, rec.ws)
	}
}

// The Zed prompt-array shape is accepted (not rejected); its content is not parsed for scope.
func TestACP_SessionPrompt_AcceptsPromptArray(t *testing.T) {
	rec := &recordReviewer{}
	cwd := t.TempDir()
	r := serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+strconv.Quote(cwd)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","prompt":[{"type":"text","text":"Use ReviewMesh to run a report-only review of README.md. Do not modify files."}]}}`)
	if r[1]["error"] != nil {
		t.Errorf("a Zed prompt array must be accepted, got error %v", r[1]["error"])
	}
}

func TestACP_SessionCancelInFlight(t *testing.T) {
	mgr := fakeReviewer{block: true, out: review.RunOutcome{Status: "stable"}}
	r := serve(t, mgr,
		`{"jsonrpc":"2.0","id":0,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"report"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/cancel","params":{"sessionId":"s-0001"}}`)
	var sawCancelled, sawAck bool
	for _, resp := range r {
		if res, ok := resp["result"].(map[string]any); ok {
			// The in-flight session/prompt response is an ACP v1 PromptResponse with
			// stopReason "cancelled".
			if res["stopReason"] == "cancelled" {
				sawCancelled = true
			}
			if res["cancelled"] == true { // session/cancel ack
				sawAck = true
			}
		}
	}
	if !sawCancelled || !sawAck {
		t.Errorf("session/cancel should make the in-flight prompt respond stopReason=cancelled + ack; cancelled=%v ack=%v", sawCancelled, sawAck)
	}
}

func TestACP_SessionBusyRejectsConcurrentPrompt(t *testing.T) {
	// a second prompt for a session with an in-flight run is rejected, not silently
	// overwritten (which would orphan the first run from session/cancel)
	mgr := fakeReviewer{block: true, out: review.RunOutcome{Status: "stable"}}
	r := serve(t, mgr,
		`{"jsonrpc":"2.0","id":0,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"report"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","workspace":"/ws","mode":"report"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/cancel","params":{"sessionId":"s-0001"}}`)
	var sawBusy bool
	for _, resp := range r {
		if e, ok := resp["error"].(map[string]any); ok && e["code"].(float64) == codeInvalidRequest {
			sawBusy = true
		}
	}
	if !sawBusy {
		t.Errorf("a concurrent prompt for a busy session should be rejected with invalid-request; got %v", r)
	}
}

func TestReviewReport(t *testing.T) {
	mgr := fakeReviewer{out: review.RunOutcome{Status: "stable", Findings: []review.Finding{{ID: "F1"}}}}
	r := serve(t, mgr, `{"jsonrpc":"2.0","id":2,"method":"review","params":{"workspace":"/ws","mode":"report"}}`)
	res := result(t, r[0])
	if res["status"] != "stable" || res["findings"].(float64) != 1 {
		t.Errorf("review report result = %v", res)
	}
}

func TestReviewPatchHasArtifact(t *testing.T) {
	mgr := fakeReviewer{out: review.RunOutcome{Status: "stable"}}
	r := serve(t, mgr, `{"jsonrpc":"2.0","id":3,"method":"review","params":{"workspace":"/ws","mode":"patch"}}`)
	res := result(t, r[0])
	if _, ok := res["patchArtifact"]; !ok {
		t.Errorf("patch result should reference a patch artifact: %v", res)
	}
}

func TestReviewHaltMapsToError(t *testing.T) {
	mgr := fakeReviewer{err: fault.New(fault.Model, "identity unverified").WithHalt("E")}
	r := serve(t, mgr, `{"jsonrpc":"2.0","id":4,"method":"review","params":{"workspace":"/ws","mode":"report"}}`)
	e := rpcErr(t, r[0])
	if e["code"].(float64) != codeReviewHalt {
		t.Errorf("error code = %v, want %d", e["code"], codeReviewHalt)
	}
	data := e["data"].(map[string]any)
	if data["exitCode"].(float64) != float64(fault.Model) {
		t.Errorf("error data exitCode = %v, want %d", data["exitCode"], fault.Model)
	}
}

func TestParseError(t *testing.T) {
	r := serve(t, fakeReviewer{}, `this is not json`)
	if rpcErr(t, r[0])["code"].(float64) != codeParse {
		t.Errorf("expected parse error %d", codeParse)
	}
}

func TestMethodNotFound(t *testing.T) {
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":5,"method":"frobnicate"}`)
	if rpcErr(t, r[0])["code"].(float64) != codeMethodNotFound {
		t.Errorf("expected method-not-found %d", codeMethodNotFound)
	}
}

func TestReviewMissingWorkspace(t *testing.T) {
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":6,"method":"review","params":{"mode":"report"}}`)
	if rpcErr(t, r[0])["code"].(float64) != codeInvalidParams {
		t.Errorf("expected invalid-params %d", codeInvalidParams)
	}
}

func TestCancelNoActiveReturnsAck(t *testing.T) {
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":7,"method":"cancel","params":{"id":1}}`)
	res := result(t, r[0])
	if res["cancelled"] != false {
		t.Errorf("cancel with no matching in-flight id should ack cancelled:false, got %v", res)
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	// requests without an `id` member are notifications → no response
	r := serve(t, fakeReviewer{out: review.RunOutcome{Status: "stable"}},
		`{"jsonrpc":"2.0","method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"review","params":{"workspace":"/ws","mode":"report"}}`,
		`{"jsonrpc":"2.0","method":"frobnicate"}`)
	if len(r) != 0 {
		t.Errorf("notifications must not produce responses, got %v", r)
	}
}

func TestIDNullGetsResponse(t *testing.T) {
	// `id: null` is a valid identifier (not a notification) → must get a response
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","id":null,"method":"initialize","params":{"protocolVersion":1}}`)
	if len(r) != 1 {
		t.Fatalf("id:null should get exactly one response, got %v", r)
	}
	if _, ok := r[0]["result"]; !ok {
		t.Errorf("id:null initialize should return a result: %v", r[0])
	}
}

func TestBatchRequestRejected(t *testing.T) {
	r := serve(t, fakeReviewer{}, `[{"jsonrpc":"2.0","id":1,"method":"initialize"}]`)
	if rpcErr(t, r[0])["code"].(float64) != codeInvalidRequest {
		t.Errorf("batch array should be invalid-request %d", codeInvalidRequest)
	}
}

func TestContentLengthFramingRoundTrip(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`
	in := "Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body
	srv := &Server{Manager: fakeReviewer{}, Framing: FramingContentLength}
	var out bytes.Buffer
	if err := srv.Serve(strings.NewReader(in), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	// response must be Content-Length framed and parse back to a result
	got := out.String()
	if !strings.HasPrefix(got, "Content-Length: ") {
		t.Fatalf("response not Content-Length framed: %q", got)
	}
	i := strings.Index(got, "\r\n\r\n")
	if i < 0 {
		t.Fatal("no header terminator in response")
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(got[i+4:]), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := resp["result"]; !ok {
		t.Errorf("expected a result, got %v", resp)
	}
}

func TestContentLengthFramer_MalformedHeader(t *testing.T) {
	f := NewFramer(FramingContentLength, strings.NewReader("NotAHeader\r\n\r\n"), &bytes.Buffer{})
	if _, err := f.ReadMessage(); err == nil {
		t.Error("a malformed header must return a transport error")
	}
}

func TestNewlineFramer_PartialThenComplete(t *testing.T) {
	// two messages split oddly across the stream still read as two messages
	in := "{\"a\":1}\n{\"b\":2}\n"
	f := NewFramer(FramingNewline, strings.NewReader(in), &bytes.Buffer{})
	m1, err := f.ReadMessage()
	if err != nil || string(m1) != `{"a":1}` {
		t.Fatalf("msg1 = %q err=%v", m1, err)
	}
	m2, _ := f.ReadMessage()
	if string(m2) != `{"b":2}` {
		t.Fatalf("msg2 = %q", m2)
	}
}

func TestCancelInFlightReview(t *testing.T) {
	mgr := fakeReviewer{block: true, out: review.RunOutcome{Status: "stable"}}
	r := serve(t, mgr,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"report"}}`,
		`{"jsonrpc":"2.0","id":99,"method":"cancel","params":{"id":1}}`)
	var sawCancelled, sawAck bool
	for _, resp := range r {
		if res, ok := resp["result"].(map[string]any); ok {
			if res["status"] == "cancelled" {
				sawCancelled = true
			}
			if res["cancelled"] == true {
				sawAck = true
			}
		}
	}
	if !sawCancelled {
		t.Error("the in-flight review should return a cancelled result")
	}
	if !sawAck {
		t.Error("cancel should acknowledge cancelled:true")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestServe_EOFWithBlockedReviewDoesNotHang(t *testing.T) {
	// A real, trusted workspace: the review must actually START (and block) for this test to
	// mean anything — a request refused pre-spend would never reach the in-flight state.
	ws := t.TempDir()
	srv := &Server{Manager: fakeReviewer{block: true}, Roots: []string{ws}}
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+strconv.Quote(ws)+`}}`+"\n"),
			&bytes.Buffer{})
	}()
	select {
	case <-done: // cancelAll on return unblocked the in-flight review
	case <-time.After(5 * time.Second):
		t.Fatal("Serve hung on EOF with a blocked in-flight review")
	}
}

func TestContentLengthFramer_RejectsHugeLength(t *testing.T) {
	f := NewFramer(FramingContentLength, strings.NewReader("Content-Length: 999999999\r\n\r\n"), &bytes.Buffer{})
	if _, err := f.ReadMessage(); err == nil {
		t.Error("an oversized Content-Length must be rejected before allocation")
	}
}

func TestContentLengthFramer_RejectsHugeHeader(t *testing.T) {
	huge := strings.Repeat("A", 100000) // > maxHeaderBytes, no newline
	f := NewFramer(FramingContentLength, strings.NewReader(huge), &bytes.Buffer{})
	if _, err := f.ReadMessage(); err == nil {
		t.Error("an oversized header line must be rejected (no headers-phase OOM)")
	}
}

func TestExitStopsCleanly(t *testing.T) {
	r := serve(t, fakeReviewer{}, `{"jsonrpc":"2.0","method":"exit"}`)
	if len(r) != 0 {
		t.Errorf("exit should produce no response, got %v", r)
	}
}
