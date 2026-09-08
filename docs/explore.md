# exploremesh

**Multi-model exploration and research on meshcore.**

`exploremesh` answers an open question by putting it to **N independent explorers** — different models, or the same model at different reasoning depths — that respond blind and in parallel, then has a single **collator** turn their answers into a structured result. The point is not a vote; it is to surface where independent models agree, where they diverge, and what remains uncertain — without silently trusting a model that can't prove its identity, and without letting a model assert a number the **host** should compute.

What "structured result" means depends on the **mode** (`--mode`): a synthesis with a disagreement register, a composed best answer, a canonicalized catalog, a severity-triaged challenge register, a host-tallied shortlist, a comparison matrix with a Pareto frontier, or a pooled numeric forecast.

exploremesh is one of the two applications in the [aimesh](../README.md) monorepo. It builds its exploration domain on the [meshcore](../meshcore/) governance substrate (adapters, model-identity verification, config, doctor, ACP transport). It has **no workspace, no patch/apply** — exploration never edits a file you own.

**What it does write.** Exactly two things, both governed and both stated up front rather than discovered later:

- **run artifacts** — the record `--dump-run` (CLI), `dumpRun` (ACP) and `aimesh explore mcp` (on by default) write under `$EXPLOREMESH_ARTIFACT_DIR`. Its default is the **project-local `.aimesh/explore/runs/`** state directory when one exists (what `aimesh init` / `repo init` creates, already VCS-excluded), and otherwise a subdirectory of the **OS temp directory** — never a new directory inside your checkout.
- **the evidence export** — `aimesh explore export --sqlite <path>`, and only when you ask for it by naming the path. It is the one place exploremesh writes where you point it, so it is governed like every other write path in the repo: the destination goes through the meshcore scope resolver, the **non-overridable denylist** (`~/.aimesh/**`, `.git/**`, `.env*`, key material, IDE/agent client config) refuses protected paths regardless of any flag, an existing destination needs `--force`, a directory/symlink/device destination is refused rather than followed, and the database is published by an **atomic rename** so an interrupted export cannot leave a truncated file where a valid one was. See [Evidence export](#evidence-export).

Module path: `github.com/Tim-Butterfield/aimesh/exploremesh`.

## The model

- **explorer** — one `(adapter, model, effort)` triple, unique by the full triple. Two explorers may share a model at different efforts (Opus-high vs Opus-low are genuinely different response distributions). **Minimum 2.**
- **collator** — exactly one `(adapter, model, effort)`. It performs the terminal collation after the fan-out. It may reuse an explorer's model (a different function, a different prompt).
- **task** (`raw_task`) — the per-run input: `{ purpose, criteria[], priorContext?, mode?, artifact?, … }`. `priorContext` lets exploration *N+1* consume the result of exploration *N*. A purpose and at least one non-blank criterion are required; each mode declares any extra input it needs (`--artifact` for `challenge`, `--options`/`--criterion` for `compare`, `--target`/`--unit`/`--horizon` for `forecast`) and every surface rejects a missing one **before any spend**.
- **profile** — a named, ordered explorer set + a collator + an optional default mode (see [Profiles](#profiles)). The slice order is **preference order**: what `--count N` selects the top-N from.
- **mode** — the app-owned, versioned contract for how one exploration runs (see [Modes](#modes)).

**The core invariant:** every explorer in a round receives a **byte-identical payload** (`{ finalPrompt, expandedSchema }`) — serialized canonically, SHA-256'd, and audited, so "identical" is verifiable rather than aspirational. The only variable across explorers is `(adapter, model, effort)`. Different models processing the *same* payload differently is the entire signal. Every mode registered today is **formulation-free**: the app owns both the explorer prompt and the explorer response schema, so the collator cannot dilute the task or contradict its own schema in round 1.

**Identity policy** (meshcore tiers): **recorded, never enforced.** Every seat's model identity is classified and reported — `verified`, `self_reported`, `unknown`, or a proven `mismatch` — and none of those change what happens. A weak or mismatched explorer is a full member of the primary panel, its alias is citable, and it counts in both denominators; the same goes for the collator and the canonicalizers. What *does* stop a run is a lack of usable work: fewer than two responding explorers, unparseable output, a role that cannot be invoked. See [../docs/model-identity.md](../docs/model-identity.md) for why a label about which model answered is not something a finding's validity can rest on.

## Modes

`--mode <name>` selects the exploration contract. Omitted, it falls back to the resolved profile's `defaultMode`, then to `map`. An unknown mode is a usage error listing the known ones.

Two families, and the difference is load-bearing:

- **Emergent-space** modes (`map`, `synthesize`, `catalog`, `challenge`, `shortlist`, `ai-collab`) — the explorers author the entities (claims, candidates, findings). Grouping those across explorers *is* entity resolution, so it may only happen through the recorded **canonicalization** step: a decoupled canonicalizer model proposes clusters, and the host keeps an **append-only, revision-hashed merge-ledger** under a **surjectivity gate** (every nomination lands in exactly one row; de-dup may cluster, never drop).
- **Fixed-space** modes (`compare`, `forecast`) — the *user* declares the option set / estimation target before any explorer speaks. There is nothing to resolve, so these run **one blind round with no canonicalizer at all**, and the host computes the entire result; the collator contributes narrative only. Their output says so explicitly (`partition revision: NONE — fixed space`).

| Mode | Rounds | Canonicalization | What the run produces |
|---|---|---|---|
| **`map`** (default) | 1 | none | The collate-only synthesis: a summary, evidence-weighted findings, a **disagreement register** (subject → resolution → residual risk), and a weak-response appendix. |
| **`synthesize`** | 1 | none | Each explorer gives its single best *complete* answer; the collator **selects and composes** one, grafting superior elements. Records component provenance + a minority report — never a tally. |
| **`catalog`** | 1 | single canonicalizer, **no** confirmation round | Explorers enumerate broadly; the raw nominations are canonicalized into clusters (single-source members tagged) plus **proposed dimensions**. Observe posture: it organizes, it does not rank. |
| **`challenge`** | 2 | dual canonicalizer + binding confirmation round | Explorers blindly attack a supplied `--artifact`; findings are canonicalized, confirmed, deepened or refuted in a collator-mediated cross-review, and returned as a severity-triaged register whose corroboration is **computed by the host** over the immutable blind round-1 responses. **Requires `--artifact`.** |
| **`shortlist`** | 2 | dual canonicalizer + binding confirmation round | Explorers blindly enumerate candidates; the confirmed universe goes to an explicit **ballot** under decision inputs the host froze and hashed beforehand. The ranking is **tallied by the host**, never asserted by a model, and rejects are carried with the host's reason. |
| **`ai-collab`** | 2 | dual canonicalizer + binding confirmation round | Each agent shortlists its *own* findings blind, then the agents challenge each other's through the same cross-review. Reuses `challenge`'s schema and collator unchanged; needs no supplied artifact. |
| **`compare`** | 1 | **none** (fixed space) | Every explorer evaluates the *declared* `--options` (at least 2, no duplicates) against the *declared* `--criterion` set (at least one scored **dimension**, alongside any pass/fail **filter** gates) blind; the host computes the consolidated matrix, per-cell agreement, the filter gate and the **Pareto frontier**. A weight on every scored criterion is what licenses a scalar ranking — without weights the result is the trade-off view and says so. |
| **`forecast`** | 1 | **none** (fixed space) | Each explorer returns a machine-readable numeric estimate + interval for the declared `--target`/`--unit`/`--horizon`; the host pools them under a declared, versioned rule and reports dispersion and identified outliers. |

Anything a count or a ranking rides on carries its provenance: both denominators (`k of panel` / `k of respondents`), an honest **WITHHELD** label where quorum or agreement fails, the frozen counting-policy hash, and the partition revision hash. Collator prose is quarantined and printed as *model prose carrying no governance value*.

## Commands

The purpose may be given as the **positional argument** or as `--purpose`; the two are the same
thing and giving both is refused. Flags may come before or after it:

```bash
aimesh explore run "how should we shard the write path?" --criteria correctness,ops-cost
```

`--criteria` is always required and is never chosen for you — the criteria are what every explorer
is judged against, so a run that let the tool pick them would be marking its own work.

```
aimesh explore run [question] --criteria <a,b,...> [--mode <name>] [--artifact <path|->]
                    [--prior-context <text>] [--profile <name>] [--count <N|all>] [--roster <path>]
                    [--canonicalizer adapter=<n>,model=<m>[,effort=<e>] --canonicalizer ...]
                    [--dry-run] [--dump-run] [--debug] [--json]
aimesh explore run --purpose <text> --criteria <a,b,...>
                    --explorer adapter=<n>,model=<m>[,effort=<e>] --explorer ...
                    --collator adapter=<n>,model=<m>[,effort=<e>]
aimesh explore run --mode compare  --options <a,b,c>
                    --criterion name=<n>,direction=higher_is_better|lower_is_better[,role=dimension|filter][,weight=<w>] ...
aimesh explore run --mode forecast --target <what> --unit <unit> --horizon <when> [--conditioning <event>]
aimesh explore export  --sqlite <out.db> --run <run-dir> [--verify] [--json]
aimesh explore list    [--json]
aimesh explore doctor  [--profile <name> | --roster <path>] [--probe] [--json]
aimesh explore acp     [--framing newline|content-length] [--turn-timeout <dur>] [--roster <path>]
aimesh explore mcp     [--protocol dual|legacy]
                    [--wait-seconds <n>] [--turn-timeout <dur>] [--no-capture] [--roster <path>]
aimesh explore setup   --adapter <name> --path <p>  |  --remove-adapter <name>
aimesh explore setup   --acp detect|add|remove [--path <p>] [--name <k>] [--title <t>] [--acp-arg <a> ...]
aimesh explore setup   --profile <name> --explorer adapter=<n>,model=<m>[,effort=<e>] ...
                    --collator adapter=<n>,model=<m>
                    [--canonicalizer ... --canonicalizer ...] [--default-mode <m>]
aimesh explore setup   --delete-profile <name> --yes
aimesh init            (repo if inside one, else plain folder)
aimesh init --require-repo       (require a Git/Mercurial repo; .aimesh is VCS-excluded)
aimesh init --require-folder     (require a non-repo directory)
aimesh --version [--json]
```

| Command | What it does |
|---|---|
| `explore` | Runs the mode: blind fan-out over the selected explorers, then the terminal collation. Prints the **selected panel** to stderr before spending anything, and warns when `--count` left configured explorers out. |
| `list` | Reports the configured adapters (name, configured?/path, shell vs ACP, declared identity-evidence capability), the resolved roster, the configured profiles (ordered explorers + collator + default mode), and the available modes. `--json` for a machine-readable projection. |
| `doctor` | Explorer/collator adapter readiness for the **full** configured panel (never a `--count` subset), via the shared meshcore doctor checks. An unrecognized adapter is a failing check — it is never silently substituted. `--probe` adds a safe, bounded, no-model readiness probe per required adapter; `--json` emits the machine-readable projection. |
| `acp` | Runs exploremesh as a local ACP agent over stdio — see [Driving it over ACP](#driving-it-over-acp). |
| `mcp` | Runs exploremesh as a local **MCP server** over stdio, so an MCP agent can drive explorations as tools — see [Driving it over MCP](#driving-it-over-mcp). |
| `setup` | Non-interactive configuration: record an adapter's binary path, detect/add/remove a user-defined ACP instance, and create/default/delete named profiles. |
| `export` | Builds the derived SQLite evidence database from a captured run directory — see [Evidence export](#evidence-export). |
| `init` / `repo init` / `folder init` | Creates the local, VCS-excluded `.aimesh/` state directory (adapter locations, run artifacts). It seeds no config. |
| `--version` | Prints the build version; `--version --json` for the machine-readable form. |

**Selecting the panel.** `--profile <name>` picks a named profile and `--roster <path>` a raw single roster file; the two are **mutually exclusive**, and with neither, the profile named **`default`** is used (the runtime default is pinned, as in reviewmesh). `--count N` then takes the top N **by preference order** (`all`, or omitted, runs every explorer). An out-of-range or `<2` count is a clear error — the count is **never clamped** (requested = executed).

**Ad-hoc runs.** Two or more `--explorer adapter=…,model=…[,effort=…]` plus a `--collator` build a one-off roster by identifier, bypassing both `--roster` and `--profile` (all three are mutually exclusive). Useful for a throwaway panel without touching persisted config.

**Choosing the canonicalizers.** A count-bearing mode runs **two canonicalizers** and holds only the merges both propose, so *which two identities* hold that rule decides which merges hold versus contest — and therefore every corroboration count. `--canonicalizer adapter=…,model=…[,effort=…]` names them: supply **exactly two, or none**. One is refused, because slot `a` defaults to the collator's identity and a single entry does not say which slot it fills; two *identical* identities are refused because the second names nothing the first did not. A profile can carry the same pair (`canonicalizers:` in `profiles.yaml`, or `setup --canonicalizer`), and the flag overrides it. Omitted, the host **derives** them: slot `a` from the collator, slot `b` from the first explorer **in preference order** whose adapter+model differs from the collator's.

The run records **two** facts about that choice, because they answer different questions and a count cannot be read honestly without both:

- `canonicalizerProvenance: explicit | derived` — who *chose* the identities.
- `canonicalizerIndependence: distinct_models | shared_model` — what the choice *bought*.

`shared_model` means both slots run the same model, reached through two different adapters. It is **allowed and reported, never refused**: a panel is configured deliberately, and a reader who wanted two different models would have named two. Nor are the two proposals identical — sampling makes two calls to one model differ, sometimes materially. What is weaker is that they are drawn from the same priors, so their errors correlate in a way two different models' do not, and a merge held by their agreement is correspondingly weaker evidence. `--dry-run` says so **before** the panel is paid for, which is the only moment changing the pair is still free; after that it rides the manifest and the ACP/MCP echoes.

The one case that still halts is a panel where *every* seat is the collator's own adapter **and** model: there is no distinct second identity to name at all, and the alternative to refusing is inventing one.

**Troubleshooting flags.** `--debug` prints a per-adapter-call diagnostic (resolved argv, exit code, identity, captured streams) to stderr. `--dump-run` records the whole run — envelopes, raw outputs, prompts, the declared task, a versioned manifest — under `$EXPLOREMESH_ARTIFACT_DIR` on **both** the success and halt paths, and prints `run: <dir>` to stderr. With that variable unset the default is the project-local `.aimesh/explore/runs/` when a `.aimesh/` state directory exists (run `aimesh init` once — `repo init` also registers it with your VCS exclude file), and otherwise a subdirectory of the OS temp directory. It is deliberately never a relative path inside your repository.

### Pricing an exploration before you pay for it (`--dry-run`)

An exploration costs real model calls, and the count grows fast: a count-bearing mode calls every
explorer three times over (blind round, confirmation round, mediated round) on top of two
canonicalizer proposals and the pre-flight. `--dry-run` resolves everything and spends nothing:

```bash
aimesh explore run --purpose "should we adopt a queue?" --criteria "throughput,ops burden" --dry-run
aimesh explore run --mode challenge --artifact design.md ... --dry-run   # prices the mode you name
```

It runs every free check a real run runs — the task contract, the mode's own task requirement, the
round contract, the terminal-contract count, the collator's registration and the **canonicalizer
derivation** — and then stops. What it prints is the panel, the resolved canonicalizers with their
provenance, every stage in execution order with its call count, the exact total, and the exact
round-1 prompt every explorer would receive.

Three things worth knowing, and the first two are where it differs from
[a review's dry run](review.md#pricing-a-run-before-you-pay-for-it---dry-run):

- **The call count is one number, not a range.** A review iterates until adjudication converges, so
  its cost depends on model behaviour. An exploration's round count is **fixed by its mode
  contract** — there is deliberately no data-dependent termination rule, because one would be
  steerable by an aggressively-merging canonicalizer — and every other multiplier (panel size, dual
  canonicalization, the confirmation round) is settled before the run starts. Only a halt makes the
  real figure smaller. Neither figure counts a retry after a schema-invalid response.
- **It does not prove any adapter is reachable.** A review's static preflight is free, so its dry run
  can promise every adapter answered `Available`. exploremesh's first stage is the identity
  **pre-flight**, and that stage *invokes* — one real call per governed role. So the stop sits in
  front of it, and the guarantee is the weaker, honest one: nothing left between here and the first
  call is a *configuration* question. Whether the collator answers is what the pre-flight is for.
- **The payload is the exact bytes.** Formulation is app-owned, so the prompt is deterministic and
  the dry run shows the one the run would send, with the same payload hash the run will record —
  not an estimate of it. Rounds 2+ carry the pooled confirmed-canonical uniques, which are derived
  from what the panel says in round 1; they cannot be shown, because they do not exist yet.

`--json` carries the whole disclosure as a `shape` object, including the full prompt. `--dry-run` is
refused with `--dump-run`: a run directory records an exploration — envelopes, raw outputs, prompts,
a manifest — and a dry run produces none of them.

The same capability is `dryRun` on [MCP](mcp.md) and `_meta.exploremesh.dryRun` on [ACP](acp.md).

### References to things exploremesh cannot see

A collator is asked to cite the `envelope#k` aliases of the panel it was shown, and the host checks
every one against the aliases this run actually produced. Sometimes it cites something else instead —
`src/foo.rs`, a URL, `RFC 9110 §8.3`.

exploremesh **reads no filesystem**, so it cannot tell you whether `src/foo.rs` exists. What it will
not do is let the pointer disappear. Three statements have to stay distinguishable, and each has its
own place in the result:

| | Where it lands | What it means |
|---|---|---|
| **Sourced** | `sources` | A ref that resolved to a response this panel produced. |
| **Checked and rejected** | dropped (counted in `refsDropped`) | A ref addressed to this run naming nothing in it — `envelope#9` on a three-seat panel. The host knows every alias it made, so this is a definite no. |
| **Not looked at** | `unverifiedReferences` | A reference to something outside the run entirely. Retained verbatim and labelled. |

An unverified reference is **never a citation**: a finding whose only source is one is still
`uncited`, because nothing was verified and so nothing was sourced. The list is bounded (eight
entries, each clipped to one line and marked when clipped) — a reference is a pointer, not a payload,
and `sources` is a model-authored array.

Grounding such a reference is the **caller's** job, and that is the posture rather than a missing
feature: a tool whose subject is the argument you handed it should not acquire filesystem access to
check that its footnotes resolve. reviewmesh, whose subject *is* a repository, checks its citations
against the tree — see [review.md](review.md#does-the-finding-point-at-real-code-citation-grounding).

### Exit codes

exploremesh and reviewmesh share **one** halt taxonomy and **one** exit-code table — the table in
[../docs/architecture.md](../docs/architecture.md#halt-taxonomy). Every failure exits on its own class:

| Exit | Meaning | exploremesh today |
|---|---|---|
| 0 | success | ✓ |
| 1 | gated findings / schema-invalid model output (Class G) | reserved — exploremesh drops a schema-invalid explorer rather than halting on it, and has no gating flag |
| 2 | usage error (bad invocation, a malformed flag or flag value) | ✓ — asking for help is *not* one: `-h`/`--help`/`help` print to stdout and exit **0**, as in reviewmesh |
| 3 | configuration error (an unresolvable profile/roster/adapter, a failing `doctor`, a `--run` directory that is not one) | ✓ |
| 4 | adapter / auth / binary problem (Class A) | ✓ (`setup --acp detect` on a CLI that does not handshake) |
| 5 | model unavailable, or a proven strong-evidence identity mismatch (Class E) | reserved |
| 6 | containment breach / a refused path (M5, M6) | ✓ (`export --sqlite` naming a protected path, a directory, or a symlink — the destination is refused, never followed) |
| 7 | halted by policy or a cap | ✓ (`export --sqlite` naming a destination that already exists, without `--force`) |
| 8 | internal error | ✓ (also the honest catch-all for an error no layer has classified) |

The reserved rows are reserved *in exploremesh*, not unused in the taxonomy: reviewmesh produces them,
and the codes mean the same thing in both binaries. Nothing else is ever assigned to them.

> A script that tests only for a non-zero exit works unchanged; a script that branches on *which* failure
> occurred reads the table above.

`doctor` is the surface where this matters most day to day: `0` means the panel is ready, `3` means it
found something. `--json` reports the same verdict as `ok`, so a script never has to choose which to
trust:

```bash
aimesh explore doctor --json | jq -e '.ok'                    # ready?
aimesh explore doctor --probe --json | jq '.probes[] | select(.ok | not)'
```

`--probe` is opt-in because it is the only thing `doctor` does that starts a process: it runs a shell
recipe's `<binary> --version` (or an ACP instance's handshake), bounded, with **no model call, no token
spend and no authentication**. Each result reports the **stage** it reached and any classified blocker
signal — the difference between "the binary is missing" and "that CLI is sitting on a login prompt". A
probe proves the CLI starts and answers; it does *not* prove provider auth, model availability or model
identity. Only a real run does that.

## Quick start

An exploration runs the real model CLIs installed on this machine (see [Real adapters](#real-adapters)).
A shell adapter finds its CLI on `PATH` by name and needs **no configuration** to be composed into a
panel. Most explorations are composed in the call itself — by a person at a shell, or by an agent
driving `aimesh` over MCP or ACP — and never touch a config file.

```bash
# from a fresh clone, at the repo root: install the aimesh binary globally.
# No make required, and identical on macOS, Linux and Windows.
go install ./cmd/aimesh

# 1. See what can be composed — adapters, profiles, modes. No model call, no tokens spent:
aimesh explore list

# 2. Compose the panel in the call (>= 2 explorers + a collator) and price it for free:
aimesh explore run "evaluate architectures for a rate limiter" \
  --criteria "correctness,operational risk,cost" \
  --explorer adapter=claude-code,model=sonnet \
  --explorer adapter=codex-cli,model=gpt-5.5 \
  --collator adapter=claude-code,model=opus \
  --dry-run

# 3. Drop --dry-run to run it.
```

The adapter must be one aimesh defines (a shell recipe or a configured ACP instance); the model is the
slug the adapter's own CLI accepts and is handed to it verbatim. Over MCP the same composition is the
`panel` argument (`{explorers[], collator}`); over ACP it is `_meta.exploremesh.panel` — see
[Driving it over MCP](#driving-it-over-mcp) and [Driving it over ACP](#driving-it-over-acp).

**Saving a panel.** A run that names no panel binds to the profile named **`default`**, which ships
deliberately **unconfigured** — nothing runs, and nothing is pre-selected, until you save one:

```bash
aimesh explore setup --profile default \
  --explorer adapter=claude-code,model=sonnet --explorer adapter=codex-cli,model=gpt-5.5 \
  --collator adapter=claude-code,model=opus
aimesh explore doctor               # readiness of the saved panel; no model call
```

There is no set-default (as in reviewmesh) — save the panel you want as `default`. Record a binary path
only for a CLI that is **not** on `PATH`: `aimesh explore setup --adapter codex-cli --path /opt/tools/bin/codex`
writes it to the shared `~/.aimesh/adapters.yaml` both domains read.

`go install` writes to `go env GOBIN`, or `$(go env GOPATH)/bin` when `GOBIN` is unset; that
directory has to be on your `PATH`. See the [README quick start](../README.md#quick-start) for the
`PATH` setup and the `make install` variant. Prebuilt, provenance-attested archives are on the
[Releases page](https://github.com/Tim-Butterfield/aimesh/releases). `go install …/cmd/aimesh@latest` is
not available: the root module `replace`s the unpublished meshcore module, and that form refuses
modules with a `replace` directive.

## Profiles

Named profiles live in `~/.aimesh/explore/profiles.yaml` (user scope) or `<repo-root>/.aimesh/explore/profiles.yaml` (project scope, preferred). Each profile is an **ordered** explorer list + one collator + an optional `defaultMode`. A no-flag run always binds to the profile named **`default`** — as in reviewmesh, there is no set-default; edit `default`'s panel (or save the panel you want under that name) to change what runs.

```yaml
schemaVersion: 1
defaultProfile: default
profiles:
  default:
    explorers:                       # ORDER = preference order (what --count takes the top-N from)
      - { adapter: claude-code, model: sonnet, effort: high }
      - { adapter: codex-cli,   model: gpt-5-codex }   # API-key accounts; ChatGPT-account Codex rejects this slug — see ../docs/adapters.md
    collator: { adapter: claude-code, model: sonnet }
  triage:
    defaultMode: challenge
    explorers: [ ... ]
    collator: { adapter: agy-cli, model: gemini-3.1-pro-high }
    canonicalizers:                    # OPTIONAL: exactly two, or omit and the host derives them
      - { adapter: agy-cli,     model: gemini-3.1-pro-high }
      - { adapter: claude-code, model: sonnet }
```

Discovery order: project `profiles.yaml` → user `profiles.yaml` → the built-in **unconfigured** `default` (an empty profile that exists so it can be configured, but which nothing runs on until it is). `AIMESH_HOME` overrides the user-scope base directory. Those two paths are the only ones searched, and a `profiles.yaml` at the **root** of a `.aimesh/` directory — beside `adapters.yaml`, which is the natural wrong guess — is refused by name rather than ignored: a config file that is silently not read is worse than one that is rejected. An explicit `--roster <path>` remains a per-invocation input.

Preference order is decoupled from **attribution** order: the executable plan sorts the *selected* explorers canonically, so reordering the full list never changes the envelope IDs of an unchanged selection — but it can change *which* explorers a `--count` subset picks, which is why `explore` always prints the selected panel. The plan carries **both** orders under distinct Go types (`roster.AttributionOrdered` / `roster.PreferenceOrdered`), so a stage that selects cannot reach for the attribution order by accident: decoupling the two is not the same as discarding one, and a governance selection made from an alphabetical ordering is a real defect, not a cosmetic one.

Profiles are editable with `aimesh explore setup` or by editing the file; `aimesh explore list` reports what is configured. Full configuration reference: **[../docs/configuration.md](../docs/configuration.md)**.

## Real adapters

Adapter support is **discovered from meshcore** — the same shell recipes reviewmesh uses (`claude-code`, `codex-cli`, `ollama`, `agy-cli`, `devin-cli`, `gemini-cli`, `cursor-cli`) — so an adapter added to meshcore is automatically usable here with no exploremesh change. Generic **ACP adapters** are user-defined instances under `acpAdapters` in the shared `~/.aimesh/adapters.yaml`; there is no fixed ACP catalog.

Resolution is **fail-closed**: a name that is neither a shell recipe nor a configured ACP instance is a configuration error reported *before* anything is spent — it is never silently substituted. Binary paths for adapters not on `PATH` live in the same shared `~/.aimesh/adapters.yaml`, which both apps read.

Check what is installed **without spending tokens**:

```bash
aimesh explore list                             # what is configured, and where
aimesh explore doctor --profile triage          # readiness of a specific profile's panel
```

A profile of real explorers/collator makes real model calls and spends tokens. Install and authenticate each CLI yourself (as with reviewmesh); see [../docs/adapters.md](../docs/adapters.md).

**Run each adapter's CLI once interactively before your first exploration** — an adapter never answers a first-run folder-trust, login or update prompt on your behalf, and a CLI still waiting on one blocks or exits empty (`aimesh explore doctor --probe` surfaces it): see [First-run preflight](../docs/adapters.md#first-run-preflight--run-each-cli-once-yourself).

## Output

`explore` prints a human summary (or the full structured result with `--json`). Common to every mode: any collator/canonicalizer caveat, the panel size and every **dropped** explorer with its audited reason. (The formulation provenance is *recorded* — `formulation.json`, the evidence export, the ACP `_meta` echo — but no longer printed: every registered mode is formulation-free, so it could only ever report one value.) Then the mode's own rendering — the map synthesis, the composed answer, the catalog clusters, the challenge register, the shortlist tally, the comparison matrix, or the pooled forecast.

Adjudicative and fixed-space results also print a governance header: the partition revision hash and ledger size (or the explicit `NONE — fixed space` line), the frozen counting-policy hash, and an `UNSETTLED` warning when contested mappings survived the confirmation round.

Explorer and collator output is **untrusted data**: it is only ever parsed as JSON, never executed, and a prior round's artifact is framed as untrusted data when it flows into a later round's prompt.

## Evidence export

`--dump-run` writes an append-only run directory, and that directory stays the **system of record**. `aimesh explore export` builds a *derived*, rebuildable SQLite view over it:

```bash
aimesh explore run --purpose "..." --criteria "..." --mode shortlist --dump-run
# --dump-run prints `run: <dir>` to stderr — pass that directory to --run:
aimesh explore export --sqlite evidence.db --run <run-dir> --verify
```

`--verify` re-exports into a scratch database **in a temporary directory of its own** and compares — byte-for-byte — proving the database is derivable from the run directory alone. It leaves nothing beside the file it checked. Nothing else in exploremesh reads the evidence package: a run never writes a database implicitly, and the export is entirely optional.

**The destination is governed.** This is the only command that writes where you point it, so `--sqlite` is checked before anything is read or written:

| Situation | What happens |
|---|---|
| a protected path — `~/.aimesh/**` or `./.aimesh/**`, the legacy `.reviewmesh`/`.exploremesh`, `.git/**`, `.env*`, key material (`*.pem`, `id_*`…), `.mcp.json`, `.claude`/`.cursor`/`.codex`/`.gemini`/`.vscode`/`.idea` | **refused**, exit **6**, naming the rule. `--force` does **not** lift this — the denylist is what keeps configuration and credentials human-only |
| the destination already exists (or one of its `-journal`/`-wal`/`-shm` sidecars does) | **refused**, exit **7**, naming `--force`. The database is derived, so rebuilding it is safe by design — but the *file* is yours, and a typo must not be how you find that out |
| the destination is a directory, a symlink, or a device | **refused**, exit **6** — refused rather than followed to whatever it names |
| anything else | exported. The database is built into a temporary file **in the destination's own directory**, fsynced, and then **renamed** into place, so an interrupted export can never replace a valid database with a truncated one |

The consent model is the CLI's own: you typed the path, so that path — and nothing wider — is the allowed root for the operation. What your consent cannot lift is the denylist.

## Driving it over ACP

`aimesh explore acp` runs exploremesh as a local [Agent Client Protocol](https://agentclientprotocol.com/) agent over stdio, so an ACP-capable IDE or host can drive one exploration as a session. The prompt text becomes the task **purpose**; the load-bearing **criteria** (and any mode-specific input) arrive under `_meta.exploremesh`, which may also *select* — never redefine — the panel. Unlike reviewmesh's ACP surface there is no workspace and no write-mode gating, because an exploration edits no file you own — the only thing an ACP turn can write is its own run directory, and only when `dumpRun` asks for it.

`_meta.exploremesh` panel + capture fields:

| Field | Meaning |
|---|---|
| `profile`, `count` | Select a configured profile, optionally narrowed to the top-N by preference order. Never clamped. |
| `panel: {explorers[], collator}` | **Compose** an ad-hoc panel by identifier (2..16 explorers + one collator) — the ACP analogue of the CLI's `--explorer`/`--collator`. Mutually exclusive with `profile`/`count`. Every adapter must already be in the set the agent bound at **startup**: a prompt selects from the configured set and can never introduce an adapter, a binary path or a launch argument. |
| `canonicalizers: [{adapter, model, effort}]` | Name the two identities that propose the canonicalization — the ACP analogue of the CLI's `--canonicalizer` and MCP's `canonicalizers`. **Exactly two, or omit it** (one is refused; two identical identities are refused). Compose-not-configure applies as it does to `panel`. It applies to whichever panel resolved and overrides that panel's own pair. The response echoes `canonicalizers` and `canonicalizerSource` (`explicit` / `derived`). |
| `dumpRun: true` | Record the run (envelopes, raw outputs, prompts, the declared task, a versioned manifest) under `$EXPLOREMESH_ARTIFACT_DIR` — the ACP analogue of `--dump-run`. The response echoes `runCaptured` and the `runId`; the record is at `$EXPLOREMESH_ARTIFACT_DIR/<runId>`. The host **path** is never put on the wire. |

Full protocol reference: **[../docs/acp.md](../docs/acp.md)**.

## Driving it over MCP

`aimesh explore mcp` runs exploremesh as a local [Model Context Protocol](https://modelcontextprotocol.io/) server over stdio, so an MCP agent can drive explorations as **tools**: `explore` (which **spends** — it launches the configured model CLIs, and takes a required `mode` that decides which other parameters are required), plus read-only `explore_list`, `explore_doctor`, `explore_run_status` and `explore_run_result`.

```json
{ "mcpServers": { "exploremesh": { "command": "aimesh", "args": ["explore", "mcp"] } } }
```

Three things are worth knowing before you point an agent at it:

- **Calls are job-shaped.** MCP clients commonly time out around 60 s and a real panel run is minutes, so a run that outlives `waitSeconds` (default 25) comes back as `{runId, state: "running"}` and is fetched with `explore_run_result`. Retrying with the same `idempotencyKey` returns the existing run instead of spending twice. On `2026-07-28`, a client that opts into the `io.modelcontextprotocol/tasks` extension gets a `CreateTaskResult` (`resultType: "task"`, `taskId == runId`) instead of `{runId, state: "running"}` and polls it with `tasks/get`; a client that does not opt in sees exactly the job shape above. See [../docs/mcp.md](../docs/mcp.md#the-iomodelcontextprotocoltasks-extension-modern-era-opt-in-per-client).
- **The governance block, the identity caveats and the requested-vs-executed panel echo are `required` in the declared output schema.** There is no `summary` mode that can drop them, and the declaration is not just prose: every payload this server can emit is validated against its own tool's `outputSchema` in the build, so one that matched no branch would fail CI rather than reach a client.
- **A call can never change configuration**, read a file, or introduce an adapter; `explore_list`/`explore_doctor` report logical identifiers only — no binary paths, launch arguments or environment detail.

Runs are captured to a run directory by default (`--no-capture` to disable), and the wire `runId` *is* that directory's name. The server imposes no admission bounds; how many explorer CLIs one call runs at once is set per call with `maxParallel` (see [mcp.md](mcp.md#admission)).

Full protocol reference: **[../docs/mcp.md](../docs/mcp.md)**.

## Documentation

- [../docs/configuration.md](../docs/configuration.md) — full configuration reference (profiles, the shared adapter layout, environment variables)
- [../docs/acp.md](../docs/acp.md) — the ACP surface in both apps, including `aimesh explore acp`
- [../docs/mcp.md](../docs/mcp.md) — the MCP surface: tools, schemas, job shape, governance block, error model, admission limits, security posture
- [../docs/adapters.md](../docs/adapters.md) — adapter contract, per-provider recipes, adding an adapter
- [../docs/model-identity.md](../docs/model-identity.md) — evidence tiers and identity capture
- [../docs/architecture.md](../docs/architecture.md) — where exploremesh sits in the monorepo
- [../docs/security.md](../docs/security.md) — what leaves the machine
