package shell

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/verify"
)

// Recipes returns the per-CLI recipes documented in docs/adapters.md. Where identity extraction is
// not captured, ParseIdentity returns "" so the verifier treats the call as unverified.
//
// CI=1 is set only for recipes that read no identity from the CLI's output (gemini-cli and
// cursor-cli), so CI mode cannot remove a recorded identity signal. It is not set for claude-code,
// whose identity envelope is untested under CI; for agy-cli and devin-cli, whose self-report is
// parsed from stdout; or for ollama, where it has no defined effect.
func Recipes() map[string]Recipe {
	return map[string]Recipe{
		"ollama":      ollamaRecipe(),
		"codex-cli":   codexRecipe(),
		"claude-code": claudeRecipe(),
		"devin-cli":   devinRecipe(),
		"agy-cli":     agyRecipe(),
		"gemini-cli":  geminiRecipe(),
		"cursor-cli":  cursorRecipe(),
	}
}

// EgressFor reports where a named adapter's content goes. ok is false for adapters this package does
// not own, such as user-defined ACP instances, whose destination is whatever the operator chose;
// callers must render that as unknown, never as local.
func EgressFor(adapter string) (Egress, bool) {
	r, ok := Recipes()[adapter]
	if !ok || (r.Egress.Destination == "" && !r.Egress.Local) {
		return Egress{}, false
	}
	return r.Egress, true
}

// selfReportTag returns the invocation tag as the identity, for adapters where it is the strongest
// available signal (Ollama).
func selfReportTag(_, _ []byte, c model.Call) string { return string(c.ModelArg) }

// unknownIdentity reports no identity, for recipes whose output does not name the answering model.
func unknownIdentity(_, _ []byte, _ model.Call) string { return "" }

func ollamaRecipe() Recipe {
	return Recipe{
		Name: "ollama", Detect: "ollama", Identity: core.IdentitySelfReport,
		Egress: Egress{Local: true, Destination: "this machine"},
		// The local runtime invocation tag is the identity signal (nothing leaves the box).
		Evidence: core.EvidenceInvocationTag,
		// --nowordwrap is required: without it `ollama run` emits cursor-control escapes into
		// piped stdout, corrupting the JSON response (verified against ollama 0.31.1).
		BuildArgs:     func(c model.Call) []string { return []string{"run", "--nowordwrap", string(c.ModelArg), c.Prompt} },
		ParseIdentity: selfReportTag, // returns the invocation tag
		// Locally installed model tags via the local runtime (no network, no tokens).
		Discovery: &Discovery{Args: []string{"list"}, Kind: "local", Parse: parseOllamaList},
	}
}

func codexRecipe() Recipe {
	return Recipe{
		Name: "codex-cli", Detect: "codex", Identity: core.IdentityAmbient,
		Egress: Egress{Destination: "OpenAI"},
		// No identity evidence: the stderr banner's `model:` line echoes the requested argument even
		// for a nonexistent model, so it cannot detect substitution. Do not add a parser unless the
		// banner is shown to reflect the served model.
		Evidence: core.EvidenceNone,
		BuildArgs: func(c model.Call) []string {
			// A read-only sandbox, with reasoning effort added only when set.
			// --skip-git-repo-check lets Codex run in the fresh non-repository directories callers
			// use; without it Codex exits with no output.
			args := []string{"exec", "-s", "read-only", "--skip-git-repo-check", "-m", string(c.ModelArg)}
			if c.Effort != "" {
				args = append(args, "-c", "model_reasoning_effort="+c.Effort)
			}
			// No prompt argument: `codex exec` reads its instructions from stdin.
			return args
		},
		PromptOnStdin: true,
		// Discovery uses the offline bundled catalog, never a networked refresh. A Codex version
		// without the subcommand reports discovery as failed.
		Discovery: &Discovery{Args: []string{"debug", "models", "--bundled"}, Kind: "bundled", Parse: parseCodexBundled},
	}
}

func claudeRecipe() Recipe {
	return Recipe{
		Name: "claude-code", Detect: "claude", Identity: core.IdentityEnvelope,
		Egress: Egress{Destination: "Anthropic"},
		// `--output-format json` emits an envelope whose `modelUsage` object is keyed by the
		// answering model.
		Evidence: core.EvidenceEnvelope,
		BuildArgs: func(c model.Call) []string {
			// `-p` with no prompt argument: claude reads the piped prompt from stdin (the flag's
			// own help calls it "useful for pipes"). See Recipe.PromptOnStdin.
			args := []string{"-p", "--permission-mode", "plan",
				"--disallowedTools", "Edit Write NotebookEdit", "--output-format", "json"}
			// Pin the model when configured, so claude-code uses the requested model rather
			// than the user's externally-configured default. Omitted when ModelArg is empty.
			if c.ModelArg != "" {
				args = append(args, "--model", string(c.ModelArg))
			}
			// Effort is requested, not verified: the CLI falls back to the highest supported
			// level at or below the request without reporting it.
			if c.Effort != "" {
				args = append(args, "--effort", c.Effort)
			}
			if c.CopyRoot != "" {
				args = append(args, "--add-dir", c.CopyRoot)
			}
			return args
		},
		PromptOnStdin: true,
		ParseIdentity: parseClaudeEnvelope,
		ParsePayload:  parseClaudePayload,
	}
}

