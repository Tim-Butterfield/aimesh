# AGENTS.md — using aimesh

aimesh runs **provider-diverse, governed** AI reviews and explorations. It orchestrates several
independent model CLIs, keeps them blind to each other, and records what each one actually said.
It is not a model and it does not decide anything on your behalf: it produces findings you can
audit, and it writes to your files only when you explicitly ask it to.

Two domains:

- **review** — a blind panel of reviewers judges a workspace and produces findings. It can `apply`
  those findings, but only when explicitly permitted.
- **explore** — a panel of explorers answers a question against declared criteria; a collator
  merges their answers. It edits nothing you own.

## Three ways to reach aimesh

- **Shell (CLI)**: `aimesh <command>`. Run `aimesh --help` for the tree. The CLI reads aimesh's saved
  configuration (profiles, model catalog, recorded adapter paths).
- **MCP**: `aimesh mcp` serves both domains' tools over stdio (`aimesh review mcp` / `aimesh explore mcp`
  serve one). Both MCP lifecycles are served — a client may open with the `initialize` handshake, or
  with `server/discover` and no handshake.
- **ACP**: `aimesh review acp` / `aimesh explore acp` run as an Agent Client Protocol agent over stdio.

Every surface reaches the same engine and enforces the same rules. The one deliberate difference is
**where configuration comes from**: the CLI reads saved configuration; the MCP and ACP servers read
**none** — their adapters and grants come only from the arguments (and environment) their host starts
them with, so they work on a fresh install with no setup step. This document is the same one the
`agents_md` MCP tool returns, so an agent arriving by any route gets identical guidance.

## Setting up an MCP or ACP server (no aimesh setup needed)

An MCP/ACP server is available only with the adapters named at launch. Each `--adapter` names a
built-in adapter — `claude-code`, `codex-cli`, `gemini-cli`, `devin-cli`, `cursor-cli`, `agy-cli`,
`ollama` — either bare (its CLI is found on `PATH`) or as `name=path` (the CLI's full path). A path
may use the host OS's own variable syntax — `%VAR%` on Windows, `$VAR` or `${VAR}` elsewhere — and a
leading `~`; an undefined variable refuses startup. `AIMESH_ADAPTERS` is the environment form (entries
separated by `;` on Windows, `:` elsewhere); give one or the other, not both. The named adapters are
exactly who may receive the content a call sends.

**Find the CLI's path yourself.** Try `where devin` (Windows) or `command -v devin` (macOS/Linux). A
CLI launched from a Start-menu shortcut or app bundle is often not on `PATH`; look in its per-user
install folder (for example `%LOCALAPPDATA%\<vendor>\...` on Windows, `~/.local/bin` on macOS/Linux).
If you are the host's agent, you know your own binary's location — use it.

**Edit the host's MCP configuration yourself** (or ask the user to). aimesh never edits a host's
configuration file. A Windows example for a host whose `config.json` lists MCP servers:

```json
{
  "mcpServers": {
    "aimesh": {
      "command": "aimesh",
      "args": ["mcp", "--adapter", "devin-cli=%LOCALAPPDATA%\\devin\\cli\\bin\\devin.exe"]
    }
  }
}
```

A macOS example:

```json
{
  "mcpServers": {
    "aimesh": {
      "command": "aimesh",
      "args": ["mcp", "--adapter", "claude-code", "--adapter", "devin-cli=~/.local/bin/devin"]
    }
  }
}
```

Optional launch flags: `--allow-writes` (let aimesh apply accepted review findings; see below),
`--root <dir>` (repeatable: a **ceiling** every declared path must lie inside), `--allow-broad-root`
(permit an over-broad `--root`), `--verify-cmd <command>` (repeatable: your own build/test command, run
on the containment copy and reported; it gates nothing), `--verify-timeout <duration>`,
`--verify-baseline` (run the verify commands on report runs too) and `--allow-protected-paths` (permit
work under `.git`, `.vscode` and similar configuration trees). Then call `review_doctor` / `explore_doctor`:
they report which adapters can be started and who performs writes, and nothing about your machine's
paths. A server launched with no adapter still starts; every run is refused until one is named.

## Rules that are expensive to get wrong

**Identity is recorded, never acted on.** aimesh records which model each seat *claims* to be, and
how well that claim is evidenced. It never downgrades, excludes, or discards a finding because of
identity. The content of a finding determines whether it is valid — nothing else. If you are
tempted to filter on an identity field, don't: it is provenance for the audit trail, not a quality
signal.

Related, and the reason that rule exists: a CLI that echoes back the model name you passed it has
told you nothing. Several do. An identity is only evidence when the tool could have contradicted
you and didn't.

**Reviewers never touch your files.** Every review runs against an isolated **containment copy**.
Model CLIs work inside that copy; findings come back as proposals. Nothing reaches your working
tree until an `apply` runs, and `apply` is separately gated on every surface (`--apply` on the CLI;
`--allow-writes` at launch plus `allowWrite: true` per call on MCP; `--allow-writes` at launch plus a
`fromRun` apply turn on ACP).

**The write denylist is absolute and `--force` does not lift it.** Credentials (`.env*`, `.ssh`,
`*.pem`, `id_*`, `.netrc`, …), version control (`.git/**`), this tool's own state (`.aimesh`, and
the legacy `.reviewmesh`/`.exploremesh`), and agent/IDE configuration (`.mcp.json`, `.claude`,
`.cursor`, `.codex`, `.gemini`, `.vscode`, `.idea`, …) are refused for writes, and the second
family is excluded from the containment copy entirely — a file that is copied is a file that can be
shown to a model, proposed as an edit, and accepted by a human reading a diff. Configuration stays
human-only at the filesystem layer, not by convention.

