package shell

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/verify"
)

// Recipes returns the per-CLI recipes documented in docs/adapters.md. The argv
// shapes and identity parsing reflect the documented best-known behavior; fields
// marked verify-on-provision there are approximated here and confirmed against the
// authenticated CLI before being trusted (ParseIdentity returns "" where the exact
// extraction is uncaptured, so the ModelVerifier treats it as unverified).
//
// PER-RECIPE ENVIRONMENT (Recipe.Env). Only NO_UPDATE_NOTIFIER is universal (model.HardenedEnv);
// `CI=1` is opt-in per recipe, and the rule applied here is narrow on purpose: it is set ONLY where
// the recipe reads no identity out of the CLI's own output, so CI mode cannot cost the lane the
// identity it RECORDS. That is `gemini-cli`, `cursor-cli` and `codex-cli` (all EvidenceNone).
// It is deliberately NOT set for:
//   - claude-code — identity is the `--output-format json` `modelUsage` envelope. Whether that
//     envelope survives CI mode cannot be established without a real authenticated call, and the
//     downside of being wrong is losing the one genuinely provider-sourced identity signal any
//     adapter has. Unverified, so unset.
//   - agy-cli / devin-cli — their ONLY identity is a weak self-report parsed out of stdout; a change
//     in how the CLI frames that stdout would silently degrade a self_reported lane to unknown.
//     Unverified, so unset.
//   - ollama — a local Go runtime, not an npm CLI: CI has no defined effect on it, and its output
//     shaping is already handled by `--nowordwrap`. Setting it would be cargo-cult environment.
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

// EgressFor reports where a named adapter's content goes, and whether that is KNOWN.
//
// ok is false for anything this package does not own — a user-defined ACP instance, the internal
// test harness — and that is a real answer rather than a gap: an ACP instance names a binary the
// OPERATOR chose, so the destination is whatever they pointed it at, and this package would be
// guessing if it said otherwise. A caller must render an unknown destination as unknown; treating it
// as local is the one reading that would be dangerous.
func EgressFor(adapter string) (Egress, bool) {
	r, ok := Recipes()[adapter]
	if !ok || (r.Egress.Destination == "" && !r.Egress.Local) {
		return Egress{}, false
	}
	return r.Egress, true
}

// selfReportTag is the local-tag identity for adapters whose model invocation tag
// is the strongest available signal (e.g. Ollama). True model self-report parsing
// is verify-on-provision.
func selfReportTag(_, _ []byte, c model.Call) string { return string(c.ModelArg) }

// unknownIdentity is used where the exact envelope/trace extraction is not yet
// captured; "" → the verifier treats the call as unverified (verify-on-provision).
func unknownIdentity(_, _ []byte, _ model.Call) string { return "" }

func ollamaRecipe() Recipe {
	return Recipe{
		Name: "ollama", Detect: "ollama", Identity: core.IdentitySelfReport,
		Egress: Egress{Local: true, Destination: "this machine"},
		// The local runtime invocation tag is the identity signal (nothing leaves the box).
		Evidence: core.EvidenceInvocationTag,
		// --nowordwrap is REQUIRED: without it, `ollama run` word-wraps its streamed output using
		// terminal cursor-control escapes (e.g. \x1b[8D\x1b[K) that it emits into stdout even when
		// stdout is a pipe, corrupting the JSON we parse (a real review then fails Class G). With
		// --nowordwrap the piped output is clean JSON. (Verified against ollama 0.31.1.)
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
		// NO IDENTITY EVIDENCE — measured, not assumed.
		//
		// Codex prints a startup status banner to stderr carrying a `model: <slug>` line, and this recipe
		// once parsed it as cli_status/trace evidence. An ECHO TEST disproved that: invoked with
		// `--model totally-not-a-real-model-9x`, the banner prints
		//
		//     model: totally-not-a-real-model-9x
		//
		// verbatim — BEFORE the API rejects the model with a 400. The banner echoes the argument we passed,
		// so it can only ever "confirm" what we already asked for. As evidence it is circular: it could
		// never disagree with the request, which means it could never detect the substitution it existed to
		// detect. (Compare claude-code, whose `modelUsage` key comes back from the provider and named
		// `claude-haiku-4-5` unprompted.)
		//
		// It is therefore declared EvidenceNone with no ParseIdentity: every codex call records an unknown
		// identity, which is the honest answer. That costs nothing — identity is recorded and never gates a
		// response (see ../../../docs/model-identity.md). Do not restore the parser without an echo test
		// showing the banner reflects the SERVED model rather than the requested one.
		Evidence: core.EvidenceNone,
		BuildArgs: func(c model.Call) []string {
			// read-only reviewer sandbox + SEPARABLE reasoning effort. The `-c
			// model_reasoning_effort=<low|medium|high>` config override is the documented
			// shape; the EXACT flag form is verify-on-provision. Effort is added only when set.
			// `--skip-git-repo-check`: Codex refuses to run in a non-git directory without it, and
			// callers legitimately run in one — exploremesh contains each call in a FRESH empty temp
			// cwd (WorkDir), which is not a repo. Without this flag Codex exits before producing any
			// output (an empty response, dropped downstream). Harmless inside a repo, so unconditional.
			args := []string{"exec", "-s", "read-only", "--skip-git-repo-check", "-m", string(c.ModelArg)}
			if c.Effort != "" {
				args = append(args, "-c", "model_reasoning_effort="+c.Effort)
			}
			// No prompt argument: `codex exec` with no PROMPT reads its instructions from stdin
			// (documented in its own --help), which is how a workspace-sized prompt gets past the
			// OS argv ceiling. See Recipe.PromptOnStdin.
			return args
		},
		PromptOnStdin: true,
		// No ParseIdentity: the only channel Codex exposes is the echoed banner (see Evidence above).
		// The OFFLINE bundled CLI catalog (slug + default/supported reasoning levels).
		// Deliberately `--bundled`: a networked `codex debug models` refresh would be a
		// separate, explicit opt-in and is NOT part of this mechanism. Older Codex versions
		// may lack the subcommand — ListModels then reports failed and the projection falls
		// back to catalog/manual with the reason visible.
		Discovery: &Discovery{Args: []string{"debug", "models", "--bundled"}, Kind: "bundled", Parse: parseCodexBundled},
	}
}

