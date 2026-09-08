# Adapters

An **adapter** turns "run this model" into a concrete CLI invocation, and turns the CLI's raw output
back into a result meshcore can trust. Adapters are meshcore's contract, not either app's: `reviewmesh`
and `exploremesh` both select adapters **by name** and never construct argv themselves. Because both
apps build their adapter set from the same meshcore sources (`shell.Registry` over `shell.Recipes()` for
the code-owned CLI recipes, plus `acpagent.Registry` over the user-defined ACP **instances** resolved
from the shared `~/.aimesh/adapters.yaml`), a recipe added to meshcore — or an ACP instance added to that
file — is automatically usable by both with no per-app change.

Adapters live under [`meshcore/model`](../meshcore/README.md): a family of real-CLI adapters driven by
shared shell-execution plumbing (`meshcore/model/shell`), a generic **ACP-client** adapter that drives
any CLI exposing an ACP server over stdio (`meshcore/model/acpagent`), and a deterministic `fake`
adapter (`meshcore/model/fake`) that is a **test-only, hidden, internal harness** — env-gated, never
user-configurable, and never resolvable from user configuration. The shipped `default` profile in each
app is deliberately **unconfigured**: nothing runs until you point it at a real provider CLI.

## The adapter contract

meshcore's `model.Adapter` Go interface (`meshcore/model/model.go`) is deliberately small:

```go
type Adapter interface {
    Name() string
    Available() (ok bool, detail string)
    Invoke(ctx context.Context, c Call) (Result, error)
}
```

Real CLI adapters don't each implement this from scratch — they are **one shell `Adapter` parameterized
by a `Recipe`** (`meshcore/model/shell`). A `Recipe` declares everything provider-specific; an adapter
may optionally also implement `Lister` (on-demand model discovery) and `ArgPreviewer` (render the
effective argv for a pending choice, no model call, no spend). Each app wraps the same recipes behind
its own richer setup/doctor/lane tooling. The invariant throughout: **the adapter is the only thing in
the system that knows a provider's CLI flags** — nothing above it constructs argv or special-cases a
provider.

A `Recipe` (`meshcore/model/shell/shell.go`) captures four or five concerns:

1. **Model invocation** — `BuildArgs(Call) []string` constructs the argv (excluding the binary) from
   the call's `ModelArg`, `Effort`, `Prompt`, and contained-workspace `CopyRoot`.
2. **Read-only vs. write.** Today every recipe's `BuildArgs` is **role-invariant** — reviewer and host
   lanes build the same (read-only-leaning) args. Reviewer safety rests on the CLI's own read-only mode
   where one exists, and on the **containment backstop** everywhere (models operate on a disposable
   workspace copy; any mutation is caught as M5). The live workspace is only ever written by the app's
   host manager, which commits accepted edits itself — never by a model. (A write-enabled host argv is a
   future addition; today host remediation is a deterministic engine, not a model edit.)
3. **Thinking / reasoning effort.** Maps to the CLI's native control in one of two families: *separable*
   (its own flag — Codex `-c model_reasoning_effort`, Claude `--effort`) or *name-bound* (the tier is
   encoded in the model argument itself — Devin's combined slug, Antigravity's `(High)` suffix). Some
   CLIs (Ollama) have no effort knob at all; the recipe emits nothing.
4. **Model identity.** `ParseIdentity` extracts the model that actually answered; `Recipe.Evidence`
   declares the tier that extraction *produces*, which [`meshcore/verify`](model-identity.md) uses to
   classify. `ParsePayload` optionally unwraps a semantic result from a provider envelope.
5. **Execution discipline** — shared by every adapter (below).

## Common execution discipline