**Nothing is silently substituted.** A missing optional layer is fine; a malformed one is an error.
A config file in a location nothing reads is refused by name rather than ignored. An unavailable
adapter is an error, never a quiet swap for a different model. If aimesh cannot do what you asked,
it says so and stops — it does not do something adjacent and report success.

**Run artifacts contain everything the models were shown.** They are written under `.aimesh/`
(VCS-excluded by `aimesh init`) or the OS temp directory — never a relative path inside whatever
checkout you happened to run from. Treat them as sensitive.

## Scope: every MCP/ACP call declares its own paths

An MCP or ACP server has no launch folder: a host can change folders after starting it, and several
conversations can use one server at once. So nothing is inferred from where the server started.

- **Every path is absolute.** A relative path is refused as a malformed request.
- **`workspace` is the call's root.** A review call names its absolute `workspace` (on ACP, the prompt's
  `workspace`, else the session's `cwd`). That call reads only inside it.
- **`roots` adds directories.** Name extra absolute directories the call reads — for example the
  project folder holding authority documents when the workspace is a temporary directory. On ACP this
  is `_meta.reviewmesh.roots`.
- **The operator's `--root` is a ceiling.** When set, every declared path must lie inside it
  (`scope_outside_root_ceiling`). Without it, a call may declare any absolute directory that is not
  the filesystem root, a home directory, a system tree or a protected directory.
- **`inlineWorkspace`** reviews content you supply in the call; it consumes no path.

On the CLI, the path you type is the root.

## Compose the panel in the call

On MCP and ACP **every run composes its own panel** — there are no profiles and no default panel.

- **review, MCP:** `panel` on `review_report` — `reviewers[]` and `author_remediator` are required;
  `cross_check` and `verifier` are optional and run only when named (a run without them lists them in
  `shape.skippedSteps`). `author_remediator` is the seat whose adjudication becomes the accepted set.
  Reasoning `effort` is per-seat on `reviewers[]` only.
- **review, ACP:** `_meta.reviewmesh.panel`, the same object.
- **explore, MCP:** `panel: {explorers: […] (at least 2), collator: {…}}` on `explore`.
- **explore, ACP:** `_meta.exploremesh.panel`, the same object.

Each seat names an adapter the server was launched with (`review_list` / `explore_list` shows them,
with `available`) and **the exact model identifier that adapter's CLI accepts**. Look that identifier
up for your adapter and the license it runs under — the set of models differs by account — and use
what the user asked for. aimesh passes it through verbatim and does not validate it; an identifier the
provider does not recognise fails in the provider's words, mid-run, after the call was paid for. Use
`verifyReadiness` to find out first (below). A seat can never introduce an adapter, a binary path or a
launch argument.

**CLI (saved configuration).** The CLI composes the same way with flags:

- **review:** one `--reviewer adapter=<a>,model=<m>[,effort=<e>]` per seat (order preserved, 1..16),
  plus the host lane, which must be a **model-catalog key** because it is never passed through:
  `--set author_remediator.adapter=<a> --set author_remediator.model=<catalog-key>`.
- **explore:** two or more `--explorer adapter=<a>,model=<m>[,effort=<e>]` plus one
  `--collator adapter=<a>,model=<m>`.