// parseClaudePayload unwraps the model's response from a claude-code envelope's `result` field. It
// returns nil for output that is not such an envelope, so the caller falls back to raw stdout.
func parseClaudePayload(stdout, _ []byte, _ model.Call) []byte {
	var env struct {
		Result     string                     `json:"result"`
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	}
	// Only treat it as an envelope when the claude wrapper shape is present.
	if json.Unmarshal(stdout, &env) != nil || len(env.ModelUsage) == 0 || env.Result == "" {
		return nil
	}
	return []byte(env.Result)
}

// parseClaudeEnvelope extracts the answering model from a claude-code envelope's `modelUsage` object,
// or "" when absent. The envelope can list an auxiliary model beside the answering one, and on a
// short response the auxiliary model may produce more output tokens. So the requested model is
// returned when it produced output; otherwise the model with the most output tokens is returned, so a
// real substitution surfaces as a mismatch. Matching uses verify.MatchesForAdapter, the verifier's
// own rule.
func parseClaudeEnvelope(stdout, _ []byte, c model.Call) string {
	var env struct {
		ModelUsage map[string]struct {
			OutputTokens int `json:"outputTokens"`
		} `json:"modelUsage"`
	}
	if json.Unmarshal(stdout, &env) != nil || len(env.ModelUsage) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env.ModelUsage))
	for k := range env.ModelUsage {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic tie-break

	// The requested model, when it actually produced output, is the answering identity.
	if req := strings.TrimSpace(string(c.ModelArg)); req != "" {
		for _, k := range keys {
			if env.ModelUsage[k].OutputTokens > 0 && verify.MatchesForAdapter(req, k, "claude-code") {
				return k
			}
		}
	}

	// Otherwise report the dominant model — a real substitution must surface as a mismatch.
	primary := keys[0]
	for _, k := range keys {
		if env.ModelUsage[k].OutputTokens > env.ModelUsage[primary].OutputTokens {
			primary = k
		}
	}
	return primary
}

func devinRecipe() Recipe {
	return Recipe{
		Name: "devin-cli", Detect: "devin", Identity: core.IdentitySelfReport,
		Egress: Egress{Destination: "the Devin CLI gateway", Note: "routes onward to the provider behind the chosen model"},
		// Devin exposes only a weak self-report, so its calls are self_reported or unknown, never
		// verified.
		Evidence: core.EvidenceSelfReport,
		// The slug encodes the tier. Read-only mode is unconfirmed, so containment is the safeguard.
		// Print mode cannot answer the CLI's workspace-trust prompt, and every call runs in a fresh
		// isolated copy that is never trusted, so the check is skipped for this invocation only.
		BuildArgs: func(c model.Call) []string {
			return []string{"--model", string(c.ModelArg), "--respect-workspace-trust", "false", "-p", c.Prompt}
		},
		ParseIdentity: parseDevinSelfReport,
		ParsePayload:  selfReportEnvelopePayload, // strip the identity wrapper before schema parsing
	}
}

// parseDevinSelfReport extracts a weak self-reported model from a Devin response (an identity wrapper
// or a bare object) and renders it with RenderDevinModelArg into the requested slug form, so "Claude
// Opus 4.8" with "Medium" compares to "claude-opus-4-8-medium". It returns "" when there is no
// self-report, and the raw reported model when the display name cannot be rendered.
func parseDevinSelfReport(stdout, _ []byte, _ model.Call) string {
	sr, ok := verify.ParseSelfReportEnvelope(stdout)
	if !ok {
		return ""
	}
	if slug, err := model.RenderDevinModelArg(sr.Model, sr.Effort); err == nil {
		return slug
	}
	return sr.Model
}

// parseAgySelfReport extracts a weak self-reported model from an Agy response (wrapper or bare) and
// joins model + effort into the display-name form the requested `--model` arg uses (e.g. "Gemini 3.1
// Pro" + "High" → "Gemini 3.1 Pro High"), which matchesForAdapter("agy-cli") compares on canonical
// surface form. No self-report → "" → unknown caveat.
func parseAgySelfReport(stdout, _ []byte, _ model.Call) string {
	sr, ok := verify.ParseSelfReportEnvelope(stdout)
	if !ok {
		return ""
	}
	return verify.AgyModelString(sr)
}

