package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// Each turn is judged against the paths THAT turn declares — its workspace (explicitly or as the
// session cwd) and its `_meta.reviewmesh.roots` — inside the operator's optional --root ceiling.

// serveCeiling runs a server launched with `ceiling` as its --root set (nil: no ceiling).
func serveCeiling(t *testing.T, mgr Reviewer, ceiling []string, lines ...string) []map[string]any {
	t.Helper()
	return runServer(t, &Server{Manager: mgr, Ceiling: ceiling, Adapters: harnessAdapters(t),
		Caps: review.SurfaceCaps{FileRead: true, FileWrite: true}}, lines...)
}

// projectDir builds an ordinary, readable project directory.
func projectDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func reasonOf(t *testing.T, resp map[string]any) any {
	t.Helper()
	data, _ := rpcErr(t, resp)["data"].(map[string]any)
	return data["reasonCode"]
}

// With no ceiling, a turn reviews the absolute workspace it declares — no setup is needed.
func TestScope_NoCeilingReviewsTheDeclaredWorkspace(t *testing.T) {
	dir := projectDir(t, "any-project")
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, nil,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+jsonQuote(dir)+`,"mode":"report"}}`)
	if res := result(t, r[0]); res["status"] != "stable" {
		t.Errorf("a declared absolute workspace must run with no ceiling, got %v", res)
	}
	if rec.ws != dir {
		t.Errorf("Manager workspace = %q, want %q", rec.ws, dir)
	}
}

// A broad workspace (the filesystem root) is refused even with no ceiling.
func TestScope_BroadWorkspaceIsRefused(t *testing.T) {
	dir := t.TempDir()
	fsRoot := filepath.VolumeName(dir) + string(filepath.Separator)
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, nil,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+jsonQuote(fsRoot)+`,"mode":"report"}}`)
	if got := reasonOf(t, r[0]); got != ReasonDegenerateRoot {
		t.Errorf("reasonCode = %v, want %q", got, ReasonDegenerateRoot)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q; the filesystem root is never a workspace", rec.ws)
	}
}

// An ordinary, readable directory outside the operator's ceiling is refused before any spend.
func TestScope_RefusesReadableDirOutsideCeiling(t *testing.T) {
	ceiling := projectDir(t, "allowed")
	other := projectDir(t, "someone-elses-project")
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, []string{ceiling},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+jsonQuote(other)+`,"mode":"report"}}`)
	if got := reasonOf(t, r[0]); got != ReasonOutsideCeiling {
		t.Errorf("reasonCode = %v, want %q", got, ReasonOutsideCeiling)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q — a path outside the ceiling must be refused BEFORE any spend", rec.ws)
	}
}

// A path INSIDE the ceiling runs normally, unrewritten.
func TestScope_AllowsPathInsideCeiling(t *testing.T) {
	ceiling := projectDir(t, "allowed")
	sub := filepath.Join(ceiling, "service")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, []string{ceiling},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+jsonQuote(sub)+`,"mode":"report"}}`)
	if res := result(t, r[0]); res["status"] != "stable" {
		t.Errorf("a subdirectory of the ceiling must run normally, got %v", res)
	}
	if rec.ws != sub {
		t.Errorf("Manager workspace = %q, want the path the host supplied (unrewritten) %q", rec.ws, sub)
	}
}

// The protected-path denylist applies inside the ceiling: a protected directory is never a workspace.
func TestScope_RefusesProtectedDirInsideCeiling(t *testing.T) {
	ceiling := projectDir(t, "allowed")
	secret := filepath.Join(ceiling, ".ssh")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, []string{ceiling},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+jsonQuote(secret)+`,"mode":"report"}}`)
	if got := reasonOf(t, r[0]); got != ReasonRootDenied {
		t.Errorf("reasonCode = %v, want %q", got, ReasonRootDenied)
	}
	if rec.ws != "" {
		t.Errorf("the Manager was invoked with %q; the refusal must happen BEFORE any run", rec.ws)
	}
}

// A session cwd the host supplies is a declared path like any other: outside the ceiling, the prompt
// that falls back to it is refused.
func TestScope_SessionCwdOutsideCeilingIsRefused(t *testing.T) {
	ceiling := projectDir(t, "allowed")
	other := projectDir(t, "elsewhere")
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, []string{ceiling},
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonQuote(other)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report"}}`)
	if got := reasonOf(t, r[len(r)-1]); got != ReasonOutsideCeiling {
		t.Errorf("reasonCode = %v, want %q", got, ReasonOutsideCeiling)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q; a session cwd outside the ceiling must be refused", rec.ws)
	}
}

// An explicit `session/prompt.workspace` overrides the session cwd, and is judged the same way.
func TestScope_ExplicitWorkspaceCannotEscapeCeiling(t *testing.T) {
	ceiling := projectDir(t, "allowed")
	other := projectDir(t, "elsewhere")
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, []string{ceiling},
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonQuote(ceiling)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report","workspace":`+jsonQuote(other)+`}}`)
	if got := reasonOf(t, r[len(r)-1]); got != ReasonOutsideCeiling {
		t.Errorf("reasonCode = %v, want %q", got, ReasonOutsideCeiling)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q; an explicit workspace outside the ceiling must be refused", rec.ws)
	}
}

// A `..` traversal out of the ceiling is refused: the declared path is resolved before it is judged.
func TestScope_TraversalOutOfCeilingRefused(t *testing.T) {
	ceiling := projectDir(t, "allowed")
	sibling := filepath.Join(filepath.Dir(ceiling), "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	// Built by concatenation, not filepath.Join: Join would clean the `..` away textually, and the
	// point is that the scope check — not the caller — is what defeats it.
	escape := ceiling + string(filepath.Separator) + ".." + string(filepath.Separator) + "sibling"
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, []string{ceiling},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+jsonQuote(escape)+`,"mode":"report"}}`)
	if got := reasonOf(t, r[0]); got != ReasonOutsideCeiling {
		t.Errorf("reasonCode = %v, want %q", got, ReasonOutsideCeiling)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q; a `..` escape must be refused", rec.ws)
	}
}