func claudeRecipe() Recipe {
	return Recipe{
		Name: "claude-code", Detect: "claude", Identity: core.IdentityEnvelope,
		Egress: Egress{Destination: "Anthropic"},
		// CAPTURED: `--output-format json` emits an envelope whose `modelUsage` object is
		// keyed by the actual model (authoritative). evidence = envelope.
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
			// SEPARABLE requested effort (documented `--effort low|medium|high|xhigh|max`).
			// CAVEAT: the CLI silently falls back to the highest supported level at or below
			// the request and does not report the effective level — reviewmesh therefore
			// treats effort as REQUESTED/configured, never verified (model identity, by
			// contrast, IS verified from the envelope).
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

// parseClaudePayload unwraps the reviewer/adjudication JSON the model produced from a
// claude-code `--output-format json` envelope's `result` (string) field. Returns nil
// when the output is not such an envelope (e.g. a fake/plain reviewer JSON), so the
// Manager falls back to raw stdout.
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

// parseClaudeEnvelope extracts the actual answering model from a claude-code
// `--output-format json` envelope's `modelUsage` object. "" if absent (e.g. a non-envelope / fake
// output → the verifier treats the call as having no evidence).
//
// The envelope routinely lists MULTIPLE models: Claude Code runs an AUXILIARY model (a small
// `claude-haiku-*`) for its own internal bookkeeping alongside the model that answers. So choosing the
// identity needs care in BOTH directions:
//
//   - If the REQUESTED model is present with real output, it answered — return it. A most-output-tokens
//     heuristic is wrong here: on a SHORT response the auxiliary model can out-token the real one
//     (measured against Claude Code 2.1.219: a one-word answer to `--model opus` reported haiku at 12
//     output tokens vs claude-opus-5 at 4), which would name haiku as the identity and make the verifier
//     halt a perfectly good run as a Class E mismatch. reviewmesh's synthetic adjudication readiness
//     probe is exactly that short-response case.
//   - If the requested model is ABSENT (or produced nothing), that is the genuine substitution case the
//     no-silent-fallback invariant exists to catch — report the dominant model so the verifier sees the
//     mismatch and halts. Never paper over it by echoing back what was requested.
//
// Matching uses verify.MatchesForAdapter, the SAME rule the verifier applies, so the parser and the
// verifier cannot disagree about whether an alias (`opus`) names an envelope model (`claude-opus-5`).
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
		// Devin exposes no strong metadata; its identity CEILING is a WEAK self-report (never verified).
		// This caps classification: a devin lane can be self_reported or unknown, never verified.
		Evidence: core.EvidenceSelfReport,
		// name-bound tier slug; read-only unconfirmed → reviewer use relies on containment
		BuildArgs:     func(c model.Call) []string { return []string{"--model", string(c.ModelArg), "-p", c.Prompt} },
		ParseIdentity: parseDevinSelfReport,
		ParsePayload:  selfReportEnvelopePayload, // strip the identity wrapper before schema parsing
	}
}

