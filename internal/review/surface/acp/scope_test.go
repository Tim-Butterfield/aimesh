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

// These tests exist because the previous ones proved nothing: they built the resolver FROM
// the requested path, so the request authorized itself and every "containment" assertion was
// tautological. Each server below is launched with a SEPARATE trusted root — the out-of-band
// consent an operator gives with `aimesh review acp --root <dir>` — and the requests then try to
// reach outside it.

// serveRoots runs a server whose trusted roots are exactly `roots` (an operator's `--root`
// set), so a request path is judged against something it did not choose.
func serveRoots(t *testing.T, mgr Reviewer, roots []string, lines ...string) []map[string]any {
	t.Helper()
	return runServer(t, &Server{Manager: mgr, Roots: roots,
		Caps: review.SurfaceCaps{FileRead: true, FileWrite: true}}, lines...)
}

// projectDir builds an ordinary, perfectly readable project directory.
// projectDir builds a directory that LOOKS LIKE A PROJECT, because cwd inference now requires
// evidence of that rather than assuming it (see hasProjectMarker). `go.mod` is the marker: these
// fixtures already hold Go source, so it is the honest one.
func projectDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// (a) THE G1 FIX. An ORDINARY, readable directory that the operator never authorized is
// refused before any spend, with the machine reason code — the peer naming it is not consent.
// Nothing about the path is suspicious: it is a normal project with normal source files, and
// that is the point. Under the old self-rooted check this request ran, and the directory's
// contents were copied into prompts shipped to external model CLIs.
func TestScope_RefusesReadableDirOutsideTrustedRoot(t *testing.T) {
	trusted := projectDir(t, "trusted-project")
	other := projectDir(t, "someone-elses-project")
	if err := os.WriteFile(filepath.Join(other, "notes.txt"), []byte("private notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &recordReviewer{}
	b, _ := json.Marshal(other)
	r := serveRoots(t, rec, []string{trusted},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+string(b)+`,"mode":"report"}}`)
	e := rpcErr(t, r[0])
	data, _ := e["data"].(map[string]any)
	if data == nil || data["reasonCode"] != "scope_outside_root" {
		t.Errorf("error data = %v, want reasonCode scope_outside_root", data)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q — a path outside every trusted root must be refused BEFORE any spend", rec.ws)
	}
}

// (b) Blast radius: a path INSIDE a trusted root runs normally, unrewritten. Confinement must
// cost an authorized session nothing.
func TestScope_AllowsPathInsideTrustedRoot(t *testing.T) {
	trusted := projectDir(t, "trusted-project")
	sub := filepath.Join(trusted, "service")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recordReviewer{}
	b, _ := json.Marshal(sub)
	r := serveRoots(t, rec, []string{trusted},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+string(b)+`,"mode":"report"}}`)
	if res := result(t, r[0]); res["status"] != "stable" {
		t.Errorf("a subdirectory of a trusted root must run normally, got %v", res)
	}
	if rec.ws != sub {
		t.Errorf("Manager workspace = %q, want the path the host supplied (unrewritten) %q", rec.ws, sub)
	}
}

// (c) The non-overridable denylist still applies INSIDE a trusted root: a trusted project does
// not make its `.env` reviewable. (Root confinement and the denylist are independent
// guarantees; authorizing the project authorizes neither the secret nor its subtree.)
func TestScope_RefusesDeniedPathInsideTrustedRoot(t *testing.T) {
	trusted := projectDir(t, "trusted-project")
	secret := filepath.Join(trusted, ".env")
	if err := os.WriteFile(secret, []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &recordReviewer{}
	b, _ := json.Marshal(secret)
	r := serveRoots(t, rec, []string{trusted},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+string(b)+`,"mode":"report"}}`)
	e := rpcErr(t, r[0])
	data, _ := e["data"].(map[string]any)
	if data == nil || data["reasonCode"] != "scope_read_denied" {
		t.Errorf("error data = %v, want reasonCode scope_read_denied", data)
	}
	if rec.ws != "" {
		t.Errorf("the Manager was invoked with %q; the refusal must happen BEFORE any run", rec.ws)
	}
}

// A server launched with NO trusted root fails CLOSED: every filesystem path is refused with
// scope_no_roots_configured, and the error teaches the operator how to grant one.
func TestScope_NoTrustedRootRefusesEveryPath(t *testing.T) {
	dir := projectDir(t, "any-project")
	rec := &recordReviewer{}
	b, _ := json.Marshal(dir)
	r := serveRoots(t, rec, nil,
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+string(b)+`,"mode":"report"}}`)
	e := rpcErr(t, r[0])
	data, _ := e["data"].(map[string]any)
	if data == nil || data["reasonCode"] != "scope_no_roots_configured" {
		t.Errorf("error data = %v, want reasonCode scope_no_roots_configured", data)
	}
	if msg, _ := e["message"].(string); !strings.Contains(msg, "--root") {
		t.Errorf("message = %q, want the remedy (`--root <project-dir>`) named", msg)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q; a rootless agent must refuse every path", rec.ws)
	}
}

// A session cwd supplied by the HOST (`session/new.cwd`) is a request path like any other: it
// cannot widen the trusted set, and the prompt that falls back to it is refused.
func TestScope_SessionCwdCannotWidenTrustedRoots(t *testing.T) {
	trusted := projectDir(t, "trusted-project")
	other := projectDir(t, "elsewhere")
	rec := &recordReviewer{}
	b, _ := json.Marshal(other)
	r := serveRoots(t, rec, []string{trusted},
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+string(b)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report"}}`)
	e := rpcErr(t, r[len(r)-1])
	data, _ := e["data"].(map[string]any)
	if data == nil || data["reasonCode"] != "scope_outside_root" {
		t.Errorf("error data = %v, want reasonCode scope_outside_root", data)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q; a host-supplied cwd must not widen the trusted set", rec.ws)
	}
}

// An explicit `session/prompt.workspace` cannot escape either — it overrides the session cwd,
// but overriding a request path with another request path grants nothing.
func TestScope_ExplicitWorkspaceCannotEscapeTrustedRoots(t *testing.T) {
	trusted := projectDir(t, "trusted-project")
	other := projectDir(t, "elsewhere")
	rec := &recordReviewer{}
	r := serveRoots(t, rec, []string{trusted},
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonQuote(trusted)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report","workspace":`+jsonQuote(other)+`}}`)
	e := rpcErr(t, r[len(r)-1])
	data, _ := e["data"].(map[string]any)
	if data == nil || data["reasonCode"] != "scope_outside_root" {
		t.Errorf("error data = %v, want reasonCode scope_outside_root", data)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q; an explicit workspace may only narrow the trusted set", rec.ws)
	}
}

// A `..` traversal out of a trusted root is refused: meshcore/scope applies `..` to the
// RESOLVED prefix, so the escape cannot be smuggled textually.
func TestScope_TraversalOutOfTrustedRootRefused(t *testing.T) {
	trusted := projectDir(t, "trusted-project")
	sibling := filepath.Join(filepath.Dir(trusted), "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	// Built by concatenation, not filepath.Join: Join would clean the `..` away textually,
	// and the point is that the resolver — not the caller — is what defeats it.
	escape := trusted + string(filepath.Separator) + ".." + string(filepath.Separator) + "sibling"
	rec := &recordReviewer{}
	r := serveRoots(t, rec, []string{trusted},
		`{"jsonrpc":"2.0","id":1,"method":"review","params":{"workspace":`+jsonQuote(escape)+`,"mode":"report"}}`)
	e := rpcErr(t, r[0])
	data, _ := e["data"].(map[string]any)
	if data == nil || data["reasonCode"] != "scope_outside_root" {
		t.Errorf("error data = %v, want reasonCode scope_outside_root", data)
	}
	if rec.ws != "" {
		t.Errorf("the Manager ran with %q; a `..` escape must be refused", rec.ws)
	}
}

// (d) The zero-config path is untouched: `inlineWorkspace` content is materialized into a
// directory THIS process owns, so it needs no trusted root and works on a rootless server.
func TestScope_InlineWorkspaceWorksWithNoRoots(t *testing.T) {
	rec := &recordReviewer{}
	r := serveRoots(t, rec, nil,
		`{"jsonrpc":"2.0","id":1,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report","inlineWorkspace":{"main.go":"package main\n"}}}`)
	last := r[len(r)-1]
	if e, ok := last["error"]; ok {
		t.Fatalf("inlineWorkspace must work with no trusted roots (the zero-config path), got %v", e)
	}
	if sr := stopReasonOf(t, last); sr != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", sr)
	}
	if rec.ws == "" {
		t.Error("the Manager should have run against the materialized inline workspace")
	}
}

// An authority document path is a REQUEST path too: declaring it must not authorize it. A
// perfectly readable document outside the trusted roots is refused pre-spend, so a peer cannot
// exfiltrate a file by calling it "the spec".
func TestScope_AuthorityPathOutsideTrustedRootRefused(t *testing.T) {
	trusted := projectDir(t, "trusted-project")
	other := projectDir(t, "elsewhere")
	spec := filepath.Join(other, "spec.md")
	if err := os.WriteFile(spec, []byte("someone else's document\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &authorityRecorder{}
	r := serveRoots(t, rec, []string{trusted},
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonQuote(trusted)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report",`+
			`"_meta":{"reviewmesh":{"authority":[{"name":"spec","path":`+jsonQuote(spec)+`}]}}}}`)
	e := rpcErr(t, r[len(r)-1])
	if e["code"] != float64(codeInvalidParams) {
		t.Fatalf("code = %v, want %d (invalid params)", e["code"], codeInvalidParams)
	}
	data, _ := e["data"].(map[string]any)
	if data == nil || data["reasonCode"] != "scope_outside_root" {
		t.Errorf("error data = %v, want reasonCode scope_outside_root", data)
	}
	if rec.called {
		t.Error("the Manager ran; an authority path outside the trusted roots must be refused BEFORE any spend")
	}
}

// An authority document INSIDE a trusted root still resolves — the rule narrows, it does not
// break the feature.
func TestScope_AuthorityPathInsideTrustedRootAllowed(t *testing.T) {
	trusted := projectDir(t, "trusted-project")
	spec := filepath.Join(trusted, "spec.md")
	if err := os.WriteFile(spec, []byte("the intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &authorityRecorder{}
	r := serveRoots(t, rec, []string{trusted},
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonQuote(trusted)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report",`+
			`"_meta":{"reviewmesh":{"authority":[{"name":"spec","path":`+jsonQuote(spec)+`}]}}}}`)
	if e, ok := r[len(r)-1]["error"]; ok {
		t.Fatalf("an authority document inside a trusted root must be accepted, got %v", e)
	}
	if !rec.called || len(rec.docs) != 1 {
		t.Errorf("the Manager received %+v, want the declared document", rec.docs)
	}
}

// (e) THE DEGENERATE-DEFAULT RULE. Adopting the launch cwd as the trusted root is an
// inference — "an IDE started me in the project the user opened". Where that inference is
// obviously false, the launch FAILS CLOSED with a machine reason and a remedy, instead of
// silently trusting everything the user can read.
func TestResolveTrustedRoots_RefusesDegenerateDefaults(t *testing.T) {
	home := t.TempDir()
	parent := filepath.Dir(home)
	root := string(filepath.Separator)
	for name, cwd := range map[string]string{
		"filesystem root": root,
		"home itself":     home,
		"home's parent":   parent,
	} {
		t.Run(name, func(t *testing.T) {
			got, _, err := ResolveTrustedRoots(RootOptions{Cwd: cwd, Home: home})
			if err == nil {
				t.Fatalf("cwd %q must NOT be adopted as a trusted root, got %v", cwd, got)
			}
			if fault.ReasonOf(err) != ReasonDegenerateDefaultRoot {
				t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonDegenerateDefaultRoot)
			}
			if !strings.Contains(err.Error(), "--root") {
				t.Errorf("error %q should name the remedy (--root)", err)
			}
		})
	}
}

// An ordinary project cwd IS adopted (the default has to keep working — that is the
// zero-configuration IDE story), and an explicit --root wins over it.
func TestResolveTrustedRoots_AdoptsOrdinaryCwdAndExplicitRoots(t *testing.T) {
	home := t.TempDir()
	proj := projectDir(t, "proj")
	got, _, err := ResolveTrustedRoots(RootOptions{Cwd: proj, Home: home})
	if err != nil {
		t.Fatalf("an ordinary project cwd must be adopted: %v", err)
	}
	if len(got) != 1 || got[0] != proj {
		t.Errorf("default roots = %v, want [%s]", got, proj)
	}
	other := projectDir(t, "other")
	got, _, err = ResolveTrustedRoots(RootOptions{Explicit: []string{proj, other}, Cwd: string(filepath.Separator), Home: home})
	if err != nil {
		t.Fatalf("explicit roots must be honored even from a degenerate cwd: %v", err)
	}
	if len(got) != 2 || got[0] != proj || got[1] != other {
		t.Errorf("explicit roots = %v, want [%s %s]", got, proj, other)
	}
}

// --no-default-root with no --root is a fail-closed launch error: an agent with no trusted
// root can serve nothing but inline workspaces, and the operator learns that at launch.
func TestResolveTrustedRoots_NoDefaultWithoutExplicitIsAnError(t *testing.T) {
	if _, _, err := ResolveTrustedRoots(RootOptions{NoDefault: true, Cwd: projectDir(t, "proj")}); err == nil {
		t.Fatal("--no-default-root with no --root must fail")
	} else if fault.ReasonOf(err) != ReasonNoTrustedRoot {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonNoTrustedRoot)
	}
}

// A --root that is itself a protected path is refused as a ROOT: the denylist is what keeps
// secrets out of prompts, so it cannot be opted out of by promoting the secret to a root.
func TestResolveTrustedRoots_RefusesProtectedRoot(t *testing.T) {
	ssh := filepath.Join(t.TempDir(), ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveTrustedRoots(RootOptions{Explicit: []string{ssh}}); err == nil {
		t.Fatal("a protected directory must never become a trusted root")
	} else if fault.ReasonOf(err) != ReasonRootDenied {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonRootDenied)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, _, err := ResolveTrustedRoots(RootOptions{Explicit: []string{missing}}); err == nil {
		t.Fatal("a nonexistent --root must fail at launch, not on first request")
	} else if fault.ReasonOf(err) != ReasonRootUnusable {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonRootUnusable)
	}
}

// TestHaltError_CarriesMachineCodes pins machine-readable halts on the agent surface: a halt's protocol error
// carries the stable reason code and the classified signal, so a host reacts to codes rather
// than parsing the message.
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
