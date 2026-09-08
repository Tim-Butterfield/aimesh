# Architecture

`aimesh` is a monorepo of one library and two applications. This document describes how they fit together, the boundary that keeps them decoupled, and the governance model they share.

## Components and dependency direction

```
        ┌─────────────┐        ┌──────────────┐
        │  reviewmesh  │        │  exploremesh │      applications (own domain, surfaces, UI)
        └──────┬───────┘        └──────┬───────┘
               │  imports              │  imports
               └───────────┬──────────┘
                           ▼
                    ┌──────────────┐
                    │   meshcore   │                  governed multi-model substrate (no domain, no UI)
                    └──────────────┘
```

- **meshcore** — a headless, domain-agnostic library. It knows adapters, model identity, containment, config, doctor, ACP transport, halt, and audit — nothing about reviews or explorations.
- **review** / **explore** — the two domains, under `internal/`. Each owns its domain model and its surfaces (CLI, ACP, MCP), and consumes meshcore for the governed substrate.

The three are **separate Go modules** joined by `go.work`:

| Module | Import path |
|---|---|
| meshcore | `github.com/Tim-Butterfield/aimesh/meshcore` |
| reviewmesh | `github.com/Tim-Butterfield/aimesh/reviewmesh` |
| exploremesh | `github.com/Tim-Butterfield/aimesh/exploremesh` |
| scripts (tooling) | `github.com/Tim-Butterfield/aimesh/scripts` |

## The meshcore boundary

The dependency direction is one-way and **enforced by `make boundary-check`** (`scripts/boundarycheck`, part of the standard gate contributors run before every change, and which CI runs on every push and pull request). It fails the build on:

1. any reverse import — meshcore importing a `reviewmesh`/`exploremesh` package;
2. any app ↔ app import — the two apps importing each other;
3. any `net/http` (or other UI/HTTP) dependency inside meshcore;
4. any app-domain vocabulary in a meshcore **exported** identifier — a denylist (`review`, `reviewer`, `finding`, `remediation`, `lane`, `roster`, `explorer`, `collator`, `synthesis`, `task`, `reviewmesh`, `exploremesh`) applied at three tiers: **exported identifiers** are strict (fail); **package docs/comments** are baseline (warn); **test-fixture text** is report-only.

**Protocol exemptions to the denylist (rule 4).** A denylisted word is sometimes the word an *external standard* uses for the thing the code implements. `scripts/boundarycheck/main.go` therefore carries `protocolExemptions`, keyed by **(package-path prefix, term)** and never by term alone, so the same word keeps failing everywhere else in meshcore. The prefix is matched against the **meshcore-relative** path, so `"mcp/"` means `meshcore/mcp/**` and reaches nothing else.

There is exactly one entry today: **`{"mcp/": {"task": true}}`** — `task` is permitted inside `meshcore/mcp/**`. The MCP `2026-07-28` `io.modelcontextprotocol/tasks` extension defines `tasks/get`, `tasks/cancel`, `CreateTaskResult` and `resultType: "task"`; naming our types anything else would leave the package implementing a protocol in vocabulary the protocol does not use, and this repo has repeatedly found that checking code against the spec's *own* words is what catches errors. exploremesh's **domain** `task` (`RawTask`, `task.json`) is a different concept and is still caught in every other meshcore package.

The exemption applies to **both enforcing tiers** — the strict exported-identifier tier and the baseline comment tier. The second is deliberate: a package that documents the specification it implements has to be able to name that specification's concepts, and warning on every mention would bury the app-vocabulary leaks the tier exists to surface.

**Adding an entry is a boundary decision, not a lint tweak.** It requires an external specification that names the term, and it must be scoped to the narrowest package implementing that specification.

This boundary is what makes meshcore extractable: publishing it later as its own module is intended to be "move a directory + one import-path rewrite," not a re-architecture.

## meshcore packages