On the CLI a panel is composed **or** selected with `--profile`, never half of each. `aimesh review
list --json` reports `modelCatalog`: model keys the saved configuration defines. A reviewer model that
is not a catalog key goes to the adapter verbatim and comes back as `modelSource: "passthrough"`;
surface that, and a `passThroughHint` if present. Run `setup` only when the operator asks for a
**saved** profile, or when a CLI is not on `PATH` and its path must be recorded. Never save
configuration as a side effect of answering a question.

## Who writes, and the diff

A review never writes. Remediation acts on a completed review's accepted findings:

- **MCP:** `review_remediate {fromRun: "<runId>", workspace: "<absolute workspace>", output}`.
  `output: "patch"` returns the complete diff on **every** server and changes no project content.
  `output: "apply"` writes the workspace; it needs the server launched with `--allow-writes` **and**
  `allowWrite: true` on the call.
- **ACP:** a `report` turn returns `runDir`; an `apply` turn carries `fromRun: <that runDir>`. Without
  `--allow-writes` an apply turn is refused (`writes_not_granted`); a `patch` turn supplies the diff.
  A turn that names no mode is a `report`.

`workspace` must be the tree the source run reviewed (`source_run_workspace_mismatch` otherwise).
`review_doctor`, `review_list` and every ACP result report `writes`: `aimesh` when aimesh can apply,
`agent` when it cannot — then ask for the patch and apply the diff with your own file tools.
`diffAvailable` is always `true`.

## Exploration modes and what each one requires

`explore` is one tool (and one CLI command) covering every mode. **`mode` is required, and it decides
which other parameters are required.** Supplying a parameter that belongs to a *different* mode is
refused, not ignored — a `map` run handed an `artifact` fails rather than quietly running without
having reviewed it.

| mode | what it does | also requires |
|---|---|---|
| `map` | collate-only synthesis + disagreement register | — |
| `synthesize` | one composed best answer | — |
| `catalog` | enumerate + canonicalize | — |
| `shortlist` | host-tallied ballot over a confirmed candidate set | — |
| `ai-collab` | the explorers challenge each other | — |
| `challenge` | blindly attack a supplied artifact | `artifact` |
| `compare` | evaluate a declared option set on declared axes | `options`, `comparisonAxes` |
| `forecast` | pool independent numeric estimates | `target`, `unit`, `horizon` (`conditioningEvent` optional) |

CLI equivalents are the same names as flags: `--artifact`, `--options` + `--criterion`,
`--target`/`--unit`/`--horizon`. Note `comparisonAxes` (MCP) and `--criterion` (CLI) are the measured
axes — *not* the same thing as `criteria`, which are the constraints the whole exploration must
satisfy.

## Price a run, and check its agents, before you start it

Both domains take a **dry run**: `--dry-run` on the CLI, `dryRun: true` on MCP,
`_meta.<domain>.dryRun` on ACP. It resolves everything, spends nothing, and reports every
configuration error a real run would hit — for free. Use it before committing to a large panel.

The two answers differ, and the difference is not cosmetic:

- **review** reports a **min/max range** (it iterates until adjudication converges) and a `payload`
  naming every file the reviewers would be shown.
- **explore** reports **one exact number** (an exploration's round count is fixed by its mode
  contract; only a halt makes it fewer) and the **exact round-1 prompt** every explorer would get.
  It stops *before* the identity pre-flight, which is a real model call per governed role.

In both, an empty result means nothing was looked at. Branch on `status: "planned"` / `dryRun: true`,
never on the empty finding set.

**`verifyReadiness: true`** (MCP; `_meta.<domain>.verifyReadiness` on ACP) asks every distinct
adapter/model the panel names whether it can do real work — one bounded one-token call each, which
**spends** — before the run dispatches anything. An agent blocked on login, folder trust or an
identifier its license cannot use halts the run before the panel is paid for. With `dryRun` the calls
are priced and none is made.

## A run takes minutes. Slow is not stuck.

A panel is several real provider CLIs doing real work. Minutes is normal, and a big review is longer
still. **Do not treat a long run as a hung one**: there is deliberately no per-seat time limit,
because elapsed time cannot distinguish a model thinking hard from a process that has stopped, and a
deadline would discard real work on exactly the hardest inputs.

