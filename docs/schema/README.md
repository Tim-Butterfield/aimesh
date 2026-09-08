# aimesh — JSON Schemas

> **Authority, honestly stated.** These files are the **reference** machine-readable shape of the payloads and run artifacts produced across the `aimesh` monorepo, and the intended source of truth when a payload's shape is *designed*. They are **not** loaded at runtime: nothing in the repo `go:embed`s this directory. What actually runs is hand-written Go validation that **mirrors** them — `internal/review/schema` checks required fields and enum membership structurally.
>
> **Two of them are now checked by tests**, against a small dependency-free draft-2020-12 validator (`meshcore/jsonschema`): [`config.schema.json`](config.schema.json) is round-tripped against the shipped seed and against layers the decoder accepts *and* refuses, and [`effective-config.schema.json`](effective-config.schema.json) is validated against a real run's `config-effective.json`. Both are marked ✅ below. Every other file here is still unchecked.
>
> Practically: when a payload changes, change the schema file here *and* the Go structs/validators; for an unchecked file, expect the two to drift silently until it gets the same treatment. Where they disagree, the Go code is what a running binary does.

Draft: JSON Schema **2020-12**. Property names are **camelCase**; enum *values* are **snake_case** where defined. Dates are RFC 3339 / ISO-8601 `date-time`. Path strings are workspace-relative unless stated. `runId`/`callId` are opaque stable strings (the run id is also the run-directory name).

Schemas are grouped below by the component that owns the concept they describe — the substrate primitives that meshcore defines and both apps produce, versus reviewmesh's own review-domain payloads.

## meshcore-leaning schemas

Shapes rooted in meshcore's domain-free primitives: model calls, identity verification, halts, config, doctor, and the event log.

| File | Validates |
|---|---|
| [call-status.schema.json](call-status.schema.json) | Normalized `CallStatus` (every model call). |
| [model-verification.schema.json](model-verification.schema.json) | `model-verification.json` — the per-spawned-call identity verification result (evidence tier → `verified`/`self_reported`/`unknown`/`mismatch`). |
| [halt-record.schema.json](halt-record.schema.json) | `halt-record.json` — written on halt: halt class, the **stable machine `reasonCode`**, the optional actionability `signal`, and the human rendering in `detail`. |
| [doctor-report.schema.json](doctor-report.schema.json) | The `doctor --json` readiness projection (`ok` + `checks[]`), produced by meshcore's domain-free doctor primitives so both apps emit the identical shape. |
| [config.schema.json](config.schema.json) ✅ | A **reviewmesh** config layer — `~/.aimesh/review/config.yaml` or its project counterpart (top-level structure; sub-shapes specified in `configuration.md`). It does **not** describe the shared `~/.aimesh/adapters.yaml` or exploremesh's `profiles.yaml` (see below). It accepts **exactly** what the Go decoder accepts: nothing is `required` (a layer is partial by design), unknown keys are refused wherever the decoder refuses them, `null` is a permitted spelling of any value, and a string's *value* is left free where the decoder leaves it free. A test drives real layers through both and fails on any disagreement. |
| [effective-config.schema.json](effective-config.schema.json) ✅ | `config-effective.json` in a run directory: the merged effective config, which is a plain serialization of the same `Config` shape (so it `$ref`s the file above) with all top-level sections present. It previously described a `_meta` resolution-provenance block that nothing wrote; the block was removed rather than manufactured — the run's `resolved-plan.json` and `logs/events.jsonl` already carry that provenance, and a second derivation of it here would be a second answer. A test validates a real run's artifact against it. |
| [event-log-line.schema.json](event-log-line.schema.json) | One line of `logs/events.jsonl` — the run event log. |
| [edit.schema.json](edit.schema.json) | Remediation `Edit`s (search/replace blocks) applied to a contained workspace copy. |
| [doctor-issue.schema.json](doctor-issue.schema.json) | A `DoctorIssue` (doctor readiness check → repair flow). |

## reviewmesh schemas

reviewmesh's review-domain payloads: findings, adjudication, patches, run state, and ACP progress notifications.