- **stdout/stderr** are both fully captured; the result is parsed from the documented channel.
- **Environment:** a spawned CLI **inherits** the host process environment (so its own credentials,
  credential helpers, `HOME` and `PATH` keep working) with non-interactive hints **appended last** —
  `os/exec` resolves a duplicate key to the last value, so a hint overrides an inherited one without
  disturbing authentication. Only `NO_UPDATE_NOTIFIER=1` is applied universally: it is npm-specific and
  identity-inert. Anything stronger is **per recipe** (`Recipe.Env`), because "non-interactive" hints
  are not universally safe — `CI=1` in particular reshapes or suppresses some CLIs' stderr chrome, which
  is where some recipes read the identity they RECORD. Today `CI=1` is set only for `gemini-cli` and
  `cursor-cli`, whose recipes extract no identity from the CLI's own output at all. (`codex-cli` now
  also extracts none, so it has become eligible — but adding it is a behavior change that has not been
  exercised against the real CLI, so it stays unset until it is.) An adapter never builds its
  environment from scratch.
- **Timeout/cancellation:** each call runs under a context bounded by the per-call timeout. On timeout
  or host cancellation the adapter terminates the process group (signal, then force-kill after a grace
  period) and returns a Class F result (one retry).
- **Malformed output:** non-JSON or schema-invalid output on a zero exit is Class G (retry once, then
  halt per policy); partial output is preserved.
