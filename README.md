# aimesh

**Governed, provider-diverse orchestration for multiple AI models — as a reusable library and two applications.**

`aimesh` is a Go monorepo. At its center is **meshcore**, a headless library for running several AI models under governance — pluggable CLI adapters, verified model identity, filesystem containment, and a strict no-silent-fallback halt policy. Two applications are built on it:

| Component | What it is |
|---|---|
| **[`meshcore/`](meshcore/)** | A headless, domain-agnostic library for **governed multi-model operations**: CLI model adapters, model-identity verification, workspace containment, config storage, readiness (doctor), an ACP JSON-RPC transport, a halt/fault taxonomy, and audit plumbing. No UI, no product opinions. |
| **[`internal/review/`](internal/review/)** | A **provider-diverse, governed AI review orchestrator**. One or more reviewer models — through any adapter — review an artifact against its intent (code, a design or architecture doc, a spec, a README, a changeset); a single host authority adjudicates the findings; then it **reports**, **patches**, or **applies** the accepted changes. Reviewers stay read-only; only the host writes. |
| **[`internal/explore/`](internal/explore/)** | An **exploration / research orchestrator**. N independent *explorers* answer the same app-formulated task blind and in parallel; one *collator* synthesizes the responses under a mode-specific contract (findings + disagreement register, catalog, shortlist, comparison, forecast, …) — with any count or ranking computed by the host, never asserted by a model. |
| **[`cmd/aimesh/`](cmd/aimesh/)** | The single binary. Both domains are reached as subcommands of it (`aimesh review …`, `aimesh explore …`); there is no separate per-domain executable and no web UI. |

The two domains are deliberately separate — they share meshcore's governance substrate instead of forking it, but keep their own domain models (review lanes vs. explorer panels) and surfaces (CLI, ACP, MCP).

## Why it exists

Running one model and trusting the result is easy. Running *several* models — different providers, at different reasoning depths — and combining their output **without silently trusting the wrong model, leaking a workspace, or fabricating agreement** is the hard part. meshcore is the substrate that makes that safe and repeatable; reviewmesh and exploremesh are two products that need it.

Governance guarantees that hold across every component:

- **Honest model identity — recorded, never enforced.** Every model call is classified by evidence tier (`verified` / `self_reported` / `unknown` / `mismatch`) and reported on every surface, so an unverifiable seat never reads like a verified one. It never decides anything: a model can be *asked* to use a model but cannot be made to *prove* it did, so findings are judged on their content. See [docs/model-identity.md](docs/model-identity.md).
- **Containment.** Models operate on isolated, read-only copies of a workspace; only the host applies changes, and any out-of-band mutation is caught (M5).
- **A closed halt taxonomy.** Every failure maps to a documented class with a deterministic exit code — no ambiguous states.

See **[docs/architecture.md](docs/architecture.md)** for how these are enforced across the monorepo.

## Repository layout

```
aimesh/                      module: github.com/Tim-Butterfield/aimesh  (the application)
├── go.work                  # workspace tying the modules together
├── cmd/                     # the shipped binaries
├── internal/
│   ├── review/              # the review domain
│   └── explore/             # the explore domain
├── meshcore/                # module: .../aimesh/meshcore  (the library — separately extractable)
├── scripts/                 # module: .../aimesh/scripts   (repo tooling: boundary check)
├── docs/                    # architecture, configuration, adapters, security, glossary, schemas
├── testdata/                # fixtures shared by meshcore + the app tests
└── Makefile                 # build/test/gate targets
```

**meshcore is a separate Go module; the two domains are not.** A module is a unit of versioning and
distribution, not organization — meshcore is meant to be published on its own, while `review` and
`explore` are never released independently and have exactly one consumer. They are therefore internal
packages of the one application, which is also why refactoring across them costs nothing.

