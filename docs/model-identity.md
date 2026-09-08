# Model identity

Requesting a model through an adapter and getting a zero exit code is **not proof** that the model
you asked for is the one that answered. `meshcore/verify` turns "which model actually answered" into
an honest, tiered classification.

**The governing rule: identity is recorded, never acted on.**

> The content of a finding determines whether it is valid. A recorded identity never does. Nothing in
> either app drops, downgrades, relegates, refuses, or de-prioritises a response because of what its
> identity classification says — not a weak one, not an unknown one, not a proven mismatch.

This is a claim about what is *knowable*, not a relaxation of standards. We can **ask** a CLI to use a
model; we cannot make it **prove** it did. Every "verification" available here is really one of:

- a **provider-sourced** signal the CLI passes through (Claude Code's `modelUsage` billing record —
  genuine evidence, because it comes back from the API rather than from our own argument);
- a **local fact** (the tag handed to `ollama run` *is* the model);
- the model's **own claim** about itself, which is worth what any unverifiable self-report is worth; or
- **an echo of the argument we passed in.**

That last category is why the rule is absolute. `codex-cli` was declared at the second-strongest tier
for months on the strength of its startup banner. An echo test — invoking it with
`--model totally-not-a-real-model-9x` — showed the banner printing that name back verbatim before the
API rejected it. The "evidence" could never disagree with the request, so it could never detect the
substitution it existed to detect, while adapters that honestly reported nothing were the ones being
penalised. A gate built on tiers like that doesn't enforce provenance; it enforces whichever adapter
flatters us most convincingly.

**What this means in practice:** every seat participates, every classification is recorded on the run
and surfaced to the reader, and *you* decide what an unexpected model means for your work. If a lane's
identity matters to you, the manifest tells you exactly what each adapter could and could not prove.

**Before declaring any adapter's evidence tier, run the echo test:** invoke it asking for a model that
cannot exist. If it reports that name back, the channel is an echo — declare `EvidenceNone`. If it
errors, or names something else, the channel is connected to reality.

See also [adapters.md](adapters.md) for how each adapter extracts its identity signal, and
[../meshcore/README.md](../meshcore/README.md) for where `verify` sits in the rest of meshcore.

## The evidence-tier ladder

Every call records an **identity-evidence tier** — the kind of signal behind the model name an
adapter reports, ranked strongest to weakest:

```
envelope > trace > cli_status > invocation_tag > self_report > none
```