What to do instead: watch the progress events (each seat reports when it starts), and on MCP use the
`{runId, state: "running"}` handback with the polling tools rather than re-issuing the call. Never
re-run a review because it felt slow — you will pay for the whole panel twice. The one time bound is
the operator's turn budget; if it is genuinely too short for the work, that is a setting for the
human to raise, not something for you to route around.

## `scope` means the review is silent about most of the tree

If a result carries `scope`, only the listed files were reviewed. **A clean result then says nothing
about the rest** — the absence of a finding elsewhere means nobody looked. Never report a scoped
review as "the code is clean"; report it as "these N files are clean".

You can request one: `--path` and `--changed-since` work in **any** directory, `--changed-vs` needs a
repository. Prefer the first two unless you know the tree is a repo — a plain folder is an ordinary
case here, not an edge case, and `--changed-vs` is refused there rather than quietly widened.

## `partialPanel` changes the denominator for everything else

If a result carries `partialPanel`, a provider ran out of capacity (quota, rate limit, timeout) and
the run continued rather than discarding the seats that had already answered. Read every agreement
count against **`answered`**, never against the panel that was configured.

It is not a failure and not a reason to distrust the findings — the surviving ones came from the
seats that ran. It is also not nothing: say the review was partial, say which seats were lost, and
say that re-running when capacity returns would give the full panel. A full panel carries no such
key, so its absence means nothing was lost.

## Never quote an agreement count without its independence

`agreementCount` says how many blind seats reported a finding. It does **not** say how independent
they were, and a panel running one model behind two vendor CLIs produces agreements whose errors
correlate. Each finding therefore carries `distinctModels` and `agreementIndependence`, and the run
carries a `composition` block for the panel as a whole.

`shared_model` is **not** a refutation — the finding may be perfectly real; the extra agreement is
just worth less than the number suggests. And `distinctModels` is not a discounted count: it counts a
different thing. Say both, as "3 seats, 2 distinct models", rather than picking one.

## `contested` is a reason to read a finding, never a reason to drop it

Each panel finding also carries `consensus` — `unanimous`, `majority`, or `contested` when a minority
of the seats that ran reported it (or they split evenly). The run carries a `dissent` tally.

**A silent seat is not a seat that disagreed.** The seats are blind: each is asked what it finds, not
to vote on a list. A seat missing from `supportingSeats` may have disagreed, may never have reached
that file, or may have stopped when its own set stabilized — indistinguishable, and not guessed at.

So a `contested` finding has **not** been disputed, voted down, or weakened. Nothing was dropped,
downgraded, reordered or made unapplyable because of this label. When you relay findings:

- Do **not** filter, deprioritise or soften a contested finding, and do not call it "disputed" or
  "low-confidence" — you would be reporting a disagreement that was never observed.
- Do **not** present a `unanimous` label as corroboration on its own; check `agreementIndependence`
  first, because unanimity across seats sharing one model is one opinion counted twice.
- If you surface the contested count, pass the `note` with it.

The useful move is the opposite of filtering: a contested finding is the one to **open and check
yourself** before repeating it, because fewer independent vantages stand behind it.

## If a review carries `verification`, read the delta — and do not treat it as a verdict

A review may run the project's **own** build/test commands on the containment copy, before and after
it applies anything. The operator enables that at launch; you cannot request it and cannot name a
command. When it happened, the result is under `verification`, and `delta` is the field:

`unchanged_fail` means the suite was **already red** and the change did not make it worse — that is an
ordinary outcome, not a problem to report. `broken` is the one worth surfacing. `not_comparable` means
a command timed out or could not start, which is a fact about the machine, not the code.

Two things you must not infer. It **gated nothing** — the second pass runs after the commit, so no
finding was dropped and nothing was blocked; do not describe a red result as having stopped anything.
And a green result does **not** mean the change is correct: the commands may not cover what changed.
Pass the `note` on with the delta.

## Read the grounding label before you repeat a finding's citation

Both apps tell you how much of a finding's pointer they checked, and neither of them checked whether
the finding is *right*.

- **review** — every finding carries `grounding.status`, and the run carries a `grounding` tally with
  a `note`. `grounded` means the cited file, lines or symbol exist. `file_missing` means the pointer
  did not resolve — the finding is still real work and was reported at full severity, because a
  reviewer that named the wrong file may have found a real defect one file over. `location_unchecked`
  means the location was prose, so nothing was examined: it is not a problem, do not report it as one.
- **explore** — a finding may carry `unverifiedReferences`. exploremesh reads no filesystem, so those
  are pointers nothing here looked at. They are not citations, and a finding whose only source is one
  is `uncited`.