- **Identity:** an identity mismatch or an unexpectedly empty identity from a strong-evidence adapter is
  Class E (no silent fallback). See [architecture.md → Halt taxonomy](architecture.md#halt-taxonomy).

## First-run preflight — run each CLI once, yourself

**Before your first governed run, run every adapter's CLI once interactively, in a terminal, and answer
whatever it asks.** This is a real prerequisite, not a nicety: a provider CLI's first launch is
interactive by design, and an adapter deliberately **never answers a prompt on your behalf**.

Three prompt families block a first run, and all three look the same from the outside — a call that
hangs until it times out, or exits non-zero having produced no output:

| Family | What you see when you run the CLI yourself | Typical trigger |
|---|---|---|
| **Folder / workspace trust** | "Do you trust the files in this folder?" | the CLI has never been run in this directory (or in *any* directory, for a global trust store) |
| **Login / authentication** | a browser handoff, a device code, an API-key prompt | no credentials cached yet, or an expired session |
| **Update / onboarding nag** | "A new version is available — update now?", a theme/telemetry picker | first launch after install or upgrade |

**Non-interactive mode suppresses these prompts only *after* the first interactive run.** The flags and
hints the recipes pass — `--skip-trust` (gemini), `--trust` (cursor), `--skip-git-repo-check` (codex),
`NO_UPDATE_NOTIFIER=1`, `CI=1` where it is safe — suppress the *asking*. They do not create the state
being asked about: they cannot mint credentials, accept an onboarding flow, or record a trust decision.
A CLI that has never been run by you can therefore still block or refuse under those flags. Installing
the binary is *not* the same as provisioning it.

**`doctor --probe` surfaces this without spending anything.** Both apps' `doctor` reports binary
availability by default; `--probe` adds a safe, bounded, no-model readiness probe per required adapter
(a `<bin> --version` for shell recipes, an `initialize` → `session/new` handshake for ACP instances). It
never authenticates and never answers a prompt — so a probe that **times out**, or whose captured output
matches a known trust/login signature, is reported as exactly that. Note the converse honestly: a
version probe cannot exercise auth, trust or the model, so a **passing probe does not prove a real call
will not block**. It catches the common case early; running the CLI yourself is what settles it.

```bash
reviewmesh  doctor --probe        # or --probe --json
aimesh explore doctor --probe        # same shared meshcore checks
```

**`doctor --probe-deep` settles it, and it spends.** The gap above is not hypothetical: `gemini-cli`
passed `--version` and then failed **every** real invocation — exit 55, no output at all — until
`--skip-trust` reached its recipe. Only a real-token campaign found that, because `--version` runs with
no working directory and every real call runs in a **fresh isolated directory the CLI has never seen**.

`--probe-deep` therefore performs **one bounded REAL invocation per required adapter**, in a throwaway,
git-initialized directory holding a file — the shape a run actually gets — through the same `Invoke`
path a run takes, with the model argument the roster resolves. The outcome is classified through
`meshcore/clihint` and reported as a `probe-deep:` check (`deep: true` in `--json`).

Three rules hold it honest:

- **It spends.** It is a separate flag, never a stronger `--probe`: a flag documented as free must not
  start costing money because a better probe was invented. `--probe-deep` implies `--probe`, so an
  unresolvable binary is still named before anything is spent finding that out the expensive way.
- **It never auto-answers a prompt.** The child gets no stdin and no flag the recipe does not already
  carry. A CLI blocked on trust or login is *detected, classified and reported with the fix you
  perform*. Answering on your behalf would grant exactly the consent the prompt exists to collect.
- **Exit 0 is not an answer.** A CLI that exits clean having written nothing is reported as failing.

```bash
reviewmesh  doctor --probe-deep          # SPENDS: one real call per required adapter
aimesh explore doctor --probe-deep --json   # `probes[].deep: true` marks the rows that spent
```

On the **MCP** surface the deep probe is a **launch** flag (`aimesh review mcp --probe-deep`,
`aimesh explore mcp --probe-deep`), not a tool parameter. It runs once, before serving, and each
domain's readiness tool (`review_doctor` / `explore_doctor`) then *reports* those rows. That is
deliberate: those tools carry
`readOnlyHint` and promises to start nothing and spend nothing, and a peer able to trigger a spend by
calling it would make that annotation a lie. The ACP surfaces expose no readiness projection, so there
is nothing there to surface it in.

If a governed run does hit one of these, the failure is reported as a Class A adapter fault (or Class F
on a timeout) with the classified signal, and the CLI hint says the same thing this section does: run
the provider's CLI directly once to finish its setup, then re-run.

### How a blocker is classified — three channels, and what each is worth

A provider's *wording* is the weakest evidence there is: it gets reworded, and our own prompt echo can
contain it (see [`meshcore/clihint`](../meshcore/clihint/clihint.go) for the misclassification that
lesson came from). So classification reads three channels, strongest first:

1. **The status the provider itself reported.** claude-code carries `api_error_status` in its
   `--output-format json` envelope; codex prints `ERROR: {"type":"error","status":<n>,…}`; gemini
   surfaces `code: <n>` from its own error object. All three were captured on 2026-08-12 by forcing a
   bad model slug. A `429` here is [RFC 6585](https://www.rfc-editor.org/rfc/rfc6585), not marketing
   copy, and reviewed source cannot plant it the way it can plant a phrase.
2. **The process exit status** — but only where one was *measured* to mean something.
3. **Prose**, as the fallback. Every pattern requires a refusal verb, an API error type, or a status.

**The exit-status table has exactly one row, and that is the measurement result rather than an
unfinished job.** Forced against the installed CLIs on 2026-08-12 (claude 2.1.227, codex-cli 0.147.0,
gemini 0.54.4, darwin/arm64):

| CLI | untrusted directory | bogus model | unknown flag |
|---|---|---|---|
| `claude` | **0** — it has no folder-trust wall in `-p` | 1 (`api_error_status: 404`) | 1 |
| `codex` | 1 (`Not inside a trusted directory…`) | 1 (`"status":400`) | 2 |
| `gemini` | **55** | 1 (`code: 404`) | 1 |

Only gemini's `55` names a cause on its own. Everything else exits `1`, which each CLI also uses for a
mistyped flag — a status meaning "something went wrong" classifies nothing, and mapping it would
manufacture confidence. So `55 → folder_trust` is the whole table, and a row is added only from a
measurement recorded with its date and CLI version. Login was not measured (it would mean logging the
user out) and quota is not forceable on demand; both are captured when a real run produces one.

Worth noting from the same session: `/tmp` is untrusted for **both** codex and gemini, so anything
measured there measures the trust wall rather than the blocker you meant to force.

## Status legend

Each recipe below is tagged with how confidently its fields were captured:

| Tag | Meaning |
|---|---|
| **verified-local** | confirmed against the installed CLI in a real environment |
| **spec-derived** | taken from documented CLI behavior; plausible, not freshly captured |
| **needs-authenticated-capture** | must still be confirmed by running the authenticated CLI once; the adapter ships buildable and fake-binary-tested regardless |

A field marked `needs-authenticated-capture` never blocks building or shipping the adapter — only its
real-CLI confirmation. See [model-identity.md](model-identity.md#manual-identity-capture-workflow).

> Each recipe below lists its **evidence tier** — the identity signal it currently *produces* (what
> `meshcore/verify` classifies). A recipe also declares a coarser *target* `Identity` method in code,
> which can differ. A tier here describes what an adapter can prove about which model answered; it does
> **not** gate anything — see [model-identity.md](model-identity.md), and run the **echo test** there
> before declaring any tier above `none`.

## Per-adapter recipes

### claude-code — verified-local

- **Binary:** `claude`, detected on `PATH` or a configured path.
- **Invoke argv:**
  ```
  claude -p <prompt> --permission-mode plan --disallowedTools "Edit Write NotebookEdit" --output-format json [--model <modelArg>] [--effort <effort>] [--add-dir <copyRoot>]
  ```
  `--model`, `--effort`, and `--add-dir` are appended only when set. Pinning `--model` makes claude-code
  use the requested model rather than the user's externally-configured default.
- **Artifact delivery:** `reads_from_dir`, via `--add-dir <copyRoot>`.
- **Evidence tier:** `envelope` — the strongest tier. `--output-format json` emits a `modelUsage` object
  keyed by the model(s) used; the adapter selects the **primary model by max output tokens** (a real
  review envelope can list a tiny helper model alongside the primary). Matching is Anthropic-alias-aware
  (`opus`/`claude-opus-4-8` matches `claude-opus-4-8[1m]`; a different family does not, and mismatches
  halt Class E).
- **Thinking:** separable — `--effort`. The CLI may silently downgrade to the highest level it supports
  and does not report the effective level, so effort is recorded as **requested/unverified** even though
  *model identity* is fully verified from the envelope.
- **Status:** binary + reviewer argv + identity verified-local (a real review smoke confirmed the full
  path: identity `verified`, payload unwrapped). Auth-state checking and the exact effort behavior remain
  needs-authenticated-capture.

### codex-cli — verified-local

- **Binary:** `codex`, detected on `PATH` or a configured path.
- **Invoke argv:**
  ```
  codex exec -s read-only --skip-git-repo-check -m <modelArg> [-c model_reasoning_effort=<low|medium|high>] <prompt>
  ```
  The effort override is emitted only when effort is set. `--skip-git-repo-check` is required: Codex
  refuses to run in a non-git directory, and calls legitimately run in one (exploremesh contains each
  call in a fresh empty temp cwd). Harmless inside a repo, so unconditional.
- **Model availability is account-dependent.** The same slug can work on one auth type and be rejected
  on another — e.g. `gpt-5-codex` is accepted with an API-key account but a **ChatGPT-account** Codex
  rejects it at invocation with HTTP 400 ("not supported when using Codex with a ChatGPT account").
  Pick a slug your account accepts from `codex debug models --bundled`; a wrong one fails at run time,
  not at configuration time.
- **Artifact delivery:** `reads_from_dir` (Codex's own sandbox reads the workspace).
- **Evidence tier:** `none` — **measured, not assumed.** `codex exec` prints a startup status banner to
  stderr including a `model: <slug>` line, and this adapter once parsed it as `cli_status` evidence. An
  echo test against the real CLI disproved that: invoked with `--model totally-not-a-real-model-9x`, the
  banner prints that name back verbatim *before* the API rejects it with a 400. The line echoes the
  argument we passed, so it can only ever agree with the request — it could never have detected the
  substitution it existed to detect. The recipe therefore declares `EvidenceNone` with no
  `ParseIdentity`, and every Codex call records an unknown identity, which is the honest answer.
  **Do not restore the parser without an echo test showing the banner reflects the SERVED model.**
- **Thinking:** separable — `-c model_reasoning_effort=<effort>`.
- **Discovery:** `codex debug models --bundled` (the offline bundled catalog).
- **Status:** binary + argv verified-local by real reviews; identity measured as an echo (above). The
  exact auth-status subcommand remains needs-authenticated-capture.

### agy-cli (Antigravity) — verified-local

- **Binary:** `agy`, detected on `PATH` or a configured path.
- **Invoke argv:** `agy --model <modelArg> -p <prompt>`. There is **no `--approval-mode` flag** on this
  CLI (it offers `--sandbox` / `--dangerously-skip-permissions`) — do not pass one.
- **Read-only:** no read-only-tools mode exists, so reviewer read-only relies on the containment backstop.
- **Thinking:** name-bound — the tier is encoded in the model argument, e.g. `"Gemini 3.1 Pro (High)"`.
- **Evidence tier:** `self_report` — a weak, inline JSON self-report wrapper. A matching report is
  `self_reported` (never `verified`); a non-matching/missing/refused one is `unknown` (pass-with-caveat),
  never Class E.
- **Discovery:** `agy models` (best-effort — the display names `--model` accepts).
- **Status:** binary, argv, and self-report identity verified-local; auth-status remains
  needs-authenticated-capture.

### ollama — verified-local (the real local-only path)

- **Binary:** `ollama`, detected on `PATH`.
- **Invoke argv:** `ollama run --nowordwrap <modelArg> <prompt>`. `--nowordwrap` is required: without it `ollama run` injects terminal cursor-control escapes into its piped stdout (word-wrapping), which corrupts the JSON we parse.
- **Artifact delivery:** `inline_content` — Ollama does not read workspace files, so the adapter inlines
  the target into the prompt.
- **Read-only:** Ollama only emits text, so it is inherently read-only for reviewer use (it can still be
  the *adjudicating* host, since adjudication is reasoning, not writing).
- **Thinking:** unsupported — no effort knob.
- **Evidence tier:** `invocation_tag` — the local run tag (e.g. `qwen2.5-coder:14b`) *is* the identity
  signal, stronger than a self-report and used directly. Nothing leaves the machine.
- **Discovery:** `ollama list` (locally installed model tags).
- **Status:** verified-local, including a full local run with exit code, output, and invocation-tag
  identity confirmed.

### devin-cli (Devin gateway) — needs-authenticated-capture

Devin is treated strictly as an **adapter/gateway profile**: one CLI fans out to several providers behind
name-bound, tier-encoded model slugs.

- **Binary:** `devin` (`devin.exe` on Windows), detected on `PATH` or a configured path.
- **Invoke argv:** `devin --model <modelArg> -p <prompt>`.
- **modelArg rendering:** configure `model` as the **full display name** from Devin's model selector (e.g.
  `Claude Opus 4.8`) with the reasoning tier in a separate `effort` field. `RenderDevinModelArg` renders
  the final `--model` slug (lowercase; spaces/periods → hyphens; collapse/trim; append normalized effort).
  A partial name without a provider prefix is rejected. Examples:
  - `Claude Opus 4.8` + `Medium` → `claude-opus-4-8-medium`
  - `Gemini 3.1 Pro` + `High Thinking` → `gemini-3-1-pro-high`
- **Read-only:** Devin's read-only mode is **unconfirmed**, so every reviewer call runs under containment
  (disposable copy; any mutation halts M5) as a non-negotiable default.
- **Thinking:** name-bound, in the rendered slug — no separate effort flag.
- **Evidence tier:** `self_report` — weak inline self-report, wrapped like agy's. A match is
  `self_reported`, never `verified`; missing/refused/non-matching is `unknown`. Class E is reserved for a
  proven strong-evidence mismatch.
- **Status:** binary detection verified-local; argv, auth status, and artifact delivery remain
  needs-authenticated-capture.

### gemini-cli — verified-local (identity not captured)

- **Binary:** `gemini`, detected on `PATH` or a configured path.
- **Invoke argv:** `gemini --model <modelArg> --approval-mode plan --skip-trust -p <prompt>`.
- **`--skip-trust` is REQUIRED, not optional.** Without it Gemini refuses to answer in any directory it
  has not been told to trust, exiting **55** with no output. Callers legitimately run in untrusted
  directories — exploremesh contains each call in a fresh empty temp cwd, which is never trusted — so
  the adapter could not succeed at all before this flag was added. Safe because `--approval-mode plan`
  already confines the model to read-only. (`GEMINI_CLI_TRUST_WORKSPACE=true` is the env equivalent.)
- **Evidence tier:** `none` — the recipe *targets* an `envelope` identity method, but gemini does not
  report which model answered, so no extraction is possible (`ParseIdentity` returns `""` → the verifier
  treats a gemini call as unverified/pass-with-caveat). It emits **no effort flag**.
- **Status:** verified running against gemini 0.52.0 — a real exploration call exited 0 with a
  schema-valid response. It is NOT spec-only; its identity simply stays uncaptured, which is a
  caveat rather than a blocker.

### cursor-cli (Cursor `agent`) — verified-local

Cursor's `agent` CLI routes to many models (Composer, Grok, Opus, GPT-5.x, Sonnet, …) through one account.

- **Binary:** `agent`, detected on `PATH` or a configured path.
- **Invoke argv:** `agent --print --mode plan --trust --output-format json --model <slug> [--workspace <copyRoot>] <prompt>`. `--print` = non-interactive; `--mode plan` = read-only (analyze, no edits); `--trust` skips the workspace-trust prompt; `--output-format json` wraps the response in an envelope; `--workspace` scopes the agent to the isolated copy.
- **modelArg:** a Cursor model slug from `agent models` — e.g. `composer-2.5`, `cursor-grok-4.5-high`, `claude-opus-4-8-thinking-high`, `gpt-5.6-sol-high`. The reasoning tier is **name-bound** in the slug (no separate effort flag).
- **Evidence tier:** `none` — `agent`'s JSON envelope carries no answering-model field, so the requested model can't be verified; a cursor lane is classified `unknown` (pass-with-caveat). The response is unwrapped from the envelope's `result` field.
- **Auth:** `CURSOR_API_KEY` env or `agent login`.
- **Status:** verified-local — a real review with `composer-2.5` completed end-to-end (found the planted bug; identity recorded as an unverified caveat, as expected).
- **Prefer an ACP instance for Cursor** (below) when you want a **verified** Cursor identity: over ACP the session reports its active model, which this one-shot `--print` envelope cannot.

## ACP-client adapters

A second adapter family drives any CLI that exposes an **[ACP](https://agentclientprotocol.com/) (Agent
Client Protocol)** server over stdio, as a *client*, reusing meshcore's own `acp` transport
(`meshcore/model/acpagent`). One generic code path backs every ACP CLI (no per-CLI parser).

**There is no fixed ACP catalog — and no `acpagent.Recipes()`.** ACP standardizes the *protocol*, not
how you start the server (each CLI picks its own flag: `--acp`, `acp`, `--acp --stdio`), and the set of
ACP-capable CLIs is open-ended. So an ACP adapter is **user configuration, not code**: each instance is
an entry under `acpAdapters` in the shared `~/.aimesh/adapters.yaml`, e.g.

```yaml
acpAdapters:
  acp-gemini:
    title: "ACP: Gemini"
    path: /path/to/gemini
    args: ["--acp"]
    model: gemini-3.5-flash   # captured at validation; the expected model for identity verification
```

Add one from the **CLI** — `aimesh explore setup --acp add --path <bin> [--acp-arg <a>] [--title <t>]` or
`aimesh review setup --acp add --path <bin> [--acp-arg <a>]` (both also support `--acp detect` to probe first
and `--acp remove --name <k>`) — or by editing that file directly:
enter the binary path,
optionally press *Detect* (probes `--help`, then confirms with a real `initialize` + `session/new`
handshake, filling in the launch args, a suggested title, and the model the session reports), then
*Save*. Both apps resolve `acpAdapters` exactly like a shell recipe — the generic driver is
parameterized per instance, so any ACP-capable CLI works with no code change.

**Per-call flow.** Spawn `<bin> <acp-args>`; `initialize` (advertising *no* filesystem/terminal
capability); `session/new`; switch the session to a **read-only** mode (`ask` preferred over `plan` — a
pure Q&A responder, not a codebase-searching agent); optionally `session/set_model` to the requested
model; `session/prompt`; aggregate the streamed `agent_message_chunk`s (thought chunks ignored). The
task content is always **inline in the prompt**, and with no contained copy the session runs in a fresh
**empty** working directory — an agentic CLI has no workspace to wander into.

**Evidence tier: `cli_status`.** An ACP session reports its active model over the protocol
(`session/new` → `models.currentModelId`) — a value the agent supplies rather than one we passed in, so
unlike an argument echo it can disagree with the request. The adapter normalizes it to a base slug and
classifies at the `cli_status` tier: **verified** on a match, **mismatch** on a proven different model.
An agent that reports no model (e.g. `devin-acp`) self-downgrades that call to `none` → `unknown`,
never a false `verified`. The adapter's evidence **ceiling** is `cli_status` (`Evidence()`), so
`verify.CapEvidence` still fail-closes exactly as for shell recipes. As everywhere, the classification
is recorded and reported; it never decides whether the response is used.

**Payload.** Agentic CLIs narrate around their answer ("Searching the codebase…") and may fence it; the
adapter surfaces the embedded JSON object as `Result.Payload` (fence/prose-tolerant) while keeping the
full turn in `Stdout` for audit. Consumers parse `Payload` when present, else `Stdout`.

**Fit.** Best where a CLI speaks ACP and you want verified identity or read-only sessions. Because ask
mode makes the CLI a chat responder over inline content, it suits reviewmesh (analyze an artifact) and
exploremesh (answer a task) alike — though a strict multi-field consumer (exploremesh's explorer/collator
schema) still depends on the underlying model conforming, which agentic models do inconsistently; a
non-conforming response is honestly dropped, never silently accepted.

- **Status:** verified-local — `cursor-acp` completed a real aimesh review run with `composer-2.5`
  **`verified`** across all lanes, and a real exploremesh exploration (two blind explorers →
  synthesis) with verified identity throughout; `devin-acp` completed a review with an honest
  `unknown`-identity caveat. `gemini-acp`'s `session/new` is account-gated (Gemini Code Assist
  individual → Antigravity) on the capture machine; `copilot-acp`/`opencode-acp`/`qwen-acp` are
  spec-derived (their CLIs were not installed to capture).

### The self-report wrapper (shared mechanism)

`agy-cli` and `devin-cli` (weak-identity CLIs) obtain identity through a **self-report wrapper**: the
review/adjudication prompt asks the model to wrap its schema-valid answer as

```json
{ "reviewmeshIdentity": {"model": "...", "effort": "...", "source": "self_report"}, "result": <the app's own result schema> }
```

The adapter's `ParsePayload` strips the wrapper so the app's strict, unknown-fields-rejecting parser only
ever sees `result`; `ParseIdentity` normalizes the reported model for comparison. This is **one call per
lane — no hidden probe or extra spend**. `verify.CapEvidence` clamps these adapters to
`self_reported`/`unknown`, so a self-report can never be classified `verified`.

> **Note:** a general user-configurable `generic-shell` adapter (an argv template for CLIs meshcore has
> no recipe for) is **design-only — not implemented**. There is no `generic-shell` recipe and no
> corresponding `config.schema.json` shape today. The self-report *wrapper mechanism* above is real (it
> backs agy/devin); a standalone generic-shell adapter is future work.

## Adding an adapter

Supporting a new provider CLI is the most common extension to meshcore, and for a straightforward CLI it
is a **single new `Recipe`** — no new package, no new architectural layer. Because both apps discover
adapters from `shell.Recipes()`, adding the recipe makes it selectable in reviewmesh profiles and
exploremesh rosters automatically.

1. **Add a `Recipe`** to `Recipes()` in `meshcore/model/shell/recipes.go`, keyed by the adapter name
   (e.g. `"opencode"`). Fill in:
   - `Name` (adapter name) and `Detect` (the binary name to look up on `PATH`);
   - `BuildArgs(model.Call) []string` — construct the argv from `c.ModelArg` / `c.Effort` / `c.Prompt`
     (and `c.CopyRoot` if the CLI reads workspace files). Keep reviewer calls read-only where the CLI
     supports it; containment is the backstop regardless.
   - `Identity` (the target `core.IdentityMethod`) and `Evidence` (the tier your `ParseIdentity`
     actually produces — set `EvidenceNone` until a real extraction is captured, so the verifier treats
     it as unverified rather than trusting an uncaptured signal);
   - `ParseIdentity(stdout, stderr, Call) string` — return the model that actually answered ("" if you
     can't tell); optionally `ParsePayload` (unwrap a semantic result from an envelope) and `Discovery`
     (a model-listing mechanism for the `Lister` capability).
2. **Extract identity at the strongest tier the CLI exposes** — prefer an envelope/trace over a
   self-report; see the [evidence ladder](model-identity.md). Never claim a tier you don't produce:
   `CapEvidence` clamps classification to the recipe's declared ceiling, fail-closed.
3. **Write the required tests** — contract tests against a **fake binary/script** covering a normal call,
   a timeout, an identity mismatch, and a non-zero/error exit, plus the emitted result shape. This is the
   existing fake-binary pattern (`testdata/adapters/**`); `go test ./...` runs no real CLI, no network,
   and no spend.
4. **(Optional) catalog/config entries.** The adapter is *usable* the moment its recipe exists (both
   apps discover it, and `doctor` reports its binary availability). Adding `modelCatalog` entries that
   map a canonical model profile to this adapter's model argument just makes it nicer to select in the
   UI/wizard — see [configuration.md](configuration.md).
5. **Verify:** `make gate` (build/test/boundary/golden-run) stays green, and `aimesh review doctor` /
   `aimesh explore doctor --roster <names it>` both list the new adapter's availability.

**Adding an ACP CLI instead.** If the CLI exposes an ACP server over stdio, don't write a shell recipe —
and don't write any code at all. Add an **instance** under `acpAdapters` in the shared
`~/.aimesh/adapters.yaml` (`{title, path, args, model}`) — via `aimesh explore setup --acp add` /
`aimesh review setup --acp add`, or by hand
(path → *Detect* → *Save*; Detect probes `--help` then confirms with a real handshake). The generic driver
handles the handshake, read-only mode, model selection, streaming, identity, and payload extraction — so
there is no per-CLI parser. Both apps discover it exactly like a shell recipe; add `adapters`/`modelCatalog`
seed entries the same way (see [configuration.md](configuration.md)). Tests use the in-package fake ACP
server (a re-exec helper process) — no real CLI or spend.

For a precise, do-this-then-that runbook (including for an AI coding agent), see
**[AGENTS.md → Adding an adapter](../AGENTS.md#task-adding-a-model-adapter)**.

See [../meshcore/README.md](../meshcore/README.md) for where adapters fit into meshcore, and
[model-identity.md](model-identity.md) for how a new adapter's identity signal is classified.