| File | Validates |
|---|---|
| [reviewer-result.schema.json](reviewer-result.schema.json) | A reviewer/cross_check/verifier JSON response (includes the shared `finding` definition). |
| [host-adjudication.schema.json](host-adjudication.schema.json) | The host's per-finding adjudication output. |
| [patch-summary.schema.json](patch-summary.schema.json) | The patch/apply summary. |
| [run-state.schema.json](run-state.schema.json) | `run-state.json` (its `halt` block is a machine-readable **summary** — class + reason code + signal — not the full halt record; its `authority` block is the inclusion manifest, absent when none was declared). |
| [review-projection.schema.json](review-projection.schema.json) | The `aimesh review run --json` run projection: findings with host **dispositions** (and the write-path rule's `applyable: false` refusal where it applies), the `fingerprint` a `--select` matches on, the blind panel's per-finding **provenance** (`supportingSeats`, host-computed `agreementCount`, `dissentingSeats`) and the qualifiers over it (`distinctModels`, `agreementIndependence`, `consensus`, `grounding`), the executed roster, identity caveats, the authority inclusion manifest, host-computed counts, the halt record on a halt, and the run-level qualifier blocks — `grounding`, `composition`, `dissent`, `partialPanel`, `scope`, `verification`, `selection` — each present only when it applies, each carrying the fixed `note` that states the limit of its own claim. Built from the same adjudication result as `run-state.json` / `review-summary.md`. |
| [panel-roster.schema.json](panel-roster.schema.json) | `panel/roster.json` — the blind reviewer panel's executed roster: one entry per **requested** seat, in order, including seats that halted or never started. Written on success and on halt, so "N seats requested = N seats accounted for" is a recorded fact rather than an implication. |
| [authority-manifest.schema.json](authority-manifest.schema.json) | `authority/inclusion-manifest.json` — per authority/context document: source, full-content hash, hash of exactly the embedded bytes, byte counts, completeness, and the caller-declared ranges. The auditable answer to "which intent was this judged against". Its `$defs/inclusion` is reused by the projection and `run-state.json`. |
| [session-update.schema.json](session-update.schema.json) | The params of an ACP `session/update` notification reviewmesh emits from its ACP surface — progress and audit-event detail carried under the `update._meta.reviewmesh` extension. |

## What is NOT here

There are **no exploremesh schemas in this directory.** exploremesh's payload shapes — the raw task, the explorer response schema each mode owns, the per-mode collator outputs (`CollatorOutput`, `SynthesizeOutput`, `CatalogOutput`, `ChallengeOutput`, `ShortlistOutput`, `CompareOutput`, `ForecastOutput`), the merge-ledger and governance records, and the derived evidence-export tables — are defined only in Go, under `internal/explore/{schema,mode,canon,govern,evidence}`. Two configuration files are likewise undocumented here: the shared `~/.aimesh/adapters.yaml` (`meshcore/config/adapterlocations`) and exploremesh's `~/.aimesh/explore/profiles.yaml` (`internal/explore/profile`). Both are covered prose-side in [`../configuration.md`](../configuration.md).

## Usage

These are the *rules* the payloads follow. At runtime they are enforced by Go code that mirrors them, not by a validator reading these files; the ✅ files are additionally checked against these files **at test time**, so the mirror and the original cannot drift apart unnoticed.

- **Adapter output after normalization** — a result that fails the `call-status`/`reviewer-result` shape is a parse failure (halt Class G, bounded retry then halt). Enforced in `reviewmesh/internal/schema`.
- **Config on load** — the structural rules here plus the semantic rules in [`../configuration.md`](../configuration.md); a failure is a configuration error (exit 3). Enforced at runtime by each app's typed strict decoder; a test additionally asserts that `config.schema.json` accepts and refuses exactly what that decoder does.
- **Run artifacts before writing** — a validation failure is classified and recorded; an artifact is never silently written malformed.
- **Golden fixtures** — see [`../../testdata/README.md`](../../testdata/README.md). The golden-run gate compares recorded artifacts byte-for-byte; it does not schema-validate them.

## Versioning strategy

Every payload carries `schemaVersion` (currently `1`). Backward-compatible additions (new optional fields) keep the version. A breaking change bumps `schemaVersion` and ships a documented migration. The `$id` URLs are **identifiers, not fetch targets** — nothing resolves them, so offline operation never depends on network access.