// `inlineWorkspace` content is materialized into a directory THIS process owns, so it consumes no
// scope and works under any ceiling.
func TestScope_InlineWorkspaceNeedsNoDeclaredPath(t *testing.T) {
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, []string{projectDir(t, "allowed")},
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report","inlineWorkspace":{"main.go":"package main\n"}}}`)
	last := r[len(r)-1]
	if e, ok := last["error"]; ok {
		t.Fatalf("inlineWorkspace must work with no declared path, got %v", e)
	}
	if sr := stopReasonOf(t, last); sr != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", sr)
	}
	if rec.ws == "" {
		t.Error("the Manager should have run against the materialized inline workspace")
	}
}

// An authority document path is read through the turn's scope: a readable document outside every
// path the turn declared is refused pre-spend, so a peer cannot exfiltrate a file by calling it
// "the spec".
func TestScope_AuthorityPathOutsideTheTurnScopeRefused(t *testing.T) {
	ws := projectDir(t, "project")
	other := projectDir(t, "elsewhere")
	spec := filepath.Join(other, "spec.md")
	if err := os.WriteFile(spec, []byte("someone else's document\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &authorityRecorder{}
	r := serveCeiling(t, rec, nil,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonQuote(ws)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report",`+
			`"_meta":{"reviewmesh":{"authority":[{"name":"spec","path":`+jsonQuote(spec)+`}]}}}}`)
	e := rpcErr(t, r[len(r)-1])
	if e["code"] != float64(codeInvalidParams) {
		t.Fatalf("code = %v, want %d (invalid params)", e["code"], codeInvalidParams)
	}
	if got := reasonOf(t, r[len(r)-1]); got != "scope_outside_root" {
		t.Errorf("reasonCode = %v, want scope_outside_root", got)
	}
	if rec.called {
		t.Error("the Manager ran; an authority path outside the turn's scope must be refused BEFORE any spend")
	}
}

// Declaring the document's directory in `_meta.reviewmesh.roots` admits it.
func TestScope_ExtraRootsAdmitAnAuthorityDocument(t *testing.T) {
	ws := projectDir(t, "project")
	other := projectDir(t, "docs")
	spec := filepath.Join(other, "spec.md")
	if err := os.WriteFile(spec, []byte("the intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &authorityRecorder{}
	r := serveCeiling(t, rec, nil,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonQuote(ws)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report",`+
			`"_meta":{"reviewmesh":{"roots":[`+jsonQuote(other)+`],"authority":[{"name":"spec","path":`+jsonQuote(spec)+`}]}}}}`)
	if e, ok := r[len(r)-1]["error"]; ok {
		t.Fatalf("an authority document inside a declared root must be accepted, got %v", e)
	}
	if !rec.called || len(rec.docs) != 1 {
		t.Errorf("the Manager received %+v, want the declared document", rec.docs)
	}
}

// An extra root outside the ceiling is refused like a workspace would be.
func TestScope_ExtraRootOutsideCeilingRefused(t *testing.T) {
	ceiling := projectDir(t, "allowed")
	other := projectDir(t, "elsewhere")
	rec := &recordReviewer{}
	r := serveCeiling(t, rec, []string{ceiling},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+jsonQuote(ceiling)+`,"mode":"report","_meta":{"reviewmesh":{"roots":[`+jsonQuote(other)+`]}}}}`)
	if got := reasonOf(t, r[0]); got != ReasonOutsideCeiling {
		t.Errorf("reasonCode = %v, want %q", got, ReasonOutsideCeiling)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q; an extra root outside the ceiling must be refused", rec.ws)
	}
}

// TestHaltError_CarriesMachineCodes pins machine-readable halts on the agent surface: a halt's
// protocol error carries the stable reason code and the classified signal, so a host reacts to codes
// rather than parsing the message.
func TestHaltError_CarriesMachineCodes(t *testing.T) {
	halt := review.HaltClass("A")
	f := fault.New(fault.Adapter, "adapter \"x-cli\" exited 1").
		WithHalt("A").WithReason("adapter_exited_nonzero").WithSignal("login_required")
	mgr := fakeReviewer{
		out: review.RunOutcome{Status: "halted", Halt: &halt, RunDir: "/tmp/run-x",
			Failure: &review.LaneFailure{Role: "reviewer", Adapter: "x-cli",
				HaltClass: "A", ReasonCode: "adapter_exited_nonzero", Signal: "login_required"}},
		err: f,
	}
	r := serve(t, mgr, `{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":"/ws","mode":"report"}}`)
	e := rpcErr(t, r[0])
	data, _ := e["data"].(map[string]any)
	if data == nil {
		t.Fatalf("halt error carries no data: %v", e)
	}
	if data["reasonCode"] != "adapter_exited_nonzero" {
		t.Errorf("reasonCode = %v, want adapter_exited_nonzero", data["reasonCode"])
	}
	if data["signal"] != "login_required" {
		t.Errorf("signal = %v, want login_required", data["signal"])
	}
	if data["haltClass"] != "A" {
		t.Errorf("haltClass = %v, want A", data["haltClass"])
	}
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "review halted") {
		t.Errorf("message = %q, want the human rendering to remain", msg)
	}
}

// jsonQuote renders a path as a JSON string literal (paths may contain characters a raw
// concatenation would break).
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