// selfReportEnvelopePayload returns a self-report wrapper's inner `result` object for schema parsing,
// or nil for a bare response, so the caller falls back to raw stdout.
func selfReportEnvelopePayload(stdout, _ []byte, _ model.Call) []byte {
	return verify.ExtractWrappedResult(stdout)
}

func agyRecipe() Recipe {
	return Recipe{
		Name: "agy-cli", Detect: "agy", Identity: core.IdentitySelfReport,
		Egress: Egress{Destination: "Google (Antigravity)"},
		// Weak self-report ceiling: CapEvidence limits calls to self_reported or unknown.
		Evidence: core.EvidenceSelfReport,
		// agy has no approval-mode flag, so read-only behavior relies on the isolated copy.
		BuildArgs: func(c model.Call) []string {
			return []string{"--model", string(c.ModelArg), "-p", c.Prompt}
		},
		ParseIdentity: parseAgySelfReport,        // weak self-report (wrapper or bare) → display-name form
		ParsePayload:  selfReportEnvelopePayload, // strip the identity wrapper before schema parsing
		// `agy models` lists the display names `--model` accepts (variant suffixes like
		// "(High)"/"(Thinking)" encode the reasoning tier — name-bound, no separate effort).
		// Best-effort: unparseable/failed output falls back to catalog/manual with the reason.
		Discovery: &Discovery{Args: []string{"models"}, Kind: "best_effort", Parse: parseAgyModels},
	}
}

func geminiRecipe() Recipe {
	return Recipe{
		Name: "gemini-cli", Detect: "gemini", Identity: core.IdentityEnvelope,
		Egress: Egress{Destination: "Google"},
		// The answering model is not reported, so identity stays uncaptured.
		Evidence: core.EvidenceNone,
		// CI=1 is safe because no identity is read from the output, and the response is extracted
		// with a fence- and prose-tolerant reader. `--version` output is identical under CI=1
		// (gemini 0.52.0).
		Env: map[string]string{"CI": "1"},
		BuildArgs: func(c model.Call) []string {
			// --skip-trust: Gemini exits 55 with no output in an untrusted directory, and callers
			// run in fresh temporary directories. It is safe because --approval-mode plan is
			// read-only. No prompt argument: `-p` input is appended to stdin, so the piped prompt
			// is the prompt.
			return []string{"--model", string(c.ModelArg), "--approval-mode", "plan", "--skip-trust"}
		},
		PromptOnStdin: true,
		ParseIdentity: unknownIdentity, // the answering model is not reported in gemini's output
	}
}

// cursorRecipe drives Cursor's `agent` CLI, which routes to many models (Composer, Grok, Opus, GPT-5.x,
// Sonnet, …). modelArg is the cursor model slug (e.g. "composer-2.5", "gpt-5.6-sol-high",
// "claude-opus-4-8-thinking-high"); the reasoning tier is name-bound in the slug, so no separate effort
// flag. `agent` does not report the answering model in its output, so identity is uncaptured
// (Evidence none → unverified/pass-with-caveat) until a stronger signal is available.
func cursorRecipe() Recipe {
	return Recipe{
		Name: "cursor-cli", Detect: "agent", Identity: core.IdentitySelfReport,
		Egress:   Egress{Destination: "Cursor", Note: "routes onward to the provider behind the chosen Cursor model slug"},
		Evidence: core.EvidenceNone,
		// CI=1 is safe for the same reason as gemini-cli, and --output-format json pins the output
		// shape. `--version` output is identical under CI=1 (agent 2026.07.23).
		Env: map[string]string{"CI": "1"},
		BuildArgs: func(c model.Call) []string {
			// --print: non-interactive; --mode plan: read-only; --trust: skip the workspace-trust
			// prompt; --output-format json: the response in `result`; --workspace: scope the agent to
			// the isolated copy.
			args := []string{"--print", "--mode", "plan", "--trust", "--output-format", "json", "--model", string(c.ModelArg)}
			if c.CopyRoot != "" {
				args = append(args, "--workspace", c.CopyRoot)
			}
			return append(args, c.Prompt)
		},
		ParseIdentity: unknownIdentity,
		ParsePayload:  parseCursorResult, // unwrap the `result` string from cursor's JSON envelope
	}
}

// parseCursorResult extracts the response from a Cursor `agent --output-format json` envelope's
// `result` field. It returns nil for other output or a reported error, so the caller falls back to
// raw stdout.
func parseCursorResult(stdout, _ []byte, _ model.Call) []byte {
	var env struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if json.Unmarshal(stdout, &env) != nil || env.IsError || env.Result == "" {
		return nil
	}
	return []byte(env.Result)
}
