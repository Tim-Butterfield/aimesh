# Contributing to aimesh

Thanks for your interest. `aimesh` is a Go monorepo built around [`meshcore/`](meshcore/), a separate module joined by `go.work`, and the application itself — [`internal/review/`](internal/review/) and [`internal/explore/`](internal/explore/) behind the single [`cmd/aimesh/`](cmd/aimesh/) binary. Read [docs/architecture.md](docs/architecture.md) first; it explains the boundary that most of these rules protect.

## Prerequisites

- **Go 1.26+** — and nothing else. `make` is a convenience, not a requirement: every target wraps
  ordinary `go` commands, and `go install ./cmd/aimesh` installs the binary on its own.

## Build & test

Because the workspace spans several modules, a root `go build ./...` does **not** cover the nested modules. Use the workspace-aware `make` targets:

```bash
make gate         # gofmt + vet + build + test + race + boundary-check + both golden runs  (~90s)
                  #   ← run this before every PR
```

Individual targets:

| Target | What it does |
|---|---|
| `make build-all` | build every workspace module |
| `make install` | install the `aimesh` binary into `GOBIN`, and remove any superseded `reviewmesh` / `exploremesh` binary. Plain `go install ./cmd/aimesh` does the install part without `make`. |
| `make test-all` | test every workspace module (`-count=1`: the gate never trusts the test cache) |
| `make race-all` | every package under the race detector — the gate's check |
| `make race` | the fast subset (transports, stdio pumps, the run registry) for a quick local loop |
| `make fmt-check` | fail on gofmt drift (`gofmt -l` alone exits 0, so its output is what is tested) |
| `make boundary-check` | enforce the meshcore import boundary (see below) |
| `make golden-run` | assert reviewmesh's behavior is unchanged against a golden fixture |
| `make golden-run-exploremesh` | the same behavior-unchanged assertion for exploremesh |
| `make fmt` / `make vet` | `gofmt -w .` / `go vet` across every module |
| `make windows-build` | cross-compile every module for `GOOS=windows` (**not** part of `make gate`) |
| `make dist` / `make smoke-dist` | build the release archives, and smoke the host archive from outside the repo — what the release workflow runs |

### Releases

A release is cut by pushing a tag: `git tag v0.2.0 && git push origin v0.2.0`. The
[release workflow](.github/workflows/release.yml) re-runs `make gate` on that commit, builds the
archives with `make dist` (version, commit and dirty flag stamped from the tag), smokes the host
archive, attests build provenance for every artifact, and publishes the GitHub Release. Nothing is
built on a laptop. A consumer verifies an archive with
`gh attestation verify <archive> --owner Tim-Butterfield`. Only a repository admin can create or move
a `v*` tag (a tag ruleset enforces it).

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs `make gate` on every push and pull
request and nothing else, so the Makefile stays the single definition of green; run it locally too, so
a red push never happens. The standard gate (`make gate`) must be green before a change lands. Both golden runs are part of it, so an intentional output change in either app means regenerating that app's baseline deliberately (`GOLDEN_UPDATE=1`), never loosening the comparison.

## The meshcore boundary

meshcore is a headless, domain-agnostic library, and that independence is mechanically enforced by `make boundary-check`. It fails on:

1. **reverse imports** — meshcore importing a `reviewmesh`/`exploremesh` package;
2. **app ↔ app imports** — the two apps importing each other;
3. **`net/http` (or any UI/HTTP) dependency** inside meshcore;
4. **app-domain vocabulary in a meshcore exported identifier** — the denylist `review`, `reviewer`, `finding`, `remediation`, `lane`, `roster`, `explorer`, `collator`, `synthesis`, `task`, `reviewmesh`, `exploremesh` (strict on exported identifiers; warn-only in comments; report-only in test fixtures) — with narrow, per-package **protocol exemptions** for a word an external standard owns. There is one today: `task` inside `meshcore/mcp/`, because the MCP `2026-07-28` `io.modelcontextprotocol/tasks` extension names the concept. Exemptions are keyed by (package-path prefix, term), apply to both enforcing tiers, and adding one is a boundary decision — see `protocolExemptions` in `scripts/boundarycheck/main.go` and [docs/architecture.md → The meshcore boundary](docs/architecture.md#the-meshcore-boundary).

Practical rules of thumb:

- Substrate logic (adapters, identity, containment, config store, doctor primitives, ACP transport, halt, audit) goes in **meshcore**, named in neutral vocabulary.
- Domain logic (review lanes/roles/`Finding`/adjudication; explorers/collator/task) stays in the **app**.
- When splitting a mixed component, **split-then-move**: separate the generic half in place (app stays green), *then* move only the generic half to meshcore. Never move-then-split — that lands domain vocabulary in meshcore and fails the check.

## Repository layout

```
cmd/aimesh/        the single binary (root module)
internal/review/   review domain (root module)  — imports meshcore
internal/explore/  explore domain (root module) — imports meshcore
meshcore/          library (own go.mod)         — imports neither domain
scripts/           repo tooling (own go.mod)    — boundarycheck; stdlib only
docs/              cross-cutting docs, schemas, diagrams
testdata/          fixtures shared by meshcore + the domain tests
```

Add a new workspace module by creating its `go.mod` and adding a `use ./<dir>` line to `go.work`.

### Module dependencies (`go.work` is not enough)

`go.work` joins the modules for *building*, but **`go mod tidy` ignores it** — tidy resolves a single
module's imports as if the workspace did not exist. Two consequences to respect:

- **Every module declares its own dependencies.** A module that imports a package must `require` it
  even when a sibling already does; the workspace will happily build without the declaration, hiding
  the gap until someone consumes the module on its own.
- **The apps `replace` meshcore to `../meshcore`.** The module is unpublished, so tidy would otherwise
  try to fetch `github.com/Tim-Butterfield/aimesh` and fail. The `replace` is ignored in workspace mode
  (go.work wins), so it changes nothing about the normal build.

Together these keep each module independently buildable. Verify with the workspace turned off:

```bash
( cd meshcore && GOWORK=off go build ./... && GOWORK=off go mod tidy )   # and likewise per module
```

If meshcore is ever published as a module of its own, drop the `replace` line and pin a real version.
Until then `go install github.com/Tim-Butterfield/aimesh/cmd/aimesh@latest` cannot work (a module with a
`replace` directive is refused by that form); install from a clone with `go install ./cmd/aimesh`.

## Tests

- Unit and integration tests run under `make test-all` and must be hermetic — no network, no real model CLIs. The deterministic **Fake** adapter (and reviewmesh's hidden `fake-smoke` profile wired to it) exists for exactly this reason, and it is a strictly **internal test harness**: both are env-gated behind `AIMESH_INTERNAL_FAKE=1` (documented here only — for contributors and tests, never a user knob). Without the gate, `fake` / `fake-smoke` fail resolution with the same fail-closed unknown-name error as any typo; tests set it per-test with `t.Setenv("AIMESH_INTERNAL_FAKE", "1")` (or in a package's shared helper/child env), and the golden-run scripts export it themselves. The user-facing `default` profile ships unconfigured, so tests select `fake-smoke` by name.
- `REVIEWMESH_FAKE_SCENARIO` selects the fake adapter's behavior (`valid`, `empty`, `malformed`, `churn`, `malformed_then_valid`, …) so a test can drive the retry, non-convergence and parse-failure paths. Like `AIMESH_INTERNAL_FAKE`, it is a **contributor/test knob only** and is deliberately absent from the user-facing docs; it does nothing unless the fake gate is also set.
- Tests that need a real provider are opt-in and never run in the standard gate. The genuinely-enforced gate is `REVIEWMESH_RUN_REAL_OLLAMA_MANAGER_SMOKE=1` (with `REVIEWMESH_OLLAMA_MODEL=<tag>`), which un-skips the real-Ollama manager smoke tests. `REVIEWMESH_SMOKE_*` and `REVIEWMESH_RUN_REAL_PROVIDER_TESTS` are **naming conventions only** — no test reads them for a value; the opt-in is running the command against the corresponding real-adapter smoke profile.
- The **golden run** captures reviewmesh's resolved plan, `call-status`, `run-state`, and `review-summary` against a fake-profile fixture. Regenerate it deliberately with `GOLDEN_UPDATE=1 make golden-run` when a change *intentionally* alters that output.

## Adding an adapter

The most common contribution is support for a new model CLI. Adapters live in `meshcore/model/<name>` and implement the `model.Adapter` contract (atomic verbs only — CLI flags never leak past the adapter). See **[docs/adapters.md](docs/adapters.md)** for the full step-by-step, the required fake-binary test scenarios, and how to register the adapter so it becomes selectable in reviewmesh lanes and exploremesh rosters.

## Coding conventions

- Match the surrounding code's style, naming, and comment density.
- Keep meshcore's exported API minimal and documented — it is treated as a contract.
- `gofmt` (`make fmt`) and `go vet` (`make vet`) should be clean.

### Sunset-path code is marked where it is written

Some code exists only to serve a protocol revision we intend to delete. **Mark it at the moment you
write it**, in the same commit — a comment naming it sunset-path and citing the removal task id:

```go
// SUNSET-PATH (MCP26-SUNSET): legacy `2025-06-18` only. `2026-07-28` has no pre-initialize state,
// so this branch is deleted — not renumbered, not generalized — when the legacy era is removed.
```

The reason is not tidiness. Transitional scaffolding that is never labelled becomes indistinguishable
from architecture within one reading, and the next person maintains it as though it were load-bearing.
Retrofitting the labels later means re-deriving *which* code was transitional, which is exactly the
knowledge that has already been lost by then.

**The task id is not decoration, and `make boundary-check` now fails the build without it.** A
`SUNSET-PATH` comment that does not cite `MCP26-SUNSET` is a failure, in any `.go` file in the repo,
test files included. The reason is specific: the removal is performed by grepping for the id, so a
marker the grep does not find is a marker that does not exist. Every marker the MCP migration wrote
named the concept ("legacy removal task") and none named the id — the convention was followed in spirit
and broken in letter at all 28 sites, invisibly, for nine phases.

The live example is the MCP legacy era. Its removal task is tracked under the id **MCP26-SUNSET**;
its yes/no trigger (adoption of the modern era by the hosts we support, not a date) and the complete
site-by-site checklist are the `SUNSET-PATH` markers that cite it. The task is re-evaluated whenever an
MCP-related change is made, so it cannot silently become permanent.

## Pull requests

- Keep changes small and self-contained; each PR should leave `make gate` green.
- Describe what changed and why; note any intentional golden-run or schema changes.
- Security-sensitive changes: see [SECURITY.md](SECURITY.md).
