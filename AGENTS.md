# AGENTS.md — instructions for AI coding agents working in `aimesh`

This file orients an AI agent (Claude Code, Codex, etc.) working in this repository. Humans: see
[README.md](README.md) and [CONTRIBUTING.md](CONTRIBUTING.md).

## What this repo is

A Go monorepo of three modules joined by `go.work`:

- **`meshcore/`** — a headless, domain-agnostic library for governed multi-model operations (adapters,
  model-identity verification, containment, **root confinement (`scope`)**, config, doctor, the ACP **and
  MCP** transports, halt taxonomy, audit). No UI, no product domain.
- **`internal/review/`** — a governed AI **review** orchestrator (any artifact, not just code).
- **`internal/explore/`** — a multi-model **exploration** orchestrator.
- **`internal/cli/`** — the root CLI: shared commands, and dispatch to the two domains.
- **`cmd/aimesh/`** — the single binary. `scripts/` — repo tooling.

## Ground rules (do not violate)

1. **The meshcore boundary is mechanically checked and mandatory** (`make boundary-check`, part of `make gate`, which CI runs on every push and pull request; run it locally before pushing):
   - meshcore must not import `reviewmesh`/`exploremesh`; the two apps must not import each other.
   - meshcore must not import `net/http` or any UI/frontend package.
   - No app-domain vocabulary in a meshcore **exported** identifier (`review`, `lane`, `finding`,
     `explorer`, `collator`, `roster`, …). Put substrate logic in meshcore with neutral names; put
     domain logic in the app.
2. **Discover, don't hardcode.** Both apps build their adapter set from `meshcore/model/shell.Registry`
   over `shell.Recipes()`. Never hardcode a per-app list of adapter names.
3. **Governance invariants** (never weaken): no silent model fallback (a strong-evidence identity
   `mismatch` halts, Class E); models run read-only in a contained workspace copy (mutation → M5); the
   host is the only writer; every path is judged by the one `meshcore/scope` resolver against roots a
   human authorized, with a non-overridable denylist inside them (a refusal is M6, never a silently
   shortened prompt); counts and rankings are computed by the host, never asserted by a model; all
   config writes go through meshcore's config store.
4. **Surface parity.** A run-forming capability must be expressible on the CLI, over ACP **and** over MCP
   with identical fail-closed semantics. Adding one to a single surface is a parity break, not a
   feature — see [docs/mcp.md → Surface parity](docs/mcp.md#surface-parity).
5. **Keep the gate green.** Every change must leave `make gate` passing. Both golden runs are part of
   it (`golden-run` for reviewmesh, `golden-run-exploremesh` for exploremesh) — an intentional output
   change means regenerating that app's baseline deliberately (`GOLDEN_UPDATE=1`), never loosening it.

## Build / test / verify (run from the repo root)

```bash
make gate        # fmt-check + vet + build-all + test-all + race-all + boundary-check + both golden runs   ← the required gate
make build       # build every workspace module (alias for build-all)
make test        # test every workspace module (alias for test-all)
make vet         # go vet across all modules
make install     # install the aimesh binary into GOBIN (plain: go install ./cmd/aimesh)
```

Tests are hermetic: no network, no real model CLIs. Real-provider tests are opt-in behind env vars and
never run in the gate.

## Task: Adding a model adapter

The single most common extension. For a straightforward CLI it is **one new `Recipe`** — no new package.
Both apps pick it up automatically (they discover adapters from `shell.Recipes()`).

**Files you will touch:**
- `meshcore/model/shell/recipes.go` — add the recipe.
- `meshcore/model/shell/shell_test.go` (or a fixture dir under `testdata/adapters/<name>/`) — add tests.

**Steps:**

1. **Add the recipe.** In `Recipes()` (`recipes.go`) add an entry keyed by the adapter name, and a
   `<name>Recipe()` constructor returning a `Recipe`:
   ```go
   func opencodeRecipe() Recipe {
       return Recipe{
           Name: "opencode", Detect: "opencode",
           Identity: core.IdentityEnvelope,      // the coarse TARGET method
           Evidence: core.EvidenceNone,          // the tier you actually PRODUCE — EvidenceNone until captured
           BuildArgs: func(c model.Call) []string {
               // read-only-leaning reviewer argv; append --model/--effort only when set.
               args := []string{"run", "-m", string(c.ModelArg)}
               if c.Effort != "" { args = append(args, "--effort", c.Effort) }
               return append(args, c.Prompt)
           },
           ParseIdentity: unknownIdentity,        // return the model that answered, or "" if you can't tell
           // ParsePayload / Discovery are optional.
       }
   }
   ```
   Rules:
   - **Never leak CLI flags above the adapter** — argv is built here and nowhere else.
   - Keep reviewer calls read-only where the CLI supports it; containment is the backstop regardless.
   - **Never declare an `Evidence` tier you don't actually produce.** `verify.CapEvidence` clamps
     classification to this ceiling (fail-closed). Use `EvidenceNone` + `unknownIdentity` until you have
     captured a real identity signal, so the verifier treats calls as unverified rather than trusting an
     uncaptured claim. (Evidence ladder: `envelope > trace > cli_status > invocation_tag > self_report > none`.)

2. **Add tests** using the fake-binary pattern (see existing `shell_test.go` and `testdata/adapters/**`):
   a normal call, a timeout, an identity mismatch, and a non-zero/error exit; assert the emitted result
   shape. No real CLI, network, or spend in `go test`.

3. **(Optional) catalog entries** in the shipped config for nicer UI selection — the adapter is usable
   the moment the recipe exists. See [docs/configuration.md](docs/configuration.md).

4. **Verify:**
   ```bash
   make gate                              # must stay green (incl. boundary-check + both golden runs)
   go run ./cmd/aimesh review doctor               # new adapter listed
   go run ./cmd/aimesh explore doctor --profile P # or --roster R; P/R names the adapter
   ```
   `doctor` reports the adapter's binary availability without spending tokens. A real invocation (a
   review or exploration whose roster/profile selects the adapter) is what exercises identity extraction —
   that spends tokens and is opt-in.

