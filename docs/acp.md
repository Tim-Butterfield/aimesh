# The ACP surface

**Both** applications run as [Agent Client Protocol](https://agentclientprotocol.com/) servers — JSON-RPC 2.0 over stdio, so any ACP-capable IDE or host can drive a run as a session instead of a one-shot CLI invocation:

- `aimesh review acp` drives a **review** (this document's first half).
- `aimesh explore acp` drives one **exploration** (see [exploremesh as an ACP agent](#exploremesh-as-an-acp-agent)).

The surface is split across the monorepo the way everything else is:

- **[`meshcore/acp`](../meshcore/acp/)** supplies the generic, domain-agnostic **transport** — JSON-RPC message framing (`newline` or `content-length`), a cross-restart session store, and child-process harness utilities. It knows nothing about reviews, findings, explorers, or reviewer roles.
- **[`reviewmesh/internal/surface/acp`](../internal/review/surface/acp/)** and **[`exploremesh/internal/surface/acp`](../internal/explore/surface/acp/)** supply the **handlers**: each dispatches ACP methods, validates and shapes payloads to the ACP v1 protocol, maps a `session/prompt` onto its own manager/pipeline, and applies its own domain policy.

This split mirrors the monorepo's [meshcore boundary](architecture.md#the-meshcore-boundary): the transport is reusable outside either app; the review and exploration semantics are not.

> **ACP is one of three run-capable surfaces**, and they are held to a **surface-parity invariant**: every
> run-forming capability (mode and mode params, panel composition, authority documents, prior context, run
> capture, dry runs, readiness checks) is expressible on the CLI, over ACP, and over MCP with identical
> fail-closed semantics. Only transport mechanics, the configuration source and write authority may differ.
> The per-capability comparison tables live in [mcp.md → Surface parity](mcp.md#surface-parity).

> **Both agents read no aimesh configuration.** Their adapters, write grant and optional root ceiling come
> from the arguments the host launches them with, so they work on a fresh install with no setup step. Every
> prompt composes its own panel from the launched adapters, with the exact model identifiers the adapter's
> CLI accepts; saved profiles, the model catalog and saved policy are for the CLI only.

> **ACP runs in two opposite directions in aimesh — don't conflate them.** *This document* covers the
> two apps as ACP **servers** (an IDE/host drives a review or an exploration). The mirror image is
> meshcore's generic ACP-**client** adapter (`meshcore/model/acpagent`): meshcore *drives* a model CLI
> that exposes an ACP server (`cursor-agent acp`, `devin acp`, …) as just another adapter, and the
> model's answer flows back as a review/exploration result. Same transport (`meshcore/acp` framing),
> opposite roles. The client adapter is documented in
> [docs/adapters.md → ACP-client adapters](adapters.md#acp-client-adapters).

## Framing

Both agents speak one message per line by default (`--framing newline`, the framing ACP hosts such as Zed use); pass `--framing content-length` for LSP-style `Content-Length: N\r\n\r\n<body>` framing. Both framings share the same message dispatch — only the wire encoding differs — and both are bounded (a single message is capped at 16 MiB; header parsing is bounded too) so a malformed or hostile local host cannot force unbounded allocation.

## Method mapping (`aimesh review acp`)

| ACP method | reviewmesh behavior |
|---|---|
| `initialize` | Returns the ACP v1 result: integer `protocolVersion: 1`, `agentCapabilities` (`loadSession:false`, text-only `promptCapabilities`, no MCP, `sessionCapabilities.resume` when the durable session store is configured), `agentInfo`, `authMethods`, and `_meta.reviewmesh.{writes, diffAvailable}` — who performs writes on this agent, disclosed before the first prompt. |
| `session/new` | Allocates an opaque session id and minimal state; stores `params.cwd` (cleaned, never made absolute against this process's directory) as the session's workspace (a later `session/prompt` that omits `workspace` falls back to it) and persists a durable record for cross-restart resume. |
| `session/prompt` | **Two paths, chosen by `fromRun`.** Without it: builds a review request from the prompt's absolute workspace, mode, authority and required panel (see [Scope](#launch-flags-and-scope-reviewmesh) and [Panel composition](#panel-composition-via-_metareviewmesh)) and runs it through `ReviewManager.RunContext`, capped to the connection's mode ceiling (see [Mode gating](#mode-gating-reviewmesh-only)). **With** `fromRun` on a `patch`/`apply` turn: resolves that handle to the source run's stored decision set and applies it through `ReviewManager.Remediate` — **no reviewers run** (see [the two-turn contract](#a-write-turn-needs-a-run-handle-and-it-applies-that-run-the-two-turn-contract-reviewmesh-only)). Either way it emits `session/update` progress notifications during the run and a `PromptResponse` at the end, with one of **three** stop reasons: `end_turn` on completion, `cancelled` after `session/cancel`, and `refusal` when a write completed but refused a protected-path finding (see [Partial refusal](#partial-refusal-stopreason-refusal)). |
| `session/update` | Not a request the host sends — reviewmesh's outbound progress-streaming notification during a prompt turn (`SessionNotification` with `update.sessionUpdate: "agent_message_chunk"`), sanitized and context-gated, tagged with the session id. |
| `session/resume` | **Durable, reconnect-without-history.** Restores a session record (session id + cwd) persisted by an earlier process from `<home>/.aimesh/review/acp-sessions/`; returns an empty `ResumeSessionResponse` — no conversation is replayed, because a stateless one-shot reviewer never held one. Advertised only when the durable store is configured (`sessionCapabilities.resume`); otherwise method-not-found. |
| `session/load` | **Method-not-found, intentionally.** Its ACP contract is to replay a prior conversation in full; a stateless reviewer has no conversation to replay, so implementing or advertising it (`loadSession:false`) would be dishonest. Reconnect is served by `session/resume` instead. |
| `review` | A compatibility method with the same handling as `session/prompt` but without a session — useful for a host or test harness that wants a single review turn without the session lifecycle. It accepts the **same** `_meta.reviewmesh` (panel, roots, authority, …) as `session/prompt`, and its `workspace` must be named: a compatibility method that could not express a run-forming capability would be a parity hole, not a simplification. |
| `session/cancel` (and legacy `cancel` / `$/cancelRequest`) | Cancels the in-flight run at the run's `context`. **No write happens after cancel**, and that is enforced rather than intended: an `apply` prompt writes through the same governed write window as every other surface (see [docs/mcp.md § The write window](mcp.md#the-write-window)), where "cancelled" and "entered the commit" are decided **once under a lock** and are mutually exclusive answers. A cancelled prompt still leaves a durable `remediation/receipt.json` in its run directory saying nothing was committed, and answers with a normal `PromptResponse` carrying `runDir`. |
| `shutdown` / `exit` | Graceful teardown and process exit. |

Any method not listed above returns a JSON-RPC method-not-found error.

### Partial refusal: `stopReason: "refusal"`

An `apply` turn that **committed** and refused at least one finding because its target is a protected path
(meshcore's non-overridable write denylist) answers `refusal` — the third official `StopReason` this agent
emits — with the exact split in `_meta.reviewmesh`:

```jsonc
{
  "stopReason": "refusal",
  "_meta": { "reviewmesh": {
    "status": "stable", "mode": "apply",
    "outcome": "partial_refusal",
    "applied": 7, "refused": 1, "runDir": "…",
    "refusals": [ { "fingerprint": "sha1:…", "file": ".env", "reason": "protected_path" } ]
  } }
}
```

**Why `refusal` and not `end_turn`.** `end_turn` is the word for a turn that ended normally, and a host
keying on `stopReason` alone would read it as clean — which is exactly the failure the recorded refusal
exists to prevent. `refusal` is the fail-safe reading. The risk it introduces is the opposite one, a host
concluding that *nothing* was applied, and the answer to that is `applied: 7` sitting right beside it: the
coarse signal over-states, the numbers are exact.

**Why it is not a `-32000` halt.** A halt is an *error* response. Telling a caller the turn failed over a
write that committed is the worst of both outcomes — the receipt exists in the run directory and the caller
has been told it does not.

`refusals[]` is keyed on the **host-computed fingerprint**, never the model-authored finding id: a model
that could relabel findings could otherwise steer which one a follow-up selection names. Nothing was
written to any refused path, every other accepted finding was applied normally, and re-running refuses them
identically — this is not a retryable failure. The same outcome on the other surfaces is MCP
`isError: true` + `outcome: "partial_refusal"` ([mcp.md](mcp.md#partial-refusal-a-protected-path-target-does-not-discard-the-run))
and CLI exit 7 ([architecture.md](architecture.md#exit-7-without-a-halt-the-protected-path-refusal)).

## Launch flags and scope (reviewmesh)

```
aimesh review acp [--adapter <name>[=<path>]]... [--allow-writes]
                  [--root <dir>]... [--allow-broad-root]
                  [--framing newline|content-length] [--turn-timeout <dur>]
                  [--verify-cmd <cmd>]... [--verify-timeout <dur>] [--verify-baseline]
                  [--allow-protected-paths]
```

| Flag | Meaning |
|---|---|
| `--adapter <name>` / `--adapter <name>=<path>` | An adapter prompts may use, found on `PATH` or at the given full path. Repeatable; also settable as `AIMESH_ADAPTERS` (entries separated by `;` on Windows, `:` elsewhere). Paths expand `%VAR%` on Windows and `$VAR`/`${VAR}` elsewhere, plus a leading `~`; an undefined variable refuses startup. See [mcp.md → At a glance](mcp.md#at-a-glance) for the names and the full grammar. |
| `--allow-writes` | Lets an `apply` turn write the workspace. Without it aimesh changes no project content; a `patch` turn supplies the diff. |
| `--root <dir>` | Optional **ceiling**, repeatable: every path a turn declares must lie inside it. |
| `--allow-broad-root` | Permit a `--root` that is normally refused as over-broad. Never waives the read denylist. |
| `--turn-timeout <dur>` | Total wall-clock budget for one turn, default **10m** — the same flag `aimesh explore acp` and both `mcp` servers take. |

Launched with no adapter, the agent still starts; every review turn is refused with a message naming
`--adapter`.

An IDE host launches this agent from its own configuration, for example:

```json
{ "command": "aimesh", "args": ["review", "acp", "--adapter", "devin-cli=%LOCALAPPDATA%\\devin\\cli\\bin\\devin.exe"] }
```

**Scope is declared per turn.** A host can change folders after launching its agent, so nothing is inferred
from where this process started. A turn's scope is exactly the directories it declares:

- the prompt's **`workspace`**, which must be **absolute** (`reasonCode: scope_call_path_relative`), or, when
  the prompt names none, the session's `cwd` from `session/new` — held to the same rule, so a relative
  session `cwd` is refused when a prompt falls back to it. With neither, the turn is refused
  (`workspace is required`);
- any extra absolute directories in **`_meta.reviewmesh.roots[]`**, for example the project folder holding
  authority documents;
- every declared path must lie inside the `--root` ceiling when one is set
  (`scope_outside_root_ceiling`), and is refused when it is the filesystem/volume root, your home directory
  or its parent (`/Users`, `/home`), a system tree (`/etc`, `/usr`, `/var`, `/System`, `\Windows`,
  `\Program Files`, …), a shared tree (`/Users/Shared`, `\Users\Public`), a mount parent (`/mnt`, `/media`,
  `/Volumes`, `/srv`) (`acp_degenerate_root`), or a protected path (`acp_root_denied`). The Windows entries
  are **drive-letter agnostic**. Only an operator's explicit `--root` can waive the breadth rule, with
  `--allow-broad-root`; a path a turn declares never can.

The non-overridable read denylist still applies inside every declared path, so a `.env` under an allowed
project is refused just the same. An `inlineWorkspace` consumes no scope: its content is materialized into a
directory this process owns. Refusals are `-32602` before any spend, with the machine `reasonCode` in
`error.data`.

## Mode gating (reviewmesh only)

A review request carries a mode (`report`, `patch`, or `apply`), but the ACP surface never lets a host push a write beyond what the operator granted and the host supports. The effective mode is `min(requested, connection ceiling, per-turn permission)`:

- **Connection ceiling** — `apply` needs **both** the operator's `--allow-writes` launch grant **and** the host's `FileWrite` capability; otherwise the ceiling is `patch`. A `patch` turn is always available: it changes no project content and supplies the complete diff for the host to apply itself.
- **Per-turn permission** — a host may deny write or patch for a single turn (`session/prompt.permissions`): denying write narrows the turn to `patch`, denying patch narrows it to `report`.

The narrowest always wins. An `apply` turn on an agent launched without `--allow-writes` is **refused** before any run (`-32602`, `reasonCode: "writes_not_granted"`), with a message naming the grant and the `patch` turn that supplies the diff instead. A mode that exceeds the host's capability or a per-request permission is **degraded, never silently**: a `mode_degraded` warn on `session/update`, plus `modeDegraded`, `requestedMode` and `modeReason` in the response's `_meta.reviewmesh`. An omitted mode is `report` on both `session/prompt` and the compatibility `review` method. Every successful or cancelled turn's `_meta.reviewmesh` carries `writes` (`"aimesh"` or `"agent"`) and `diffAvailable: true`. **exploremesh has no equivalent** — it has no write surface to gate (see [exploremesh as an ACP agent](#exploremesh-as-an-acp-agent)).

## A write turn needs a run handle, and it applies that run: the two-turn contract (reviewmesh only)

> A prompt whose **effective** mode reaches `apply` must carry `fromRun`. A turn without it is refused
> with `-32602` (`reasonCode: write_without_run_handle`) **before any work starts**. `report` and `patch`
> turns are unaffected. There is no one-turn review-and-write form on ACP.
>
> An `apply` turn is reachable only on an agent launched with `--allow-writes`.

**The problem.** ACP has no lookup surface. Its whole method set is `initialize`, `review`, `session/new`,
`session/resume`, `session/prompt`, `session/cancel`, `cancel`, `shutdown`, `exit`; not one of them accepts a
run identifier or returns anything about a prior run, and `session/resume` deliberately replays nothing. So a
one-turn review-and-write whose response was lost — cancelled, dropped, or the process killed — would leave
the host holding **nothing at all** about a run that may already have written to its files.

**The rule.** A governed apply is naturally two turns, and the handle rides the one channel that has a
delivery property: a **response to the host's own request**.

| Turn | Call | What the host holds afterwards |
|---|---|---|
| 1 | `session/prompt {mode: "report", …}` | `_meta.reviewmesh.runDir`, plus `accepted[]` — one row per accepted finding, carrying the **host-computed fingerprint**, its file and its title |
| 2 | `session/prompt {mode: "apply", fromRun: <runDir>, …}` | the receipt — and, whatever happens to *this* response, a handle it already had before it sent the request |

If the host does not receive turn 1's response, the host **knows**: its own request is still outstanding.
Notifications have no such property in either direction, which is why the `runDir` on `session/update` is
kept as a live-progress convenience with **no guarantee attached** — it is not the durability mechanism.

The same rule binds the compatibility `review` method, because a rule with one unguarded door is not a rule.

**Two guarantees, stated exactly.**

1. *A governed write over ACP cannot begin unless the host already holds, from a completed response to its
   own earlier request, a run handle. Therefore any write whose own response is lost remains discoverable by
   the host, at the handle it held before it asked.*
2. *A write turn carrying `fromRun` **applies the decision set that run adjudicated**. No reviewers run, no
   second adjudication happens, and the applied set is the set the host inspected in turn 1.*

**Where the set comes from — the run directory, not a registry.** A completed `report` run records its
accepted set to `decisions/decision-set.json` inside its own run directory: the findings and decisions **as
adjudicated**, the files the reviewers were shown, the base hash of every targeted file **as reviewed**, and
the reviewed root's canonical identity. Turn 2 reads that file. Because it is a file, it survives an agent
restart. MCP reads the **same file** for the same reason, whenever its in-memory registry no longer holds
the handle; what remains particular to that surface is its in-process source-run guard, and [what a second
apply gets across a restart because of it](mcp.md#one-form-fromrun).

**The handle is verified, not trusted.** `fromRun` is a path from a peer process. It must be **absolute**,
and it is resolved only among the run-record locations of **the turn's workspace** (the named `workspace`,
or the session `cwd`): its `.aimesh/` state directory when it has one, else the temp run directory. It must
be an immediate child of one of those, both as spelled and after symlink resolution, and it must carry a
decision set whose recorded run id is the directory's own name. Everything else — a relative handle, a
directory this agent did not produce, a nested path, a traversal, a symlink that is merely *spelled* like one
of our runs, a run that recorded no decision set (a `patch`/`apply` run does not) — is refused with one
uniform `-32602` `reasonCode: run_handle_unknown`. The containment ordering is deliberate: the check that a
handle is inside a run-record location is **lexical**, made before the filesystem is consulted, so this
surface never becomes an existence oracle over arbitrary absolute paths.

**What gates the write** — all of it the same machinery every other surface gets, none of it re-implemented:

| Gate | What it refuses |
|---|---|
| Workspace identity binding | The canonical path must still resolve to what it resolved to, and the reviewed root's device+inode must still match. `remediation_workspace_identity_changed`, pre-spend. |
| Base-hash pins (pass one) | Any targeted file whose content changed since the report run — checked **before** the write window opens, before a copy is made and before any model call. `stale_decision_set`. |
| `governedWrite` content pins (pass two) | The same question again, per destination, inside the commit: a file saved during the write is refused there. `stale_decision_set`, with `commitAttempted: true` on the receipt and a rolled-back commit. |
| Turn scope | The write is re-gated against the scope **this turn** declared, not the one the source run was reviewed under — a decision set is durable and can outlive it. |
| Protected-path denylist, journal, cancel/commit gate, receipt | Unchanged; one governed write path. |

**Run-forming arguments are refused, not ignored.** `_meta.reviewmesh.panel` and `.authority` cannot ride a
`fromRun` turn (`reasonCode: from_run_args_refused`): each names a governance input to an adjudication that
has already happened, and honouring the request is not something this turn can do. Same rule, same
reasoning as MCP's `review_remediate {fromRun, workspace}`.

**The workspace.** The turn's workspace — named, or the session `cwd` — must be the tree the source run
judged (`reasonCode: from_run_workspace_mismatch`). The write is applied where the decisions were made, and a
turn whose workspace is somewhere else is told rather than silently redirected.

**The response carries two handles.** `_meta.reviewmesh.runDir` is **this write's own** run directory — its
journal, commit-attempt marker and receipt live there — and `sourceRunDir` is the run whose decisions it
applied. `sourceRunDir` is the canonical directory (symlinks resolved), so compare it to the handle you sent
by resolved path rather than by string.

**Retrying is safe without an idempotency key.** A successful write changes the bytes it wrote, so a second
`fromRun` turn naming the same run finds its own pins stale and halts. There is no ACP equivalent of MCP's
in-memory source-run guard, and none is needed: the pins are durable and the registry was not. MCP reaches
this same answer once its registry no longer holds the run — the guard is the difference between the two
surfaces, not the resolution.

**What is still NOT offered, stated because it would otherwise be assumed.** ACP has no run-state polling —
no `run_status`/`run_result` equivalent, and it gains none. A host observes a run in flight with
`session/update` and afterwards by reading the run directory. And if the *host itself* loses the handle
without persisting it, ACP offers no way to enumerate runs; the durable record of a run is its directory on
disk.

### `select` — selective apply over the stored set

`accepted[].fingerprint` is the **host-computed** identity, never the model-authored `Finding.ID`: keying a
write set on a model-controlled identifier would let a model relabel findings until "apply only this one"
selected something else. `select: ["sha1:…", …]` on a `patch` or `apply` turn consumes exactly those values.

```json
{ "mode": "apply", "fromRun": "/runs/20260805T…", "select": ["sha1:1a2b…"] }
```

**What `select` guarantees:** it narrows **the decision set the `fromRun` run recorded**, and nothing outside
the named fingerprints is written. A fingerprint from turn 1's `accepted` list names the same *object* in
turn 2 because it is the same object — not because a fresh panel happened to raise it again. The narrowing
runs in the same one governed write path the CLI and MCP use, so *"these and no others"* holds there too.

On a write turn with no `fromRun` — a `patch` turn deciding for itself — `select` narrows *that turn's own*
adjudication, which is the rule the CLI's full cycle has always applied.

Every write turn that supplied a selection carries
`_meta.reviewmesh.selection = {requested, matched, unmatched}` — all three lists always present — so a host
can always see what its selectors named.

The rules, all fail-closed and identical to the other surfaces:

- **A selection can only narrow.** An unmatched fingerprint is dropped and reported, never resolved
  anywhere else.
- **An empty `select` is refused** (`reasonCode: select_empty`), never widened to "apply everything".
- **`select` on a `report` turn is refused** (`reasonCode: select_without_write`) — report mode writes
  nothing, so a filter over its writes could not have had an effect.
- **A selection that matches nothing refuses the turn** rather than writing nothing silently.
- **`select` narrows the stored decision set** the `fromRun` handle names — not a fresh adjudication. The
  findings written are the findings the host inspected.

## Withheld files in the result (reviewmesh only)

A containment rule can keep a file **out of the reviewed set** without stopping the run: a hardlinked regular file is withheld from the isolated copy, because its innocuous in-tree name may be a second name for protected material — while an in-tree `cp -al` tree or dedup store is ordinary, so halting on one would let anything that can write inside a trusted tree stop every review at will (see [security.md](security.md#containment--path-safety)).

Withholding is therefore **never silent**. A successful turn's `_meta.reviewmesh` echo carries `withheld[]` when it is non-empty — per entry the workspace-relative `path`, the stable machine `reason` (e.g. `workspace_hardlink_denied`), the `rule` that refused, a `detail`, and the `stage` (`copy` = never placed in the isolated copy, so also never visible to an agentic reviewer CLI working in it; `snippets` = in the copy but never rendered into a prompt). The same list appears in the run record (`run-state.json` → `withheld[]`) and in the `aimesh review run --json` projection.

A host should treat it as a gap in coverage, not as noise: a reviewer cannot object to a file it was never shown, so a withheld file's absence from the findings means nothing at all.

## Authority documents via `_meta.reviewmesh`

A review can be judged against declared **authority/context** documents — requirements, a design doc, a spec. On ACP they travel under the sanctioned `_meta` extension point, in reviewmesh's own namespace, on `session/prompt` (and on the compatibility `review` method, so the two do not diverge):

```json
{
  "jsonrpc": "2.0", "id": 2, "method": "session/prompt",
  "params": {
    "sessionId": "s-…", "mode": "report",
    "_meta": {
      "reviewmesh": {
        "authority": [
          { "name": "spec",
            "path": "/abs/or/relative/docs/spec.md",
            "mediaType": "text/markdown",
            "expectedHash": "sha256:…",
            "completeness": "requireFull" }
        ]
      }
    }
  }
}
```

The schema is **identical to the CLI's** `--authority` / `--authority-manifest` declaration — same fields, same defaults, same refusals — because both surfaces funnel through one implementation. Per the [surface-parity invariant](architecture.md), only the *encoding* differs (a flag vs `_meta`), which is a transport mechanic; the semantics are not allowed to.

**Everything is validated AND fully resolved before any spend.** The ACP surface refuses with **`-32602`**, carrying the stable `reasonCode` in `error.data`, for every one of:

| Condition | `reasonCode` |
|---|---|
| Malformed `_meta.reviewmesh` (wrong types, or an **unknown key inside the reviewmesh namespace** — a silently dropped governance field is exactly what a fail-closed surface exists to prevent) | `authority_doc_invalid` |
| Both or neither of `path`/`content`; missing or duplicate `name`; bad `expectedHash`; incoherent `completeness`/`ranges` | `authority_doc_invalid` |
| More than 8 documents | `authority_too_many_docs` |
| Inline `content` in a write-capable effective mode (the provenance split) | `authority_inline_mode_invalid` |
| A path outside the directories the turn declared, or one the non-overridable read denylist protects (`.env*`, key material) | `scope_outside_root` / `scope_read_denied` |
| An `expectedHash` that does not match the bytes read | `authority_hash_mismatch` |

Unknown keys *elsewhere* in `_meta` belong to the host and are ignored.

> **Scope of the reviewmesh namespace.** On a request, `_meta.reviewmesh` carries exactly these keys, and it
> is accepted on `session/prompt` and the compatibility `review` method **only**; `session/new` reads
> nothing but `cwd`:
>
> | Key | Meaning |
> |---|---|
> | `panel` | **Required** on a review turn: `{reviewers[], author_remediator, cross_check?, verifier?}` — see [Panel composition](#panel-composition-via-_metareviewmesh). Refused on a `fromRun` turn. |
> | `roots` | Extra absolute directories the turn reads beside its workspace. |
> | `authority` | Authority documents, as above. |
> | `maxParallel` | How many reviewer seats invoke at once (absent → the whole panel). Wall clock only. |
> | `dryRun` | Price the turn instead of running it — see below. |
> | `verifyReadiness` | Before dispatch, one bounded one-token call per distinct adapter/model; a blocked agent halts the turn before the panel is paid for. **Spends**; `dryRun` prices it. |
>
> The response's `_meta.reviewmesh` adds `writes` and `diffAvailable` to every successful or cancelled turn.
> The review `mode` is a **top-level** param, not a `_meta` key, so `_meta.reviewmesh.mode` is a `-32602`
> like any other unknown key. One rough edge worth knowing: because the whole namespace is decoded as a
> unit, *any* malformed or unknown key inside it — including a `profile` key — reports
> `reasonCode: authority_doc_invalid`, which is broader than its name suggests.

`dryRun: true` prices the turn instead of running it: the plan, panel, authority documents and
preflight all resolve, then the turn stops before its first model call and answers with
`status: "planned"` and a `shape` — the seats it would convene, the lanes it would call, the
model-call range it is bounded by, and a `payload` saying what those calls would carry (every file
and the byte total; the workspace is sent whole). Every configuration error a real turn would
raise is raised by it, for free. It prices the mode it was given (a dry run of an `apply` turn prices an apply turn)
and writes nothing regardless. Its finding set is empty because it looked at nothing, so a host must
branch on `status` rather than on the count.

A successful turn's `_meta.reviewmesh` echo carries the **inclusion manifest** as `authority[]` — per document the source, `fullHash`, `embeddedHash`, `bytesEmbedded`/`bytesTotal`, `complete`, and any declared `ranges` (see [schema/authority-manifest.schema.json](schema/authority-manifest.schema.json)) — so a host can record exactly which intent the run was judged against. Findings the [write-path rule](prompts.md#authority--context-inputs) refused are marked `applyable: false` in the run projection; the host never writes them.

## Panel composition via `_meta.reviewmesh`

Every review turn composes its own panel — the host chooses *how* a review runs, not only *what* it
reviews. The same object works on `session/prompt` and on the compatibility `review` method:

```json
{
  "_meta": {
    "reviewmesh": {
      "panel": {
        "reviewers": [
          { "adapter": "codex-cli",   "model": "gpt-5" },
          { "adapter": "claude-code", "model": "sonnet", "effort": "high" }
        ],
        "author_remediator": { "adapter": "claude-code", "model": "opus" },
        "cross_check":       { "adapter": "codex-cli",   "model": "gpt-5" }
      }
    }
  }
}
```

- **`reviewers[1..16]`** — the [blind reviewer panel](review.md#the-blind-reviewer-panel), in the order
  given. A different `effort` is a different vantage and part of the seat's identity.
- **`author_remediator`** — **required**: the host-adjudication seat whose judgment becomes the accepted
  set. It is never defaulted.
- **`cross_check`**, **`verifier`** — optional. A turn without them does not run those steps, and the run's
  shape lists them as skipped.
- A single-slot role seat takes no `effort`; put an effort-bearing identifier in its `model`.

**The launched adapters, the caller's models.** A seat may name only an adapter this agent was launched
with (`--adapter`), and its CLI must be startable now. `model` is the exact identifier that adapter's CLI
accepts for the license it runs under: the calling agent looks it up, and aimesh passes it through verbatim.
There is deliberately **no field** for a binary path, launch arguments, or an adapter definition, and
because the namespace is strict-decoded, adding one is `-32602` rather than a silently ignored key.

Refusals are **pre-spend `-32602`** with a stable `reasonCode`, exactly like authority:

| Condition | `reasonCode` |
|---|---|
| A review turn with no `panel` | `panel_required` |
| `reviewers` empty | `panel_reviewers_empty` |
| More reviewer seats than the cap (16) | `panel_too_large` |
| A seat missing `adapter` or `model` | `panel_seat_incomplete` |
| No `author_remediator` | `panel_author_remediator_required` |
| `effort` on `author_remediator`, `cross_check` or `verifier` | `panel_role_effort` |
| The agent was launched with no adapter (the message names `--adapter`) | `panel_no_adapters` |
| A seat names an adapter the agent was not launched with (the message lists the launched set) | `panel_adapter_not_launched` |
| A seat's adapter CLI cannot be started now | `panel_adapter_unavailable` |
| An unknown key inside the reviewmesh namespace (a `profile` key, an array-shaped `panel`, a `path` on a seat) | `authority_doc_invalid` |

A duplicate seat identity is refused by the same resolver every surface uses, before any model call.

A successful turn's `_meta.reviewmesh` echo carries `panel[]` — the **executed roster**: one entry per
*requested* seat, in order, with `seatId`, `adapter`, `model`, `status`, `rounds`, `findings` and the
seat's `identityTier`. A host that asked for N seats can therefore verify that N ran.

## The ambient host-session trust boundary

When an ACP host runs the model itself and hands reviewmesh an already-in-progress session (rather than reviewmesh invoking an adapter it controls), reviewmesh never constructed that model invocation. There is no `modelArg` to compare and no runtime envelope to read — reviewmesh can only see what the host told it.

This makes an ambient host-session identity **trust-only**, recorded as `identityEvidence: ambient_host`, `verificationStatus: ambient_unverifiable`. This is a **permanent trust boundary**, not a gap waiting to close: capturing and verifying a real host does not promote ambient identity to `verified`, no matter how many hosts are tested. The only thing that can move it is the host itself supplying **structured or cryptographic attestation** — a scheduler/billing envelope, a signed inference receipt — strong enough to author the identity independently. Absent that, an ambient call stays `ambient_unverifiable` (or `unverified`) permanently.

Because of this, ambient identity is scoped narrowly by lane:

| Lane | Identity expectation |
|---|---|
| `reviewer` / `cross_check` / `verifier` | Use reviewmesh-invoked adapters where an identity signal can be read and audited (see [docs/model-identity.md](model-identity.md)). These lanes drive the findings, so they should not run on an unverifiable ambient model. |
| `author_remediator` / host adjudication | May use the ambient host session, but only when explicitly allowed by policy (`allowAmbientHostSessionUnverified`). |

Ambient safety therefore rests on reviewmesh's other guarantees, not on knowing which model actually ran: reviewer **containment** (isolated read-only copy; mutation halts the run), the **read-only reviewer** policy, **host-only writes** gated by the mode ceiling above, **no-write-after-cancel**, **provider diversity** (no single model decides the verdict), and **audit transparency**. Those hold regardless of whether the host's claimed model name is true.

## exploremesh as an ACP agent

```
aimesh explore acp [--adapter <name>[=<path>]]... [--framing newline|content-length] [--turn-timeout <dur>]
```

`aimesh explore acp` serves one **exploration** per `session/prompt`. `--adapter` (repeatable, or `AIMESH_ADAPTERS`) names the adapters a prompt's panel may use, with the same names and path expansion as `aimesh review acp`; the agent reads no aimesh configuration, and launched with no adapter it starts and refuses every exploration with a message naming `--adapter`. `--turn-timeout` is the total wall-clock budget for a single turn (default **10m**); when it expires the turn ends as a **halt** (`-32000`), not as `stopReason: cancelled` — `cancelled` is reserved for a cancel the host actually asked for. `aimesh review acp` behaves the same way, with the same flag and the same default. Framing, the 16 MiB frame cap, session ids, cancellation and `shutdown`/`exit` behave exactly as above — the transport is the same `meshcore/acp` code.

**There is no write-gating and no mode ceiling.** exploremesh reviews nothing on disk and the pipeline runs every model call in a fresh isolated work directory, so there is no workspace, no `report|patch|apply` mode, no permission ceiling, and no capability narrowing from the host's advertised capabilities. `session/new.params.cwd` is stored only so a later `session/resume` can round-trip it; it is never a run root.

### The task arrives via `_meta.exploremesh`

The ACP prompt text becomes the exploration **purpose**. Everything load-bearing beyond that travels under the sanctioned `_meta.exploremesh` extension point on `session/prompt`:

| Key | Meaning |
|---|---|
| `criteria` | **Required.** The non-empty list of criteria the responses must satisfy. Absent or all-blank is `invalid-params` — never silently invented. |
| `purpose` | Optional override; wins over the prompt text when a driver wants an exact purpose independent of the human prompt. |
| `priorContext` | Optional prior context (e.g. a previous exploration's result). |
| `mode` | Optional exploration mode (absent/blank → the default `map`). An unknown name is `invalid-params` listing the known modes. |
| `artifact` | The artifact under review. Optional at the protocol level, **required by whichever mode says so** (`challenge`). |
| `options`, `compareCriteria` | The fixed-space declarations for `--mode compare`. |
| `target`, `unit`, `horizon`, `conditioningEvent` | The fixed-space declarations for `--mode forecast`. |
| `panel` | **Required.** `{explorers: [{adapter, model, effort?}] (2..16), collator: {adapter, model, effort?}}` — the ACP analogue of the CLI's `--explorer`/`--collator` and of MCP's `panel`. Every adapter must be one the agent was launched with whose CLI can start now; `model` is the exact identifier that adapter's CLI accepts, passed through verbatim. There is deliberately no field for a binary path or launch argument. A prompt with no panel is `invalid-params` naming the corrected shape. |
| `canonicalizers` | Exactly two `{adapter, model, effort?}` seats, or absent (the host derives them). Held to the same launched-adapter rule. |
| `verifyReadiness` | `true` asks every distinct adapter/model/effort the panel names, before the run, whether it can do real work — one bounded one-token call each. A blocked agent halts the turn before the panel is paid for. **Spends**; `dryRun` prices it as a `readiness` stage in `shape.calls`. |
| `dumpRun` | `true` records the whole run (envelopes, raw outputs, prompts, the declared task, a versioned manifest) under `$EXPLOREMESH_ARTIFACT_DIR` — the ACP analogue of `--dump-run`. The response echoes `runCaptured` and the `runId`; the record is at `$EXPLOREMESH_ARTIFACT_DIR/<runId>`. The host **path** is never put on the wire. |
| `maxParallel` | Bounds how many explorers invoke their model CLI at once (absent → the whole panel). Wall clock only, never cost. |
| `dryRun` | `true` prices the turn instead of running it — the ACP analogue of `--dry-run` and MCP's `dryRun`. See below. **Mutually exclusive with `dumpRun`.** |

`dryRun: true` resolves everything and spends nothing: the task and round contracts, the terminal
contract, the collator's registration and the **canonicalizer derivation** all run, then the turn
stops **before the identity pre-flight** — an exploration's first model call — and ends with
`stopReason: end_turn` and `_meta.exploremesh.status: "planned"`, `dryRun: true` and a `shape`: the
panel, the resolved canonicalizers with their provenance, every stage in execution order with its
call count, the exact `modelCalls` total, and the exact round-1 prompt every explorer would receive.

Two differences from reviewmesh's `dryRun`, both structural rather than incidental. `modelCalls` is
**one number, not a range** — an exploration's round count is fixed by its mode contract, so only a
halt makes the real figure smaller. And it **proves nothing about reachability**: a review's static
preflight is free, exploremesh's pre-flight invokes, so the stop sits in front of it and the claim is
only that nothing left is a *configuration* question. It is refused with `dumpRun`, because a run
directory records an exploration and a dry run performs none. A host branches on `status`/`dryRun`,
never on the absent result.

A prompt composes from the adapters the agent was launched with; it can never supply an unnamed adapter, a
binary path, or a launch argument.

The `_meta.exploremesh` namespace is **strict-decoded**, like `_meta.reviewmesh`: an unrecognized key inside
it — a typo, or a `profile` or `count` key — is `invalid-params` naming the key, rather than a silently
ignored governance field.

Every one of these fails closed with `invalid-params` (-32602) rather than being clamped or coerced: missing criteria, an unknown mode, an unknown key, a missing panel, a panel outside 2..16 or naming an adapter the agent was not launched with (the message lists the launched set) or one whose CLI cannot start, `dryRun` combined with `dumpRun`, and any mode-specific task requirement the prompt did not meet.

### Method mapping (`aimesh explore acp`)

| ACP method | exploremesh behavior |
|---|---|
| `initialize` | ACP v1 result: integer `protocolVersion: 1`, `agentInfo` (`name: exploremesh`), `agentCapabilities` (`loadSession:false`; `promptCapabilities` text-only — no image, audio, or embedded context; no MCP; `sessionCapabilities.resume` **only** when the durable session store is configured), empty `authMethods`. A missing, non-integer or too-low client `protocolVersion` is a deterministic protocol error. |
| `session/new` | Allocates a session id, records `params.cwd`, and persists a durable record (best-effort) for cross-restart resume. |
| `session/prompt` | Resolves the required `_meta.exploremesh.panel` against the launched adapters, builds the task, and runs the exploremesh pipeline. Emits `session/update` progress during the run. Returns a `PromptResponse` (`stopReason: end_turn`, or `cancelled` after a cancel) whose `_meta.exploremesh` echoes back exactly what ran. |
| `session/update` | Outbound progress notification during a turn: a `SessionUpdate` with `sessionUpdate: "agent_message_chunk"` carrying the progress line as text, plus the structured audit-event fields (`eventType`, `level`, `message`, `timestamp`) under `_meta.exploremesh`. Context-gated, so none arrive after cancellation or after the terminal response. |
| `session/resume` | **Durable, reconnect-without-history.** Restores a session record persisted by an earlier process from `<AIMESH_HOME>/.aimesh/explore/acp-sessions/` and returns an empty `ResumeSessionResponse`. Method-not-found when no store is configured (which happens only if the home cannot be resolved). |
| `session/load` | **Method-not-found, intentionally** — its contract is to replay a prior conversation, and a stateless one-shot explorer has none. |
| `session/cancel` (and legacy `cancel` / `$/cancelRequest`) | Cancels the in-flight run for that `sessionId` at the run's `context`. exploremesh keys every run on a session, so a bare `cancel` also takes `sessionId`. |
| `shutdown` / `exit` | Graceful teardown and process exit. |

Any other method returns method-not-found; JSON-RPC **batch** requests are rejected.

### What comes back

A successful turn's `_meta.exploremesh` echo carries the applied purpose + criteria, the effective `mode`, the **panel that actually ran** (`panelSource: "adhoc"`, `explorers`, `collator`), whether prior context and an artifact were supplied (never their content), the run outcome (`formulationSource`, `collatorStatus`, `panelSize`, `dropped`), the mode's one-line `summary` plus that mode's own structured detail, `runCaptured` + `runId` when `dumpRun` was requested, and — for any count-bearing mode — a `governance` block. A **dry** turn instead carries `status: "planned"`, `dryRun: true` and the `shape` described above, and none of the run-outcome fields, because there was no run.

Alongside `canonicalizerSource` (who chose the canonicalizers) a canonicalizing run echoes `canonicalizerIndependence`: `distinct_models`, or `shared_model` when both canonicalizers ran one model behind two adapters. The pair is allowed — a panel is configured deliberately — so this echo is the whole safeguard: a merge held by two instances of one model is weaker evidence than one held across two models, and every corroboration count in the result rests on that difference.

That governance block is deliberately shaped so a driver cannot present a contested result as a settled one by inattention: alongside the claim count it carries `claimsByLabel`, `claimsWithheld`, `claimsContested`, `claimsHash`, `rulesVersion`, `countingPolicyHash`, `panelSize`, `respondents`, `quorumMet`, and the `partitionRevisionHash` — or, for a fixed-space mode, the explicit `"none — the space was declared before any explorer spoke"` instead of an absent field.

A halt is a JSON-RPC error with code **-32000**, whose `data` carries `exitCode` (the aimesh [halt taxonomy](architecture.md#halt-taxonomy) code), `haltClass`, and a structured `failure` breakdown including each dropped explorer and its reason.

### Budget caps

Bounded before any model subprocess is spawned: the `session/prompt` params payload is capped at **1 MiB**; a selected panel larger than **16 explorers** is refused; the whole turn is bounded by `--turn-timeout`. A second `session/prompt` on a session that already has a run in flight is rejected (`session busy`) rather than silently overwriting the first.

## Verifying against a real host

`go test ./...` needs no real ACP host — a fake harness and a real-subprocess harness (driving the actual agent binary over stdio, both framings) cover the protocol deterministically in both apps. Verifying interoperability with an actual IDE host is a separate, manual, opt-in procedure, because only a real host frames, initializes, and drives sessions the way it actually will in production.

The procedure below is written for `aimesh review acp`. For `aimesh explore acp` the shape is the same minus the workspace: skip the mode-gating step (8), and in step 6 send a `session/prompt` whose `_meta.exploremesh` carries a non-empty `criteria` list and a `panel` naming the launched adapters, then confirm the `_meta.exploremesh` echo names the mode and the panel that ran.

1. **Prerequisites.** Build the agent (`make dist` or `go build ./cmd/aimesh`); note `aimesh --version`. Have the host installed and signed in manually — do not automate GUI login or trust prompts.
2. **Point the host at the agent.** Configure the host to launch `aimesh review acp --adapter <name>` (add `--allow-writes` to exercise an apply turn, and `--framing content-length` if the host expects LSP framing; default is `newline`). Record the exact launch command the host uses.
3. **initialize.** Start a session from the host. Capture the host's `initialize` params and reviewmesh's response; confirm the response is ACP v1-shaped (integer `protocolVersion: 1`, `agentCapabilities`, `agentInfo`, `authMethods`, no legacy `capabilities`/`serverInfo`) and carries `_meta.reviewmesh.writes` and `diffAvailable`. Confirm capability narrowing behaviorally — e.g. a host that advertises no write capability should later see an `apply` request degrade to `patch`.
4. **session/new.** Confirm the host calls it and accepts the returned `sessionId`.
5. **session/resume** (optional). Restart the agent and have the host call `session/resume` with the prior `sessionId`; confirm an empty `ResumeSessionResponse` and that a following `session/prompt` (omitting `workspace`) runs against the restored cwd. (`session/load` should surface as method-not-found.)
6. **session/prompt (report mode).** Send a review request for a small workspace, by absolute path, with a `_meta.reviewmesh.panel` naming the launched adapter. Confirm a result with `mode`, `findings`, `runDir`, `writes`, and audit artifacts under `runDir`.
7. **session/update.** During step 6, confirm the host receives progress notifications tagged with the session id, and record whether it consumes or ignores them. Confirm none arrive after the terminal response.
8. **Mode gating.** Request `apply` from a host with no write capability. Confirm reviewmesh degrades to patch/report with a reason and performs no live workspace write.
9. **session/cancel.** Cancel a longer-running prompt mid-run. Confirm a `cancelled` result and no write after cancel.
10. **shutdown / exit.** Confirm graceful teardown and clean process exit.
11. **Errors.** Trigger an error (a missing workspace, an induced halt) and record how the host renders the JSON-RPC error / exit code / halt class.
12. **Capture and sanitize.** Save the raw transcript to a local, gitignored location. Sanitize it (see below) before it goes anywhere else, and only promote a minimal sanitized transcript to a committed fixture if it adds real verification value.

### Sanitize before capture

Any transcript captured from a real host may contain session ids, UUIDs, account/org/user identifiers, tokens, machine-specific home paths, or provider-internal metadata. Sanitize before saving or sharing a transcript: strip all of the above. Never commit a raw transcript. Do not automate a host's GUI setup, login, or trust prompts — if a verification step requires them, record the blocker and stop rather than scripting around it.

### Host coverage

Real-host verification is inherently per-host, per-framing work, and it is expected to lag the protocol surface. Rather than a running log, treat coverage as a standing checklist:

| Host / framing | Coverage |
|---|---|
| Any host, newline framing | Supported — verify per the procedure above before relying on it in production |
| Any host, Content-Length framing | Supported — verify per the procedure above before relying on it in production |
| Host fs-callback (read) transport | Supported for host-mediated materialization; specific host wire shape to be verified on provision |
| Host write-back callback transport | To be verified on provision |
| Ambient host-session model identity | Not a coverage gap — see [the trust boundary above](#the-ambient-host-session-trust-boundary); it never becomes `verified` regardless of which hosts are tested |

New hosts (or new framings on an already-verified host) should run the procedure above before being treated as production-verified; nothing here should be read as a status log of specific hosts tested on specific dates.

## See also

- [docs/mcp.md](mcp.md) — the MCP surface in both apps, and the CLI/ACP/MCP surface-parity tables
- [docs/review.md](review.md) — the `acp` command and reviewmesh's other surfaces
- [docs/explore.md](explore.md) — the exploration modes a `session/prompt` can select, and exploremesh's other surfaces
- [meshcore/README.md](../meshcore/README.md) — the `acp` package (transport) alongside meshcore's other governance primitives
- [docs/prompts.md](prompts.md) — the prompt/result contracts a `session/prompt` review turn runs under the hood
- [docs/model-identity.md](model-identity.md) — evidence tiers, including why ambient host-session identity stays `ambient_unverifiable`
- [docs/architecture.md](architecture.md) — where the ACP surface sits relative to the meshcore boundary
