# meshcore

**A headless, domain-agnostic library for governed multi-model operations.**

`meshcore` is the substrate the [aimesh](../README.md) applications share. It knows how to invoke AI-model CLIs, prove which model actually answered, contain filesystem side effects, confine every path to roots a human authorized, resolve configuration, probe readiness, speak the ACP and MCP transports, and classify failures — and **nothing about any product domain**. It has no review lanes, no explorers, no `Finding` type, no UI, and no HTTP server. Those live in the applications; meshcore is what keeps their multi-model orchestration *governed*.

Module path: `github.com/Tim-Butterfield/aimesh/meshcore`. API shape: exported Go functions and interfaces — each app builds its own HTTP endpoints and UI on top; the UI never calls meshcore directly.

## Packages

| Package | Responsibility |
|---|---|
| `core` | Domain-free primitives shared across packages: `ModelArg`, identity evidence/method, verification results, `HaltClass`, `Edit`, capability descriptors. |
| `model` | The **adapter** contract and implementations — shell-recipe adapters for real CLIs (Claude Code, Codex, Antigravity/`agy`, Ollama, …), plus an internal test-only `fake` adapter (hidden, env-gated, never user-configurable). Adapters implement a small interface (`Name`/`Available`/`Invoke`, plus an optional `Lister` for model discovery); a call goes in and a result comes out, with CLI flags never leaking past the adapter. |
| `verify` | Model-identity classification: evidence tiers `envelope > trace > cli_status > invocation_tag > self_report > none` → `verified` / `self_reported` / `unknown` / `mismatch`. |
| `workspace` | Containment — disposable, isolated copies of a workspace; edit apply/diff/commit; out-of-band mutation detection (M5). File I/O runs through `os.Root` handles bound to the approved directory by file identity. |
| `scope` | Fail-closed **root confinement**: one path resolver for every surface (CLI, ACP, MCP) — the allowed-root set, resolved-path canonicalization, the **non-overridable** read/write denylist (app/IDE config, `.git/config`, `.env*`, key material), and the stable machine reason codes a refusal carries. No roots configured means every path is refused, never "unrestricted". |
| `config` | The governed config **store**: atomic writes, raw-map load, validated patch application (the validator is injected by the app, so meshcore never learns an app's schema). |
| `config/adapterlocations` | The **shared adapter configuration** both apps read, under `~/.aimesh/adapters.yaml` (user scope overlaid by a root-anchored project scope): `adapters.<name>.path` binary-path overrides for code-owned recipes — presence-aware, so an explicit empty string is a deliberate "use `PATH`" clear — plus `acpAdapters.<name>` user-defined ACP instances. Writes go through a content-hash compare-and-swap, so two processes can't clobber each other's unrelated edits. |
| `localstate` | VCS-root discovery (a pure walk-up for `.git`/`.hg`) and the local, VCS-excluded `.aimesh/` state directory both apps' `init` / `repo init` / `folder init` create. |
| `doctor` | Domain-free readiness primitives: adapter availability probes, writable-dir checks, a `Prober` interface and `Check`/`Report` types. |
| `clihint` | Pure classification of a *failed* provider-CLI invocation into a typed `Signal` (folder-trust, login-required, model-invalid, update-prompt), read from stderr and a structured JSON error envelope — never raw stdout, which a model could poison. No prose and no I/O: the caller owns the human wording. |
| `acp` | The generic ACP transport: JSON-RPC framing (newline + `Content-Length`), a cross-restart session store, and child-process harness utilities. App-specific protocol *validation* stays in the app. |
| `mcp` | The generic MCP (Model Context Protocol) stdio transport, layered on the `acp` framing. It is **dual-era**, and a process latches one era for its lifetime (`--protocol dual\|legacy` sets the launch posture, default `dual`). **LEGACY** (`2025-06-18`, `2025-03-26`, `2024-11-05`) is session-based: lifecycle + `protocolVersion` negotiation, `tools/list` with cursor paging, `tools/call`, progress (only with a caller-supplied token), cancellation by request id, `ping`, `logging/setLevel`, `resources/list` + `resources/read` + `resources/templates/list`, and server→client `roots/list`. **MODERN** (`2026-07-28`) is sessionless: `server/discover`, the per-request `_meta` protocol context, `resultType` + `ttlMs`/`cacheScope` + `_meta.serverInfo` on every result, content-addressed resource URIs, and the opt-in `io.modelcontextprotocol/tasks` extension. The modern era deliberately **removes** `ping` and `logging/setLevel` (both `-32601`), emits **no** `notifications/message` at all (the Logging feature is deprecated there and its named stdio migration is stderr, which this transport already does), and issues **no** server→client requests. See [docs/mcp.md § The two eras](../docs/mcp.md#the-two-eras-and-how-a-request-picks-one) and [§ Conformance](../docs/mcp.md#conformance). It knows **no tools** — an application registers each one with its own JSON Schemas and handler. |
| `audit` | Domain-neutral run plumbing: run directories, event logs, per-call records. |
| `fault` | The halt/fault taxonomy — halt classes (A–G, M1–M6), reason codes, and the actionability signal a halt record carries. |
| `jsonschema` | A small, dependency-free JSON Schema (draft 2020-12) **subset** validator. It is a **test/verification utility, not a runtime validator** — no shipped binary imports it; the build uses it to hold declared schemas (each MCP tool's `outputSchema`, the files under `docs/schema/`) to the payloads the code actually emits. It lives in meshcore because JSON Schema is domain-free and because both apps' conformance tests must judge payloads by the same rules. |

## Governance guarantees

- **Identity is tiered, not boolean — and recorded, not enforced.** Every call is classified (`verified` / `self_reported` / `unknown` / `mismatch`) and a self-report is never labeled verified. No classification gates anything: an adapter can be *asked* to use a model but cannot be made to *prove* it did, so `verify` describes what each channel can support and callers report it rather than acting on it. See [../docs/model-identity.md](../docs/model-identity.md).
- **Containment by construction.** Models see isolated read-only copies; only the host applies edits; any mutation of the isolated copy is detected (M5).
- **Fail-closed confinement.** Every path a caller supplies is judged by one `scope` resolver against roots a human authorized, with a non-overridable read/write denylist inside them; a refusal is a recorded halt (M6), never a silently shortened prompt. With no roots, every path is refused.
- **One governed write path.** All config writes go through the `config` store; there is no bypass.

See [docs/adapters.md](../docs/adapters.md), [docs/model-identity.md](../docs/model-identity.md), and [docs/architecture.md](../docs/architecture.md).

## Extending — adding an adapter

Supporting a new provider CLI is usually a **single new `Recipe`** in `model/shell/recipes.go`; both apps
discover it automatically from `shell.Recipes()`, with no per-app change. Step-by-step:
[docs/adapters.md → Adding an adapter](../docs/adapters.md#adding-an-adapter), with a precise runbook for
AI coding agents in [AGENTS.md](../AGENTS.md#task-adding-a-model-adapter).

**A second adapter family — the generic ACP adapter.** Besides shell recipes, meshcore ships
`model/acpagent`: a generic client that drives any CLI exposing an [ACP](https://agentclientprotocol.com/)
server over stdio (`cursor-agent acp`, `devin acp`, `gemini --acp`, `copilot --acp --stdio`, `opencode acp`,
`qwen --acp`, …), reusing meshcore's own `acp` transport as a *client*. One code path, no per-CLI recipe and
**no fixed catalog**: ACP CLIs are user-defined **instances** under `acpAdapters` in the shared
`~/.aimesh/adapters.yaml` (`{title, path, args, model}`), so adding one takes no code at all. Its key win
is **protocol-verified identity**: an
ACP session reports its active model (`session/new` → `currentModelId`), classified at the **cli_status**
tier (verified on match) — stronger than a one-shot `--print` call can offer. The session runs read-only
(`ask`/`plan`), content stays inline in the prompt, and the JSON answer is surfaced as the semantic payload.
Reach for it when a CLI speaks ACP and you want verified identity; the shell recipe stays the simplest choice
for a plain one-shot CLI.

## The boundary

meshcore's independence is **enforced by `make boundary-check`** (part of the standard gate contributors run before every change; there is no CI wired up yet — see [CONTRIBUTING.md](../CONTRIBUTING.md)). The check fails on any reverse import (meshcore importing an app), any app↔app import, any `net/http` dependency in meshcore, and any app-domain vocabulary in a meshcore **exported** identifier (`review`, `lane`, `finding`, `explorer`, `collator`, …). This keeps meshcore extractable: publishing it later as its own module is meant to be "move a directory + rewrite import paths," not a re-architecture.

## Status

meshcore is **not published** as a standalone module yet. Its only consumers are the two in-repo apps via `go.work`, so its import path stays internal to the monorepo; it will be published only when there is a concrete external consumer and the API has been stable. Treat exported symbols as a contract regardless. Run `go doc ./...` here for the API reference.