| Evidence | Strength | Meaning |
|---|---|---|
| `envelope` | strongest | a structured JSON/usage envelope emitted by the CLI or provider (e.g. Claude Code's `modelUsage`) |
| `trace` | strong | a deterministic CLI trace/event/status line the CLI itself emits (not the model) |
| `cli_status` | medium | a CLI status/startup banner confirms the selected model (e.g. Codex's stderr `model:` line) |
| `invocation_tag` | medium | the local runtime invocation tag *is* the identity (e.g. the tag passed to `ollama run`) |
| `self_report` | **weak** | the model was asked to name itself and its answer was parsed out of its own output |
| `none` | — | no usable identity evidence at all |

`invocation_tag` and above are **STRONG** — authoritative enough both to mark a call `verified` and
to prove a `mismatch` on a non-match. `self_report` and `none` are **WEAK** — they can, at best,
produce `self_reported`, never `verified`, and a non-match there is `unknown`, not `mismatch`.

An adapter's tier is a **ceiling declared by its recipe** (`verify.CapEvidence`), so a model's own
output can never inflate its way to a stronger tier. A recipe with no identity parser records **no**
model — never the one we requested, which would be our own argument handed back as a "confirmation".

`meshcore/verify` classifies `(evidence tier, requested model, actual model)` into one of:

- **`verified`** — strong evidence, and the actual model matches the requested one. (Claude Code
  identity matching is Anthropic-alias-aware: a requested `opus` or `claude-opus-4-8` matches the
  envelope's `claude-opus-4-8[1m]`; a different family, e.g. `sonnet`, does not.)
- **`self_reported`** — weak evidence, and the self-report matches the requested model. Recorded
  distinctly from `verified` and never silently promoted to it.
- **`unknown`** — no or weak evidence with no confirmable model, or a self-report that doesn't
  clearly match.
- **`mismatch`** — strong evidence proves a *different* model answered.

(`unverified` is a fifth status, but it's reserved for non-identity halts — containment breaches,
cancellations, invocation failures — not identity classification itself.)

**All four are labels.** They change what a reader is told and nothing else.

## What each classification does — and does not — do

| Classification | Recorded | Response used | Run continues |
|---|---|---|---|
| `verified` | yes | yes | yes |
| `self_reported` | yes, as a caveat | yes | yes |
| `unknown` | yes, as a caveat | yes | yes |
| `mismatch` | yes, as a **prominent** caveat | yes | yes |

A `mismatch` is the strongest identity signal there is — a provider envelope naming a model nobody
asked for — and it is still only a caveat. It says the configuration is not doing what you think, which
is worth knowing loudly. It says nothing about whether the review that came back is correct.

**What still halts a run:** output the host cannot parse, a containment breach, a cancelled context, an
adapter that cannot be invoked, a panel with fewer than two responding explorers. Every one of those is
about whether there is usable work to continue with — never about who produced it.

## The rules that remain

- **A self-report is never labeled `verified`.** It's a fallback for adapters that expose nothing
  stronger — useful when there's no envelope or trace, but permanently weaker and recorded separately.
- **A declared tier is a ceiling, not a promise about the world.** It is our assertion about a CLI's
  behavior, and it can be wrong (it was, for `codex-cli`). That is precisely why nothing load-bearing
  is allowed to rest on it.
- **Every surface reports identity.** CLI, ACP, MCP and the run manifest all carry the per-seat
  classification. An empty caveat list is the positive statement that every seat verified — a
  different fact from a surface that simply didn't say.

In short: adapters are described honestly by what they can prove, weak claims are never dressed up as
strong ones, and no claim of either kind is allowed to decide what your findings are worth.

## Manual identity-capture workflow

> **Opt-in and manual.** This workflow invokes real, authenticated CLIs and **may spend
> tokens/credits**. Nothing here runs as part of `go test ./...` — it's something you run
> deliberately, once, to confirm or upgrade an adapter's evidence tier.

meshcore never authenticates a CLI, opens a browser/device login, stores secrets, or refreshes a
session — native auth, model access, and folder trust stay with each provider's own CLI. Capture is
purely about confirming *what identity signal that CLI exposes once you're already logged in*.

### Step 1 — the echo test (do this FIRST)

Before capturing anything, establish that the channel is capable of disagreeing with you. Ask for a
model that cannot exist and watch what the candidate channel says:

```sh
cd "$(mktemp -d)"
<cli> --model totally-not-a-real-model-9x -p 'reply with the single word: ok'
```

| What it does | What that means |
|---|---|
| errors / refuses the model | the channel validates against something real — proceed to capture |
| reports a *different* model | the value came from the provider — genuine evidence |
| **reports `totally-not-a-real-model-9x` back** | **it is an ECHO of your argument. Declare `EvidenceNone` and stop.** |

Skipping this step is how `codex-cli` came to be declared at the `trace` tier for months while proving
nothing at all.

### Step 2 — capture commands

Prefer the strongest evidence the CLI exposes (an envelope/usage field or a trace/status line); use
a bare self-report prompt only where nothing stronger is available.

```sh
# envelope — modelUsage key is the billed/answering model (passes the echo test: a bogus
# model yields HTTP 404 and an EMPTY modelUsage, and a real call names the model unprompted)
claude -p 'Output only your model identifier/name' --output-format json

# self_report — no structured signal, so ask the model directly
agy --model <model> -p 'Output only your model identifier/name'

# invocation_tag — the run tag already IS the identity; a self-report adds little
ollama run <tag> 'Output only your model identifier/name'
```

For a gateway CLI like Devin, run each model slug you use from an **empty directory** (so no
repository code is in scope) and confirm the exact `--model` value your version accepts before
capturing:

```sh
cd "$(mktemp -d)"
devin -p 'Output only your model identifier/name' --model <slug>
```

### Fixture directory shape

Save a sanitized capture under `testdata/adapters/<adapter>/<case>/`, with:

```
argv.txt               # the exact command line run
stdout.txt              # raw stdout
stderr.txt              # raw stderr
exit-code.txt           # process exit code
expected-identity.json  # the evidence tier + identity the fixture should classify to
notes.md                # what was captured, when, and any caveats
```

`meshcore/verify`'s classifier is exercised deterministically against these fixtures — no real CLI
is ever invoked by the test suite itself.

### Sanitize before saving a fixture

Real CLI output can carry sensitive data. Before committing a capture as a fixture, **remove**:
account IDs, access tokens, machine paths, usernames, project/private filenames, organization IDs,
and any sensitive request IDs. Keep only the identity-relevant text a parser actually needs, and
note in `notes.md` whether the fixture is a real capture or a synthetic one built to exercise a
specific case (mismatch, timeout, malformed output, etc.).

### After capture

Once a CLI's strongest identity signal has **passed the echo test** and been captured, wire its
extraction into that adapter's recipe (`ParseIdentity`) and set its declared evidence tier
accordingly, backed by the new sanitized fixture. Until then, an adapter ships with its honest current
ceiling (often `none` / `self_report`) rather than an unconfirmed guess — the safe default is to
under-claim, never over-claim, identity strength. Under-claiming costs nothing: a lower tier changes
the label on the record and nothing else. See [adapters.md](adapters.md) for the per-adapter recipes
and their current status legend.