> **Two adapter families.** Besides the shell recipe above, meshcore ships a **generic ACP-client
> adapter** (`meshcore/model/acpagent`) that drives any CLI exposing an ACP (Agent Client Protocol)
> server over stdio. There is **no fixed ACP catalog and no `acpagent.Recipes()`** — ACP is an open
> protocol with no closed CLI list, so an ACP adapter is **user config, not code**: each instance is an
> entry under `acpAdapters` in the shared `~/.aimesh/adapters.yaml` (`{title, path, args, model}`),
> added via `aimesh explore setup --acp add --path <bin>` / `aimesh review setup --acp add --path <bin>` (both
> also offer `--acp detect` and `--acp remove`), or by hand
> (enter a binary path → *Detect* probes
> `--help` and a real handshake to fill in the launch args + the model the session reports → *Save*).
> Both apps then discover it exactly like a shell recipe. Use it (not a shell recipe)
> when a CLI exposes ACP and you want **protocol-verified identity**: an ACP session can report its
> active model (`session/new` → `currentModelId`), which the adapter classifies at the **cli_status**
> tier (VERIFIED on match) — stronger than a one-shot `--print` call. The adapter runs the session
> read-only (`ask`/`plan` mode), keeps content **inline** in the prompt (empty cwd — the agent has no
> workspace to wander), and surfaces the JSON answer as `Result.Payload` (agentic agents narrate around
> it). For a plain one-shot CLI with no ACP server, the shell recipe stays the standard, simplest choice.
> (Do not confuse either with the apps' own `acp` COMMANDS — `aimesh review acp` and `aimesh explore acp`,
> each app acting as an ACP *server* so another tool can drive it.)

Reference: [docs/adapters.md](docs/adapters.md) (the adapter contract, shell recipes + the ACP adapter)
and [docs/model-identity.md](docs/model-identity.md) (evidence tiers + capture workflow).

## Where to put things (quick map)

- Substrate (adapters, identity, containment, path confinement, config store, doctor, the ACP and MCP
  transports, halt, audit) → `meshcore/`, neutral names.
- Review domain (the reviewer panel/seats, roles/lanes, `Finding`, adjudication, authority documents,
  prompts) → `internal/review/`.
- Exploration domain (explorers/collator/task, modes, canonicalization, synthesis) → `internal/explore/`.
- Anything both domains reach through one command (`init`, `doctor`, `agents-md`, `--version`, and the
  composed `mcp` server) → `internal/cli/`, which must stay a thin dispatcher.
