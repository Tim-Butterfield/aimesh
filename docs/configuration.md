# Configuration

`aimesh` is a monorepo of one library and two application domains: [meshcore](../meshcore/) (the governed multi-model substrate), [reviewmesh](../internal/review/) (review), and [exploremesh](../internal/explore/) (exploration/research). Configuration is split by ownership even though, today, most of it lands in one file per app:

- **meshcore** owns the domain-free config **store** — atomic writes, YAML/JSON parsing, and validated patch application (`meshcore/config`). It never learns an app's schema; each app injects its own validator.
- **reviewmesh** owns its own typed schema and file layout (adapters, model catalog, profiles, lanes, surface defaults, the review loop's iteration caps) on top of that store.
- **exploremesh** owns a separate schema — named **profiles**, each an ordered explorer set + one collator + an optional default mode — persisted to `~/.aimesh/explore/profiles.yaml`. An explicit `--roster <file>` per invocation is still supported.
- **Adapter binary paths and user-defined ACP instances are SHARED** between the two apps in `~/.aimesh/adapters.yaml` — see [the shared `~/.aimesh` layout](#the-shared-aimesh-layout-shipped) below.

This page documents **current behavior**.

## reviewmesh: file locations & precedence

Resolution order, highest priority first:

1. **Invocation override** — values passed per run (CLI flags such as `--set`, or `--config <path>`).
2. **Project-local config** — `./.aimesh/review/config.yaml` in the working directory. Optional and override-only (a project-specific profile, a project default profile, per-project lane/model choices). Not required for normal use.
3. **User/global config** — `~/.aimesh/review/config.yaml` (Windows: `%USERPROFILE%\.aimesh\review\config.yaml`). This is the normal home for personal defaults — a preferred profile, personal model/role choices. (Adapter **binary paths** are not here: they live in the shared `~/.aimesh/adapters.yaml`, which overlays this file's `adapters` section — see [the shared layout](#the-shared-aimesh-layout-shipped).)
4. **Shipped defaults** — built into the binary.

`LoadLayered` composes exactly this order: shipped ← user/global ← project ← explicit `--config`. The user and project layers are each optional — a missing one is not an error. `aimesh review setup` defaults to `--scope user` (writes `~/.aimesh/review/config.yaml`); `--scope project` writes `./.aimesh/review/config.yaml`. `aimesh review doctor` reports which layers were loaded. `AIMESH_HOME` overrides the user/global base directory (used by tests and for hermetic runs, so tests never touch a real home directory).

Within `~/.aimesh/review/` (and `./.aimesh/review/`), the config file is looked up as `config.yaml`, then `config.yml`, then a legacy `config.json` — YAML is written by `setup`, but a hand-authored or older `config.json` is still read for compatibility.

Overrides apply at field granularity — override just the reviewer's model or just the cross-check's adapter, and inherit everything else.

## Config sections, by owner

The single `config.yaml` file contains sections owned by different layers of the codebase:

**meshcore-shape sections** (config *shape* is app-defined today, but the concepts — adapters, models, resolution defaults — are meshcore's domain):

| Section | Contents |
|---|---|
| `adapters` | Per-adapter capabilities: `path` (binary location when not on `PATH` — sourced from the shared `.aimesh/adapters.yaml`, see below), `modelIdentity` (the adapter's intended identity-evidence ceiling), `enabled`. The probe command, argv, and identity extraction are **code-owned** (the meshcore shell recipe or the generic ACP adapter), not configurable. |
| `modelCatalog` | Canonical model profiles: `provider`, `runtime` (only the literal `local` is read — it is what marks an entry as on-machine for the Privacy view; any other value, including `cloud`, is inert), `canonicalModel`, `effort`, `displayName`, `authoritative`, `adapterDefault` (this entry is an adapter's fallback default, not a saved model preset), and an `adapters:` map giving each adapter's `modelArg` (+ optional per-adapter `effort`). **`authoritative` does not gate the host role.** Despite the name, nothing in resolution consults it: it is read only by the setup housekeeping flows, where — together with `adapterDefault` — it marks an entry as shipped or hand-authored and therefore **protected** from saved-model overwrite and generated-key cleanup. Every seeded entry (including `ollama-local`) sets it `true`. Which model fills the host role is decided by the profile's `lanes.author_remediator`, and by nothing else. |
| `defaults` | Zero-config resolution: `adapterPreference` (walk order) and `profileForAdapter` (which profile each adapter family defaults to), both consulted **only** when `defaultProfile` is empty. |

**reviewmesh-owned sections** (the review domain — lanes, roles, findings, the review loop):

| Section | Contents |
|---|---|
| `profiles` | Named, selectable bundles of the ordered `reviewers` panel (the blind primary stage) plus the single-slot `lanes` (role → adapter/model assignment) and an `adapterPreference`. `defaultProfile` names the one used when nothing is selected. See [Profiles and the reviewer panel](#profiles-and-the-reviewer-panel). |
| `review` | Iteration/convergence: `maxInnerIterations` (default **4** — stabilization rounds per reviewer seat per outer cycle), `maxOuterCycles` (default **3** — full review → adjudicate → remediate cycles), and `maxPanelRounds` (optional — the ceiling on the TOTAL blind-primary rounds one outer cycle may spend across ALL seats). All are enforced by the review loop. |
| `surfaces` | Per-surface write-authority policy: `defaultModeBySurface`, `capabilitiesBySurface`, and `degradeWhenModeUnavailable`. See [Surface ceilings and capabilities](#surface-ceilings-and-capabilities). |

### Surface ceilings and capabilities

A **surface** is where a run was driven from — `cli`, `ci`, `acp`, `mcp`. Each has a **write-authority
ceiling**: a request may resolve narrower than its surface's ceiling, never wider.

```yaml
surfaces:
  defaultModeBySurface:      # shipped seed
    cli: apply
    ci: report
    acp: report
    mcp: report
  capabilitiesBySurface:     # not seeded — grants are explicit
    mcp: [allowRemediate]
  degradeWhenModeUnavailable: true    # default true
```

- **`defaultModeBySurface`** is the **ceiling — not the default**, despite the name (which is kept for
  config compatibility). A run that names **no mode gets `report` on every surface**; these entries bound
  what a surface may do when it *is* asked. So `cli: apply` means "`--apply` is permitted here", **not**
  "`aimesh review run .` writes" — an unadorned run reports and writes nothing.

  The two were once the same value, which is how the plain CLI command came to modify a user's tree
  without being told to. They are different questions: the ceiling is the most a surface may **ever** do;
  the default is what it does when not told. `acp` and `mcp` sit at `report` because both are driven by an
  external caller — an ACP host, or a client *model* — so even an explicit write request there is refused
  until the ceiling is deliberately widened. A surface with **no** entry keeps the historical fall-through
  ceiling of `apply`; a surface that must fail closed therefore ships with an explicit `report` entry
  rather than relying on the absence of one.
- **`capabilitiesBySurface`** grants named policy capabilities to a surface. The only capability today is
  **`allowRemediate`**, which **raises** that surface's ceiling to `apply`. It is deliberately part of the
  config store rather than an out-of-band launch exception: the ceiling is computed *from* this config
  (`config.SurfaceCeiling`), so a grant raises the ceiling instead of stepping around it — a ceiling a flag
  could bypass would not be a ceiling, it would be a default.
- **`aimesh review mcp --allow-remediate`** grants `allowRemediate` on the `mcp` surface **in that server
  process's own config snapshot**. It writes nothing to disk; an operator who wants it for every MCP server
  they launch sets it in config instead. Without the capability the `review_remediate` tool is not listed at
  all, and with it every call must still pass `allowWrite: true`. See [mcp.md](mcp.md#review_remediate--the-double-opt-in).
- **`degradeWhenModeUnavailable`** (default `true`) decides what happens when a request exceeds the
  effective ceiling: degrade it and say so (a `mode_degraded` warning plus `modeDegraded`/`requestedMode`/
  `modeReason` on the result), or refuse the request outright before anything is spent.

Effective mode is always `min(requested, surface-capability, policy)` — see
[acp.md → Mode gating](acp.md#mode-gating-reviewmesh-only).

**There are no reserved-for-later sections.** Earlier drafts accepted `policy` (including `policy.providerDiversity`), `validation`, `containment`, `audit`, and `timeouts` as free-form maps that nothing read. They are **gone**: reviewmesh does not accept-and-ignore settings, so those keys now fail strict parsing with a configuration error (exit 3) instead of silently having no effect. The concepts they named are live but not config-driven — reviewer containment and the run-directory base (default `.aimesh/review/runs/` when a `.aimesh/` state directory exists, else the OS temp directory; overridable only with `REVIEWMESH_ARTIFACT_DIR`) are hardcoded Go behavior, and provider diversity is an advisory note when you create a single-adapter profile, not an enforced policy.

Two keys survived that rule by accident and have since gone the same way: `defaults.autoDetect` and
`profiles.<name>.lanes.<role>.optional` parsed and merged across layers while being read by **nothing**.
Both are now rejected like any other unknown field. (Adapter detection is a `setup`/`doctor` action a human
runs, not a config-driven behavior; and whether a lane's phase runs is decided by the lane's **presence**,
so a boolean saying "this one may be missing" added nothing to a map whose keys already say which lanes
exist.) A test asserts both are refused — the rule is only worth stating if something checks it.

### Profiles and the reviewer panel

A reviewmesh profile has two distinct parts, and the distinction is governance rather than
convenience:

| Key | Contents |
|---|---|
| `profiles.<name>.reviewers` | The **ordered blind primary panel**: 1..16 seats, each `{execution: adapter, adapter, model}`. Every seat reviews the workspace independently and in parallel, seeing only its own earlier rounds. The slice order is the panel's authored order and is recorded with every finding's provenance. **A seat here takes no `effort`** — a configured seat's effort comes from the model-catalog entry it names (`modelCatalog.<key>.effort`, or the per-adapter `adapters.<a>.effort` under it). A *per-seat* effort override exists only on an ad-hoc panel (`--reviewer …,effort=…`, ACP `_meta.reviewmesh.panel`, MCP `panel.reviewers[]`), so to run the same model at two depths from a profile, give the two seats two catalog entries. |
| `profiles.<name>.lanes` | The **single-slot roles**: `author_remediator` (the host — the only writer), `cross_check` (informed: it sees the *adjudicated* set, and its findings can be applied), `verifier` (report-only). These are not seats and never become a list, however large the panel grows: informed-vs-blind and applyable-vs-report-only are different information topologies, not different counts. |
| `profiles.<name>.adapterPreference` | Ordered adapter fallback used when a lane names no adapter. |

```yaml
profiles:
  three-seat-panel:
    description: Three independent vantages, adjudicated by one host.
    reviewers:
      - { execution: adapter, adapter: codex-cli,   model: codex-cli-default }
      - { execution: adapter, adapter: claude-code, model: claude-code-default }
      - { execution: adapter, adapter: agy-cli,     model: agy-cli-default }
    lanes:
      author_remediator: { execution: host, adapter: claude-code, model: claude-code-default }
      verifier:          { execution: adapter, adapter: agy-cli, model: agy-cli-default }
```

**Migration — `lanes.reviewer` is a panel of one.** The historical single-reviewer spelling is
accepted verbatim and normalized on read to `reviewers[0]`; nothing about such a profile's behavior,
call ids, or artifacts changes. A config layer that names **both** `reviewers` and `lanes.reviewer`
for the same profile is a **configuration error** — there must never be a question of which one won.
Across layers there is no ambiguity to resolve: a layer supplying one spelling *replaces* the other
(and a panel is replaced wholesale, never element-merged — a seat list is an ordered composition, not
a bag of keyed settings), so the effective config always carries exactly one. Saving a panel through
`setup` performs that migration in the same atomic write.

**Fail-closed panel rules**, all checked before anything is spent:

| Refusal | Why |
|---|---|
| A panel with 0 seats, or more than 16 | The blind stage is what produces findings; each seat is a real model CLI, so an unbounded panel is a spend hazard. The count is never trimmed for you. |
| Two seats with the same `(adapter, model, effort)` | An identical seat adds no independent vantage and would double-count as agreement. (Configured seats carry no per-seat effort, so in a profile this reduces to a duplicate `(adapter, model)`.) |
| A seat naming an unknown/disabled adapter, an unknown model, or a model with no argument for that adapter | A seat that cannot run is not silently dropped. |
| `review.maxPanelRounds` below the seat count | Every requested seat must be able to run at least one round — a budget that quietly drops a seat is still silent degradation. |

At run time, a seat failure (identity mismatch, adapter failure, containment breach, unparseable
output after the corrective retry) **halts the run**: there is no partial-panel success. The run
record's `panel/roster.json` and the `--json` projection's `panel[]` echo one entry per *requested*
seat either way.

**exploremesh-owned schema** — `profiles.yaml`, a separate file, not part of `~/.aimesh/review/config.yaml`:

| Key | Contents |
|---|---|
| `schemaVersion` | The profiles-file version this build writes and reads (currently **1**). An absent version is treated as current; a **newer** version is refused rather than partially understood. |
| `defaultProfile` | Names the profile a no-flag run binds to. Required, and must name a defined profile. Every app surface keeps it at `default` — there is no set-default feature (as in reviewmesh); a hand-written pointer is honored at load but never rewritten. |
| `profiles.<name>.explorers` | An **ordered** list of `{adapter, model, effort}` triples. Minimum 2, no duplicate triples (differing only in `effort` is fine and intentional). **The slice order is preference order** — what `--count N` takes the top-N from — and is deliberately *not* the attribution order (the executable plan sorts the selected subset canonically, so reordering the full list never changes an unchanged selection's envelope IDs). |
| `profiles.<name>.collator` | Exactly one `{adapter, model, effort}`. Performs the terminal collation; may reuse an explorer's model. There is no identity opt-in: a collator whose model identity cannot be verified runs anyway, with the classification recorded (see [model-identity.md](model-identity.md)). |
| `profiles.<name>.defaultMode` | Optional. The exploration mode a run uses when `--mode` is omitted (empty → the app default, `map`). A non-empty value must name a registered mode — an unknown one is a config error, caught before it can silently coerce at run time. |

```yaml
# ~/.aimesh/explore/profiles.yaml
schemaVersion: 1
defaultProfile: default
profiles:
  default:
    explorers:
      - { adapter: claude-code, model: sonnet, effort: high }
      # gpt-5-codex requires an API-key Codex account; a ChatGPT-account Codex rejects it with an
      # HTTP 400 at invocation. Pick a slug from `codex debug models --bundled` for your account.
      - { adapter: codex-cli,   model: gpt-5-codex }
    collator: { adapter: claude-code, model: sonnet }
  triage:
    defaultMode: challenge
    explorers: [ { adapter: agy-cli, model: gemini-3.1-pro-high }, { adapter: codex-cli, model: gpt-5-codex } ]
    collator: { adapter: agy-cli, model: gemini-3.1-pro-high }
```

The `adapter` named in an `explorers[]`/`collator` entry is resolved against the **same adapter registry meshcore builds for reviewmesh** (`shell.Recipes()` plus the user-defined ACP instances from `~/.aimesh/adapters.yaml`): a real recipe name (`claude-code`, `codex-cli`, `ollama`, `agy-cli`, `devin-cli`, `gemini-cli`, `cursor-cli`) or a configured ACP instance runs that CLI. Resolution is **fail-closed**: any other name is a config error reported before anything is spent — it is never silently substituted.

**explore config lives in two places**, both with user + root-anchored project scopes:
- `~/.aimesh/explore/profiles.yaml` — the named profiles. Discovery order: project `profiles.yaml` → user `profiles.yaml` → the built-in **unconfigured** `default` profile (empty — nothing runs until a panel is configured). Those are the only two locations searched: a `profiles.yaml` at the **root** of a `.aimesh/` directory (beside `adapters.yaml`, the plausible wrong guess) is an explicit **error** naming both valid paths, never a file silently ignored. Selected with `--profile <name>`; `--count N|all` runs the top-N by preference order (out of range, or `<2`, is a clear error — the count is never clamped). An explicit `--roster <path>` overrides discovery, and carries no `defaultMode`.
- `~/.aimesh/adapters.yaml` — the **shared** adapter binary paths + ACP instances (see below). Configured per-adapter paths apply to exploremesh exactly as they do to reviewmesh.

Profiles are editable with `aimesh explore setup` or by editing the file; adapter paths and ACP instances are editable with `aimesh explore setup` / `aimesh review setup`. `aimesh explore list [--json]` reports the configured adapters, the resolved roster, every profile, and the available modes.

## Merge rules (reviewmesh)

These rules are exact so behavior is deterministic. Each layer is validated against the typed schema; a structural failure is a **configuration error (exit 3)**.

- **Maps merge recursively** by key — a higher layer adds/overrides keys; unspecified keys are inherited.
- **Scalars replace** — a higher layer's value wins.
- **Lists replace wholesale** — a higher layer's list is used as-is, not concatenated. There is no implicit element merge. The only keyed-list-like merge is the `modelCatalog`/`profiles`/`adapters` **maps** (objects keyed by name, so they merge per key, not as lists).
- **Omission means inherit.** A key a layer does not mention keeps the lower layer's value. An explicit
  JSON/YAML `null` is currently treated the *same* as omission — it inherits. There is **no
  override-to-null / delete-this-key mechanism** today; to remove a value, edit the layer that sets it.
  (The one presence-aware exception is `adapters.<name>.path` in the shared `adapters.yaml`, where an
  explicit empty string `""` is a deliberate "use `PATH`" clear that overrides an inherited path.)
- **Unknown keys are rejected** under strict parsing (unknown struct fields fail to load); free map keys (profile names, adapter names, catalog keys, lane names) are always allowed where the schema expects a map.
- **No environment-variable expansion** — config is data, not a shell; `${VAR}` is a literal. Secrets never belong in config.
- **Paths are used as written.** There is **no `~` expansion and no relative-path rebasing** in the config
  loader: a path in config is passed through verbatim, so a relative `adapters.<name>.path` is resolved by
  the OS against the *process* working directory, not against the config file. Prefer an **absolute** path,
  or a bare `PATH`-resolvable binary name. (`~/.aimesh/review/…` in this document names a location the code
  computes from the home directory — it is not a literal string you can write into a config value.)

## The write path: meshcore's config store

Every governed config write — from any surface (CLI `setup`, `doctor --fix --interactive`) — goes through the same path: `meshcore/config`'s domain-free store, given an app-injected validator.

- **`meshcore/config.LoadRawMap`** reads a file into a generic map after the injected validator accepts it.
- **`meshcore/config.ApplyPatchToFile`** is the single mutation path: it validates the existing file (refusing to touch one that doesn't already validate), applies a `Patch`, ensures `schemaVersion`, re-validates the serialized result, and only then writes — atomically (temp file + fsync + rename), so a crash never leaves a partial config.
- **`meshcore/config.Patch`** is an ordered list of `SetOp{Path, Value, Delete}` operations:
  - `set <path> <value>` — set/replace a scalar or object at a nested path. Creates intermediate maps as needed; errors rather than overwriting a non-map intermediate. A full-subtree `set` also expresses **profile copy** (`profiles.<target>` = the source profile's content).
  - `delete <path>` — removes the leaf key at a nested path (no intermediate maps created; deleting an absent path is a no-op).
  - `append <path> <value>` and `replaceList <path> <list>` are **not implemented** — future ops.
- No op ever writes a secret. Ops apply in order; unrelated content is always preserved.

reviewmesh's own config package builds its typed schema, layered loading, and resolution logic on top of this store; exploremesh's `profile` and `roster` packages do the same for `profiles.yaml` and a raw roster file (`Decode`/`Load`/`Save`, via `meshcore/config`'s strict decode, YAML/JSON conversion and atomic writer). The shared `adapters.yaml` has its own write path in `meshcore/config/adapterlocations` — a content-hash compare-and-swap read-modify-write, so a concurrent editor cannot clobber unrelated entries.

### `--set` grammar (CLI per-lane overrides)

`--set <role>.adapter=<name>` / `--set <role>.model=<name>` (repeatable) — a per-run **lane override**, addressing a lane of the resolved profile:

```sh
aimesh review run --set reviewer.adapter=codex-cli --set reviewer.model=openai-gpt-5.4-medium .
```

`--set reviewer.*` addresses the primary stage only when it is a **panel of one** — there is no
unambiguous way to point a per-ROLE override at one of N seats, and applying it to all of them would
collapse the panel's independence. Combining it with an ad-hoc `--reviewer` panel is a usage error;
put the value in the seat spec instead.

The grammar is deliberately narrow: **exactly two segments**, and the second must be `adapter` or `model`. It is a lane-override shorthand, NOT a general config editor — anything else (a deeper path such as `adapters.codex-cli.path=…`, a list value, a bool/int) is a **usage error (exit 2)**. To change adapter binary paths, edit `~/.aimesh/adapters.yaml` (directly, via `aimesh review setup --adapter <n> --path <p>`); to change other config, use `setup`, which writes through the governed patch path. Effort is set through the model catalog's `effort` field — there is no `role.thinking` shorthand.

## CLI

**reviewmesh:**

```
aimesh review run  [--profile <name>]
                    [--reviewer adapter=<name>,model=<m>[,effort=<e>]]…   # ad-hoc blind panel

aimesh review setup   [--scope user|project] [--profile <name>]
                    [--adapter <name> --path <path>]
                    [--acp detect|add|remove [--path <p>] [--name <k>] [--title <t>] [--acp-arg <a> …]]
                    [--interactive]
                    [--from user [--yes] [--include-adapter-paths]]

aimesh review config clean-model-keys [--apply]

aimesh review acp     [--framing newline|content-length] [--root <dir> …]
                    [--no-default-root] [--allow-broad-root] [--turn-timeout <dur>]
aimesh review mcp     [--protocol dual|legacy] [--framing newline|content-length]
                    [--root <dir> …] [--no-default-root] [--allow-broad-root]
                    [--allow-inferred-root] [--allow-remediate]
                   
                    [--wait-seconds <n>] [--turn-timeout <dur>]
```

- `setup` writes/updates config; defaults to `--scope user` (`~/.aimesh/review/config.yaml`). `--adapter <name> --path <path>` records a validated binary path for an adapter not on `PATH`, patching only that field. `--interactive` runs a guided wizard (inspect config + detect adapters → choose profile → record adapter paths → per-role lane/model selection → confirm before writing). `--from user [--yes] [--include-adapter-paths]` promotes selected keys (`defaultProfile` always; `adapters.<name>.path` only with `--include-adapter-paths`) from user scope into `--scope project` — it never bulk-copies the user config or any secret, and requires confirmation (`--interactive` prompt, or `--yes`).
- `review --reviewer adapter=<name>,model=<m>[,effort=<e>]` (repeatable, order-preserving) composes an **ad-hoc blind reviewer panel** for one run. The grammar is `key=value` — deliberately *not* the `adapter:model` colon shorthand, which is unsafe because model tags contain colons (`llama3:8b`). It may name only adapters and models the configuration already defines (compose-not-configure), and it is mutually exclusive with `--profile`: a panel is composed *or* selected. The same panel is expressible over ACP as `_meta.reviewmesh.panel`.
- `mcp --protocol dual|legacy` (default `dual`, on **both** binaries) is the **protocol-era posture**. `dual` serves whichever era the first client opens with and latches it for the process lifetime; `legacy` makes the process a pre-`2026-07-28` server in every observable respect, including answering `server/discover` as an unknown method. `legacy` is a compatibility fallback for a host that mis-probes, not a conformant `2026-07-28` deployment. See [mcp.md § The two eras](mcp.md#the-two-eras-and-how-a-request-picks-one).
- `aimesh review mcp --allow-inferred-root` is a **confinement waiver**, default off. On `2026-07-28` an *inferred* launch cwd is not a trusted root — that revision removed `roots/list`, so a client can no longer narrow the server for itself and an inferred root would silently widen what a modern client reaches. Without the flag, a modern client on an inferred cwd is refused every filesystem path (`scope_no_roots_configured`) and must use `--root` or `inlineWorkspace`. The waiver never overrides the degenerate-root rule or the denylist, and its state is reported by the `review_doctor` tool (`inferredRootWaived`) rather than only on stderr.
- `config clean-model-keys` (preview by default; `--apply` to write) renames generated `modelCatalog` keys that an older UI build materialized with a redundant adapter-key prefix (e.g. `codex-cli-gpt-5.5-high`), in the user-layer config only, leaving hand-authored keys and keys defined in a higher layer for manual editing.

**exploremesh** — there is no interactive setup wizard (reviewmesh's `setup --interactive` has no
counterpart here); configuration happens through the **non-interactive** `aimesh explore setup`
subcommand, or by editing `profiles.yaml` / `adapters.yaml`
directly. The config-relevant commands are:

```
aimesh explore run [--profile <name>] [--count <N|all>] [--roster <path>] [--mode <name>] …
aimesh explore list    [--json]
aimesh explore doctor  [--profile <name> | --roster <path>] [--probe] [--json]
aimesh explore setup   --adapter <name> --path <p> | --remove-adapter <name>
aimesh explore setup   --acp detect|add|remove [--path <p>] [--name <k>] [--title <t>] [--acp-arg <a> …]
aimesh explore setup   --profile <name> --explorer adapter=<n>,model=<m>[,effort=<e>] …
                    --collator adapter=<n>,model=<m> [--default-mode <m>]
aimesh explore setup   --delete-profile <name> --yes
aimesh explore acp     [--framing newline|content-length] [--turn-timeout <dur>] [--roster <path>]
aimesh explore mcp     [--protocol dual|legacy] [--roster <path>]
                   
                    [--wait-seconds <n>] [--turn-timeout <dur>] [--no-capture]
aimesh explore export  --sqlite <out.db> --run <run-dir> [--verify] [--json]
aimesh init [--require-repo | --require-folder]
```

- **Panel selection precedence:** `--roster <path>` (a raw YAML/JSON file with `explorers[]` + `collator`, carrying no named profiles and no `defaultMode`) > `--profile <name>` > the `defaultProfile`. `--roster` and `--profile` are mutually exclusive. `--count N` then takes the top N by preference order. With nothing persisted, exploremesh falls back to the built-in **unconfigured** `default` profile — a fresh install runs nothing until a panel is configured.
- **Ad-hoc, no config at all:** two or more `--explorer adapter=…,model=…[,effort=…]` plus a `--collator` build a one-off roster by identifier, bypassing both `--roster` and `--profile`.
- `init` / `repo init` / `folder init` create the local, VCS-excluded `.aimesh/` state directory. They seed **no** config — adapter locations are written by an explicit configuration step.
- Full command reference: [explore.md](explore.md).

## Environment variables

**Home / location overrides** — each is a *base* directory, and each is what tests set so a run never touches a real home:

| Variable | Effect |
|---|---|
| `AIMESH_HOME` | Overrides the base directory containing the user-scope `.aimesh/` — **one** variable for the whole tool. It covers the shared `adapters.yaml`, `review/config.yaml`, `explore/profiles.yaml`, and both ACP session stores. (Each domain used to carry its own home variable; three that had to be set together to get one hermetic run was a trap rather than a feature.) |
| `REVIEWMESH_ARTIFACT_DIR` | Where review writes run artifacts. It is the **only** way to relocate them — there is no `audit` config section. Unset, they follow the same rule as explore's below: `.aimesh/review/runs` when a state directory exists, else the OS temp directory. |
| `EXPLOREMESH_ARTIFACT_DIR` | Where exploremesh writes captured run directories — for CLI `explore --dump-run`, ACP `_meta.exploremesh.dumpRun`, and `aimesh explore mcp`, which captures **by default** (`--no-capture` disables it). Unset, the default is the **project-local `.aimesh/explore/runs`** when a `.aimesh/` state directory exists (root-anchored from the cwd, exactly like project-scope adapter config; `init` / `repo init` creates it and `repo init` adds it to the VCS exclude file), and otherwise a subdirectory of the **OS temp directory**. It is deliberately never a relative path inside your repository — a relative default resolved against the process cwd creates a tree inside whatever checkout you happened to run from, and run artifacts embed verbatim copies of everything the models were shown. The wire `runId` is the run directory's name; the absolute path is never put on the wire. |

**Model / adapter behavior:**

| Variable | Effect |
|---|---|
| `REVIEWMESH_OLLAMA_MODEL` | The Ollama model tag used by the local-Ollama example profile (`fully-local-ollama`); an empty/unset tag fails resolution with guidance. |
| `REVIEWMESH_ACP_VALIDATE_HOSTADJ` | Set to `1` **only** by the web ACP-validation harness when it spawns the child agent, enabling a synthetic host-adjudication probe. A normal `aimesh review acp` invocation leaves it unset. |

**Opt-in real-model test gates** (these spend tokens/credits; the default `go test ./...` sets none of them):

| Variable | Effect |
|---|---|
| `REVIEWMESH_RUN_REAL_OLLAMA_MANAGER_SMOKE` | Set to `1` (together with `REVIEWMESH_OLLAMA_MODEL=<tag>`) to un-skip the real-Ollama manager smoke tests. Without both, those tests skip. This is the **only** real-provider test gate the code actually reads. |
| `REVIEWMESH_SMOKE_CLAUDE` / `_CODEX` / `_AGY` / `_DEVIN` | **Naming conventions, read by nothing.** They mark intent to run an opt-in real-adapter smoke profile (`claude-code-smoke`, `codex-cli-smoke`, `agy-cli-smoke`, `devin-cli-smoke`); the actual opt-in is running the command with that profile. Setting them alone changes no behavior. |

The default `go test ./...` spends no tokens and calls no real model — the hermetic test suite runs against an internal, test-only fake harness and fake binaries (never user-configurable; see [CONTRIBUTING.md](../CONTRIBUTING.md)).

## The shared `~/.aimesh` layout (SHIPPED)

Config is split by ownership on disk, matching the code's boundary:

- **`~/.aimesh/adapters.yaml`** — **shared, meshcore-owned, and the single source of truth for adapter
  binary paths.** Both apps read it; `config.yaml` no longer carries adapter paths. Two sections:
  - `adapters.<name>.path` — the machine-local "where is the binary" override for a CODE-OWNED recipe
    (claude-code, codex-cli, agy-cli, devin-cli, gemini-cli, cursor-cli, ollama). The value is
    **presence-aware**: an absent key inherits a lower layer; an explicit empty string is a deliberate
    "use `PATH`" clear that overrides an inherited path.
  - `acpAdapters.<name>` — **user-defined generic ACP adapter instances** (`{title, path, args, model}`).
    There is no fixed ACP catalog; see [adapters.md](adapters.md).
- **`~/.aimesh/explore/profiles.yaml`** — exploremesh-owned: named profiles, each an ORDERED explorer list
  (preference order, which `--count` selects the top-N from) + a collator + an optional `defaultMode`.
- **`~/.aimesh/review/config.yaml`** — reviewmesh-owned: `profiles`, lanes, `modelCatalog`, `review`, …
  referencing adapters by name.

```yaml
# ~/.aimesh/adapters.yaml
schemaVersion: 1
adapters:
  claude-code: { path: /Users/you/.local/bin/claude }
  codex-cli:   { path: /Users/you/.nvm/versions/node/v24.18.0/bin/codex }
  ollama:      { path: "" }        # explicit clear → resolve on PATH
acpAdapters:
  acp-gemini:
    title: "ACP: Gemini"
    path: /Users/you/.nvm/versions/node/v24.18.0/bin/gemini
    args: ["--acp"]
    model: gemini-3.5-flash        # captured at validation; the expected model for identity checks
```

**Scopes.** Each file resolves user-scope (under `$AIMESH_HOME`, else the OS home) overlaid by a
**root-anchored project scope** (`<repo-root>/.aimesh/…`), so a run from a
subdirectory sees the repo-wide file. Writes go through a content-hash compare-and-swap, so two processes
(two concurrent CLI writers) cannot clobber each other's unrelated edits.

**Inspect what is configured** with `aimesh review list [--json]` / `aimesh explore list [--json]` — adapters
(configured?, path, shell vs ACP, declared identity-evidence capability) plus profiles.

## See also

- [review.md](review.md) — commands, quick start, diagnostic codes
- [explore.md](explore.md) — the explorer/collator model, commands
- [adapters.md](adapters.md) — the adapter contract, per-provider recipes, adding an adapter
- [model-identity.md](model-identity.md) — evidence tiers and identity capture
- [architecture.md](architecture.md) — components, layering, the meshcore boundary
- [security.md](security.md) — what leaves the machine, containment
