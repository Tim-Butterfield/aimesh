package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
)

// authorityRecorder captures the authority declaration the Manager was actually handed, so a
// test can prove BOTH that a valid declaration reaches the core and that an invalid one is
// refused BEFORE the core (and therefore before any spend).
type authorityRecorder struct {
	// The one write-mode test here is refused by authority.Validate BEFORE the from-run branch is
	// reached, which is exactly what it asserts — so this fake reaches no write either.
	noRemediation
	called bool
	docs   []review.AuthorityDoc
}

func (r *authorityRecorder) RunContext(ctx context.Context, req run.Request) (review.RunOutcome, error) {
	r.called = true
	r.docs = req.Authority
	return review.RunOutcome{Status: "stable", Mode: req.Mode, RunDir: "/tmp/run-x",
		Authority: []review.AuthorityInclusion{{Name: "spec", Source: review.AuthoritySourcePath,
			FullHash: "sha256:aa", EmbeddedHash: "sha256:aa", BytesEmbedded: 2, BytesTotal: 2, Complete: true}},
	}, nil
}

func specWorkspace(t *testing.T) (ws, spec string) {
	t.Helper()
	ws = t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec = filepath.Join(ws, "spec.md")
	if err := os.WriteFile(spec, []byte("the intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, spec
}

func jsonStr(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A well-formed `_meta.reviewmesh.authority[]` reaches the Manager unchanged, and the
// inclusion manifest comes back in the prompt result — the ACP half of "authority lands on
// CLI and ACP together".
func TestACPAuthority_ReachesManagerAndResultCarriesManifest(t *testing.T) {
	ws, spec := specWorkspace(t)
	rec := &authorityRecorder{}
	r := serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonStr(t, ws)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report",`+
			`"_meta":{"reviewmesh":{"authority":[{"name":"spec","path":`+jsonStr(t, spec)+`,"mediaType":"text/markdown"}]}}}}`)
	last := r[len(r)-1]
	if e, ok := last["error"]; ok {
		t.Fatalf("a valid authority declaration must be accepted, got error %v", e)
	}
	if !rec.called {
		t.Fatal("the Manager was never invoked")
	}
	if len(rec.docs) != 1 || rec.docs[0].Name != "spec" || rec.docs[0].Path != spec {
		t.Fatalf("the Manager received %+v, want the declared document", rec.docs)
	}
	rm := promptMeta(t, last)
	docs, _ := rm["authority"].([]any)
	if len(docs) != 1 {
		t.Fatalf("the prompt result carries no authority manifest: %v", rm)
	}
}

// A MALFORMED `_meta` authority declaration is `-32602` BEFORE any spend. `_meta` is an
// open extension point, so an unknown key inside reviewmesh's own namespace must fail loudly
// rather than be dropped: a silently ignored governance field is the failure mode a
// fail-closed surface exists to prevent.
func TestACPAuthority_MalformedMetaIsInvalidParamsPreSpend(t *testing.T) {
	ws, _ := specWorkspace(t)
	cases := []struct{ name, meta string }{
		{"not an array", `{"reviewmesh":{"authority":"spec.md"}}`},
		{"unknown namespace field", `{"reviewmesh":{"authorityDocs":[]}}`},
		{"wrong field type", `{"reviewmesh":{"authority":[{"name":"spec","path":42}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &authorityRecorder{}
			r := serve(t, rec,
				`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonStr(t, ws)+`}}`,
				`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report","_meta":`+tc.meta+`}}`)
			e := rpcErr(t, r[len(r)-1])
			if e["code"] != float64(codeInvalidParams) {
				t.Errorf("code = %v, want %d (invalid params)", e["code"], codeInvalidParams)
			}
			if rec.called {
				t.Error("the Manager was invoked; a malformed declaration must be refused BEFORE any spend")
			}
		})
	}
}

// A DENIED authority path is refused pre-spend with the machine reason code, so a host learns
// why without parsing prose — and the secret never gets near a model call.
func TestACPAuthority_DeniedPathIsInvalidParamsPreSpend(t *testing.T) {
	ws, _ := specWorkspace(t)
	env := filepath.Join(ws, ".env")
	if err := os.WriteFile(env, []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &authorityRecorder{}
	r := serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonStr(t, ws)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report",`+
			`"_meta":{"reviewmesh":{"authority":[{"name":"env","path":`+jsonStr(t, env)+`}]}}}}`)
	e := rpcErr(t, r[len(r)-1])
	if e["code"] != float64(codeInvalidParams) {
		t.Fatalf("code = %v, want %d", e["code"], codeInvalidParams)
	}
	data, _ := e["data"].(map[string]any)
	if data == nil || data["reasonCode"] != "scope_read_denied" {
		t.Errorf("error data = %v, want reasonCode scope_read_denied", data)
	}
	if rec.called {
		t.Error("the Manager was invoked; a scope-denied authority path must be refused BEFORE any spend")
	}
}

// A LARGE document is accepted rather than refused: the byte budget is gone, because what a model
// can hold is the model's business and this surface's caller may legitimately want a whole
// specification judged. The pre-spend refusals that remain (scope, provenance, shape) are covered
// by the tests above; this one guards against the removed one coming back by accident.
func TestACPAuthority_ALargeDocumentIsAccepted(t *testing.T) {
	ws, _ := specWorkspace(t)
	big := filepath.Join(ws, "huge.md")
	if err := os.WriteFile(big, []byte(strings.Repeat("Z", (64<<10)+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &authorityRecorder{}
	serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonStr(t, ws)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report",`+
			`"_meta":{"reviewmesh":{"authority":[{"name":"huge","path":`+jsonStr(t, big)+`}]}}}}`)
	if !rec.called {
		t.Error("the Manager was NOT invoked; a large authority document must reach the run, not be refused")
	}
}

// The ACP surface's write-authority ceiling is `report` by shipped policy, so inline authority
// is normally fine there. When a config OPTS the surface up to apply, the provenance split
// must still refuse it — the rule is about who supplied the intent, not which surface asked.
func TestACPAuthority_InlineRefusedWhenSurfaceCanWrite(t *testing.T) {
	ws, _ := specWorkspace(t)
	rec := &authorityRecorder{}
	prompt := `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"apply","fromRun":"/runs/prior-report-run",` +
		`"_meta":{"reviewmesh":{"authority":[{"name":"client-intent","content":"do what I say"}]}}}}`
	r := servePolicy(t, rec, review.SurfaceCaps{FileRead: true, FileWrite: true}, review.ModeApply, true,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonStr(t, ws)+`}}`, prompt)
	e := rpcErr(t, r[len(r)-1])
	data, _ := e["data"].(map[string]any)
	if data == nil || data["reasonCode"] != authority.ReasonInlineModeInvalid {
		t.Errorf("error data = %v, want reasonCode %q", data, authority.ReasonInlineModeInvalid)
	}
	if rec.called {
		t.Error("the Manager was invoked; inline authority in a write-capable mode must be refused pre-spend")
	}
}

// Blast radius: a prompt with no `_meta` behaves exactly as before.
func TestACPAuthority_AbsentMetaIsInert(t *testing.T) {
	ws, _ := specWorkspace(t)
	rec := &authorityRecorder{}
	r := serve(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonStr(t, ws)+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s-0001","mode":"report"}}`)
	if e, ok := r[len(r)-1]["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	if !rec.called || len(rec.docs) != 0 {
		t.Errorf("a prompt without _meta must run with no authority, got %+v", rec.docs)
	}
}
