# aimesh documentation

A map of the docs for the [`aimesh`](../README.md) monorepo: the **meshcore** substrate library and the two applications built on it, **reviewmesh** and **exploremesh**.

## Start here

1. [`../README.md`](../README.md) — what the monorepo is, the three components, quick start, repository layout.
2. [`architecture.md`](architecture.md) — components, the meshcore import boundary, layering, the halt taxonomy, and exit codes.
3. `aimesh agents-md` ([source](../internal/agentguide/AGENTS.md)) — the guide for an agent *driving* aimesh, also served by the `agents_md` MCP tool: composing a panel per call, pricing it with a dry run, reading results honestly. Most usage arrives this way.

## Cross-cutting (all components)

| Doc | Covers |
|---|---|
| [`architecture.md`](architecture.md) | How meshcore, reviewmesh, and exploremesh fit together; the one-way import boundary; halt classes and exit codes. |
| [`configuration.md`](configuration.md) | Config file locations, resolution precedence, adapters/models, per-app **profiles** (reviewmesh's reviewer panel + lanes; exploremesh's explorers + collator), surface ceilings and capabilities, environment variables. |
| [`security.md`](security.md) | What leaves the machine, containment guarantees, the remediation double opt-in. |
| [`acp.md`](acp.md) | The ACP surface in **both** apps (`aimesh review acp` / `aimesh explore acp`): method mapping, trusted roots, `_meta` contracts, host-verification procedure. |
| [`mcp.md`](mcp.md) | The MCP surface in **both** apps (`aimesh review mcp` / `aimesh explore mcp`): tools and schemas, the job shape, the error model, admission limits, the remediation write window, and the CLI/ACP/MCP surface-parity tables. |
| [`glossary.md`](glossary.md) | Terminology across all three components, labeled by owner. |

## meshcore — the substrate library

meshcore has no UI and no product domain; it is consumed by both apps via `go.work`. Module path: `github.com/Tim-Butterfield/aimesh/meshcore`.

| Doc | Covers |
|---|---|
| [`../meshcore/README.md`](../meshcore/README.md) | Package map (`core`, `model`, `verify`, `workspace`, `scope`, `config`, `config/adapterlocations`, `localstate`, `doctor`, `clihint`, `acp`, `mcp`, `audit`, `fault`, `jsonschema`) and governance guarantees. |
| [`adapters.md`](adapters.md) | The adapter contract, per-provider recipes, and how to add a new adapter. |
| [`model-identity.md`](model-identity.md) | Evidence tiers and how model identity is captured and classified. |

**Substrate schemas** (see [`schema/README.md`](schema/README.md) for the full index): `call-status`, `model-verification`, `halt-record`, `config`, `effective-config`, `event-log-line`, `edit`, `doctor-issue`.

## reviewmesh — the review app

Package path: `github.com/Tim-Butterfield/aimesh/internal/review` (part of the root module, reached as `aimesh review …`).

| Doc | Covers |
|---|---|
| [`review.md`](review.md) | Commands, quick start, configuration, diagnostic codes, surfaces (CLI, ACP, MCP). |
| [`prompts.md`](prompts.md) | Reviewer/adjudication prompt templates and their JSON result contracts. |
| [`acp.md`](acp.md) | `aimesh review acp` — trusted roots, mode gating, `_meta.reviewmesh` (authority, profile/panel), withheld files. |
| [`mcp.md`](mcp.md) | `aimesh review mcp` — `review_report`, the `review_remediate` double opt-in, staleness, the journal → apply → receipt write window. |

**Review schemas** (see [`schema/README.md`](schema/README.md)): `reviewer-result`, `host-adjudication`, `patch-summary`, `run-state`, `session-update`.

## exploremesh — the exploration app

Package path: `github.com/Tim-Butterfield/aimesh/internal/explore` (part of the root module, reached as `aimesh explore …`).

| Doc | Covers |
|---|---|
| [`explore.md`](explore.md) | The explorer/collator model, the eight exploration **modes**, profiles, commands, quick start, output shape, and evidence export. |
| [`acp.md`](acp.md) | `aimesh explore acp` — the criteria-via-`_meta` contract, panel selection and run capture, the response echo, and the budget caps. |
| [`mcp.md`](mcp.md) | `aimesh explore mcp` — the `explore` tool and its modes, the job shape and run registry, the required governance block, and the admission/spend governor. |

exploremesh has no other dedicated docs; it shares `configuration.md`, `architecture.md`, `adapters.md` and `security.md` with the rest of the monorepo. Its payload shapes are defined only in Go — there are no exploremesh files under [`schema/`](schema/).

## Diagrams and schemas

- [`schema/`](schema/) — JSON Schema (Draft 2020-12) definitions for every payload and artifact; see [`schema/README.md`](schema/README.md) for the grouped index.
- [`diagrams/`](diagrams/) — architecture and sequence diagrams (SVG/PlantUML) referenced from the docs above.

## Go API reference

For exported types and function signatures, use `go doc` inside each module — it is always in sync with the code:

```bash
cd meshcore     && go doc ./...
cd reviewmesh   && go doc ./...
cd exploremesh  && go doc ./...
```
