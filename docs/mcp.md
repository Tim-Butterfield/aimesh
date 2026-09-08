# The MCP surface

Both apps run as local **[Model Context Protocol](https://modelcontextprotocol.io/) servers over stdio**, so
an MCP-speaking agent can drive a governed, blind, multi-model run and get back the **host-computed**
result:

- **`aimesh explore mcp`** — exploration. It takes no filesystem paths and edits no file the caller owns; the
  only thing it writes is its own run directory under `$EXPLOREMESH_ARTIFACT_DIR` (on by default,
  `--no-capture` turns it off). The evidence export, which is the one exploremesh command that writes where
  a human points it, is **not** exposed over MCP at all.
- **`aimesh review mcp`** — review, and (only when the operator enables it) **remediation**. It takes
  filesystem paths, and one of its tools writes. See [aimesh review mcp](#reviewmesh-mcp) below.

The protocol transport is shared: `meshcore/mcp`, a hand-rolled, domain-free JSON-RPC 2.0 server over the
newline-delimited framing MCP stdio uses (itself shared with `meshcore/acp`). Everything from
[Protocol surface](#protocol-surface) to [Error model](#error-model) describes that shared transport and
applies to both servers; the tools, schemas, job registry and admission governor are each app's own.

The sections up to [Security posture](#security-posture) use `aimesh explore mcp` as the worked example.

Related: **[acp.md](acp.md)** (the ACP surface), **[architecture.md](architecture.md)** (where the
surfaces sit), **[security.md](security.md)** (what leaves the machine).

## One server, both domains (`aimesh mcp`)

`aimesh mcp` serves **both** domains' tools over one stdio connection — the single entry a host
config needs:

```json
{ "mcpServers": { "aimesh": { "command": "aimesh", "args": ["mcp"] } } }
```

`--only review` or `--only explore` narrows it to one domain's tools. Narrowing is worth having: a
tool list costs a model attention on every call, and a review-only user should not carry explore's
tools. Beyond `--only` it accepts **every flag of the domains it serves** — naming one whose domain
`--only` excluded is refused rather than ignored, because `--allow-remediate` under `--only explore`
would otherwise read as a capability grant that silently did nothing.

**Every flag is launch-time, set once.** The capability grants (`--allow-remediate`,
and the trusted-root ceiling cannot be per-call: the caller is a model,
and one that could grant itself write access or widen its own filesystem reach would make the grant
meaningless. Everything a caller legitimately varies per run — the workspace, a narrowing `roots`,
the profile or panel, `waitSeconds`, `maxParallel`, `dryRun` — is a tool parameter, so **one
configured server serves many repos, folders and non-repos without editing a config or restarting
the host.**

`dryRun: true` is the one every calling model should know about: it resolves the plan, the panel and
the preflight, then stops before the first model call and answers with `status: "planned"` and a
`shape` object — the seats it would convene, the lanes it would call, the minimum and maximum model
calls the run is bounded by, a `payload` naming every file those calls would carry with the
byte total, and an `egress` list saying which destinations would receive that payload. Every
configuration error a real run would raise is raised by it, for free. A model
deciding whether a five-seat panel on a large tree is worth it has no other way to find out
cheaply, and `payload.bytes` is the figure that decides it: the workspace is sent whole, once per
seat, per round. Its `findings` array is empty because it looked at nothing, never because the tree
was clean — branch on `status`, not on the count.

A domain whose configuration is incomplete refuses at launch rather than serving a half-built tool
list, so a composed server needs both domains configured; use `--only` while the other is still
unconfigured.

## At a glance (`aimesh explore mcp`)

```bash
aimesh explore mcp [--roster <path>] [--protocol dual|legacy]
                [--wait-seconds <n>] [--turn-timeout <dur>] [--no-capture]
```

The per-domain commands remain, and serve exactly one domain each. Example client entry:

```json
{ "mcpServers": { "exploremesh": { "command": "aimesh", "args": ["explore", "mcp"] } } }
```

The server binds **at startup** to exactly the configuration a no-flag `aimesh explore run` binds to: the
default panel, the selectable profile set, and the configured adapter set. Nothing on the wire can change
any of it.

## Protocol surface

| Method | Behavior |
|---|---|
| `initialize` | Negotiates and **echoes** the client's `protocolVersion`. Declares `tools` (`listChanged: false` — the tool set is bound at startup), `logging`, and `resources` (`subscribe: false`, `listChanged: false`). Reads the **client's** `capabilities.roots`. Returns `serverInfo` and the cross-tool `instructions`. |
| `notifications/initialized` | Required. Until it arrives, every request except `ping` is refused with `-32002`. It is also what starts the `roots/list` round trip, when the client declared the capability. |
| `resources/list` | Every artifact a finished run published, addressed by an **opaque run-scoped URI**. One page, no cursor. |
| `resources/read` | Fetches one published artifact. A URI this server did not publish is `-32002` (the spec's *Resource not found*). |
| `roots/list` **(server → client)** | Issued after `notifications/initialized` when the client declared `capabilities.roots`, and again on every `notifications/roots/list_changed`. The answer is **intersected** with the server's launch-time roots. |
| `notifications/roots/list_changed` | Triggers a **re-fetch**. The notification carries no roots, so acknowledging it without re-asking would leave a withdrawn set in force. |
| `ping` | Always allowed, including before initialization (it is how a host proves liveness during a slow handshake). |
| `tools/list` | Accepts **and honors** `cursor`. With the shipped tool count everything comes back in one page with no `nextCursor`; an unrecognized cursor is `-32602`, never silently ignored. |
| `tools/call` | Runs a tool. Concurrent with other calls, so cancellation can reach an in-flight one. |
| `logging/setLevel` | Sets the minimum level for `notifications/message`. All eight RFC 5424 levels; an unknown level is refused. |
| `notifications/progress` | Emitted **only** when the caller supplied `_meta.progressToken`. |
| `notifications/cancelled` | Correlated by JSON-RPC request id. Cancels the call *and* the run it started. |

**The table above is the `2025-06-18` (LEGACY) method surface.** Both servers are now **dual-era**: they
also implement `2026-07-28`, whose surface is different enough that it gets its own table below.

### The two eras, and how a request picks one

`2026-07-28` does not extend the legacy surface, it **deletes** most of it. There is no `initialize` and no
`notifications/initialized` — the session is gone, so there is no pre-initialization state and no `-32002`
either — no `ping`, and no `logging/setLevel`. Every request instead carries its own protocol context in
`params._meta`.

| Method | Modern (`2026-07-28`) behavior |
|---|---|
| `server/discover` | **MUST-implement, and the stdio backward-compatibility probe.** Returns every revision this server implements, **modern first** (`2026-07-28`, `2025-06-18`, `2025-03-26`, `2024-11-05`), the capabilities, `instructions`, `_meta.serverInfo`, and caching hints. It is **era-neutral: it never latches the process.** |
| `tools/list`, `tools/call`, `resources/*` | As on the legacy era, plus the modern result envelope: `resultType: "complete"`, `ttlMs` + `cacheScope` on the cacheable operations, and `_meta["io.modelcontextprotocol/serverInfo"]` on every result. |
| `initialize`, `notifications/initialized` | Not implemented. Once the process has latched modern, `initialize` gets a JSON-RPC error **naming our supported versions** — legacy clients have no fall-forward mechanism, so that message may be the only diagnostic a user ever sees. |
| `ping`, `logging/setLevel` | `-32601`, with a message naming the replacement. Both were **removed** in this revision. |
| `notifications/message` | **Never emitted.** See [No log notifications on `2026-07-28`](#no-log-notifications-on-2026-07-28). |
| `notifications/progress` | Unchanged, and still the live in-flight channel. Requires `_meta.progressToken`. |
| `roots/list` (server → client) | Not issued: the revision forbids a server to write a JSON-RPC *request* to stdout, and there is no MRTR implementation here. Legacy only. |

**Required `_meta` on every modern request** (`basic/index`): `io.modelcontextprotocol/protocolVersion` and
`io.modelcontextprotocol/clientCapabilities` are **required** — a request missing either is `-32602`;
`io.modelcontextprotocol/clientInfo` is optional and **advisory only** (nothing branches on it); and
`io.modelcontextprotocol/logLevel` is optional, **accepted and ignored**. A `protocolVersion` this server
does not implement modernly is `-32022` (`UnsupportedProtocolVersionError`) carrying `data.supported`.

**The era latch.** A process commits to ONE era and stays there:

- A valid `initialize` latches **legacy**; a fully valid modern request latches **modern**.
- A request latches **only after it has passed every step that could reject it** — envelope, method,
  required `_meta`, supported version, and the method's own params — and the latch is a single atomic
  compare-and-set. So a malformed opener cannot strand the well-formed caller behind it, and **`-32022`
  never latches**: the client retries from our advertised list and *that* request chooses the era.
- A loser of the race does not error on the race; it re-reads. Two concurrent modern openers both succeed.
- Crossing eras afterwards is a **teaching refusal**, not a silent behavior change: a modern request on a
  legacy-latched process is `-32600` explaining that a new process is needed.

**`--protocol dual|legacy`** (default `dual`, on both servers). `legacy` makes the process a
pre-`2026-07-28` server *in every observable respect*: no modern `_meta` is parsed, no modern method is
reachable, and **`server/discover` answers `-32601` with no `result` member of any kind**. That last part
is the mode's whole point — per `basic/transports/stdio`, a `DiscoverResult` **of any content** identifies
a modern server, so a dual-era client receiving one would stop looking for `initialize`, find no modern
version it could use, and fail with this process's legacy surface sitting right there unreachable.
Answering the probe as an unknown method puts the process in the specification's own
"Dual-era client / Legacy server" row, whose outcome is *Works*. A legacy-pinned process is a documented
compatibility fallback, **not** a conformant `2026-07-28` deployment, and the `explore_doctor` tool reports it.
There is deliberately no `--protocol modern`: it would strand legacy hosts and buys nothing an operator
cannot get by not sending `initialize`.

Deprecated features carry the spec's **twelve-month floor**, so `roots`, `sampling` and `logging` stay in
the specification for at least that long. Our own legacy-era removal is gated on adoption, not on a date:
it is tracked as `MCP26-SUNSET`, the id every `SUNSET-PATH` marker in the code cites (see
[CONTRIBUTING.md](../CONTRIBUTING.md)).

### Version negotiation is a clean refusal, not a downgrade

On the legacy era the client's version is echoed back **verbatim** when this server implements it. When it
does not, the handshake is refused with `-32602` naming the supported versions. The specification also
permits answering with the server's own latest version and letting the client decide — this server does
not, because that hides a mismatch inside a *successful* handshake, which is the silent-degradation shape
the rest of the codebase refuses. `initialize` never echoes `2026-07-28`: that revision has no handshake,
so answering the handshake with it would name a contract the caller cannot reach through the call it made.

### No log notifications on `2026-07-28`

**A modern client that sets `_meta.io.modelcontextprotocol/logLevel` receives nothing, and that is a
decision rather than a defect.** The Logging feature is **Deprecated** as of `2026-07-28`; the deprecation
names two migrations, and one of them — "log to `stderr` for stdio transports" — is what these servers
already do through `Server.Diagnostics`, with a real-binary stdout-purity test proving the separation.
Building a per-request log channel now would be *new adoption of a deprecated feature*, which this design
refuses elsewhere (Roots) for the same reason.

So on the modern era: `Call.Log` is a no-op, **zero** `notifications/message` frames are written at any
point in a request's life, `server/discover` advertises **no** `logging` capability (declaring the
capability that gates a notification we will never send would be a false advertisement), and an
unrecognized `logLevel` value does **not** refuse the request — refusing a whole `tools/call` over the
spelling of a field this server never reads would be hostile, and would make the non-adoption more visible
than adoption would have been. That last point is a SHOULD on `server/utilities/logging` we deliberately do
not follow, and it is stated here rather than quietly dropped.

Nothing governance-relevant is lost, because no governance-relevant fact has ever been allowed to exist
only in a log notification. In-flight visibility survives on `notifications/progress` (not deprecated),
on `explore_run_status` / `explore_run_result` (which are *responses*), and on stderr.

**The legacy era is untouched**, including the post-response log lines a backgrounded run still emits
there. An unconditional change would have silently removed lines a legacy client still expects — an
observable change to the one surface this migration must leave alone.

### Trace context (`traceparent` / `tracestate` / `baggage`)

`basic/index` reserves three **unprefixed** `_meta` keys for OpenTelemetry trace-context propagation, as
an explicit exception to its own prefix rule: "the keys `traceparent`, `tracestate`, and `baggage` are
reserved for OpenTelemetry trace context propagation. When present, their values **MUST** follow W3C
Trace Context and W3C Baggage formats respectively."

A modern request that carries them has them **written into the run record**, verbatim — `run-state.json`'s
`trace` object for `aimesh review mcp`, `manifest.json`'s for `aimesh explore mcp`. That is the whole feature: a
run directory that can be correlated with whatever traced the caller. A request that carries none writes no
`trace` key at all, so every run record without one is byte-identical to a record written before the field
existed.

**They are carried and never interpreted.** Nothing branches on a value, and a malformed `traceparent` is
recorded exactly as it arrived rather than refused. The format MUST above binds the **sender**, and the
same page says of every reserved `_meta` key that "implementations **MUST NOT** make assumptions about
values at these keys" — so validating one here would be this server asserting a meaning for a field it
does not own. On the legacy era the keys are not part of the revision's `_meta` contract; a legacy client
that sends them anyway is not refused, and nothing reaches the record.

**What this does NOT give you, said in the record itself.** The `trace` object carries
`"measures": "correlation_only"`. No run in either app records token counts or cost — a real and
documented observability gap — so a reader who finds a trace id must not conclude the run was metered.
The constant exists so that the day a run *does* record spend, the value changes and a consumer can tell
the two generations of the record apart rather than inferring it from which fields happen to be present.

### Progress

Progress notifications require a caller-supplied token. Without one they are not merely unwanted, they are
**unroutable** — and inventing a token is worse than staying quiet. The token is echoed exactly as sent.

Progress is a **monotonic fraction of run phases complete** (`panel_frozen` → `formulate_*` →
`synthesize_*` → complete), not a per-item counter: a counter that resets when a phase is retried goes
backwards, and a client that sees progress go backwards cannot distinguish a retry from a bug. A
non-increasing value is dropped by the transport rather than sent.

The progress sink is live **only for the inline wait**. Both servers disarm it on every return path — when
the run finished in time, when it outlived the budget and came back `running`, and when the client
cancelled — so a run that keeps going after its starting call was answered stops emitting progress against
that request. On the **legacy** era its events continue to reach `notifications/message`, which is not
request-correlated; on **`2026-07-28`** they reach nothing, because that era emits no log notifications at
all. Both halves are asserted by test, with the legacy half as the control — an assertion that "no log
frames arrived" is worth nothing unless the same harness is shown to see them when they exist.

### Cancellation

`notifications/cancelled` cancels the handler's context and, for a run-starting tool, the **run** — the
pipeline aborts, the subprocess tree is torn down, and nothing further is spent. The cancelled request
receives **no response**: on `2025-06-18` the server **SHOULD NOT** send one and the client is told to ignore
a late one anyway, and under `2026-07-28` over stdio it **MUST NOT** send any further message for that
request at all. The receipt therefore lives elsewhere and always exists: the
run record on disk, and the run itself, which stays reachable through its `runId` and through the caller's
`idempotencyKey` in state `cancelled`.

### Wire tolerance

JSON-RPC batching existed in the `2025-03-26` revision and was removed in `2025-06-18`. An **array** on the
wire is refused as one invalid request; it never crashes the reader, because the client that sent it is
still owed an answer it can act on. Unparseable input yields `-32700` and the session survives.

### Stdout purity

These servers spawn model CLIs, so the protocol stream is guarded in three places:

1. `meshcore/mcp` writes JSON-RPC frames and nothing else to its output stream, and exposes a separate
   `Diagnostics` sink so server-side logging has an explicit non-stdout home.
2. **Both** servers **repoint `os.Stdout` at stderr** before serving; the protocol stream is the writer
   captured beforehand. Any stray print — from this process, from a library, or from a child that
   inherited the descriptor — lands on stderr instead of corrupting a frame.
3. meshcore's adapters capture child stdio explicitly (`cmd.Stdout` is a buffer, never the parent's).

**Test coverage.** The real-binary stdout-purity test — driving the built binary over real stdio
through a full run with progress and log notifications interleaved, asserting that every stdout line
parses as JSON-RPC 2.0 — exists for **both** servers
(`exploremesh/internal/surface/mcp/subprocess_test.go`,
`reviewmesh/internal/surface/mcp/subprocess_test.go`). The reviewmesh one launches with
`--allow-remediate` on purpose: that launch prints a capability banner *before* serving, so the test
also proves the banner lands on stderr rather than in a frame.

### Conformance

**Both** surfaces are exercised end to end by the **official MCP Go SDK client**
(`github.com/modelcontextprotocol/go-sdk`) as a **test-only** dependency: `initialize` →
`notifications/initialized` → `tools/list` (including paged, with a cursor) → `tools/call` success →
`tools/call` `isError` → protocol error → progress with **and** without a token → cancellation →
`logging/setLevel` → `ping`. `go build` pulls none of it; either shipped binary's dependency set is
unchanged (`GOWORK=off go list -deps ./cmd/...` contains no `modelcontextprotocol` entry).

**Which revision this certifies, and by whose client.** There are now **two** transcripts, and they are
not equally strong. State the difference rather than average it:

| Era | Client that walks it | Where |
|---|---|---|
| Legacy (`2025-06-18`, `2025-03-26`, `2024-11-05`) | the **official MCP Go SDK client**, `github.com/modelcontextprotocol/go-sdk v1.4.0` (test-only) | `*/internal/surface/mcp/conformance_test.go`, `subprocess_test.go` |
| Modern (`2026-07-28`) | a **hand-written** client, over the real binary and real stdio | `*/internal/surface/mcp/modernsubprocess_test.go` |

**The modern transcript is the weaker of the two, and it is weaker on purpose.** A hand-rolled server
checked only by a hand-rolled client proves that two pieces of the same author's understanding agree,
which is not the property anyone needs. An SDK release that speaks `2026-07-28` **does exist**
(`go-sdk v1.7.0` sets its latest protocol version to `2026-07-28`). It was not adopted here because Go
admits one version of a module per build: upgrading would have replaced the **legacy** conformance client
too, and that release's supported set is `{2026-07-28, 2025-11-25}` — neither of which this server's
`initialize` accepts. The upgrade would therefore have voided the byte-identical legacy transcript, which
during the sunset window covers the only clients there are, in order to strengthen a proof about the era
that has none yet. The trade is recorded, not hidden: **the SDK upgrade is owed at legacy removal**, and
until then the modern era's conformance claim rests on a client written alongside the server.

What the modern transcript does assert against the shipped binary: the probe's advertised versions and
absent `logging` capability, the result envelope on every method, `doctor.protocolMode` /
`doctor.protocolEra`, `-32601` for both removed methods, the `initialize`-after-modern refusal naming our
versions, and the two MUST NOTs below.

**The two MUST NOTs, asserted over every frame the real process wrote to stdout:**

1. **No frame carries both an `id` and a `method`.** A frame with both is a server-initiated *request*,
   and `basic/transports/stdio` says "The server **MUST NOT** write JSON-RPC *requests* to `stdout`."
   `Server.Request` refuses outright once the process has latched modern, at the write function rather
   than at its one caller — a mechanism that is safe only because today's single caller checks first is
   not safe, it is lucky.
2. **Zero `notifications/message` frames**, with `_meta.logLevel` absent, present-and-valid and
   present-and-malformed, before and after every response.

Both run against **both** binaries. The legacy transcript is re-run unchanged and must stay
byte-identical.

The shared transport (`meshcore/mcp`) is the same code both servers run, but each transcript is walked
against that server's own **tool layer** — so reviewmesh's tool list, its `destructiveHint` on the write
tool, and its cancellation settlement are read back through a client nobody here wrote. reviewmesh keeps
its in-repo client too: that one asserts the reviewmesh *contract* (which tools exist, what they refuse,
what a write may do), which is a different question from protocol conformance.

## Job-shaped execution

Common MCP client request timeouts are around **60 seconds**. A real panel run is minutes. A synchronous
call would be killed **mid-spend**, with the subprocesses still running and no handle to reach them.

So every run-starting tool is job-shaped:

- `waitSeconds` (default **25**, max **120**) is the **inline** budget. If the run finishes inside it, the
  full result comes back inline.
- Otherwise the call returns `{runId, state: "running", panel: {...}}` — and the panel echo is there so the
  caller can report honestly what it started before it has an answer.
- `explore_run_status` polls; `explore_run_result` fetches. Re-reading a finished run is free and never re-runs it.

`waitSeconds` is **not** the run's own timeout. The run's wall clock is `--turn-timeout` (default 10
minutes), set by the operator at launch, not by the caller.

### Run registry

Runs are held per server process, bounded two independent ways because they fail differently:

| Bound | Default | What it protects |
|---|---|---|
| retention | 50 finished runs / 1 hour | memory; a **running** run is never evicted (losing the handle would leave a live subprocess tree with nothing able to cancel it) |

**There is no lifetime run cap.** A cumulative "this process may start N runs ever" bound is cleared
by stopping and starting the server, so it was never the spend ceiling it read as — and a limit whose
guarantee evaporates on restart is worse than none, because it invites an operator to rely on it. The
in-flight bound is not cumulative, so there is nothing for a restart to reset. How much a client
spends is governed by what it is instructed to ask for, and by the panel it composes per call.

A `runId` that has been evicted comes back as an `isError` result with `reasonCode: "unknown_run_id"` — a
legitimate outcome of the retention bound, not a malformed request.

**The registry lives and dies with the server process.** It is in-memory: when the stdio transport reaches
EOF the server cancels every in-flight run and the records are gone. So `runId` polling survives a dropped
*request* (which is the case the job shape exists for) but **not** a dropped *connection* or a restarted
server — an MCP server is normally a subprocess of its client, so closing the client ends the runs. The
durable record of a run is the run directory on disk, not the registry.

**The `runId` itself is durable, even though polling it is not.** A `runId` **names the run's own
directory** under the artifact directory, so a completed `report` run's accepted set stays readable at that
id after the entry is evicted and after a restart. That is what `review_remediate {fromRun}` reads when the
registry no longer holds the handle — see [One form: `fromRun`](#one-form-fromrun). `explore_run_status` and
`explore_run_result` are *not* backed by disk and still answer `unknown_run_id` for an evicted run: they report a
run's live state, which is exactly the thing that does not outlive the process. The id is still opaque —
it is not a path and it discloses nothing about this host.

### The `io.modelcontextprotocol/tasks` extension (modern era, opt-in per client)

On `2026-07-28` this server implements the `io.modelcontextprotocol/tasks` extension as a **projection
over the run registry above**: `taskId == runId`, always. No second identifier is minted and no second
execution model exists.

**How it engages.** The server advertises `capabilities.extensions["io.modelcontextprotocol/tasks"] = {}`
in `server/discover`. A client opts in **per request**, by putting the same key in
`_meta["io.modelcontextprotocol/clientCapabilities"].extensions`. A client that does not opt in sees
exactly the job shape described above — the advertisement in `server/discover` is the **only** wire
difference the extension makes to it.

**What changes for a client that does opt in.** When a run outlives `waitSeconds`, the `tools/call`
answers with a `CreateTaskResult` (`resultType: "task"`) carrying the run id as `taskId`, instead of
`{runId, state: "running"}`. Poll it with `tasks/get`. A run that finishes **inside** `waitSeconds`
still returns its full result inline — the extension leaves that choice to the server per request, and
a finished result beats a handle to it.

| Run state | Task `status` | Notes |
|---|---|---|
| `running` | `working` | `ttlMs` is `null`: a running run is never evicted, so there is no lifetime bound to advertise |
| `complete` | `completed` | `result` is what `explore_run_result` returns |
| `complete` + `outcome: "partial_refusal"` | `completed`, with `isError: true` inside `result` | writes committed **and** at least one finding refused |
| `halted` | `completed`, with `isError: true` inside `result` | **not** `failed`. `failed` is for a JSON-RPC protocol fault; a governed halt is a completed request whose tool failed |
| `cancelled` | `cancelled` | no `result` field — the durable answer is the run directory's receipt |

**`ttlMs` is task lifetime, not a caching hint.** On a terminal task it carries the registry's real
remaining retention and decreases on each poll. Two caveats a client should know:

- Retention is bounded by **count as well as time** (50 finished runs, oldest first). A busy server can
  therefore discard a task *before* its advertised `ttlMs` elapses.
- A task past its advertised `ttlMs` is answered `-32602`, not served with `ttlMs: 0`.

**`taskId` is durable for the life of the server process, not across restarts of it.** The registry is
in memory. A `taskId` from a server that has since restarted is answered `-32602` — the honest answer,
not a silently-empty task. The extension's overview describes a task id as a durable handle that
survives a *client* restart; that much holds, because the server outlives the client. Surviving a
*server* restart would need on-disk run records and is out of scope.

**Cancellation.** Use `tasks/cancel`. `notifications/cancelled` **must not** be used for task
cancellation and cannot achieve it here: once the `CreateTaskResult` is returned, the originating
`tools/call` is complete and its request id is no longer in flight, so a cancellation naming it reaches
nothing. The acknowledgement to `tasks/cancel` means **the cancel signal has been delivered to the run's
context — nothing more.** It is not a promise about the terminal status. What this server does
guarantee is stronger where it matters and weaker where it does not:

> **Binding with respect to writes, cooperative with respect to terminal status.** A cancellation
> delivered *before* a governed write window is entered guarantees that no byte is written to the live
> workspace by that run, and the task reaches `cancelled`. A cancellation that arrives *after* the
> window was entered is acknowledged identically, the commit completes, and the task reaches
> `completed`. The extension explicitly permits the second.

**We decline `notifications/tasks`.** Task notifications ride `subscriptions/listen`; the extension
makes them optional and polling the default. We advertise `listChanged: false` everywhere and adding a
push channel for one extension would be the only subscription surface in either server. Use
`pollIntervalMs` (2000 ms).

**We never emit `input_required`.** This server issues no mid-flight request, so no task reaches that
status and `tasks/update` is an empty acknowledgement that ignores every `inputResponses` key.

#### One declared deviation: `-32021`, not `-32003`

A `tasks/*` request from a client that never declared the extension is answered
**`-32021` (`MissingRequiredClientCapabilityError`)** with
`data.requiredCapabilities: ["io.modelcontextprotocol/tasks"]` — **not** the `-32003` the extension
specifies. This is deliberate, and both texts were read before choosing:

- The extension MUSTs `-32003` for exactly this condition.
- Core `2026-07-28` `basic/index` MUSTs `-32021` for exactly this condition, and specifies the
  `data.requiredCapabilities` shape.
- Core `basic/index` §Error Codes additionally says receivers **MUST NOT** assume any specific meaning
  for codes in that sub-range apart from `-32002`. So emitting `-32003` to a conforming modern client
  would not merely break a SHOULD NOT — it would **fail to communicate** the thing the extension wants
  communicated.

Between two contradictory MUSTs we follow the stable, versioned core specification over an extension
whose own repository is labelled experimental. **The concrete cost: a client hard-coded to `-32003`
will not recognise our error.** That is on the record rather than hidden.

**Accepted risk.** The extension self-describes as experimental and "may change significantly or be
discontinued". We adopted it anyway, deliberately, because the blast radius is one adapter layer: the
projection is deleted and the job shape — which is not going away, since the extension is opt-in per
client — is already there and already tested.

## Tools

| Tool | Spends? | Purpose |
|---|---|---|
| `explore` | **yes** | every mode, selected by the required `mode` parameter |
| `explore_list` | no | configured adapters, profiles, modes, admission limits |
| `explore_doctor` | no | static adapter readiness |
| `explore_run_status` | no | poll a run |
| `explore_run_result` | no | fetch a run's full result |

The mode split is deliberate. `challenge`/`compare`/`forecast` each require declarations the others do not,
and a **flat, all-required** schema is filled correctly on the first try far more often than a
conditionally-required one. `explore` covers exactly the modes whose parameter shape is the common one.

### Annotations

| Tool | `readOnlyHint` | `openWorldHint` | `idempotentHint` | `destructiveHint` |
|---|---|---|---|---|
| `explore*` | **false** | **true** | **false** | false |
| `explore_list`, `explore_doctor`, `explore_run_status`, `explore_run_result` | **true** | false | true | false |

The explore tools spawn processes and spend money, so they are **not** read-only. A host uses these hints
to decide whether to ask the human first; mis-marking a spending tool read-only is the single most
dangerous mistake an annotation can make. Annotations are advisory — nothing server-side keys off them.

### Common parameters

Every run-starting tool takes:

| Field | Required | Notes |
|---|---|---|
| `purpose` | ✓ | what the panel is asked to explore |
| `criteria[]` | ✓ | ≥1 non-blank entry. **Never invented** on the caller's behalf: a run without criteria is refused |
| `priorContext` | | free text |
| `panel` | | strict `oneOf` — see below. Omitted: the server's default panel |
| `waitSeconds` | | 1..120, default 25 |
| `maxParallel` | | ≥1. Bounds how many explorers invoke at once — wall clock only, never cost |
| `dryRun` | | resolve everything, spend nothing — see below |
| `idempotencyKey` | | ≤200 chars. A repeat returns the **existing** run |

**`dryRun: true`** answers with `dryRun: true` and a `shape` block instead of a result: the panel, the
resolved canonicalizers and their provenance, every stage in execution order with its call count, the
exact `modelCalls` total, and the exact round-1 prompt every explorer would receive. Every
configuration error a real run would raise is raised by it, for free — including a dual-canonicalizer
plan that cannot yield two independent identities, which is invisible until something tries to pick
them. It writes no run directory, because there is no exploration to record.

Two ways it differs from `aimesh review mcp`'s `dryRun`, both structural:

- **`modelCalls` is one number, not a min/max pair.** A review iterates until adjudication converges;
  an exploration's round count is fixed by its mode contract and every other multiplier is settled
  before the run starts. Report the number as given — only a halt makes it smaller.
- **It proves nothing about reachability.** A review's static preflight is free, so its shape can
  promise every adapter answered. exploremesh's first stage is the identity **pre-flight**, which
  *invokes*; the stop sits in front of it. The claim is that nothing left is a configuration
  question, not that the collator replied.

The call's `state` is still `complete` — the *call* finished — so branch on `dryRun`, not on `state`,
and never read the absent result as "the panel ran and found nothing".

**`mode` is required, and it decides which other parameters are.** This mirrors the CLI, where
`aimesh explore run --mode <name>` plus mode-specific flags is likewise one entry point.

| `mode` | additionally requires |
|---|---|
| `map`, `synthesize`, `catalog`, `shortlist`, `ai-collab` | — |
| `challenge` | `artifact` (inline — this server reads no files) |
| `compare` | `options[]` (≥2, unique) + `comparisonAxes[]` (each `{name, direction?, role?, weight?}`) |
| `forecast` | `target`, `unit`, `horizon` (+ optional `conditioningEvent`) |

A parameter belonging to a **different** mode is **refused, not ignored** — passing `artifact` to a
`map` run fails rather than quietly running a map that never saw it. When the four modes had four
tools this came free from per-tool decoding; with one tool it is an explicit check, and the error
names the mode that *does* take the parameter.

The requirement is declared in four places on purpose — the input schema (as a `oneOf` discriminated
union stating both the requires and the forbids), this tool's description, the server's `initialize`
instructions, and the agent guide. Conditional subschemas are unevenly supported across clients, so a
rule expressed only in the schema can fail to reach the calling model; a test asserts the four agree.

`comparisonAxes` is deliberately **not** named `criteria`: `criteria` are the constraints the whole
exploration must satisfy, while the axes are what the option set is measured on. Two same-named arrays with
different meanings are a first-try error generator.

`mode` is required on `explore` rather than defaulted from the profile, because a per-profile default would
make the same call mean different things on different installs — a difference a machine caller cannot see.

### `panel`: select or compose

```jsonc
// branch A — select a configured profile
{ "panel": { "profile": "deep", "count": 3 } }

// branch B — compose an ad-hoc panel by identifier
{ "panel": {
    "explorers": [ {"adapter": "claude-code", "model": "sonnet"},
                   {"adapter": "codex-cli",   "model": "gpt-5", "effort": "high"} ],
    "collator":  {"adapter": "claude-code", "model": "opus"} } }
```

A strict `oneOf` with `additionalProperties: false` on both branches. Supplying both is refused: it asks
for two different panels.

- **Compose, never configure.** Composition resolves **fail-closed** against the adapter set bound at
  **startup**. A call can select or re-arrange the configured seats; it can never introduce an adapter, a
  binary path or a launch argument. The refusal names the configured set, so the corrected call follows
  from the error.
- 2..16 explorers plus exactly one collator. The cap is a spend control and is **never clamped** — a
  request for 20 seats fails; it does not quietly run 16.
- Duplicate `(adapter, model, effort)` triples are rejected: an identical triple adds no independent
  vantage. A different `effort` *is* a different vantage, so it is part of the seat's identity.
- `count` takes the top-N by the profile's **preference order** and is never clamped either (requested =
  executed, or the call fails).

Schemas are advisory to a client, so the server enforces all of this **itself**: arguments are decoded with
unknown fields rejected, which is what makes `additionalProperties: false` a real boundary rather than a
suggestion.

## Results

Every terminal result carries **both** channels: `structuredContent` (machine-readable, validating against
the tool's declared `outputSchema`) and a short human rendering in `content`. Some clients show the model
only the text; some pass only the structured payload to a validator. A result living in one channel would
be invisible to half of them.

There is **no** `summary` vs `full` detail parameter. The result is always complete.

> **Where that is enforced.** Neither server validates its own `structuredContent` at send time; **both**
> validate it in the build. Each has a matched pair of tests: one walks every result builder at every
> state it can produce and judges the payload against the literal declared schema, and a second re-reads
> each tool's `outputSchema` back off `tools/list` and validates real calls against it — so a payload
> that matches no branch fails the build rather than reaching a client. The pair exists because the two
> did drift: five payloads across the two servers matched no branch of the very `oneOf` their tool
> advertised (on `aimesh explore mcp`, a **cancelled** call's result carried the *running* shape with
> `state` overwritten, and `explore_run_result` on a **still-running** run omitted the `panel` the running branch
> requires). Both servers are validated against the same subset validator — literally one package,
> [`meshcore/jsonschema`](../meshcore/jsonschema) — so neither can be held to a looser standard than
> the other.

#### `--strict-schema`: the same check, opt-in, at send time

Build-time validation cannot speak for a payload the tests do not construct — a field whose value depends
on a provider's output, a branch reached only by a rare halt. So both servers accept `--strict-schema`
(or `AIMESH_MCP_STRICT_SCHEMA=1`), which validates every `structuredContent` against its own tool's
declared `outputSchema` **immediately before the frame is written**.

- **On a violation the call fails loudly.** The result becomes an `isError` internal-error naming the
  offending tool and quoting the validator, and it carries **no** `structuredContent` — the only
  structured payload available is the one just judged invalid, and attaching it would ship exactly what
  the check refused. A violation is a *server bug*, and the message says so: the same call succeeds with
  the check off, returning a payload a strict client may reject.
- **It is OFF by default, because it is not free.** Each tool's schema is compiled once and cached, but
  every result is then re-marshalled through `encoding/json` and walked against the schema. The cost
  scales with the **payload**, not the schema: a review result carrying hundreds of findings with their
  per-seat provenance is the expensive case, and it is paid on every call rather than once in CI. On a
  surface whose calls take minutes and cost money it is negligible; on `explore_run_status` polling in a loop it
  is not nothing.
- **The env var is not a convenience alias.** An MCP server is normally launched by a *host* from a
  config file, and the common hosts let an operator set `env` for a server entry but not extra argv. A
  diagnostic switch the people who most need it cannot reach would be a switch in name only.

### Fetching what a run produced (`resources/*`)

Both servers publish their finished runs' artifacts as MCP **resources**. On `aimesh review mcp` that is what
makes `output: "patch"` an output mode at all rather than a receipt for something unobtainable: the run
directory is deliberately withheld from every payload, and stdout is the JSON-RPC transport, so before
this existed a caller held a run-record-relative file name it could never resolve.

**The URI is opaque and run-scoped:**

```
aimesh://run/<runId>/<artifact>
```

Two identifiers the server resolves internally against a table it built itself. `<runId>` is the same
opaque id the tool result returned; `<artifact>` is a **logical name** (`patch`, `manifest`, …), not a
file name and not a path. No host filesystem path appears in a URI, in a `resources/list` row, or in a
`resources/read` result — that was a deliberate decision and it is preserved rather than traded away for
retrievability.

**What each server publishes:**

| Server | Artifacts |
|---|---|
| `aimesh review mcp` | `patch` — the complete unified diff a `review_remediate` produced. Its digest is `receipt.patchSha256`. |
| `aimesh explore mcp` | `manifest` plus every artifact the run's own manifest indexes (envelopes, ledger, dropped-explorer raw bodies, …). Requires capture, which is on by default; `--no-capture` leaves nothing to publish. |

**How a patch is collected.** A patch-mode remediation now returns three things that agree: a
`resource_link` content block, `receipt.patchResource` (the same URI, for a client reading the structured
payload), and `receipt.patchSha256`. Fetch with `resources/read`, verify against the digest:

```jsonc
{ "name": "review_remediate", "arguments": { "fromRun": "run-9f3c…", "output": "patch", "allowWrite": true } }
// ← content: [ {"type":"text", …}, {"type":"resource_link", "uri":"aimesh://run/run-4a1b…/patch", "mimeType":"text/x-diff"} ]
//   receipt: { patchArtifact: "patches/changes.patch", patchSha256: "sha256:…", patchResource: "aimesh://run/run-4a1b…/patch" }

{ "method": "resources/read", "params": { "uri": "aimesh://run/run-4a1b…/patch" } }
// ← contents: [ { "uri": "aimesh://run/run-4a1b…/patch", "mimeType": "text/x-diff", "text": "--- a/…" } ]
```

The patch is **linked, never inlined**, at any size. A truncated-but-syntactically-valid diff reads as
complete, which is worse than a reference.

**What a resource read enforces.** The store resolves a *registration*, never a caller-supplied path, so
there is no request shape that reaches a file the server did not itself publish — an unpublished URI is
unaddressable rather than merely refused. The read itself goes through the same identity-bound,
no-follow, regular-file-only path the containment layer uses (`workspace.ReadUnder`), it is bounded at
4 MiB and **refuses** an oversized artifact rather than truncating it, and when the artifact was
published with a recorded digest that digest is **re-verified on every read** — an artifact that no
longer matches what its receipt says is refused rather than served as though the receipt still described
it.

**Retention follows the run.** A run's resources are dropped when the run is evicted from the registry, at
the same moment `explore_run_result` stops answering for it. See [Known gaps](#known-gaps).

### The governance invariant

`outputSchema` is a **four-branch** `oneOf` keyed on `state`, each `state` a `const` rather than an
`enum`, so exactly one branch can match any payload:

| `state` | Required fields |
|---|---|
| `running` | `runId`, `state`, `panel` |
| `complete` | `runId`, `state`, `mode`, `panel`, **`governance`**, **`identityCaveats`** |
| `halted` | `runId`, `state`, `exitCode`, `haltClass`, `reasonCode` |
| `cancelled` | `runId`, `state`, `haltClass`, `reasonCode`, `panel` |

`cancelled` is its own branch because a cancelled call is genuinely a fourth shape: nothing failed, there
is no result, and there may have been no time to compute an `exitCode` — but the panel echo and the
taxonomy are still owed, because a cancelled exploration still spent whatever it spent before the cancel.
It must not be emitted as the `running` shape with `state` overwritten — that payload matches no branch at
all. `aimesh review mcp`'s result schema has the identical four-branch shape.

Branches exist precisely so that `governance`, `identityCaveats` and `panel` can be **`required`** on the
`complete` branch, instead of being softened to optional so a "running" reply can validate. `panel` is
required on `running` and `cancelled` too: the requested-vs-executed echo exists from the moment a run is
admitted, so "which panel is this?" is answerable from the first reply rather than only from the last.
That is the invariant a summary must never drop, declared rather than merely promised — and validated
against every emitted payload in the build (see the note under [Results](#results)).

- **`governance`** — the host-computed record: claim tally by label, `claimsWithheld`, `claimsContested`,
  `claimsHash`, `countingPolicyHash` (hashed *before* any judgment was solicited), panel size, respondents,
  quorum. A mode that emits no counts sets `countsEmitted: false` with a note, rather than omitting the
  block: an absent key and an honest "this mode does not count" are different facts.
- **`identityCaveats`** — every seat whose model identity could not be verified, with the evidence tier and
  the caveat text. An **empty array is the positive statement** that every seat was verified — which is why
  it is required rather than omitted when empty. A synthesis from an unverifiable model must not read like
  one from a verified model.
- **`panel`** — `requested` **and** `executed`. A count that was narrowed, a profile that resolved somewhere
  unexpected, or a seat that dropped mid-run is visible only by comparing the two halves.
- `result` — the mode's own terminal detail, with its honesty pairs intact: a ballot's `decision` label
  beside its counts, a comparison's `disagreedCells`/`rankingWithheld` beside its Pareto frontier, a
  forecast's `computedBy`/`poolingRule` beside its aggregate.

The governance and mode-detail projections are computed by `exploremesh/internal/surface/runview`, shared
with the ACP surface, so the two surfaces cannot drift on what a run reported.

### `instructions`

The cross-tool contract the client model reads before choosing a tool: how the tools relate, that the job
shape exists and `idempotencyKey` is the retry mechanism, that **counts are host-computed and must never be
restated**, that the governance block and identity caveats must always be surfaced, that a shortlist is a
ballot and a Pareto frontier has no winner, that the explore tools spend money, and that the server changes
no configuration and reads no files.

It is returned by `initialize` on the legacy era and by `server/discover` on the modern one, **byte for
byte the same string** — parity for free, and one place to change it. It gained a modern-era section: that
`resultType`, `state` and `task` are three different things and two of them are spelled "complete"; on
reviewmesh, that trusted roots are launch-established and this revision offers no way for a client to
narrow them (and that `review_remediate` has only the `fromRun` form); and that **this server emits no
protocol log notifications on this revision**, naming where in-flight visibility actually lives. Each of
those three has a mechanism behind it that fails closed without the sentence — the sentence is a
disclosure, never the enforcement.

## Error model

Two carriers, and the split is deliberate.

**Domain halts ride `CallToolResult` with `isError: true`** — a *successful* JSON-RPC response:

```jsonc
{
  "isError": true,
  "content": [{"type": "text", "text": "Exploration halted (run 20260728T…): … [haltClass=A reasonCode=adapter_exited_nonzero exitCode=4]"}],
  "structuredContent": {
    "runId": "20260728T140311-0421",
    "state": "halted",
    "exitCode": 4,
    "haltClass": "A",
    "reasonCode": "adapter_exited_nonzero",
    "failure": { "purpose": "…", "message": "…", "panelSize": 2, "dropped": [ … ] },
    "panel": { "requested": …, "executed": … },
    "identityCaveats": []
  }
}
```

JSON-RPC `error.data` is routinely flattened or dropped by clients — the halt taxonomy would be lost
exactly when the model needs it in order to react. Identity mismatch, adapter failure, quota, an admission
refusal and an unknown `runId` all take this path. Branch on `reasonCode`, never on the message text.

`exitCode` and `haltClass` are always present on a halt. `failure` is present only when the halt has a
structured breakdown to give (an admission refusal, for instance, has none). **`reasonCode` is never
empty, on either server**: an error nobody classified still yields a code-shaped reason from meshcore's
stable per-exit-code table (`internal_error`), never `""` and never a sentence. A cancelled run says
`run_cancelled`. Branch on it directly — there is no empty case to special-case.

`exitCode` is the shared aimesh halt taxonomy (see [architecture.md](architecture.md#halt-taxonomy)): 3
config, 4 adapter, 5 model/identity, 6 containment, 7 policy/cap, 8 internal.

**JSON-RPC protocol errors are reserved** for malformed requests, unknown tools and pre-initialization
calls — cases where no run exists and nothing has been spent. Every message is a teaching error: it names
the field and the accepted values, so the corrected call is derivable rather than guessable.

| Situation | Carrier |
|---|---|
| Unknown tool, unknown method | `-32601` / `-32602` |
| Unknown field, wrong type, blank required field, out-of-range `count` or `waitSeconds` | `-32602` |
| Unknown profile, unconfigured adapter, over-cap panel | `-32602` (pre-spend parameter resolution) |
| Request before `notifications/initialized` | `-32002` — **legacy era only.** `2026-07-28` has no pre-initialization state to be in, and forbids implementations of that revision to emit `-32002` at all (it meant *resource not found* in earlier revisions). It is deliberately not renumbered: doing so would change wire-visible behavior for the legacy clients that are, during the sunset window, the only clients there are, in order to tidy a path already scheduled for deletion. |
| `resources/read` for a URI this server does not publish (or a stale content digest) | `-32002` — **on BOTH eras**, and on the modern era that is a **declared deviation**, not conformance. `2026-07-28` forbids implementations of that revision to emit `-32002` at all and names `-32602` as the replacement. The code is shared with the pre-init refusal above by numeric accident: the spec assigned `-32002` to *resource not found* and this transport had already taken it for the handshake case. The two are unambiguous in practice (one occurs only before the handshake, the other only after) and each message names its own case, but the modern-era emission is a real gap — see [Known gaps](#known-gaps). |
| Modern request missing `_meta.protocolVersion` or `_meta.clientCapabilities` | `-32602`, naming both keys and our supported list |
| Modern request naming a version we do not implement modernly | `-32022` (`UnsupportedProtocolVersionError`) with `data.supported` + `data.requested` — the one **recognized modern error**, so a probing client stays modern and retries from the list rather than falling back to `initialize` |
| `ping` / `logging/setLevel` on the modern era | `-32601`, naming the replacement |
| Modern request on a legacy-latched process (or the reverse) | `-32600` / `-32601`, teaching: start a new process for the other era |
| Admission refusal (concurrency, lifetime cap) | `isError`, `haltClass: policy`, exit 7 |
| Run halt (adapter, config, containment, unparseable output) | `isError`, the halt's own class. **Model identity is never one of these** — it is reported in `identityCaveats` and the run completes normally. |
| Unknown / expired `runId` | `isError`, `reasonCode: unknown_run_id` |
| Remediation that COMMITTED but refused a protected-path finding | `isError`, `state: "complete"`, `outcome: "partial_refusal"`, `reasonCode: apply_refused_protected_path` — see [Partial refusal](#partial-refusal-a-protected-path-target-does-not-discard-the-run) |

Note the last row: `isError` here does **not** mean the run failed. It is the coarse "this is not a clean
success" signal, and the payload beside it says exactly what was and was not written.

Log notifications (`notifications/message`) are **best-effort by contract**: on the legacy era they are
dropped below the client's level, and on `2026-07-28` there are none at all. Either way no
governance-relevant fact exists only there — the result and the run record carry it. Declining the channel
turns that standing rule into a structural fact rather than a discipline.

## Admission and spend governor

Every run-starting tool spends money on someone else's account, so every refusal is a **teaching error that
names the limit and the current count**.

| Control | Default | Refusal |
|---|---|---|
| Panel size | 2..16 | `-32602`, naming the cap |
| Arguments per call | 1 MiB | `-32602`, naming the limit |
| `waitSeconds` | 1..120 | `-32602` |
| `idempotencyKey` | — | a duplicate returns the **existing** run, active or finished; it never re-spends |

The idempotency guard is checked **before** admission accounting: a retry after a dropped connection must
return the original run, not a second panel billed to the same person for the same question.

## Security posture

Both servers share the first, third, fourth and last of these; the reviewmesh-specific posture (trusted
roots and the write primitive) is in [aimesh review mcp](#reviewmesh-mcp).

- **No configuration mutation.** There is no setup, repair, adapter-add or model-key tool, and no
  parameter that reaches configuration. A call selects or composes from what the operator already
  configured.
- **No filesystem reads.** The artifact under review is supplied inline. There is no path parameter on any
  tool.
- **`explore_list` and `explore_doctor` are sanitized.** They report logical identifiers, selectable models, profile shapes
  and readiness booleans — and **no binary paths, launch arguments or environment detail**. A tool result
  is inference input for a third party: whatever these return is likely to end up in someone else's model
  provider's logs. The projection types carry no path field at all, so the rule cannot be forgotten by the
  next person to add a field; readiness details composed by meshcore (which legitimately name where a
  binary was found) are redacted to `<path>` on the way out, and the redaction is **visible** rather than
  silent.
- **`explore_doctor` is static only.** No process is started and nothing is spent, which is what lets it honestly
  carry `readOnlyHint`. The live probe stays CLI-only (`aimesh explore doctor --probe`) because it starts
  provider CLIs.
- **Run capture is on by default.** Every run writes a run directory under `$EXPLOREMESH_ARTIFACT_DIR`
  (unset: the project-local `.aimesh/explore/runs/` when a `.aimesh/` state directory exists, else a
  subdirectory of the OS temp directory — never a relative path inside the caller's repository), and the
  wire `runId` **is** that directory's name — a run id that did not
  name its own audit record would make the record unfindable from the only handle the caller has. The
  absolute path is never put on the wire. `--no-capture` turns it off.
- **The transport is not an authorization boundary.** Annotations and hints coordinate with the host's
  permission layer. What actually holds is the startup binding: the adapter set, the profile set, the
  panel caps and the spend governor are all fixed before the first frame is read.

## `aimesh review mcp`

```bash
aimesh review mcp [--root <dir>]... [--no-default-root] [--allow-broad-root]
               [--allow-inferred-root] [--allow-remediate]
               [--protocol dual|legacy] [--framing newline|content-length]
              
               [--wait-seconds <n>] [--turn-timeout <dur>]
```

```json
{ "mcpServers": { "reviewmesh": { "command": "aimesh", "args": ["review", "mcp", "--root", "/path/to/project"] } } }
```

Everything above about the transport, the job shape, the run registry, the error model and the admission
governor applies unchanged. What follows is what is different — and all of it follows from one fact:
**this server takes filesystem paths, and one of its tools writes.**

### Trusted roots are mandatory

`aimesh explore mcp` reads no files, so it needs no roots. This one does, and the consent model is the ACP
surface's, reused unchanged (`reviewmesh/internal/surface/acp/roots.go` — one implementation, so the two
agent surfaces cannot diverge on what a peer process may read):

- `--root <dir>` (repeatable) — the launching human's consent, given **before any request exists**.
- No `--root` → the launch working directory, because an editor spawns its agents in the project the user
  opened. That inference is only honest when the cwd looks like a project, so a degenerate cwd (`/`, a home
  directory, a system tree) is **refused** rather than adopted.
- `--no-default-root` declines the inference: explicit roots only.
- `--allow-broad-root` is the explicit, recorded opt-in for a `--root` the degenerate rule would refuse. It
  never waives the non-overridable read denylist.
- `--allow-inferred-root` is the explicit opt-in that lets an **inferred** launch cwd stand as a trusted
  root on protocol revisions that removed the client's ability to narrow this server (`2026-07-28` and
  later). See *Root provenance* below. Like `--allow-broad-root`, it waives nothing else: the degenerate
  rule still refuses `/`, a home directory, home's parent and the system trees **with the flag set**, so
  the most it can admit is a plausible project directory — the same consent the CLI already accepts from a
  human-typed path.

### Tools (`aimesh review mcp`)

| Tool | Spends? | Writes? | Purpose |
|---|---|---|---|
| `review_report` | **yes** | no | run the governed cycle; return the adjudicated findings with per-seat provenance |
| `review_remediate` | **yes** | **yes** | apply an already-adjudicated accepted set — *listed only when the capability is granted* |
| `review_list` | no | no | configured adapters, profiles, the write modes (`report`/`patch`/`apply` — reviewmesh's sense of "mode", not exploremesh's exploration modes), the remediation policy (ceiling + whether this server can write), and the admission limits |
| `review_doctor` | no | no | static adapter readiness |
| `review_run_status` | no | no | poll a run |
| `review_run_result` | no | no | fetch a run's full result (or a remediation's receipt) |

#### Annotations

| Tool | `readOnlyHint` | `openWorldHint` | `idempotentHint` | `destructiveHint` |
|---|---|---|---|---|
| `review_report` | **false** | **true** | **false** | false |
| `review_remediate` | **false** | **true** | **false** | **true** |
| `review_list`, `review_doctor`, `review_run_status`, `review_run_result` | **true** | false | true | false |

The two review tools cross-reference each other in their descriptions ("makes **no** changes — to apply the
accepted fixes use `review_remediate`", and the mirror), because the client model chooses between them from
those descriptions alone. Annotations are advisory; nothing server-side keys off them.

An annotation's **title** is held to the same standard as its hints, and a test enforces it: a title may
not say "read-only" beside `readOnlyHint: false`, nor claim to write or spend beside `readOnlyHint: true`.
The title is what a human reads in a permission prompt while the host decides on the strength of the hint,
so a disagreement between them is exactly the mis-signal these annotations exist to prevent.
`review_report` is titled *"Review (reports findings; writes no workspace changes)"* — it spends and
writes a run directory, and it changes no workspace file.

### `review_report`

| Field | Required | Notes |
|---|---|---|
| `workspace` **XOR** `inlineWorkspace` | ✓ | strict top-level `oneOf`. A path must resolve inside the trusted roots |
| `profile` **XOR** `panel` | | select a configured profile, or compose an ad-hoc one. Omitted: the configured default |
| `authority[]` | | the P2 authority manifest — see below |
| `waitSeconds` | | 1..120, default 25 |
| `idempotencyKey` | | ≤200 chars. A repeat returns the **existing** run |

An ad-hoc `panel` is `{reviewers[1..16], cross_check?, verifier?, author_remediator}` — and
**`author_remediator` is required**. It is the host-adjudication seat whose judgment becomes the accepted
set; defaulting it silently would mean composing a panel whose judge the caller never saw. A different
`effort` is a different vantage, so it is part of a reviewer seat's identity; the single-slot lanes take no
per-call `effort` (there is no per-role effort in the configuration model, and accepting one would silently
drop it, so it is refused instead). Naming a lane the target profile does not define is refused for the same
reason: it would be silently ignored.

Compose-not-configure is enforced **server-side**, fail-closed against the adapter set bound at startup: a
call selects or re-arranges configured identities and can never introduce an adapter, a binary path or a
launch argument.

`authority[]` entries are `{name, path XOR content, mediaType?, expectedHash?, completeness?, ranges?}` —
the same declaration the CLI takes via `--authority-manifest` and ACP via `_meta.reviewmesh.authority[]`,
enforced by the same engine and therefore with the identical fail-closed rules: root-scoped path reads,
hash pinning, **no silent truncation** (an oversized `requireFull` document is a refusal, not a shortened
prompt), and inline `content` is **report-mode only** and excluded from the host-adjudication prompt. The
budget is the same everywhere: **8 documents, 64 KiB embedded per document, 192 KiB total** — and the
tool schema's `maxItems` is *derived from* the engine's constant rather than restated beside it, with a
test asserting the two agree, so a caller learns the bound from the declaration instead of from a refusal.
(It once said `16` against an enforced `8`: fail-closed, but the declaration over-promised.) "Exactly one
of `path`/`content`" is enforced by the server rather than expressed as a schema `oneOf`.

#### Result

A **four-branch** `oneOf` keyed on `state` — `running`, `complete`, `halted`, `cancelled` — each `state` a
`const` rather than an `enum`, so exactly one branch can match any payload. `cancelled` is its own branch
because a cancelled call is genuinely a fourth shape: nothing failed, there is no result, and the panel
echo and taxonomy are still owed. The panel echo is `required` on `running` and `cancelled` as well as on
`complete`, since it exists from the moment a run is admitted.

`review_run_result` declares the **union** of this schema and `review_remediate`'s, because it fetches any run
this server started and a remediation's result is a receipt, not a review; branch on `tool`.

On `complete`, **all** of these are `required` — the governance invariant made client-side checkable
rather than promised in prose:

| Field | Why it is required |
|---|---|
| `findings[]` | each with `supportingSeats[{seatId, adapter, model, identityTier}]`, host-computed `agreementCount`, `dissentingSeats[]`, the qualifiers that say what the count was worth (`distinctModels`, `agreementIndependence`) and what the seats that ran did with it (`consensus`), and `applyable: false` + `applyRefusalReason` when the write-path rule refused it |
| `counts` | host-computed totals, including `accepted` (exactly what `review_remediate` would write) and `quarantined` |
| `identityCaveats[]` | an **empty array is the positive statement** that every lane was verified |
| `authority[]` | the inclusion manifest: per document the source, full/embedded hashes, byte counts, `complete` |
| `withheld[]` | files a containment rule kept out of the reviewed set — silence about a file nobody was shown is not approval |
| `panel` | `requested` **and** `executed`: one entry per requested seat, including one that halted |
| `status`, `mode`, `workspaceSource`, `remediable` | |

`runId` has the shape `<UTC timestamp>-<sub-second>-<random>`, e.g. `20260728T140311-0421-9f8e7d6c5b4a3210`.
It carries **nothing about this host and is never a filesystem path** — the audit run directory is
deliberately absent from the payload. The id *is* that directory's own name, which is what makes it a
durable handle for `review_remediate {fromRun}` after the registry entry is gone; but a name is not a
location, and where this server keeps its runs is still never disclosed.

The timestamp is the run's start time, which discloses nothing to the caller that made the call. It is
there so run directories sort chronologically alongside the CLI's and ACP's, which share the same prefix.
The random component makes the name collision-free: two runs admitted in the same 0.1 ms would otherwise
be handed the same directory.

A finding with `applyable: false` is **reportable and never writable** — it is supported only by
weak-identity seats, or traces only to authority text. That verdict is computed once, by the host, during
the run that produced it, and the remediation path re-reads it rather than re-deriving it.

### `review_remediate` — the double opt-in

**Gate 1 — the operator, at launch.** The tool is not merely refused without permission; it is **not
listed**. A model cannot be talked into calling a tool it was never told exists.

Permission is a **config-store policy capability**, not a flag that overrides config:

```yaml
surfaces:
  defaultModeBySurface:
    mcp: report            # the shipped ceiling
  capabilitiesBySurface:
    mcp: [allowRemediate]  # what RAISES it to apply
```

`--allow-remediate` grants exactly that capability in the server process's own config snapshot. The
write-authority ceiling is then computed **from the config** (`config.SurfaceCeiling`), which is the point:
a flag that reached above the ceiling would make the ceiling a default rather than a ceiling. The Manager
resolves the run against the same snapshot, so the tool list, the per-call refusal and the write itself
cannot disagree. A write-capable server also announces itself on stderr at launch.

**Gate 2 — the caller, per call.** Every call must pass `allowWrite: true`, literally. Omitted or `false` is
a field-addressed teaching error **before any spend**, carrying a corrected example call.

> **`allowWrite` is not human consent.** The value is supplied by the **calling model**, in the same call it
> authorizes — and the teaching error above even shows the corrected call with `"allowWrite": true` in it,
> because refusing to say what a well-formed call looks like would only make the model guess. What the field
> honestly provides is an **auditable declaration** that this request was a write request, and an **argument
> a host can show a human** before forwarding the call — a per-call human prompt lives in the MCP host, not
> here. The enforcement a model cannot supply is the operator's **launch flag**, the **config capability** it
> grants, and **root confinement** established before any request existed. An out-of-band approval token
> (issued to a human, presented by the call, verified by the server) would be genuine per-call consent; it is
> a legitimate future design and is deliberately not built here.

#### One form: `fromRun`

> **BREAKING CHANGE.** The full-cycle form — `review_remediate {workspace}`, which reviewed and wrote in
> one call — **has been removed from the MCP surface.** It is refused with a teaching error naming the
> two-step path. The CLI is unaffected and keeps its full cycle.
>
> **Why.** Under `2026-07-28` stdio a cancelled request may receive *no further message at all*. A
> one-call review-and-write is the only write shape whose caller can therefore be left holding
> **nothing**: the run it would need in order to find out what happened is a run only that same,
> cancelled response would have named. `fromRun` closes that with no new parameter, because it is not a
> key the caller invents — it is a value the caller **already received, in a completed response, before
> the write request was ever sent**, and cancellation cannot retract a message already delivered. It is
> also the specification's own prescribed pattern: state spanning requests "**MUST** be referenced by an
> explicit identifier the client passes on each request" (`basic/index`).
>
> **The migration is two calls instead of one**: `review_report {workspace}` → `review_remediate
> {fromRun: <runId>}`. Nothing is lost — the accepted set becomes readable *before* anything is written,
> which was already the documented preferred path.
>
> The rule is **era-neutral**: it applies on the legacy revision too, because two write shapes on one
> surface would be worse than the breaking change.

```jsonc
// The only write form on this surface.
{ "fromRun": "run-9f3c…", "output": "patch", "allowWrite": true }
```

Adjudication is paid **once**, and the accepted set is readable by a human *before* anything is written —
the checkpoint the double opt-in only gestures at. There is exactly **one** write path in this server.

The accepted set, the panel and the authority manifest all come from the source run.
`profile`, `panel` and `authority` are therefore meaningless there, and the schema forbids them — so the
server **refuses** them, naming the parameter, rather than parsing and dropping them. Each names a
governance input (which models judged, which panel produced the accepted set, which intent it was judged
against); accepting the call and ignoring the parameter would tell the caller the opposite of what
happened. Run a fresh `review_report` with the panel and authority you want, then apply *that* run.
A `workspace` beside `fromRun` is refused for the same reason: the workspace is the source run's, and
re-naming it would let a write land somewhere the accepted set was never judged against.

#### `select` — selective apply, keyed on the fingerprint

`select: ["sha1:…", …]` narrows the write to the accepted findings you name. Omit it and the whole
accepted set is applied, exactly as before.

**The selector is the HOST-COMPUTED `fingerprint`, never a finding's `id`.** Every finding in
`review_report`'s result carries both. The `id` is model-authored and is renumbered by the run; keying
a write set on one would let a model relabel findings until "apply only this one" selected something
else. A fingerprint is derived by us from the finding's own file and normalized location, so the
only way a model changes what a fingerprint names is by proposing a **different finding, openly**.

```jsonc
// 1. read the accepted findings and their fingerprints
{ "workspace": "/abs/path" }                       // → review_report
// 2. write exactly two of them
{ "fromRun": "run-9f3c…", "output": "apply", "allowWrite": true,
  "select": ["sha1:1a2b…", "sha1:9f8e…"] }         // → review_remediate
```

The rules, all fail-closed:

- **A selection can only narrow.** A fingerprint that names no finding in the source run is **dropped**
  — never resolved against another run, never fetched from anywhere — and reported in
  `selection.unmatched`, so a mistyped selector is visible instead of silently shrinking your write.
- **An empty `select` is refused**, not treated as "apply everything". It names zero findings, and a
  call that read as a normal apply while writing nothing is the one outcome a caller can neither detect
  nor survive.
- **A selection that matches *nothing* is refused too**, with `reasonCode:
  "apply_selection_matched_nothing"` and the unmatched list. Nothing is written.
- The result carries `selection: {requested, matched, unmatched}` — all three lists always present —
  and the receipt records it durably, so a caller whose response was lost can still answer "what did I
  ask to narrow to" from the run directory.

**One remediation per source run, selection or not.** The idempotency guard that stops a decision set
being applied twice does not distinguish selections: a second `review_remediate {fromRun: X}` with a
*different* `select` attaches to the first and returns the original receipt. So **"apply 1–3, then
4–5" is not available from one report run** — choose the selection in one call, or produce a fresh
`review_report` for the second tranche. Loosening this would trade a caller convenience for the one
guarantee this surface exists to keep.

The same capability is `--select <fingerprint>` (repeatable) on the CLI and `select: [...]` on ACP's
`session/prompt`. All three thread into the one governed write path, and all three guarantee the same
thing about the **write set**: nothing outside the named fingerprints is written.

**Both from-run forms also guarantee the *inspected* set.** `review_remediate {fromRun}` replays a
**stored** decision set — the exact findings adjudicated by the run you read — and re-verifies its base
hashes before writing. ACP's `session/prompt {mode: "apply", fromRun}` does the same
([acp.md](acp.md#a-write-turn-needs-a-run-handle-and-it-applies-that-run-the-two-turn-contract-reviewmesh-only)),
so `select` narrows the stored set on that surface too. On a full
cycle with no `fromRun` — the CLI's ordinary `--apply`, or an ACP `patch` turn deciding for itself —
`select` narrows *that run's own* adjudication, which is a different and weaker thing and is documented
as one.

**A `fromRun` handle outlives the registry.** The in-memory entry is the fast path, not the only one: on a
miss — an evicted run, or one from before a restart — the server resolves the handle from the source run's
**own directory** (`decisions/decision-set.json`), which is the same file, written by the same run, that
ACP reads. The handle is **verified, not trusted**: the `runId` names a directory that must be an immediate
child of this server's artifact directory both as spelled and after symlink resolution, and must carry a
decision set whose recorded run id is that directory's own name. Every other outcome — a fabricated id, a
path, a traversal, a run that recorded no set (a remediation does not) — is the same
`reasonCode: "unknown_run_id"`, so a caller learns "not a run of mine" and nothing else.

**What a second apply gets, and why it differs across a restart.** Neither answer writes twice; they
report differently, and a client must be able to tell them apart:

| When | Answer | Why |
|---|---|---|
| the server is still running | the **original receipt**, `isError: false` | the source-run guard is in memory and recognizes the replay |
| after a restart (or after the entry was evicted) | a **halt**, `reasonCode: "stale_decision_set"`, nothing written | the guard is gone with the registry — but the first apply changed the very files the stored set pins, so the durable base hashes no longer match |

This is deliberate and is exactly what ACP does. An on-disk "already applied" marker would make the second
call return the original receipt across a restart too; it was considered and **not** built, because a
durable record on a write path is where a wrong record becomes a false success — and an honest halt costs a
caller a round trip, while a wrong receipt costs it the truth about its own files.

`output: "apply"` needs the ceiling to be `apply`; `output: "patch"` produces the diff artifact and changes
nothing. `inlineWorkspace` is refused on this tool — there is nothing real to write to — and a `fromRun`
naming an inline review halts with `inline_workspace_not_remediable`.

#### What the write is bound to

Before anything is written, four bindings are verified. Each answers a question the others cannot.

| Binding | Refusal | Why a pathname/position/hash alone is not enough |
|---|---|---|
| **Decisions ↔ findings, by ID** | `decision_set_unreconciled` | The wire form is two parallel arrays. Pairing by position silently authorizes a *different* finding for any record that was reordered, truncated or migrated — every individual value still well-formed. |
| **The reviewed root, by identity** | `remediation_workspace_identity_changed` | The decision set stores the workspace as a *string*. The root's device+inode and canonical path are captured when the report completes; point the string at a different checkout whose targeted files carry the same bytes and every content check still passes. |
| **Each edit target, by review record** | `remediation_edit_outside_finding`, `remediation_edit_target_unreviewed` | Scope answers *where* a write may land, never *what was reviewed*. A hunk must target its own finding's file, a file a reviewer was shown, and a file the review base-hashed. |
| **Each destination's content** | `stale_decision_set` | See below. |

#### Staleness, checked twice

The base hashes of every file the accepted findings target are captured **when the report run completes**
and re-verified before the write window opens. One changed byte halts with
`reasonCode: "stale_decision_set"`, naming the files: the decisions describe a workspace that no longer
exists, and applying them anyway would be acting on a stale judgment. A file that did **not** exist at
review time is recorded as absent, so its later appearance is a mismatch too.

That first check is not the last word, because everything between it and the commit takes time — a model
call can take minutes. So each destination's **content** is digested again **inside the commit, immediately
before it is replaced**, and compared against the same pin. This is what catches a concurrent *in-place*
save: an editor rewriting a file keeps its device+inode, so the identity re-verification the commit already
performed is satisfied by a file whose every byte changed. A mismatch refuses and the whole commit rolls
back, with the same `stale_decision_set` code.

> **The residue, stated plainly.** There is no filesystem lock. The content check narrows the unprotected
> window to the few syscalls between reading a destination and creating its replacement; a writer inside
> *that* window is neither prevented nor detected. Closing it would need an exclusive lock or filesystem
> compare-and-swap, and this server does not pretend to have one.

#### The write window

**journal → apply → receipt.** This window is **not MCP's** — it is the one governed write path in the
product (`reviewmesh/internal/manager/review/writepath.go`), and `aimesh review run --apply` and an ACP
`apply` prompt open exactly the same one, with the same artifacts, the same content pins and the same
cancel/commit lock. It is documented here because MCP is where a *model* asks for a write, but nothing
below is a property of this surface. (A structural test asserts there is exactly one commit call site, so
a second write path cannot quietly reappear beside a surface — which is how these guarantees came to be
true on MCP alone in the first place.)

1. Every intended hunk is computed and written to `remediation/journal.json` — **fsynced** — **before the
   first edit is applied**, and emitted as a log notification. A journal that cannot be made durable is a
   **halt**: the contract is "journal before write", so a full or read-only disk must make the *write*
   unreachable rather than make the *journal* optional. Each hunk records the digest of its replacement text
   (`replacementSha256`) alongside the byte count, and the journal carries an `intentSha256` over the whole
   ordered set — a byte count identifies no replacement, since every string of that length shares it.
2. Edits are applied to an **isolated copy**, and a finding is **all-applied or none-applied**: its hunks are
   staged, so a finding whose second hunk fails has its first hunk rolled back out of the copy rather than
   shipped by a commit while the receipt says "not applied". Only `apply` mode commits, and only through the
   same `scope.Resolver` guard and non-overridable write denylist every other reviewmesh write passes.
3. The **receipt** is written to `remediation/receipt.json` — durably — on **every** path: success, halt,
   cancellation, nothing-to-do. Its `applied[]`, `files[]` and `committed` are derived **only from a commit
   that actually succeeded**; a commit that failed rolls back and is reported as `notApplied`. A receipt that
   cannot be persisted is a **terminal failure** of the run, not a logged inconvenience.

Cancellation is honored **only at the window's boundaries** (before the journal, and before the commit), and
the second boundary is decided **once under a lock**: "cancelled" and "entered the commit" are mutually
exclusive answers. A cancelled call never commits; a call whose commit had already begun completes it and
does **not** report itself cancelled, because a receipt saying "cancelled" over a repository that changed is
the one outcome worse than either.

On `2026-07-28` over stdio the server **MUST NOT** send any further message — response, log, or progress —
for a cancelled request. On `2025-06-18` the server **SHOULD NOT** send a response, and a client is told to
ignore one that arrives anyway. In neither case may a cancelled call's outcome be delivered on the cancelled
request; it is reachable only by lookup. Under `2025-06-18` — the revision this server implements today — the
receipt is therefore *also* emitted as a `notifications/message`, which that revision permits and
`2026-07-28` stdio would not. So **"what did it actually write" is always answerable** — from the response
when there is one, the notification, or the run record, which is the one channel that survives every
revision.

The receipt carries `{status, reasonCode?, baseHashesVerified, intended[], intentSha256, applied[],
notApplied[], files[], patchArtifact, patchSha256, commitAttempted, committed}`. `commitAttempted`
distinguishes the two ways `committed: false` happens — nothing was tried, or something was tried and rolled
back. `patchArtifact` is a **reference plus a hash** to the complete patch in the run record — the content is
never inlined and never truncated, because a syntactically valid truncated patch is worse than a link: it
reads as complete.

**If the process dies mid-commit.** A crash between the commit and the receipt leaves the tree written and no
receipt, and no lock-free design can prevent that. The run directory is made **reconcilable** instead:
`remediation/journal.json` says what was intended (with its intent digest) and
`remediation/commit-attempt.json` — fsynced *before* the first live write — says a commit was entered. A run
directory holding an attempt and no receipt is the signature of an interrupted write window, not an
ambiguity.

#### Partial refusal: a protected-path target does not discard the run

**Nothing is ever written to a protected path.** meshcore's write denylist (`.env*`, `.git/`, agent client
config, `~/.aimesh/**`) is non-overridable and stays that way. What changed is what happens to *the other
findings*.

A protected-path target used to **halt** the whole remediation. Nothing was written, which was right — but
every other accepted finding was discarded with it, and because the halt happens at *authorization* time, a
caller who did not already know which finding was protected had to pay for a run to discover it. The
workaround that invites is hand-editing the patch, which abandons containment, rollback and the receipt in
one step.

So a protected-path target is now a **recorded refusal**: the finding is marked not-applied with
`applyRefusalReason: "protected_path"`, the remaining findings are applied, and the refusal is surfaced as a
first-class fact. That last clause is load-bearing. The objection the halt answered was to a *silent* skip
letting a run report success — so this refusal is not silent anywhere:

```jsonc
{
  "isError": true,                    // the coarse "not clean" signal
  "content": [{"type": "text", "text": "REFUSED: 1 finding(s) target a PROTECTED PATH … "}],
  "structuredContent": {
    "runId": "run-…",
    "state": "complete",              // the write DID commit — see below
    "outcome": "partial_refusal",
    "counts":   { "applied": 7, "refused": 1 },
    "refusals": [ { "fingerprint": "sha1:…", "file": ".env", "reason": "protected_path" } ],
    "receipt":  { "status": "complete", "committed": true,
                  "notApplied": [ { "findingId": "…", "file": ".env", "state": "reported_valid",
                                    "reason": "protected_path" } ] }
  }
}
```

- **`isError: true` on a `state: "complete"` result.** The two do not move together here, and that is the
  design. The commit succeeded, so `state` is honestly `complete`; three run-status vocabularies already
  exist in this tree and adding a fifth value to one of them would make every consumer's exhaustive switch
  wrong. `halted` would be unambiguous about "not clean" and false about the facts. So the not-clean signal
  goes where a consumer reads it without parsing anything — `isError`. It over-signals on purpose: a caller
  that looks and finds `applied: 7` has lost nothing, whereas a caller that believes eight findings were
  written when seven were has lost the thing this mechanism protects.
- **`outcome`** is `"applied" | "partial_refusal" | "nothing_applied"`, present on every write-producing
  result and absent elsewhere. A consumer that ignores it is less informed but never *wrong*, because the
  coarse signal is keyed on `counts.refused`, not on this field.
- **`refusals[]` is keyed on the host-computed fingerprint**, never the model-authored `findingId`. A model
  that could relabel findings could otherwise steer which one a caller's follow-up selection names — and the
  id is renumbered after the write window anyway.
- **`review_run_status` repeats `outcome` and `refusedCount`.** A poller that never fetches `review_run_result` sees only
  `state: "complete"`, so the two distinguishing facts are lifted into the status payload. `review_run_result`
  replays the full payload, `isError` included: the call that pays for a write is often not the call that
  collects it.
- **The `content` block leads with the refusal**, before the applied summary. Some clients show the model
  nothing else, and a refusal buried under seven successes is a refusal nobody reads.
- **It is not retryable.** Re-issuing `review_remediate {fromRun: X}` attaches to the existing reservation
  and returns the *original* receipt, refusals included. The refused finding would be refused identically;
  edit those paths by hand if they need changing.

The same outcome, in each surface's own vocabulary: CLI **exit 7** with reason
`apply_refused_protected_path` and **no halt record** (see
[architecture.md](architecture.md#exit-7-without-a-halt-the-protected-path-refusal)); ACP
`stopReason: "refusal"` with the numbers in `_meta.reviewmesh` (see [acp.md](acp.md)). One rule, three
spellings — the behavior itself is decided once, in the single governed write path, so the surfaces cannot
diverge.

#### Idempotency, in two independent forms

| Guard | Covers |
|---|---|
| `idempotencyKey` | an explicit retry |
| the source run | a retry that forgot the key, or invented a new one — a `fromRun` that has already been applied returns the **original receipt** |

Both return the original receipt without a second application. On a write primitive this is not a
convenience: a second application is an unrequested write.

**Both guards are in memory, and what takes their place across a restart is the base-hash pins.** A
replay that reaches a server which no longer holds the reservation is not admitted twice: it is resolved
from disk and then **halts** on stale pins, because the first apply changed the files the stored set pins.
Same guarantee — the set is never applied twice — reported as `reasonCode: "stale_decision_set"` rather
than as the original receipt. See [One form: `fromRun`](#one-form-fromrun).

Both are decided by **one atomic reservation** taken before any write window opens, keyed by the idempotency
key *and* by the source run. Checking "has this already been applied?" and then admitting a run are two acts,
and two calls arriving together can both pass the check before either admits. The reservation collapses them:
the first caller opens the window, every later caller **attaches to the winner** and receives the winner's
receipt rather than starting a second write.

### Honest limits

The double opt-in and the annotations are **hints that coordinate with the host's permission layer**. They
are *not* an authorization boundary against a confused or hostile client model, and `allowWrite` is supplied
by that model rather than by a human (see the note under Gate 2). What holds regardless:

- **root confinement** — a path outside the trusted roots is refused before any spend;
- **the non-overridable write denylist** — `~/.aimesh/**`, app config dirs, MCP client config, `.git/config`,
  `.env*`, key-material patterns, enforced at the write layer, inside a trusted root or not;
- **the write-path rule** — every applied hunk must trace to evidence in the workspace copy, so a finding
  supported by authority text alone is reportable and never applyable;
- **the shown-files gate, per hunk** — the host only edits files a reviewer was actually shown, a `fromRun`
  remediation inherits that set from the run that produced the decisions rather than re-deriving it, and each
  individual hunk must target its own finding's base-hashed file;
- **identity binding** — the reviewed root by device+inode, the decisions to their findings by ID;
- **`fromRun`'s inspectable decision set** — the accepted findings exist as an artifact before the write.

See [security.md](security.md#5-remediation-over-mcp-what-the-double-opt-in-does-and-does-not-protect).

### Worked example — report, then apply

```jsonc
// 1. review — writes nothing
{ "name": "review_report", "arguments": {
    "workspace": "/path/to/project",
    "panel": { "reviewers": [ {"adapter": "claude-code", "model": "opus"},
                              {"adapter": "codex-cli", "model": "gpt-5", "effort": "high"} ],
               "author_remediator": {"adapter": "claude-code", "model": "opus"} },
    "authority": [ {"name": "spec.md", "path": "docs/spec.md", "expectedHash": "sha256:…"} ],
    "idempotencyKey": "review-2026-07-28" } }
// ← { runId: "run-9f3c…", state: "complete", counts: {accepted: 3, quarantined: 1}, … }

// 2. read the accepted set, decide, then apply it
{ "name": "review_remediate", "arguments": {
    "fromRun": "run-9f3c…", "output": "apply", "allowWrite": true } }
// ← receipt: { intended: […], applied: […], files: ["…"], committed: true, patchSha256: "sha256:…" }
```

## Known gaps

Stated here rather than left for a reader to discover. None of these weakens a governance guarantee —
every one of them is either a *declaration* that is looser than the enforcement, a bound that is
deliberate, or coverage that has not been extended to the second server — but each is a real difference
between what this page describes and what a client will observe.

Three rows that used to live here are gone because the gaps are closed: the **`roots/list` round trip** is
now live (the transport issues server→client requests; see [Trusted roots](#trusted-roots-are-mandatory)),
**`resources/*`** is implemented on both servers (see [Fetching what a run produced](#fetching-what-a-run-produced-resources)),
and **send-time output-schema conformance** now exists as the opt-in `--strict-schema` (see
[`--strict-schema`](#--strict-schema-the-same-check-opt-in-at-send-time)). What remains of each is the
narrower, honest residue in the table below.

| Gap | What is true today |
|---|---|
| The `2026-07-28` conformance client is our own | The legacy transcript is walked by the official Go SDK client; the modern one is not, because adopting an SDK release that speaks `2026-07-28` would replace the legacy client too and void the byte-identical legacy transcript. See [Conformance](#conformance). The upgrade is owed at legacy removal. |
| `2025-11-25` is not implemented | The version set skips it. It is a legacy-era revision published between the two we do implement; a client that only speaks it falls back through `initialize` and is refused with our supported list, which is the diagnostic that revision's clients can act on. |
| The probe answers a legacy-latched process | `server/discover` is deliberately era-neutral and never latches, so a process that has already answered `initialize` still returns a `DiscoverResult` — and then refuses the modern request that follows, with the `-32600` teaching refusal. **Fixing it would make the probe latch, which is the trap the era-neutral rule declines**: a probe that latched would let a mis-probing client lock a process into an era it cannot use. On stdio a process serves one client, so this needs a client that both handshakes and then switches eras mid-process. |
| `resources/read` emits `-32002` on the modern era | A **declared deviation**, recorded rather than hidden. `2026-07-28` says implementations of that revision "**MUST NOT** emit these codes: `-32002`" and names `-32602` as the replacement; `RunStore.ReadResource` is era-blind and returns `-32002` for an unpublished URI or a stale content digest on both eras. The message and `data.reasonCode` name the case, so a client is not misled about *what* happened — but the code is wrong for the era. Renumbering it is a wire-visible change to a live path and is deliberately not bundled into the legacy-sunset deletion, which touches a *different* `-32002` (the pre-initialization refusal). See [Error model](#error-model). |
| Run correlation is trace context only — there is no spend accounting | A modern request's `_meta.traceparent` / `tracestate` / `baggage` are carried verbatim into the run record (`run-state.json` `trace` for reviewmesh, `manifest.json` `trace` for exploremesh), which makes a run externally correlatable with whatever traced the caller. It does **not** make the run metered: no run in either app records token counts or cost, and the record says so in its own `trace.measures: "correlation_only"` field rather than leaving a reader to assume. Every spend figure in this repo's evaluation documents is an estimate and says so. The values are never validated — the W3C-format MUST binds the sender, and the spec forbids implementations from assuming anything about a reserved `_meta` key's value — so a malformed `traceparent` is recorded as it arrived rather than refused. |
| `resources/*` paging and change notifications | `resources/list` returns the whole set in one page with no cursor, `resources/templates/list` is declared-and-empty, and neither `subscribe` nor `listChanged` is offered (both are declared `false`). A client learns that a finished run published something by re-listing. The set is small by construction — a handful of artifacts per retained run — but this is a real difference from `tools/list`, which does honor a cursor. |
| Resource retention follows the run registry | A run's resources are dropped when the run is evicted (bounded by count and TTL, in memory), so a `resource_link` handed out earlier stops resolving at the same moment `review_run_result` stops answering. That is deliberate — a link the server can no longer explain is worse than no link — but it means a patch must be fetched within the retention window. |
| Send-time schema validation is opt-in | `--strict-schema` / `AIMESH_MCP_STRICT_SCHEMA=1` is **off by default**, because it costs a schema walk per call. With it off, a payload that matched no branch would fail the build rather than the call — still the right trade for a hot path, but still not a runtime guarantee unless the operator asks for one. |
| Windows | Every MCP path rule inherits the platform matrix in [security.md](security.md#7-platform-matrix--where-each-guarantee-actually-holds). The MCP root intersection's case-folding branch is tested by substitution on any platform; its separator handling is not, and nothing here has run against a real NTFS volume. |
| `waitSeconds` schema `default` | Declared as a literal `25`; it does not track an operator's `--wait-seconds`. The *effective* default is the flag's value. |
| Run-level call budget | Only the wall-clock bound (`--turn-timeout`) exists. There is no "maximum total provider calls per run" — spend is bounded by concurrency, lifetime run count, panel size and elapsed time. |
| Registry durability, and what it costs `taskId` | In-memory only; runs do not survive the transport closing or the process restarting. See [Run registry](#run-registry). This is also the honest limit on the tasks extension: the extension requires that a server not return a `CreateTaskResult` until the task is durably created, and ours is durable **for the life of the process** and no longer. A `taskId` presented to a restarted server yields `-32602` (unknown task), not a silently-empty one — which is the right refusal, but it is a refusal, and a host that persists task handles across its own restarts will meet it. The durable record of a run is the run directory on disk, not the registry. |
| The `fromRun` **source-run guard** is in memory, so a second apply reports differently after a restart | `review_remediate {fromRun}` itself survives: the handle is resolved from the source run's own `decisions/decision-set.json` when the registry no longer holds it, so an evicted or pre-restart run applies exactly as it does on ACP. What does **not** survive is the guard that answers a *replay* with the original receipt. Across a restart the second call is refused by the durable base-hash pins instead, as a `stale_decision_set` halt with nothing written — the same guarantee, a different report, and a client must branch on both. An on-disk "already applied" marker would unify them and was deliberately not built: a durable record on a write path is where a wrong record becomes a false success. See [One form: `fromRun`](#one-form-fromrun). |
| `-32021` for `tasks/*` without the extension, where the extension says `-32003` | A **declared deviation with a MUST on each side**, not an oversight. The tasks extension says servers "**MUST** return this error [`-32003`] for non-declaring clients issuing `tasks/get`, `tasks/update`, and `tasks/cancel` requests." The core base protocol says a server "**MUST** return a `MissingRequiredClientCapabilityError` (`-32021`) whose `data.requiredCapabilities` lists the missing capabilities" when a request needs an undeclared capability — and separately that, of the `-32000..-32019` range `-32003` belongs to, "receivers **MUST NOT** assume any specific meaning for these codes", which makes `-32003` semantically inert to a conforming client. We emit `-32021` with `data.requiredCapabilities`. This is our declared behavior, asserted as such at the test site, and it is not a conformance claim about the extension. |

## Surface parity

> For both apps, there is parity between ACP, MCP and CLI when performing reviews or explorations.

Every run-forming capability is expressible on all three surfaces with identical fail-closed semantics.

**reviewmesh:**

| Capability | CLI | ACP | MCP |
|---|---|---|---|
| Profile selection | `--profile` | `_meta.reviewmesh.profile` | `profile` |
| Ad-hoc panel composition | `--reviewer` ×N | `_meta.reviewmesh.panel[]` | `panel: {reviewers[], …}` |
| Per-role lane override | `--set role.adapter=`/`role.model=` | *(via the profile)* | `panel: {cross_check, verifier, author_remediator}` |
| Authority documents | `--authority`, `--authority-hash`, `--authority-manifest` | `_meta.reviewmesh.authority[]` | `authority[]` |
| Output mode | `--report`/`--patch`/`--apply` | `mode` (capped by the surface ceiling) | `review_report` (report) / `review_remediate` `output` |
| Apply the set you inspected (from-run write) | *(the one-turn full cycle: the human is the caller, is shown the run directory as the run starts, and has no response to lose)* | `session/prompt {mode: "apply", fromRun, select}` — reads the source run's **on-disk** decision set | `review_remediate {fromRun, select}` — the in-memory registry entry, falling back to the same **on-disk** decision set when it is gone |
| Selective apply | `--select` ×N (narrows the run's own adjudication) | `select[]` (narrows the **stored** set on a `fromRun` turn; the turn's own adjudication otherwise) | `select[]` (narrows the **stored** set) |
| Workspace | positional path (the human typed it — that *is* the consent) | `workspace` / session `cwd` / `inlineWorkspace`, judged against `--root` | `workspace` / `inlineWorkspace`, judged against `--root` |
| Turn budget | *(a foreground process the user can interrupt)* | `--turn-timeout` | `--turn-timeout` |
| Machine projection | `review --json` | the prompt response `_meta.reviewmesh` | `structuredContent` |

The **write-authority ceiling** is the deliberate per-surface difference: `cli: apply`, `ci: report`,
`acp: report`, `mcp: report` **+ the `allowRemediate` capability**. The capability exists on every surface;
only the default exposure differs by risk, and every one of them is config-visible.

**exploremesh:**

| Capability | CLI | ACP | MCP |
|---|---|---|---|
| Mode + mode params | `--mode`, `--artifact`, `--options`/`--criterion`, `--target`/`--unit`/`--horizon` | `_meta.exploremesh.{mode,artifact,options,compareCriteria,target,unit,horizon}` | `explore` (mode + its mode-specific params) |
| Profile + count | `--profile`, `--count` | `_meta.exploremesh.{profile,count}` | `panel: {profile, count}` |
| Ad-hoc panel composition | `--explorer` ×N + `--collator` | `_meta.exploremesh.panel {explorers[], collator}` | `panel: {explorers[], collator}` |
| Prior context | `--prior-context` | `_meta.exploremesh.priorContext` | `priorContext` |
| Parallelism bound | `--max-parallel` | `_meta.exploremesh.maxParallel` | `maxParallel` |
| Price it without running it | `--dry-run` | `_meta.exploremesh.dryRun` → `status: "planned"` + `shape` | `dryRun` → `dryRun: true` + `shape` |
| Run capture | `--dump-run` | `_meta.exploremesh.dumpRun` | on by default (`--no-capture` to disable) |

Only two kinds of per-surface difference are allowed, and both are explicit:

1. **Transport mechanics** — encodings (flags vs `_meta` vs tool params), the job shape and `waitSeconds`
   (MCP only), framing, and error carriers (exit codes / JSON-RPC `-32xxx` / `isError`). The same taxonomy
   payload rides all of them.
2. **Write-authority policy ceilings** — per-surface mode defaults are deliberate, config-visible policy.
   exploremesh has no write surface at all, so this reduces to nothing there; for reviewmesh it is the
   `mcp: report` ceiling plus the `allowRemediate` capability described above.

The **per-invocation vs launch** split in the last row of each table is not a third kind of difference;
it is the first one applied consistently. `--verify-cmd` decides what this server EXECUTES. On the CLI the person typing the flag is the operator, so the consent arrives with the
request. On ACP and MCP the caller is a peer process or a model, so the consent has to **precede** the
request — otherwise the party the grant protects against is the party issuing it. Same capability, same
fail-closed semantics, different point in time at which a human agreed to it.

Configuration introspection (`review_list`/`review_doctor`) is CLI/MCP-only by nature: ACP is a host-driven agent
protocol for running a turn, not a configuration surface. The invariant governs **run execution**.

## Worked example — a progress-tracked compare

```jsonc
// → tools/call
{ "name": "explore",
  "_meta": { "progressToken": "t1" },
  "arguments": {
    "mode": "compare",
    "purpose": "choose a datastore for the ingest service",
    "criteria": ["operable by a 3-person team", "no per-row licensing"],
    "options": ["Postgres", "ClickHouse", "SQLite"],
    "comparisonAxes": [
      {"name": "write throughput", "direction": "higher_is_better", "weight": 2},
      {"name": "operational burden", "direction": "lower_is_better", "weight": 1},
      {"name": "runs on one box", "role": "filter"}
    ],
    "panel": {"profile": "deep", "count": 3},
    "waitSeconds": 25,
    "idempotencyKey": "datastore-compare-2026-07-28"
  } }

// ← notifications/progress ×N (token "t1", monotonic)
// ← result (inline if it finished inside 25s, else {runId, state:"running"})
```

If it came back `running`, poll `review_run_status` with the `runId`, then `review_run_result`. If a *request* was lost
(a client-side timeout, a retry), re-issuing the **same** `idempotencyKey` returns that run rather than
starting a second one. If the *connection* drops the server exits and the run is cancelled with it — the
idempotency key then names nothing, and the surviving evidence is the run directory on disk.
