package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/verify"
)

// TestCodex_BannerIsNotIdentityEvidence pins that the codex banner's `model:` line, which echoes the
// requested argument even for a nonexistent model, is not treated as identity evidence. The fake binary
// reproduces that echo.
func TestCodex_BannerIsNotIdentityEvidence(t *testing.T) {
	rec := codexRecipe()
	if rec.Evidence != core.EvidenceNone {
		t.Fatalf("codex Evidence = %q, want none — the banner echoes our own --model argument", rec.Evidence)
	}
	if rec.ParseIdentity != nil {
		t.Error("codex must not parse an identity: the only channel it exposes is the echoed banner")
	}
	// The banner names whatever slug it was handed — here a model that does not exist anywhere.
	bin := fakeBin(t, `printf 'OpenAI Codex v0.147.0\n--------\nmodel: totally-not-a-real-model-9x\nprovider: openai\n--------\n' 1>&2; echo '{"schemaVersion":1,"role":"reviewer","verdict":"approve","findings":[]}'`)
	a := New(rec, bin, time.Minute)
	res, err := a.Invoke(context.Background(), model.Call{Role: "reviewer", Phase: "semantic_iterate", ModelArg: "totally-not-a-real-model-9x", CopyRoot: t.TempDir(), Prompt: "p"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.ActualModel != "" {
		t.Errorf("captured identity = %q, want empty — the echoed banner must not be read as evidence", res.ActualModel)
	}
	// Trusting the banner would classify a nonexistent model as verified.
	if st, ok := verify.ClassifyIdentity(res.Evidence, "totally-not-a-real-model-9x", res.ActualModel, "codex-cli"); ok || st != core.VerifUnknown {
		t.Errorf("ClassifyIdentity = %q,%v; want unknown,false", st, ok)
	}
	// The call still returns its output: an unknown identity never discards it.
	if len(res.Stdout) == 0 {
		t.Error("codex lane produced no output; an unknown identity must not suppress the response")
	}
}

// TestDevin_SelfReportNormalization pins that a display-name and effort self-report normalizes to the
// requested slug and classifies as self_reported, and that output without a self-report yields no
// identity.
func TestDevin_SelfReportNormalization(t *testing.T) {
	rec := devinRecipe()
	if rec.Evidence != core.EvidenceSelfReport {
		t.Fatalf("devin Evidence ceiling = %q, want self_report (weak, never verified)", rec.Evidence)
	}
	// A probe-style response: a Devin display name with a separate effort.
	bin := fakeBin(t, `echo '{"result":"hello (source: system prompt)","model":"Claude Opus 4.8","effort":"Medium"}'`)
	a := New(rec, bin, time.Minute)
	res, err := a.Invoke(context.Background(), model.Call{Role: "reviewer", Phase: "semantic_iterate", ModelArg: "claude-opus-4-8-medium", CopyRoot: t.TempDir(), Prompt: "p"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.ActualModel != "claude-opus-4-8-medium" {
		t.Errorf("normalized self-report = %q, want claude-opus-4-8-medium (display+effort → slug)", res.ActualModel)
	}
	if st, ok := verify.ClassifyIdentity(res.Evidence, "claude-opus-4-8-medium", res.ActualModel, "devin-cli"); !ok || st != core.VerifSelfReported {
		t.Errorf("ClassifyIdentity = %q,%v; want self_reported,true (weak match, not verified)", st, ok)
	}
	// Output without a self-report captures no identity.
	bin2 := fakeBin(t, `echo '{"schemaVersion":1,"role":"reviewer","verdict":"approve","findings":[]}'`)
	res2, err := New(rec, bin2, time.Minute).Invoke(context.Background(), model.Call{Role: "reviewer", Phase: "semantic_iterate", ModelArg: "claude-opus-4-8-medium", CopyRoot: t.TempDir(), Prompt: "p"})
	if err != nil {
		t.Fatalf("invoke2: %v", err)
	}
	if res2.ActualModel != "" || res2.Evidence != core.EvidenceNone {
		t.Errorf("ordinary review must capture no identity, got actual=%q evidence=%q", res2.ActualModel, res2.Evidence)
	}
	if st, _ := verify.ClassifyIdentity(res2.Evidence, "claude-opus-4-8-medium", res2.ActualModel, "devin-cli"); st != core.VerifUnknown {
		t.Errorf("ordinary review identity = %q, want unknown", st)
	}
}

// TestDevin_SelfReportWrapper pins that ParsePayload strips the identity wrapper to the inner result and
// ParseIdentity normalizes the reported display name and effort to the requested slug.
func TestDevin_SelfReportWrapper(t *testing.T) {
	rec := devinRecipe()
	inner := `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","summary":"s","verdict":"approve","findings":[]}`
	bin := fakeBin(t, `echo '{"reviewmeshIdentity":{"model":"Claude Opus 4.8","effort":"Medium","source":"self_report"},"result":`+inner+`}'`)
	res, err := New(rec, bin, time.Minute).Invoke(context.Background(), model.Call{Role: "reviewer", Phase: "semantic_iterate", ModelArg: "claude-opus-4-8-medium", CopyRoot: t.TempDir(), Prompt: "p"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if string(res.Payload) != inner {
		t.Errorf("ParsePayload must strip the wrapper to the inner result:\n got %q\nwant %q", res.Payload, inner)
	}
	if res.ActualModel != "claude-opus-4-8-medium" {
		t.Errorf("wrapper self-report normalized = %q, want claude-opus-4-8-medium", res.ActualModel)
	}
	if st, ok := verify.ClassifyIdentity(res.Evidence, "claude-opus-4-8-medium", res.ActualModel, "devin-cli"); !ok || st != core.VerifSelfReported {
		t.Errorf("ClassifyIdentity = %q,%v; want self_reported,true", st, ok)
	}
}

// TestAgy_SelfReportWrapper pins agy's wrapper handling: model "Gemini 3.1 Pro" with effort "High"
// matches the requested "Gemini 3.1 Pro (High)" on canonical form as self_reported, and the inner result
// is unwrapped.
func TestAgy_SelfReportWrapper(t *testing.T) {
	rec := agyRecipe()
	if rec.Evidence != core.EvidenceSelfReport {
		t.Fatalf("agy Evidence ceiling = %q, want self_report", rec.Evidence)
	}
	inner := `{"schemaVersion":1,"role":"verifier","phase":"semantic_verify","summary":"s","verdict":"approve","findings":[]}`
	bin := fakeBin(t, `echo '{"reviewmeshIdentity":{"model":"Gemini 3.1 Pro","effort":"High","source":"self_report"},"result":`+inner+`}'`)
	res, err := New(rec, bin, time.Minute).Invoke(context.Background(), model.Call{Role: "verifier", Phase: "semantic_verify", ModelArg: "Gemini 3.1 Pro (High)", CopyRoot: t.TempDir(), Prompt: "p"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if string(res.Payload) != inner {
		t.Errorf("ParsePayload must strip the wrapper to the inner result:\n got %q\nwant %q", res.Payload, inner)
	}
	if st, ok := verify.ClassifyIdentity(res.Evidence, "Gemini 3.1 Pro (High)", res.ActualModel, "agy-cli"); !ok || st != core.VerifSelfReported {
		t.Errorf("ClassifyIdentity(agy, actual=%q) = %q,%v; want self_reported,true", res.ActualModel, st, ok)
	}
	// A bare result without a wrapper captures no identity and no payload.
	bin2 := fakeBin(t, `echo '{"schemaVersion":1,"role":"verifier","phase":"semantic_verify","verdict":"approve","findings":[]}'`)
	res2, _ := New(rec, bin2, time.Minute).Invoke(context.Background(), model.Call{Role: "verifier", Phase: "semantic_verify", ModelArg: "Gemini 3.1 Pro (High)", CopyRoot: t.TempDir(), Prompt: "p"})
	if res2.ActualModel != "" || res2.Evidence != core.EvidenceNone || res2.Payload != nil {
		t.Errorf("bare output must capture no identity + no payload, got actual=%q evidence=%q payload=%q", res2.ActualModel, res2.Evidence, res2.Payload)
	}
}

// readArgv reads the NUL-separated argv a fake binary recorded.
func readArgv(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	parts := strings.Split(string(b), "\x00")
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1] // drop trailing empty after the final NUL
	}
	return parts
}

// recipeExpect declares, per recipe, the argv flags that must be present and whether the model argument
// is passed in argv.
var recipeExpect = map[string]struct {
	flags      []string
	modelInArg bool
	// onStdin: the recipe delivers the prompt on standard input (see Recipe.PromptOnStdin), so the
	// prompt must be absent from argv.
	onStdin bool
}{
	"ollama":      {flags: []string{"run"}, modelInArg: true},
	"codex-cli":   {flags: []string{"exec", "-s", "read-only", "--skip-git-repo-check", "-m"}, modelInArg: true, onStdin: true},
	"claude-code": {flags: []string{"-p", "--permission-mode", "plan", "--disallowedTools", "--output-format", "json", "--model"}, modelInArg: true, onStdin: true},
	"devin-cli":   {flags: []string{"--model", "-p"}, modelInArg: true},
	"agy-cli":     {flags: []string{"--model", "-p"}, modelInArg: true},
	// No `-p` for gemini: the piped stdin is the prompt.
	"gemini-cli": {flags: []string{"--model", "--approval-mode", "plan"}, modelInArg: true, onStdin: true},
	"cursor-cli": {flags: []string{"--print", "--mode", "plan", "--trust", "--output-format", "json", "--model"}, modelInArg: true},
}

// TestShellAdapterMatrix_FakeBinary drives every shell recipe against a fake binary in each role and
// phase, checking argv construction, prompt delivery, output capture and identity extraction without a
// real CLI. BuildArgs ignores Role and Phase, so iterating them covers call plumbing only.
func TestShellAdapterMatrix_FakeBinary(t *testing.T) {
	roles := []struct {
		role  string
		phase string
	}{
		{"reviewer", "semantic_iterate"},
		{"cross_check", "semantic_cross_check"},
		{"verifier", "semantic_verify"},
		{"author_remediator", "semantic_adjudicate"},
	}
	for name, rec := range Recipes() {
		exp, ok := recipeExpect[name]
		if !ok {
			t.Fatalf("no expectation declared for recipe %q", name)
		}
		for _, rp := range roles {
			t.Run(name+"/"+string(rp.role), func(t *testing.T) {
				dir := t.TempDir()
				argvOut := filepath.Join(dir, "argv")
				stdinOut := filepath.Join(dir, "stdin")
				t.Setenv("REVIEWMESH_FAKE_ARGV", argvOut)
				t.Setenv("REVIEWMESH_FAKE_STDIN", stdinOut)
				// record argv (NUL-separated) AND stdin, emit stderr + role-appropriate JSON, exit 0.
				// `cat` returns immediately when the adapter wires no stdin (Go supplies /dev/null).
				bin := fakeBin(t, `: > "$REVIEWMESH_FAKE_ARGV"; for a in "$@"; do printf '%s\0' "$a" >> "$REVIEWMESH_FAKE_ARGV"; done; cat > "$REVIEWMESH_FAKE_STDIN"; echo "diagnostic line" 1>&2; echo '{"schemaVersion":1,"ok":true}'`)
				a := New(rec, bin, time.Minute)
				call := model.Call{
					Role: string(rp.role), Phase: string(rp.phase), Model: "mcat", ModelArg: "model-XYZ",
					CopyRoot: t.TempDir(), Prompt: "PROMPT-MARKER-" + string(rp.phase),
				}
				res, err := a.Invoke(context.Background(), call)
				if err != nil {
					t.Fatalf("invoke: %v", err)
				}
				if res.ExitCode != 0 {
					t.Errorf("exit = %d, want 0", res.ExitCode)
				}
				if !strings.Contains(string(res.Stdout), `"ok":true`) {
					t.Errorf("stdout not captured: %q", res.Stdout)
				}
				if !strings.Contains(string(res.Stderr), "diagnostic line") {
					t.Errorf("stderr not captured: %q", res.Stderr)
				}
				argv := readArgv(t, argvOut) // proves the fake binary ran
				if exp.modelInArg && !slices.Contains(argv, "model-XYZ") {
					t.Errorf("%s: model arg not passed in argv: %v", name, argv)
				}
				// The prompt is delivered one way, never both.
				if exp.onStdin {
					b, rerr := os.ReadFile(stdinOut)
					if rerr != nil {
						t.Fatalf("%s: stdin was never written: %v", name, rerr)
					}
					if strings.TrimSpace(string(b)) != call.Prompt {
						t.Errorf("%s: prompt not delivered on stdin: got %q", name, b)
					}
					if slices.Contains(argv, call.Prompt) {
						t.Errorf("%s: prompt sent on stdin AND in argv — the CLI would see it twice: %v", name, argv)
					}
				} else if !slices.Contains(argv, call.Prompt) {
					t.Errorf("%s: prompt not delivered in argv: %v", name, argv)
				}
				for _, f := range exp.flags {
					if !slices.Contains(argv, f) {
						t.Errorf("%s: missing recipe flag %q in argv: %v", name, f, argv)
					}
				}
				// identity: ollama self-reports the tag; cloud recipes leave it uncaptured
				// ("") so the verifier treats them as unverified (verify-on-provision).
				if name == "ollama" {
					if res.ActualModel != "model-XYZ" {
						t.Errorf("ollama identity = %q, want model-XYZ", res.ActualModel)
					}
				} else if res.ActualModel != "" {
					t.Errorf("%s identity = %q, want \"\" (verify-on-provision)", name, res.ActualModel)
				}
			})
		}
	}
}

// TestShell_EffortArg pins that codex-cli passes a separable effort only when set, and that name-bound or
// unsupported recipes never add a separate effort flag.
func TestShell_EffortArg(t *testing.T) {
	argvOut := filepath.Join(t.TempDir(), "argv")
	t.Setenv("REVIEWMESH_FAKE_ARGV", argvOut)
	bin := fakeBin(t, `: > "$REVIEWMESH_FAKE_ARGV"; for a in "$@"; do printf '%s\0' "$a" >> "$REVIEWMESH_FAKE_ARGV"; done; echo '{}'`)
	recipes := Recipes()

	// codex-cli (separable): effort set → `-c model_reasoning_effort=high` present
	if _, err := New(recipes["codex-cli"], bin, time.Minute).Invoke(context.Background(), model.Call{ModelArg: "gpt-5.5", Effort: "high", Prompt: "P"}); err != nil {
		t.Fatal(err)
	}
	argv := readArgv(t, argvOut)
	if !slices.Contains(argv, "-c") || !slices.Contains(argv, "model_reasoning_effort=high") {
		t.Errorf("codex effort not passed: %v", argv)
	}

	// codex-cli: effort unset → no effort arg
	if _, err := New(recipes["codex-cli"], bin, time.Minute).Invoke(context.Background(), model.Call{ModelArg: "gpt-5.5", Prompt: "P"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range readArgv(t, argvOut) {
		if strings.HasPrefix(a, "model_reasoning_effort=") {
			t.Errorf("codex added an effort arg when effort was unset: %v", a)
		}
	}

	// name-bound (devin-cli, agy-cli) + unsupported (ollama): effort set but NO effort flag
	for _, name := range []string{"devin-cli", "agy-cli", "ollama"} {
		if _, err := New(recipes[name], bin, time.Minute).Invoke(context.Background(), model.Call{ModelArg: "m", Effort: "high", Prompt: "P"}); err != nil {
			t.Fatal(err)
		}
		for _, a := range readArgv(t, argvOut) {
			if strings.Contains(a, "model_reasoning_effort") || strings.HasPrefix(a, "effort=") || a == "--effort" {
				t.Errorf("%s must not add a separate effort flag (name-bound/unsupported): %v", name, a)
			}
		}
	}
}

func TestClaude_EnvelopeIdentity(t *testing.T) {
	// real (sanitized) claude-code --output-format json envelope fixture
	stdout, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "adapters", "claude", "real-envelope-opus-4-8", "stdout.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got := parseClaudeEnvelope(stdout, nil, model.Call{})
	if got != "claude-opus-4-8[1m]" {
		t.Errorf("claude envelope model = %q, want claude-opus-4-8[1m]", got)
	}
	// non-envelope / fake output → no evidence
	if parseClaudeEnvelope([]byte(`{"schemaVersion":1,"ok":true}`), nil, model.Call{}) != "" {
		t.Error("non-envelope output must yield no identity")
	}
}

func TestClaude_ModelArgFlag(t *testing.T) {
	r := claudeRecipe()
	with := r.BuildArgs(model.Call{Prompt: "p", ModelArg: "claude-opus-4-8", CopyRoot: "/x"})
	if !slices.Contains(with, "--model") || !slices.Contains(with, "claude-opus-4-8") {
		t.Errorf("claude recipe should pin --model when set, got %v", with)
	}
	// read-only flags preserved
	for _, f := range []string{"-p", "--permission-mode", "plan", "--disallowedTools", "--output-format", "json", "--add-dir"} {
		if !slices.Contains(with, f) {
			t.Errorf("claude recipe missing %q: %v", f, with)
		}
	}
	without := r.BuildArgs(model.Call{Prompt: "p"})
	if slices.Contains(without, "--model") {
		t.Errorf("claude recipe must omit --model when ModelArg empty, got %v", without)
	}
}

func TestClaude_RealReviewEnvelope_PrimaryModelAndUnwrap(t *testing.T) {
	// A sanitized real envelope: modelUsage lists a small haiku helper and the opus-4-8 model that
	// answered. parseClaudeEnvelope picks the answering model; parseClaudePayload unwraps `result`.
	dir := filepath.Join("..", "..", "..", "testdata", "adapters", "claude", "real-review-envelope-haiku-opus")
	stdout, err := os.ReadFile(filepath.Join(dir, "stdout.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if got := parseClaudeEnvelope(stdout, nil, model.Call{}); got != "claude-opus-4-8[1m]" {
		t.Errorf("primary model = %q, want claude-opus-4-8[1m] (max output tokens, not the haiku helper)", got)
	}
	payload := parseClaudePayload(stdout, nil, model.Call{})
	if !strings.Contains(string(payload), `"verdict": "approve"`) || strings.Contains(string(payload), "modelUsage") {
		t.Errorf("unwrapped payload should be the reviewer JSON, not the envelope; got: %s", payload)
	}
}

// TestClaude_ShortResponse_RequestedModelBeatsAuxiliary guards against a false identity mismatch: on a
// short response, the auxiliary haiku model Claude Code reports in modelUsage can produce more output
// tokens than the requested model.
func TestClaude_ShortResponse_RequestedModelBeatsAuxiliary(t *testing.T) {
	// An observed shape: the auxiliary model out-tokens the requested one.
	stdout := []byte(`{"is_error":false,"modelUsage":{` +
		`"claude-haiku-4-5-20251001":{"outputTokens":12},` +
		`"claude-opus-5":{"outputTokens":4}}}`)

	// Requested by alias: resolves to the requested model, not the auxiliary.
	if got := parseClaudeEnvelope(stdout, nil, model.Call{ModelArg: "opus"}); got != "claude-opus-5" {
		t.Errorf("identity for --model opus = %q, want claude-opus-5 (the auxiliary haiku must not win)", got)
	}
	// Requested by full id — same answer.
	if got := parseClaudeEnvelope(stdout, nil, model.Call{ModelArg: "claude-opus-5"}); got != "claude-opus-5" {
		t.Errorf("identity for --model claude-opus-5 = %q, want claude-opus-5", got)
	}
	// Requesting the auxiliary model itself still reports it (no special-casing of haiku).
	if got := parseClaudeEnvelope(stdout, nil, model.Call{ModelArg: "haiku"}); got != "claude-haiku-4-5-20251001" {
		t.Errorf("identity for --model haiku = %q, want the haiku entry", got)
	}
}

// TestClaude_RequestedModelAbsent_ReportsSubstitute pins that when the requested model never ran, the
// parser reports the model that did, so the verifier sees the mismatch; it never echoes the request.
func TestClaude_RequestedModelAbsent_ReportsSubstitute(t *testing.T) {
	stdout := []byte(`{"is_error":false,"modelUsage":{` +
		`"claude-haiku-4-5-20251001":{"outputTokens":12},` +
		`"claude-sonnet-5":{"outputTokens":400}}}`)

	// Opus was requested but never ran → report the dominant model that did (a real substitution).
	if got := parseClaudeEnvelope(stdout, nil, model.Call{ModelArg: "opus"}); got != "claude-sonnet-5" {
		t.Errorf("identity = %q, want claude-sonnet-5 — a substituted model must be reported, not hidden", got)
	}

	// Present with no output is not evidence that it answered, so the substitute is still reported.
	zero := []byte(`{"is_error":false,"modelUsage":{` +
		`"claude-opus-5":{"outputTokens":0},` +
		`"claude-sonnet-5":{"outputTokens":400}}}`)
	if got := parseClaudeEnvelope(zero, nil, model.Call{ModelArg: "opus"}); got != "claude-sonnet-5" {
		t.Errorf("identity = %q, want claude-sonnet-5 — a zero-output requested model must not be claimed "+
			"as the answering identity", got)
	}
}

func TestClaude_PayloadUnwrap(t *testing.T) {
	reviewerJSON := `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"approve","findings":[]}`
	// claude envelope: result is the model's text; modelUsage present
	envelope := `{"type":"result","result":` + jsonQuote(reviewerJSON) + `,"modelUsage":{"claude-opus-4-8[1m]":{"inputTokens":1}}}`
	got := parseClaudePayload([]byte(envelope), nil, model.Call{})
	if string(got) != reviewerJSON {
		t.Errorf("unwrapped payload = %q, want %q", got, reviewerJSON)
	}
	// non-envelope output → nil (the caller falls back to stdout)
	if parseClaudePayload([]byte(reviewerJSON), nil, model.Call{}) != nil {
		t.Error("plain reviewer JSON must not be treated as an envelope")
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestShell_IdentityExtraction(t *testing.T) {
	rec := Recipe{
		Name: "idtest", Detect: "x",
		BuildArgs: func(c model.Call) []string { return []string{c.Prompt} },
		ParseIdentity: func(stdout, _ []byte, _ model.Call) string {
			for line := range strings.SplitSeq(string(stdout), "\n") {
				if m, ok := strings.CutPrefix(strings.TrimSpace(line), "model="); ok {
					return m
				}
			}
			return ""
		},
	}
	// verified: binary reports the requested model
	bin := fakeBin(t, `echo "model=model-XYZ"`)
	res, err := New(rec, bin, time.Minute).Invoke(context.Background(), model.Call{ModelArg: "model-XYZ", Prompt: "p"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.ActualModel != "model-XYZ" {
		t.Errorf("verified identity = %q, want model-XYZ", res.ActualModel)
	}
	// mismatch: the binary reports a different model
	bin2 := fakeBin(t, `echo "model=some-other-model"`)
	res2, _ := New(rec, bin2, time.Minute).Invoke(context.Background(), model.Call{ModelArg: "model-XYZ", Prompt: "p"})
	if res2.ActualModel != "some-other-model" {
		t.Errorf("mismatch identity = %q, want some-other-model", res2.ActualModel)
	}
}

// fakeBin writes an executable /bin/sh script and returns its path.
func fakeBin(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake /bin/sh binaries are POSIX; exec mechanics are verify-on-windows")
	}
	p := filepath.Join(t.TempDir(), "fakebin")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func testRecipe() Recipe {
	return Recipe{
		Name: "fake", Detect: "definitely-not-on-path-xyz",
		BuildArgs:     func(c model.Call) []string { return nil },
		ParseIdentity: func(_, _ []byte, c model.Call) string { return string(c.ModelArg) },
	}
}

func TestShell_NormalOutput(t *testing.T) {
	bin := fakeBin(t, `echo '{"ok":true}'`)
	a := New(testRecipe(), bin, time.Second)
	res, err := a.Invoke(context.Background(), model.Call{ModelArg: "m1", Prompt: "hi"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "ok") {
		t.Errorf("result = %+v", res)
	}
	if res.ActualModel != "m1" {
		t.Errorf("identity = %q, want m1", res.ActualModel)
	}
}

func TestShell_NonZeroExitClassifiedByCode(t *testing.T) {
	bin := fakeBin(t, `echo oops 1>&2; exit 3`)
	a := New(testRecipe(), bin, time.Second)
	res, err := a.Invoke(context.Background(), model.Call{ModelArg: "m1"})
	if err != nil {
		t.Fatalf("non-zero exit should not be a Go error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "oops") {
		t.Errorf("stderr not captured: %q", res.Stderr)
	}
}

func TestShell_Timeout(t *testing.T) {
	bin := fakeBin(t, `sleep 5`)
	a := New(testRecipe(), bin, 100*time.Millisecond)
	res, err := a.Invoke(context.Background(), model.Call{ModelArg: "m1"})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if res.ExitCode != 124 {
		t.Errorf("exit code = %d, want 124 (timeout)", res.ExitCode)
	}
}

func TestShell_ContextCancel(t *testing.T) {
	bin := fakeBin(t, `sleep 5`)
	a := New(testRecipe(), bin, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	res, err := a.Invoke(ctx, model.Call{ModelArg: "m1"})
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if res.ExitCode != 125 {
		t.Errorf("exit code = %d, want 125 (cancelled)", res.ExitCode)
	}
}

func TestShell_AvailabilityAndPath(t *testing.T) {
	bin := fakeBin(t, `echo hi`)
	if ok, _ := New(testRecipe(), bin, 0).Available(); !ok {
		t.Error("configured path should be available")
	}
	if ok, _ := New(testRecipe(), "", 0).Available(); ok {
		t.Error("a bogus Detect name should not be available")
	}
	if ok, _ := New(testRecipe(), "/no/such/binary", 0).Available(); ok {
		t.Error("a missing configured path should not be available")
	}
}

func TestShell_PathDiagnostics(t *testing.T) {
	// configured non-executable path
	if runtime.GOOS != "windows" {
		p := filepath.Join(t.TempDir(), "noexec")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, detail := New(testRecipe(), p, 0).Available(); ok || !strings.Contains(detail, "not executable") {
			t.Errorf("non-executable: ok=%v detail=%q", ok, detail)
		}
	}
	// configured path is a directory
	if ok, detail := New(testRecipe(), t.TempDir(), 0).Available(); ok || !strings.Contains(detail, "directory") {
		t.Errorf("directory: ok=%v detail=%q", ok, detail)
	}
	// configured path missing
	if ok, detail := New(testRecipe(), "/no/such/bin", 0).Available(); ok || !strings.Contains(detail, "not found") {
		t.Errorf("missing: ok=%v detail=%q", ok, detail)
	}
	// not on PATH (bogus Detect)
	if ok, detail := New(testRecipe(), "", 0).Available(); ok || !strings.Contains(detail, "not found on PATH") {
		t.Errorf("PATH-missing: ok=%v detail=%q", ok, detail)
	}
}

func TestShell_Probe(t *testing.T) {
	bin := fakeBin(t, `echo "fakecli 1.2.3"`)
	if r := New(testRecipe(), bin, time.Minute).Probe(context.Background()); !r.OK || !strings.Contains(r.Detail, "probe ok") {
		t.Errorf("probe ok: %+v", r)
	}
	if r := New(testRecipe(), "/no/such/bin", time.Minute).Probe(context.Background()); r.OK {
		t.Error("probe of a missing binary should fail")
	}
}

func TestShell_Probe_BothStreams(t *testing.T) {
	// writes to BOTH stdout and stderr — run under -race to catch a shared-buffer race
	bin := fakeBin(t, `echo "ver 9.9 (stdout)"; echo "warn (stderr)" 1>&2`)
	r := New(testRecipe(), bin, time.Minute).Probe(context.Background())
	if !r.OK || !strings.Contains(r.Detail, "ver 9.9") {
		t.Errorf("probe both streams: %+v", r)
	}
}

func TestShell_Probe_ClassifiesTrust(t *testing.T) {
	// a CLI that prints a folder-trust message and fails → the probe carries the folder_trust signal.
	bin := fakeBin(t, `echo "Not inside a trusted directory" 1>&2; exit 1`)
	r := New(testRecipe(), bin, time.Minute).Probe(context.Background())
	if r.OK || r.Signal != clihint.FolderTrust {
		t.Errorf("expected a failed probe classified folder_trust, got %+v", r)
	}
}

func TestRecipes_CoverDocumentedAdapters(t *testing.T) {
	r := Recipes()
	for _, name := range []string{"ollama", "codex-cli", "claude-code", "devin-cli", "agy-cli", "gemini-cli"} {
		rec, ok := r[name]
		if !ok || rec.Detect == "" || rec.BuildArgs == nil {
			t.Errorf("recipe %q missing or incomplete", name)
		}
	}
}