// parseDevinSelfReport extracts a WEAK self-reported model from a Devin response — either the
// review-path WRAPPER `{reviewmeshIdentity:{model,effort}, result:{…}}` or the bare probe object
// `{result, model, effort}` — and normalizes the display name + effort to the requested slug shape via
// RenderDevinModelArg (so "Claude Opus 4.8" + "Medium" is comparable to "claude-opus-4-8-medium").
// Ordinary review output with no self-report yields "" → the shell adapter forces EvidenceNone → an
// unknown caveat (no extra spend). A render error (ambiguous display name) returns the raw reported
// model, so classification records a weak non-match as `unknown` (never a mismatch halt).
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

// parseAgySelfReport extracts a WEAK self-reported model from an Agy response (wrapper or bare) and
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

// selfReportEnvelopePayload strips a self-report WRAPPER down to its inner `result` object for schema
// parsing (the wrapper's identity metadata never reaches the strict DisallowUnknownFields parser); a
// non-wrapper (bare) response returns nil so the Manager falls back to raw stdout.
func selfReportEnvelopePayload(stdout, _ []byte, _ model.Call) []byte {
	return verify.ExtractWrappedResult(stdout)
}

func agyRecipe() Recipe {
	return Recipe{
		Name: "agy-cli", Detect: "agy", Identity: core.IdentitySelfReport,
		Egress: Egress{Destination: "Google (Antigravity)"},
		// Weak self-report CEILING (agy 1.0.16 emits a structured JSON self-report when asked; captured
		// this batch). CapEvidence clamps a lane here to self_reported/unknown — never verified.
		Evidence: core.EvidenceSelfReport,
		// CAPTURED (agy 1.0.13 `--help`): the installed agy has NO `--approval-mode` flag
		// (it offers `--sandbox` / `--dangerously-skip-permissions`). reviewer read-only
		// relies on the isolated-copy containment backstop. `--model <m> -p <prompt>`.
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
		// VERIFIED RUNNING against gemini 0.52.0. The model that answered is still not reported in
		// the output, so identity stays uncaptured (EvidenceNone → unverified/pass-with-caveat)
		// rather than claiming a tier we cannot produce.
		Evidence: core.EvidenceNone,
		// CI=1 is safe HERE, and the reason is structural rather than optimistic: this recipe reads NO
		// identity out of the CLI's own output (ParseIdentity is unknownIdentity / EvidenceNone), so the
		// failure mode that keeps CI off codex-cli — CI suppressing the chrome the identity is parsed
		// from — has nothing to act on. The answer channel is plain `-p` text that the caller extracts
		// with a fence/prose-tolerant JSON reader, so extra or missing chrome is already tolerated.
		// Verified: `--version` (the bounded readiness probe) is byte-identical under CI=1 (gemini 0.52.0).
		Env: map[string]string{"CI": "1"},
		BuildArgs: func(c model.Call) []string {
			// `--skip-trust`: Gemini refuses to answer in a directory it has not been told to trust,
			// exiting 55 with "not running in a trusted directory" and producing NO output — the one
			// provider exit status measured to identify a cause on its own, which is why it is the only
			// row in clihint's exitSignals table (re-measured 2026-08-12, gemini 0.54.4). Callers
			// legitimately run untrusted dirs — exploremesh contains each call in a FRESH empty temp
			// cwd, which is never trusted, so without this the adapter can never succeed. This is the
			// documented headless escape hatch (the alternative is GEMINI_CLI_TRUST_WORKSPACE=true).
			// Safe here because the model is already confined: --approval-mode plan is read-only.
			// No prompt argument: gemini's own `-p` help says it is "appended to input on stdin (if
			// any)", so a piped prompt IS the prompt. See Recipe.PromptOnStdin.
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
		// CI=1 is safe here for the same structural reason as gemini-cli: this recipe extracts NO
		// identity from the CLI's output (unknownIdentity / EvidenceNone), so CI cannot cost the lane
		// an identity signal it never had. The answer channel is pinned by an explicit
		// `--output-format json` flag rather than by terminal detection, and parseCursorResult already
		// returns nil on anything that is not that envelope (the caller then falls back to raw stdout),
		// so a chrome change degrades to the existing fail-soft path instead of breaking parsing.
		// Verified: `--version` is byte-identical under CI=1 (agent 2026.07.23).
		Env: map[string]string{"CI": "1"},
		BuildArgs: func(c model.Call) []string {
			// --print: non-interactive; --mode plan: read-only (analyze, no edits) for reviewer safety;
			// --trust: skip the workspace-trust prompt; --output-format json: a clean envelope whose
			// `result` field holds the response; --workspace: scope the agent to the isolated copy.
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

// parseCursorResult extracts the model's response from a Cursor `agent --output-format json` envelope's
// `result` (string) field. Returns nil when the output is not such an envelope or reports an error, so
// the Manager falls back to raw stdout (and a schema-invalid result still corrective-retries → Class G).
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