The dependency direction is one-way and enforced by `make boundary-check` (part of the standard gate,
which CI runs on every push and pull request): **the domains import meshcore; meshcore imports neither; the two domains never
import each other.** That last rule used to be enforced by the module graph as well; inside one module
the static check is the only thing holding it, which is why it is not optional (see
[CONTRIBUTING.md](CONTRIBUTING.md#the-meshcore-boundary)).

## Quick start

Requires **Go 1.26+** .

Both domains run real model CLIs you have installed and authenticated yourself (Claude Code, Codex,
Ollama, …); there is no demo adapter. A shell adapter finds its CLI on `PATH` by name and needs no
configuration to be used. **Most runs are composed in the call itself** — a person at a shell, or an
agent driving `aimesh` over MCP or ACP, names the panel per run and never touches a config file. A
saved profile is the optional shortcut that lets a no-flag run mean something; a fresh install ships
the `default` profile **unconfigured**, and `doctor` says so honestly until you save one.

**1. Install `aimesh`.** There is one binary; `review` and `explore` are subcommands of it. Prebuilt
archives for macOS and Linux (and an experimental Windows one) are on the
[Releases page](https://github.com/Tim-Butterfield/aimesh/releases); each carries a signed build-provenance
attestation you can check with `gh attestation verify <archive> --owner Tim-Butterfield`. Or build
from a clone, which needs nothing but Go:

```bash
git clone https://github.com/Tim-Butterfield/aimesh.git
cd aimesh
go install ./cmd/aimesh
```

`go install` requires no `make` and behaves identically on macOS, Linux and Windows. It installs into
`go env GOBIN`, falling back to `$(go env GOPATH)/bin` when `GOBIN` is unset — print the destination
with `go env GOBIN GOPATH`.

If you have `make`, `make install` runs that same `go install` and additionally removes the
superseded `reviewmesh` and `exploremesh` binaries left by earlier versions. That cleanup is its only
extra behaviour, so skip it freely on a fresh clone.

**Put the install directory on your `PATH`** if it is not already, then confirm with
`aimesh --version`:

```bash
# macOS / Linux (bash, zsh) — add to your shell profile to make it permanent
export PATH="$PATH:$(go env GOPATH)/bin"
```

```powershell
# Windows (PowerShell) — for the current session; use the Environment Variables UI to persist it
$env:Path += ";$(go env GOPATH)\bin"
```

**2. Compose a panel in the call and price it.** `--dry-run` resolves everything, reports every
configuration error a real run would hit, and spends nothing:

```bash
# review — an ordered blind reviewer panel. The host lane (author_remediator) is the one seat that
# must name a model-catalog key (`aimesh review list` shows them); it is never passed through.
aimesh review run --dry-run --report . \
  --reviewer adapter=claude-code,model=sonnet \
  --reviewer adapter=codex-cli,model=gpt-5.5 \
  --set author_remediator.adapter=claude-code --set author_remediator.model=claude-code-default

# explore — two or more explorers plus a collator. `model` is the slug the adapter's own CLI accepts.
aimesh explore run "evaluate approach X" --criteria "correctness,risk" --dry-run \
  --explorer adapter=claude-code,model=sonnet \
  --explorer adapter=codex-cli,model=gpt-5.5 \
  --collator adapter=claude-code,model=opus
```

The adapter must be one aimesh defines (a shell recipe, or an ACP instance you added); a model that is
not a catalog key is handed to the adapter verbatim and reported as `passthrough`. Over MCP the same
composition is the `panel` argument; over ACP it is `_meta.<domain>.panel` — see
[docs/mcp.md](docs/mcp.md#panel-select-or-compose) and [docs/acp.md](docs/acp.md). Drop `--dry-run` to run.

**Before your first run, launch each provider CLI once yourself** and answer whatever it asks (folder
trust, login, an update nag). An adapter deliberately never answers a first-run prompt on your behalf, so
a CLI still sitting on one will hang or exit empty — see
[docs/adapters.md → First-run preflight](docs/adapters.md#first-run-preflight--run-each-cli-once-yourself).
Each CLI is installed and authenticated by you — the apps detect and safely probe binaries but never log in for you. See **[docs/configuration.md](docs/configuration.md)** and **[docs/adapters.md](docs/adapters.md)**.

**3. Optionally, save a panel as a profile** so a no-flag run means something. Record a binary path
only for a CLI that is **not** on `PATH`; paths go to the shared `~/.aimesh/adapters.yaml` both
domains read, so recording one once serves both:

```bash
# review — guided: detect CLIs, record binary paths, assign the panel and lanes
aimesh review setup --interactive

# explore — non-interactive; ORDER is preference order, and a panel needs >= 2 explorers
aimesh explore setup --profile default \
  --explorer adapter=claude-code,model=sonnet \
  --explorer adapter=codex-cli,model=gpt-5.5 \
  --collator adapter=claude-code,model=opus

# a CLI that is not on PATH
aimesh review setup --adapter codex-cli --path /opt/tools/bin/codex
```

Then verify without spending anything. Both `doctor` commands exit `0` when the panel is ready and `3`
when they found something, and `--json` reports the same verdict as `ok`, so a script never has to
choose which to trust:

```bash
aimesh review doctor                   # adapter binaries, profiles, surfaces
aimesh review list --json              # adapters + modelCatalog + profiles: what a caller can compose from
aimesh explore doctor                  # explorer/collator readiness for the saved panel
aimesh explore doctor --probe          # + a bounded `<binary> --version` probe: no model call, no spend
```

**4. Run.** With a saved profile, a review or an exploration needs no panel flags — and both make
real model calls through the CLIs you configured:

```bash
aimesh review run --report .        # review the working tree, findings only

aimesh explore run --purpose "evaluate approach X" --criteria "correctness,risk"
```

`aimesh` is a single static binary. Each domain is reached by its own noun:

- **review** — `aimesh review run|list|doctor|setup|config|acp|mcp` — see **[docs/review.md](docs/review.md)**
- **explore** — `aimesh explore run|export|list|doctor|setup|acp|mcp` — see **[docs/explore.md](docs/explore.md)**
- **shared** — `aimesh init`, `aimesh doctor`, `aimesh agents-md`

Configuration is CLI-only: `aimesh review setup` and `aimesh explore setup`. There is no web
workbench — it was removed rather than kept as a second, parallel way to author the same
configuration.

**Driving it from an agent.** `aimesh agents-md` prints the guide an agent should read before its
first call — the same document the `agents_md` MCP tool returns, so an agent arriving by either route
gets identical guidance. It covers composing a panel per call, pricing it with a dry run, and how to
read agreement, identity and scope honestly. [Pointing an MCP host at it](#pointing-an-mcp-host-at-it)
below shows the server entry.

## Development

The workspace spans several Go modules, so build/test go through workspace-aware `make` targets rather than a single root `go build ./...`:

```bash
make gate         # fmt + vet + build-all + test-all + race-all + boundary-check + both golden runs
```

Individual targets: `make build-all`, `make test-all`, `make boundary-check` (enforces the meshcore import boundary), `make golden-run` / `make golden-run-exploremesh` (assert each domain's behavior is unchanged against a golden fixture), `make windows-build` (cross-compile for `GOOS=windows`; not part of the gate). See **[CONTRIBUTING.md](CONTRIBUTING.md)**.

## Documentation

- **[docs/README.md](docs/README.md)** — the documentation index
- **[docs/architecture.md](docs/architecture.md)** — components, the meshcore boundary, layering, halt taxonomy, exit codes
- **[docs/configuration.md](docs/configuration.md)** — config files, precedence, adapters/models, per-app profiles, surface ceilings, environment variables
- **[docs/adapters.md](docs/adapters.md)** — the adapter contract, per-provider recipes, and how to add one
- **[docs/model-identity.md](docs/model-identity.md)** — evidence tiers and identity capture
- **[docs/acp.md](docs/acp.md)** — the ACP surface in both apps: method mapping, trusted roots, `_meta` contracts
- **[docs/mcp.md](docs/mcp.md)** — the MCP surface in both apps: tools, job shape, error model, surface parity
- **[docs/prompts.md](docs/prompts.md)** — reviewmesh's prompt and result contracts, and the authority/context block
- **[docs/security.md](docs/security.md)** — what leaves the machine, containment, remediation gating
- **[docs/glossary.md](docs/glossary.md)** — terminology
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — build/gate workflow and the meshcore boundary in practice
- **[AGENTS.md](AGENTS.md)** — orientation + task runbooks for AI coding agents working *on* this repository (e.g. adding an adapter)
- **`aimesh agents-md`** ([source](internal/agentguide/AGENTS.md)) — the guide for an agent *driving* aimesh: composing panels per call, dry runs, reading results honestly
- Go API reference: `go doc ./...` in any module.

## Status

meshcore is intentionally **not published** as a standalone module yet: its only consumers are the two in-repo apps, so its import path stays internal to the monorepo until there is a concrete external consumer and a stable API. Both apps now ship four surfaces: a CLI, an **ACP server** (`aimesh review acp` / `aimesh explore acp`, so an ACP-capable IDE can drive one run over stdio), an **MCP server** (`aimesh review mcp` / `aimesh explore mcp`, so an MCP-speaking agent can drive runs as tools — see **[docs/mcp.md](docs/mcp.md)**). The three run-capable surfaces are held to a **surface-parity invariant**: every run-forming capability is expressible on all of them with identical fail-closed semantics, and the only deliberate differences are transport mechanics and the config-visible per-surface write-authority **ceilings** (`cli: apply`, `ci: report`, `acp: report`, `mcp: report` + the `allowRemediate` capability). A ceiling is the most a surface may *ever* do; it is not the default. **A run that names no mode reports and writes nothing on every surface** — `cli: apply` means `--apply` is permitted there, not that a plain `aimesh review run .` writes.

### Pointing an MCP host at it

```json
{ "mcpServers": { "aimesh": { "command": "aimesh", "args": ["mcp", "--root", "/path/to/project"] } } }
```

**Pass `--root` explicitly.** Without it the server adopts its launch working directory only when that directory carries a project marker (`.git`, `go.mod`, `package.json`, …). Hosts vary in where they start a server — some use `/` — so an inferred root is not something to rely on. With no root the server still starts and still serves `inlineWorkspace`; it refuses filesystem paths with `scope_no_roots_configured` until you give it one.

Whatever posture the server ends up in, **`review_doctor` reports it** — trusted roots, their provenance, and every capability grant. Prefer that over the launch banner: the MCP stdio spec permits a host to discard a server's stderr, so a message printed at launch may reach nobody.

reviewmesh runs an ordered **blind reviewer panel** (1..16 seats, host-computed agreement), judges an artifact against declared **authority documents**, and confines every path to roots a human authorized. exploremesh has named **profiles** (ordered explorers + a collator + a default mode, with `--profile` / `--count` subset selection) and app-owned **exploration modes** selected with `--mode`. Adapter binary paths and user-defined ACP instances are shared between the apps through `~/.aimesh/adapters.yaml`.

**Stated limitations.** CI is minimal on purpose: one workflow runs `make gate` on every push and pull request, and the release workflow runs it again before cutting artifacts — the same command contributors run locally, so the two cannot drift. Nothing runs on a Windows or macOS runner. Windows support is coded for (drive-letter-agnostic root rules, reparse-point detection, NTFS stream stripping) and there is a `make windows-build` cross-compile target, but **Windows is untested against real CLIs and against a real filesystem**: it is not part of the standard gate, no adapter has been exercised against a real CLI there, and no containment assertion on this page or in [docs/security.md](docs/security.md) has ever been executed on a real NTFS volume. The Windows-conditional code paths are exercised *as branches* on any platform (by substituting the variable that selects them), which tests the decision the code makes — not the answer the filesystem gives it. Two guarantees are genuinely **weaker or absent** there rather than merely untested: the hardlink rule does not fire (Windows cannot report a link count through the API used), and there is no `O_NOFOLLOW` equivalent, so no-follow rests on the reparse-point checks alone. [docs/security.md → Platform matrix](docs/security.md#7-platform-matrix--where-each-guarantee-actually-holds) gives the per-guarantee answer. The per-adapter status legend in [docs/adapters.md](docs/adapters.md#status-legend) says which adapters are confirmed against a real CLI and which are not; [docs/security.md](docs/security.md) states plainly what containment does and does not stop.

A generic **ACP adapter** (`meshcore/model/acpagent`) additionally drives any [ACP](https://agentclientprotocol.com/)-capable model CLI (`cursor-agent`, `devin`, `gemini`, `copilot`, `opencode`, `qwen`, …) over one protocol — reporting the agent's active model as a **verified** identity where the protocol exposes it. There is **no fixed ACP catalog**: each ACP adapter is a user-defined instance under `acpAdapters` in `~/.aimesh/adapters.yaml`, added by hand or with `setup --acp add`. See [meshcore/README.md](meshcore/README.md#extending--adding-an-adapter) and [docs/adapters.md](docs/adapters.md).

## License

[MIT](LICENSE) © 2026 Tim Butterfield. Security policy: [SECURITY.md](SECURITY.md).