The failure to avoid in both is upgrading a floor into a verification. "12 of 14 findings grounded"
does **not** mean 12 findings were confirmed; it means 12 pointers resolve. Pass the `note` on when
you pass the numbers on, and when you repeat a citation into your own output, repeat its status too.

## Recommended loop

**MCP / ACP:**

1. `review_doctor` / `explore_doctor` — which launched adapters can be started, and (review) who
   writes. If no adapter is listed, the host configuration needs `--adapter` (see setup above).
2. `review_list` / `explore_list` — the adapter names you may compose from.
3. Compose the panel in the call with the model identifiers the user asked for, each looked up for
   your adapter and license.
4. `dryRun` it and read what it would cost; add `verifyReadiness` to check the agents.
5. Run it with an absolute `workspace` (review) or the task fields (explore).
6. Read the findings. For review, ask for `output: "patch"`, or apply deliberately and separately.

**CLI:**

1. `aimesh doctor` — read-only readiness. Run it *first*: it tells you whether initialization is
   needed instead of assuming. It reports rather than blocking, so it is safe to lead with.
2. `aimesh init` — create the VCS-excluded `.aimesh/` state directory. Idempotent. Add
   `--require-repo` in CI, where landing in folder mode by accident is a silent wrong answer.
3. `aimesh review list --json` / `aimesh explore list --json` — the adapters and the model catalog
   you can compose from. `aimesh review doctor` / `aimesh explore doctor` add adapter readiness.
4. Compose the panel with flags, or select a saved profile when the operator has one.
5. `--dry-run` the run you intend, and read what it would cost.
6. `aimesh review run <path>` or `aimesh explore run --purpose … --criteria …`.
7. Read the findings. For review, apply deliberately and separately.

## CLI commands

Shared:

- `aimesh init [--require-repo|--require-folder] [--json]` — prepare `.aimesh/`. Adaptive: repo mode
  inside a repository, folder mode outside one. The flags turn that adaptive choice into an
  assertion.
- `aimesh doctor [--require-root] [--json]` — shared readiness; creates nothing; exits 0 even when
  nothing is set up. `--require-root` makes the absence of a root fail the run.
- `aimesh agents-md` — print this guide. `AGENTS_MD=<path>` serves that file instead; an unreadable
  path is an error, and an active override is reported on stderr so you can tell which document you
  received.
- `aimesh mcp` — the MCP server for both domains (`--only review|explore` narrows it).
- `aimesh --version [--json]`, `aimesh help`.

Review (`aimesh review <command>`): `run`, `setup`, `list`, `doctor`, `config`, `acp`, `mcp`.
Explore (`aimesh explore <command>`): `run`, `setup`, `list`, `doctor`, `export`, `acp`, `mcp`.

Each domain documents its own flags: `aimesh review run --help`, `aimesh explore run --help`.

## State and configuration

**State** is what runs leave behind: run records under a workspace's `.aimesh/` (or the OS temp
directory). Every surface writes it.

**Configuration (CLI only)** lives under one state root, `.aimesh/`:

- `.aimesh/adapters.yaml` — adapter binary paths and user-defined ACP instances. **Shared** by both
  domains.
- `.aimesh/review/config.yaml` — profiles, lanes, model catalog.
- `.aimesh/explore/profiles.yaml` — named explorer profiles.

Each resolves user scope (under `AIMESH_HOME`, else the OS home) overlaid by a root-anchored project
scope. `AIMESH_HOME` is the single home override for the whole tool.

The MCP and ACP servers read none of it — not the user files, and not a project's `.aimesh/` inside a
workspace a call names. Their configuration is their launch arguments.

On every surface a call may use and order the adapters it was given, but can never introduce a new
one. That is what keeps a prompt — or a model driving aimesh — from pointing a "review" at an
arbitrary binary.

## Safety posture

aimesh spawns real model CLIs with the privileges of the aimesh process, and those CLIs may call
paid providers. It is not a sandbox. What it does guarantee is narrower and worth knowing exactly:
reviewers see only a containment copy, protected paths are never copied or written, aimesh's own
configuration cannot be authored by a model (a host's MCP configuration is edited by the host's agent
or the user, never by aimesh), content goes only to the adapters an operator named, findings are never
filtered by claimed identity, and every run records what was shown and what came back.

Exit codes: 0 success, 1 gated findings, 2 usage, 3 config, 4 adapter, 5 model/identity,
6 containment, 7 policy/cap, 8 internal.