| Package | Responsibility |
|---|---|
| `core` | Domain-free primitives (`ModelArg`, identity evidence/method, verification results, `HaltClass`, `Edit`, capabilities). |
| `model` | The adapter contract + implementations (shell recipes in `model/shell`, the generic ACP-client adapter in `model/acpagent`, plus the internal test-only `fake` — hidden, env-gated, never user-configurable). Atomic verbs; no CLI-flag leakage. |
| `verify` | Model-identity classification (evidence tiers → `verified`/`self_reported`/`unknown`/`mismatch`). |
| `workspace` | Containment: isolated read-only copies, edit apply/diff/commit, mutation detection (M5). |
| `scope` | Fail-closed **root confinement**: the one path resolver every surface funnels through — allowed roots, resolved-path canonicalization, the non-overridable read/write denylist, and the machine reason codes a refusal carries (M6). |
| `config` | The governed config store (atomic write, patch apply, injected validator). |
| `config/adapterlocations` | The **shared** adapter configuration both apps read: `adapters.<n>.path` binary-path overrides for code-owned recipes, plus user-defined `acpAdapters` ACP instances, under `~/.aimesh/adapters.yaml` (user + root-anchored project scope, compare-and-swap writes). |
| `localstate` | VCS-root discovery and the local, VCS-excluded `.aimesh/` state directory (`init` / `repo init` / `folder init` in both apps). |
| `doctor` | Readiness primitives (adapter probes, writable-dir checks). |
| `clihint` | Pure classification of a failed provider-CLI invocation into a typed signal (folder-trust / login-required / model-invalid / update-prompt) so a caller can render actionable guidance. No prose, no I/O. |
| `acp` | Generic ACP transport (JSON-RPC framing, session store, child-process harness). |
| `mcp` | Generic MCP (Model Context Protocol) stdio transport over the `acp` framing. **These servers are DUAL-ERA.** A process serves one of two protocol families and commits to it with an atomic latch on the first request; `--protocol dual\|legacy` sets the launch posture (default `dual`). See [mcp.md § The two eras](mcp.md#the-two-eras-and-how-a-request-picks-one) and [§ Conformance](mcp.md#conformance). **LEGACY (`2025-06-18`, `2025-03-26`, `2024-11-05`) — session-based.** *Client → server:* lifecycle (`initialize` + `notifications/initialized`) with protocolVersion negotiation, `tools/list` with cursor paging, `tools/call`, progress (only with a caller-supplied token), cancellation by request id, `ping`, `logging/setLevel`, and `resources/list` + `resources/read` + `resources/templates/list` (declared only when an application supplies a `ResourceProvider`). *Server → client (**legacy only**):* outgoing requests with namespaced ids, a pending table, response routing in the read loop, a per-request deadline and cancellation safety — which is what makes `roots/list` (and its `list_changed` re-fetch) possible at all; the answer is handed to the application through `OnRoots` and the transport itself has no opinion about it. `2026-07-28` forbids a server to write a JSON-RPC *request* to stdout, so `Server.Request` refuses there with `ErrModernEra` and a modern client narrows the server through the `roots` **tool argument** instead. **MODERN (`2026-07-28`) — sessionless.** `server/discover` (mandatory); the per-request protocol context in `_meta` (`io.modelcontextprotocol/protocolVersion` and `clientCapabilities` required, `clientInfo` and `logLevel` optional, plus the unprefixed OpenTelemetry `traceparent`/`tracestate`/`baggage`, carried and never interpreted); the era latch; a result envelope carrying `resultType`, the `ttlMs`/`cacheScope` caching hints on the six cacheable operations, and `_meta.io.modelcontextprotocol/serverInfo`; content-addressed resource URIs; and the opt-in `io.modelcontextprotocol/tasks` extension. **What the modern era deliberately does NOT do:** `ping` and `logging/setLevel` are **removed** (both answer `-32601` with a message naming the replacement); the Logging *feature* is deprecated there and its named stdio migration is stderr, so this transport emits **no `notifications/message` at all** on that era and advertises no `logging` capability; and there are no server-initiated requests. Also an opt-in send-time check that a result's `structuredContent` satisfies its tool's declared `outputSchema`, and `RunStore` — an opaquely-addressed (`aimesh://run/<runId>/<artifact>`) publisher for run artifacts that resolves registrations, never caller-supplied paths. It knows no tools — an application registers each one with its own JSON Schemas and handler. |
| `audit` | Domain-neutral run directories, event logs, per-call records. |
| `fault` | The halt/fault taxonomy and reason codes. |
| `jsonschema` | A dependency-free draft-2020-12 **subset** validator, used by the build to hold declared schemas (MCP `outputSchema`, `docs/schema/`) to what the code emits. A **test/verification utility**: no shipped binary imports it. It lives here because JSON Schema is domain-free and both apps' conformance tests must judge payloads by the same rules. |

`config/adapterlocations` + `localstate` are the **shared adapter substrate**: where a binary lives, and where local state goes, are the same questions in both apps, so they are answered once in meshcore and read identically by `reviewmesh` and `exploremesh`.

## The application pattern

Both apps follow the same layered shape (an IDesign volatility-based decomposition): **Clients** (CLI / ACP / web surfaces) → **Managers** (the write/decision authority) → **Engines** (stateless algorithms) → meshcore (adapters, workspace, config, audit) → **Resources**. Capability is added by *re-composing* the same components — a new adapter behind the existing contract, a new surface, a new sequence — not by adding structural layers.

- **reviewmesh** composes them into the review domain: an ordered **blind reviewer panel** (1..16 seats) plus the single-slot cross_check/verifier/author_remediator roles, a `Finding` schema, and a blind-panel → adjudicate → cross-check → verify → remediate pipeline. Reviewers are read-only; the host is the only writer. Its surfaces are the CLI, an ACP agent (`aimesh review acp`), an MCP server (`aimesh review mcp` — `review_report`, plus `review_remediate` only when the operator grants the `allowRemediate` capability). See [review.md](review.md).
- **exploremesh** composes them into the exploration domain: named **profiles** of ordered explorers + one collator, a byte-identical explorer payload, and an app-owned **mode** registry that selects what one run means — from the default `map` synthesis through `catalog`/`challenge`/`shortlist`/`ai-collab` (emergent space: the explorers author the entities, so any cross-explorer grouping goes through a recorded, append-only **canonicalization** ledger) to `compare`/`forecast` (fixed space: the user declares the universe up front, so no canonicalizer runs and the host computes the whole result). Counts, rankings and pooled numbers are computed by the **host**, never asserted by a model. Its surfaces are the CLI, an ACP agent (`aimesh explore acp`), an MCP server (`aimesh explore mcp` — job-shaped tools, an admission/spend governor, and a governance block that the declared output schema makes `required`) that deliberately cannot run an exploration. It has no workspace and no patch/apply, so it never edits a file the user owns; the two things it does write are its own run artifacts (under `$EXPLOREMESH_ARTIFACT_DIR` — by default the project-local `.aimesh/explore/runs/` when one exists, else the OS temp dir) and, only on an explicit `aimesh explore export --sqlite <path>`, the derived evidence database — whose destination is scope-resolved, denylisted, no-clobber-gated behind `--force` and published by an atomic rename. See [explore.md](explore.md).

The three run-capable surfaces are held to a **surface-parity invariant**: every run-forming capability — mode and mode params, profile selection, ad-hoc panel composition by identifier, `count`, prior context, run capture — is expressible on all of them with identical fail-closed semantics. Only transport mechanics (flags vs `_meta` vs tool params; the MCP job shape) and deliberate, config-visible write-authority ceilings may differ. See [mcp.md](mcp.md#surface-parity).

The one-way UI rule holds in both: **UI → app HTTP endpoints → app manager + meshcore**. The UI never calls meshcore directly, and meshcore contains zero UI.

## Governance model

The guarantees meshcore enforces for every model call, in every app:

- **Verified identity, tiered.** Each call is classified by evidence (`envelope > trace > cli_status > invocation_tag > self_report > none`) into `verified` / `self_reported` / `unknown` / `mismatch`. Weak identity is recorded honestly and never treated as a peer of verified. See [model-identity.md](model-identity.md).
- **Identity is recorded, never enforced.** A `mismatch` is a prominent caveat, not a halt: a label about which model answered cannot make a finding true or false, and (per the `codex-cli` echo test) cannot be trusted to be accurate. See [model-identity.md](model-identity.md).
- **Containment.** Models operate on isolated, read-only copies; only the host applies edits; any mutation of the isolated copy is detected (M5).
- **One governed write path.** All config writes go through meshcore's config store; there is no bypass.

## Halt taxonomy

Every failure maps to a documented class with a deterministic process exit code.

**Both binaries share this table.** `reviewmesh` and `exploremesh` map an error to an exit code the same
way and at the same place: the error carries its class (`meshcore/fault`), and the CLI's print boundary
reads it. Neither surface invents a code, and an unclassified error is reported honestly as `8`
(internal) rather than being folded into a neighbouring class. This is a *surface-parity invariant* — a
script that branches on exploremesh's exit codes branches on reviewmesh's the same way.

| Class | Meaning | Exit |
|---|---|---|
| — | success | 0 |
| (gating) | findings, with `--fail-on-findings`/`--ci` | 1 |
| (usage) | bad invocation | 2 |
| Class A | adapter/auth/binary problem (CLI not found, non-zero exit) | 4 |
| Class B/C/D | provider quota / capacity / model-unavailable *(reserved)* | — |
| Class E | silent model fallback, or proven strong-evidence identity mismatch | 5 |
| Class F | transient failure or cancellation | — |
| Class G | output not parseable as schema-valid JSON (after one retry) | 1 |
| M5 | containment breach (isolated copy modified) | 6 |
| M6 | scope refusal: a write **outside the allowed roots** | 6 |
| (config) | configuration error | 3 |
| (policy) | policy/capability cap exceeded, **or a completed apply that refused a protected-path finding** | 7 |
| (internal) | internal error | 8 |

(M1–M4 are reserved mechanical-hook codes for future cross-model verification phases.)

### Exit 7 without a halt: the protected-path refusal

One case exits 7 **without halting**, and it is stated here rather than discovered.

A finding whose target resolves to a **protected path** (meshcore's non-overridable write denylist:
`.env*`, `.git/`, agent client config, `~/.aimesh/**`) used to halt the entire run at authorization
time. Nothing was written — that part was right — but every *other* accepted finding in a paid run was
discarded with it, and a caller who did not already know which finding was protected had to burn a run
to find out. So a protected-path target is now a **recorded refusal**: the finding is marked
`applyable: false` with `applyRefusalReason: "protected_path"`, the remaining findings are applied
normally, and the refusal is surfaced as a first-class fact.

Surfaced, because a *silent* skip letting a run report success is the failure the original halt existed
to prevent. Every surface therefore reports the run as **not cleanly successful**:

| Surface | Coarse "not clean" signal | Structured detail |
|---|---|---|
| CLI | **exit 7**, reason `apply_refused_protected_path` | `outcome`, `counts.applied`/`counts.refused` and `refusals[]` in the `--json` projection |
| MCP | `isError: true` on a `state: "complete"` result | `outcome: "partial_refusal"`, `counts.refused`, `refusals[]`; `review_run_status` repeats `outcome` + `refusedCount` |
| ACP | `stopReason: "refusal"` | `_meta.reviewmesh.{outcome, applied, refused, refusals}` |

Two consequences, both deliberate:

- **No halt record is written.** There was no halt; the run completed and the commit succeeded. A
  reader who expects "exit 7 ⇒ `halt-record.json`" will be surprised exactly once.
- **Exit 6 is wrong for this case and is not used.** Exit 6 means a containment *breach*. Nothing was
  breached — the denylist held, which is precisely why the finding was refused.

The other two refusal reasons (`authority_only`, `no_workspace_evidence`)
continue to exit **0**: they describe findings that were never eligible to be applied, not a finding
the model targeted at a path we refuse to touch.

## Diagrams

`docs/diagrams/` holds the reviewmesh volatility decomposition and activity/call-chain diagrams (`static-architecture.svg`, `activity-review.svg`, `call-chain.svg`, …). They depict reviewmesh's internal layering, which is the reference shape both apps follow.

## Further reading

- [configuration.md](configuration.md) — config files, precedence, per-app profiles, surface ceilings
- [mcp.md](mcp.md) — the MCP surface in **both** apps: the two protocol eras, tools, schemas, the job shape and the tasks extension, error model, admission limits
- [adapters.md](adapters.md) — the adapter contract and per-provider recipes
- [model-identity.md](model-identity.md) — evidence tiers and capture
- [security.md](security.md) — data flow, containment
- [glossary.md](glossary.md) — terminology
- [../CONTRIBUTING.md](../CONTRIBUTING.md) — build, gate, and the boundary in practice
