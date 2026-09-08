# aimesh — Test fixtures (specification)

> Fixtures and golden files used by the tests described in [../CONTRIBUTING.md → Tests](../CONTRIBUTING.md#tests). They are **data, not code**. JSON payload fixtures validate against the schemas in [docs/schema/](../docs/schema/). This directory is **shared by meshcore and reviewmesh tests** (it lives at the repo root, not inside either module). This file specifies the tree; representative fixtures are committed here, and the remaining directories are populated during implementation.

```
testdata/
  acp/                sanitized real-ACP-host transcript fixtures (see acp/README.md)
    sanitized-transcript.example.jsonl   illustrative sanitized lifecycle transcript
  adapters/           captured/sanitized samples + fake-binary fixtures per adapter
    agy/  claude/  codex/  devin/
  call-status/        normalized CallStatus samples
    success.json
    identity-mismatch.json
  golden-run/         expected run-directory artifacts for the E2E golden comparison (`make golden-run`)
    call-status.json
    resolved-plan.json
    review-summary.md
    run-state.json
    summary.txt
  reviewer-output/    reviewer JSON responses (valid + adversarial)
    valid-findings.json
    empty-approve.json
    malformed-findings.json
    schema-invalid.json
    duplicate-findings.json
    reraise-rejected.json
  workspaces/         input trees for workspace/E2E tests
    simple/           a small clean tree to review
    mutation/         a tree whose fake reviewer mutates the copy (drives M5)
```

## What each fixture exercises

- **reviewer-output/** — the reviewer parser + adjudicator: a normal result, a clean approve, unparseable JSON (→ Class G), schema-invalid JSON (→ Class G), duplicates (→ dedup by fingerprint), and a re-raise of a previously-rejected finding (→ addressed-context honored). See [docs/prompts.md → Golden fixture expectations](../docs/prompts.md#golden-fixture-expectations).
- **call-status/** — identity verification: `success.json` verifies; `identity-mismatch.json` drives Class E (a proven STRONG-evidence mismatch; not retried — unknown/weak self-report is a pass-with-caveat).
- **workspaces/** — `simple/` for report/patch/apply E2E via a fake adapter; `mutation/` for the containment M5 path.
- **adapters/** — fake binaries/scripts implementing each adapter's recipe shape ([docs/adapters.md](../docs/adapters.md)) plus, once captured, sanitized real-CLI samples (the `needs_authenticated_capture` items).
- **golden-run/** — the expected run-directory artifacts (`call-status.json`, `resolved-plan.json`, `review-summary.md`, `run-state.json`, `summary.txt`) for the end-to-end golden comparison run by `make golden-run`; see the run-artifact layout described in [docs/security.md](../docs/security.md) (the `audit` run-directory paragraph) and the schemas in [docs/schema/](../docs/schema/).
- **acp/** — sanitized real-ACP-host transcripts for verify-on-provision ([docs/acp.md](../docs/acp.md)). Reference material, **not** a default-test gate; raw captures are never committed (they stay under `tmp/`). See [acp/README.md](acp/README.md).

## Fixture conventions

- JSON fixtures are formatted (2-space) and must validate against their schema — validated by `make test-all` (there is no CI; this is enforced locally and by anyone running the standard gate).
- A fixture intended to be invalid (e.g. `malformed-findings.json`, `schema-invalid.json`) documents *why* in a top-of-file comment where the format allows, or in this README otherwise (`malformed-findings.json` is deliberately not valid JSON, so its rationale lives here: it is truncated mid-object to exercise the unparseable path).
- Captured real-CLI samples are **sanitized** (no secrets, no private code) before commit.
