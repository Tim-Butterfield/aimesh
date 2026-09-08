# testdata/acp — sanitized ACP transcript fixtures

This directory holds **sanitized** ACP transcripts captured from a **real host** (Devin Desktop / Zed / JetBrains) during the manual verification procedure in [docs/acp.md](../../docs/acp.md) (which also has the ACP method mapping, the host-coverage checklist, and the sanitize-before-capture rules). They exist so a captured real-host interaction can be reviewed + (optionally) replayed as a reference, **without** committing raw captures or secrets.

## Rules

- **Sanitize before committing.** Remove/replace: session IDs (`s-XXXX`), UUIDs/request IDs, account/org/user IDs, auth tokens, machine-specific home paths, run directories (`<RUNDIR>`), timestamps (`<TS>`), and any provider raw metadata. A capture that cannot be sanitized must stay under `tmp/acp-host-verification/<timestamp>/` (keep `tmp/` in your local `.git/info/exclude`) and **must not** be promoted here.
- **No secrets, ever.** If in doubt, redact.
- **Raw captures are never committed.** Only minimal sanitized transcripts that add verification value belong here.
- **Default `go test ./...` does not read these as a pass/fail gate** and never requires a real host — the deterministic protocol coverage is the fake + `internal/surface/acp/testhost` subprocess harnesses. These fixtures are reference material for verify-on-provision.

## Format

One JSON object per line (`.jsonl`), in send order, each tagged with a `dir` (`host→agent` request/notification, or `agent→host` response/notification). Volatile fields are replaced with placeholders (`s-XXXX`, `<RUNDIR>`, `<TS>`, `<VERSION>`, `<WORKSPACE>`) so the transcript is deterministic + diff-friendly. The `initialize` exchange uses the **ACP v1 shape** (integer `protocolVersion: 1`; the agent replies with `agentCapabilities`/`agentInfo`/`authMethods`; `agentCapabilities.sessionCapabilities.resume: {}` is present when a durable session store is configured). `session/update` notifications are ACP v1 `SessionNotification`s (`params: {sessionId, update}` with the `sessionUpdate` discriminator — reviewmesh audit details under `update._meta.reviewmesh`; the `content.text` carries a trailing display newline while `_meta.reviewmesh.message` stays clean), and the terminal `session/prompt` response is an ACP v1 `PromptResponse` (`{stopReason, _meta.reviewmesh}`). See `sanitized-transcript.example.jsonl` for the expected shape (it is an **illustrative reference**, not a capture from a specific host).

## Capture helper

`scripts/acp-sanitize-transcript.sh <raw-capture-under-tmp>` reads a raw transcript from `tmp/` and emits a sanitized version to stdout (redacting the fields above). It is opt-in and never invoked by tests.
