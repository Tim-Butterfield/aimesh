# reviewmesh

**Provider-diverse, governed AI review — for code and any other artifact.**

`reviewmesh` runs one or more reviewer models — through pluggable [adapters](../docs/adapters.md) — over an artifact (code, a design or architecture doc, a spec, a README, a changeset — anything with an intent to check against), has a single **host authority** adjudicate the findings, and then **reports**, **patches**, or **applies** the accepted changes. It runs across several surfaces (a standalone CLI, ACP-capable IDEs, and CI), with any provider lane backed by any adapter, and it keeps reviewers **read-only** while only the host writes.

Nothing in the pipeline is code-specific: the same review loop that critiques an implementation can just as well critique the design or requirements that implementation is meant to fulfil.

reviewmesh is one of the two applications in the [aimesh](../README.md) monorepo. It builds its review domain — lanes, roles, the `Finding` schema, and the review pipeline — on top of the [meshcore](../meshcore/) governance substrate (adapters, model-identity verification, containment, config, doctor, ACP transport, halt taxonomy, audit).

Module path: `github.com/Tim-Butterfield/aimesh/reviewmesh`.

## How a review runs

1. **Resolve a plan** from configuration and auto-detection — no flags required — and announce the `role → adapter → model` mapping once.
2. The **blind reviewer panel** (read-only, each seat in its own contained copy of the workspace) produces structured `Finding`s.
3. The **host** (`author_remediator`) adjudicates the union: accept, reject, or request remediation, then optionally `cross_check` and `verifier`.
4. Depending on mode, the host **reports** findings, **emits a patch**, or **applies** accepted changes. Only the host writes.

**Roles** are configuration, not components: `author_remediator` (the host), `reviewer`, `cross_check`, `verifier`. **Modes** — `report` (findings only), `patch` (emit a diff), `apply` (write changes) — are gated by `min(requested, surface, policy)`.

### The blind reviewer panel

The primary review stage is a **panel**: an ordered list of reviewer seats, each a distinct
`(adapter, model, effort)` vantage. Compose as many as the review is worth, up to 16 — one seat is
perfectly ordinary and is exactly what a single-reviewer configuration has always done.

- **Blind.** Every seat runs in parallel and sees only the workspace and *its own* earlier rounds — never a peer's findings, never the host's decisions. Host adjudication starts only after every seat has finished.
- **Nothing is trimmed.** N seats requested is N seats executed, or the run fails with a reason. An unresolvable seat, a duplicate seat identity, or a panel above the cap is a configuration error *before* anything is spent; a seat that fails at runtime (identity mismatch, adapter failure, unparseable output) halts the whole run rather than yielding a quiet partial panel.
- **The host counts.** Each finding carries the seats that reported it, a host-computed `agreementCount`, and the seats that ran and did *not* report it. No model is ever asked how many agreed. Each supporting seat carries its model-identity tier so you can see what the support consists of; a weak tier never changes what happens to the finding (see [docs/model-identity.md](../docs/model-identity.md)).
- **`cross_check` and `verifier` are not seats.** They are separate roles with deliberately different information: cross-check sees the *adjudicated* set (informed, and its findings can be applied), the verifier is report-only. Growing the panel never changes either.

Compose a panel ad hoc from the CLI — the flag order is the panel's order:

```bash
aimesh review run \
  --reviewer adapter=codex-cli,model=codex-cli-default \
  --reviewer adapter=claude-code,model=claude-code-default \
  --reviewer adapter=agy-cli,model=agy-cli-default,effort=high \
  .
```

`--reviewer` may name only adapters and models your configuration already defines (it composes, it
never configures), and it cannot be combined with `--profile` — a panel is composed *or* selected.
To save one instead, put a `reviewers` list on a profile ([configuration](../docs/configuration.md#profiles-and-the-reviewer-panel))
or `aimesh review setup --profile <name>`. The same panel is composable over ACP with
`_meta.reviewmesh.panel` ([ACP](../docs/acp.md#profile--panel-selection-via-_metareviewmesh)).

## Commands

```
reviewmesh <command> [flags]

  review <path>   review an artifact/workspace
                  --report | --patch | --apply | --mode report|patch|apply
                  --profile, --config, --set role.adapter=NAME|role.model=NAME (repeatable),
                  --reviewer adapter=NAME,model=M[,effort=E] (repeatable; ad-hoc blind panel,
                    1..16 seats, mutually exclusive with --profile),
                  --fail-on-findings, --ci, --include-host-review, --json, --debug,
                  --authority <path> (repeatable), --authority-hash <name>=<sha256>,
                  --authority-manifest <file.json>
  list            configured adapters + profiles (--json for the machine projection)
  doctor [<path>] static readiness checks (no model calls)
                  --probe, --fix, --interactive, --profile, --json
                  (--json cannot be combined with --fix/--interactive)
  setup           write/update config (defaults to user scope: ~/.aimesh/review)
                  --scope user|project, --profile, --adapter <name> --path <path>,
                  --interactive, --from user [--yes] [--include-adapter-paths],
                  --acp detect|add|remove [--path <p>] [--name <k>] [--title <t>]
                    [--acp-arg <a> ...]   (user-defined ACP adapter instances)
  config          config housekeeping
                  clean-model-keys [--apply]  rename generated modelCatalog keys an older
                    UI build materialized with a redundant adapter prefix (preview by default)
  init            create the local, VCS-excluded .aimesh/ state directory
                  (variants: init | repo init | folder init)
  acp             run as an ACP agent server (JSON-RPC 2.0 over stdio; --framing newline|content-length)
                  --root <dir> (repeatable) trusted workspace root a session may review
                  --no-default-root (do not adopt the launch working directory)
                  --allow-broad-root (opt in to an explicit --root the degenerate rule refuses;
                    never applies to the inferred cwd, never waives the denylist)
                  --turn-timeout <dur> (wall-clock budget for one turn; default 10m)
  mcp             run as an MCP server (Model Context Protocol over stdio)
                  tools: review_report (writes nothing), list, doctor, run_status, run_result
                  --protocol dual|legacy  era posture (default dual; legacy = a
                    pre-2026-07-28 server, a compatibility fallback, not conformance)
                  --root <dir> (repeatable; MANDATORY consent — as acp, and STRICTER on
                    2026-07-28: an inferred cwd is not a root there),
                  --no-default-root, --allow-broad-root
                  --allow-inferred-root  accept the launch cwd as a trusted root on
                    2026-07-28+ (default: refused, every path denied)
                  --allow-remediate  ALSO expose review_remediate, which WRITES
                  --framing, --wait-seconds, --turn-timeout

  --version [--json], --help
```

A command is required. `review` needs **no configuration flags** — it resolves a complete plan from auto-detection and defaults.

## Quick start

A review always runs real model CLIs you have installed and authenticated. A shell adapter finds its
CLI on `PATH` by name and needs **no configuration** to be composed into a panel, so most reviews are
composed in the call itself — by a person at a shell, or by an agent driving `aimesh` over MCP or ACP —
and never touch a config file.

```bash
# from a fresh clone, at the repo root: install the aimesh binary globally.
# No make required, and identical on macOS, Linux and Windows.
go install ./cmd/aimesh

# 1. See what can be composed — adapters, the model catalog, profiles. No model call, no tokens spent:
aimesh review list --json

# 2. Compose the panel in the call and price it for free. The host lane (author_remediator) is the one
#    seat that must name a model-catalog key: it is never passed through.
aimesh review run --dry-run --report . \
  --reviewer adapter=claude-code,model=sonnet \
  --reviewer adapter=codex-cli,model=gpt-5.5 \
  --set author_remediator.adapter=claude-code --set author_remediator.model=claude-code-default

# 3. Drop --dry-run to run it:
#    --report   findings only (the default when no mode is named)
#    --patch    emit a diff to apply yourself
#    --apply    write the accepted findings into the tree
#    --ci       non-interactive CI: report + gate (exit 1 on findings)
```

Over MCP the same composition is the `panel` argument of `review_report` (`{reviewers[], author_remediator, …}`);
over ACP it is `_meta.reviewmesh.panel` — see [docs/mcp.md](../docs/mcp.md#review_report) and
[docs/acp.md](../docs/acp.md#profile--panel-selection-via-_metareviewmesh).

**Saving a panel.** A run that names neither `--reviewer` nor `--profile` binds to the profile named
**`default`**, which ships deliberately **unconfigured** — `doctor` reports it as needing adapters until
you save one:

```bash
aimesh review setup --interactive   # guided: detect CLIs, record binary paths, assign the panel and lanes
aimesh review doctor                # readiness of the saved profile; no model call
aimesh review run --report .        # then a no-flag run reviews with it
```

Record a binary path only for a CLI that is **not** on `PATH`:
`aimesh review setup --adapter codex-cli --path /opt/tools/bin/codex` writes it to the shared
`~/.aimesh/adapters.yaml` both domains read.

## Pricing a run before you pay for it (`--dry-run`)

A review costs real model calls, and a large panel costs several per cycle. `--dry-run` resolves
everything and spends nothing:

```bash
aimesh review run --apply --dry-run .    # what would --apply do, and what would it cost?
```

It stops in the window after the last knowable configuration error and before the first model call,
so it either prints the run's shape — every seat with its adapter, model and effort; every other
lane; the outer-cycle and panel-round caps; the model-call range; and what those calls would carry
— or it hands you the exact refusal the real run would have hit, at no cost. That includes an
unresolvable seat, a duplicate panel identity, a missing adapter binary, an oversized authority
document, and a workspace the containment copy would refuse.

Three things worth knowing:

- **The call count is a range.** The floor is a run that converges on its first cycle with nothing
  contested; the ceiling is every configured cap multiplied out. A single number would understate
  the iterating run or overstate the ordinary one. Neither bound counts a bounded retry after a
  schema-invalid response.
- **It prices the mode you name.** `--apply --dry-run` prices an apply run and writes nothing. The
  mode is the expensive variable, so a dry run that silently became a report run would answer a
  question nobody asked.
- **Read the workspace line, not just the price.** It reports the files and bytes each reviewer
  would be shown:

  ```
    workspace: 660 file(s), 6454.9 KB — every one shown to every reviewer, in full
  ```

  That is the whole workspace minus what containment excludes. A review does not sample: no file
  is clipped and the walk never stops early, because a defect in a file nobody opened is
  indistinguishable from a file with no defects. The byte figure is worth reading before a large
  panel — it goes out once per seat, per round — and `run-shape.json` lists every file by name.

It is refused with `--ci` and `--fail-on-findings`: a dry run reviews nothing, so a findings gate
over one always passes, and a green CI job that priced a review instead of running it is the single
most dangerous way to misread this flag. `--json` carries the same disclosure as `status: "planned"`
plus a `shape` object.

> An existing `~/.aimesh/review/config.yaml` (from a prior `setup`) can override `defaultProfile` and
> escalate lanes to real adapters. To force a pristine config regardless, run with
> `AIMESH_HOME=$(mktemp -d)`.

`go install` writes to `go env GOBIN`, or `$(go env GOPATH)/bin` when `GOBIN` is unset; that
directory has to be on your `PATH`. See the [README quick start](../README.md#quick-start) for the
`PATH` setup and the `make install` variant. Prebuilt, provenance-attested archives are on the
[Releases page](https://github.com/Tim-Butterfield/aimesh/releases). `go install …/cmd/aimesh@latest` is
not available: the root module `replace`s the unpublished meshcore module, and that form refuses
modules with a `replace` directive.

## How long a review takes, and why nothing here is on a stopwatch

A panel is several real provider CLIs, each doing real work on your code. **A review takes minutes,
not seconds.** It is a pre-merge gate, not an editor-latency tool, and treating it as the latter will
disappoint you no matter how it is tuned.

**There is no per-seat time limit, deliberately.** Elapsed time is not evidence: a seat thinking hard
about a large diff and a seat that is hung look identical under a stopwatch. A deadline would discard
real work, and it would do so *systematically on the hardest inputs* — the ones most worth reviewing.
A long run is allowed to be long.

What answers "is it actually running?" is not a clock:

- **Before dispatch** — the readiness preflight probes every configured agent, and `--probe-deep`
  runs one real bounded invocation in a throwaway directory. The things that make a CLI hang forever
  — an unanswered folder-trust prompt, a login it needs, an update notifier blocking on input — are
  classified and caught *at the front door*, before a single seat is paid for.
- **During the run** — a seat whose call dies is a
  [capacity loss](#when-a-provider-runs-out-capacity-is-not-integrity): the panel degrades and reports
  `partialPanel` rather than discarding the seats that answered.

**The only time-based bound is the turn budget** (`--turn-timeout`, default 10m on the agent
surfaces), and it is *yours*. It exists so an agent-driven turn cannot run unbounded with nobody
waiting for it — not as a judgement about how long good work should take. Raise it for a large review;
nothing else will cut a run short.

Per-seat progress is streamed on both agent surfaces, and MCP hands a long run back as
`{runId, state: "running"}` with polling tools, so a slow panel is visible rather than silent.

## Reviewing part of a tree (scope)

Until recently there was no file-level scope at all: the collector took a root and collected it, so
narrowing meant pointing at a smaller directory.

The primitive is **a set of files**. Everything else is a *baseline* — a way of producing that set —
and which baselines are available depends on what the tree can offer:

| Baseline | In a repository | In a plain folder |
|---|---|---|
| `--path <path\|glob>` (repeatable; a directory takes its subtree) | ✓ | ✓ |
| `--changed-since <2h \| RFC3339>` (modification time) | ✓ | ✓ |
| `--changed-vs <diff\|staged\|ref>` | ✓ | refused |

**Not all usage is repo-focused**, and that ordering is the design rather than a convenience. `folder
init` is a peer of `repo init`, so a plain folder needs a real answer to "review what I just changed"
— which `--changed-since` gives it. Making a git ref the organising idea would have handed half the
usage a first-class feature and the other half a documented gap. The VCS baselines are **one resolver
among several**.

`--changed-since` is coarser than a diff — a touched-but-unchanged file is selected — and coarse in
the safe direction: the cost is reviewing a file that did not need it, not missing one that did.

**Baselines union.** Naming two means "review what either selects", which is how people actually say
it: *the files I touched, plus this one I'm worried about*. The intersection reading has no natural
phrasing.

**A selector that matches nothing is refused.** Falling back to the whole tree would review everything
at full cost while you believed you had narrowed it; proceeding with nothing would report "no
findings" about nothing. Likewise `--changed-vs` in a directory under no version control is **refused,
not ignored** — and the refusal points you at the two baselines that do work there.

**Scope narrows what reviewers are SHOWN, and nothing else.** Containment is unchanged: the copy still
holds what it always did. And because a narrowed review is silent about everything it was not shown,
the result carries a `scope` block with the selected count, the file list, and a fixed note saying so
— the absence of a finding elsewhere means nobody looked, not that there is nothing there.

## When a provider runs out (capacity is not integrity)

A seat can fail for two very different reasons, and until now both halted the run:

- **Integrity** — a proven model-identity mismatch, a containment refusal, an unresolvable adapter.
  The run's *premises* are broken, so nothing the panel produced can be trusted. It still halts.
- **Capacity** — a quota wall, a rate limit, a wall-clock timeout. That is a fact about a billing
  relationship or a clock. It says nothing about whether the seats that *did* answer were sound.

Halting on the second throws away everything already paid for. With several billing mechanisms in
play at once — an enterprise quota, a flat subscription, purchased tokens, standalone API keys — one
seat hitting its wall late in a panel is ordinary, and discarding three completed seats because the
fourth was out of credit is the tool wasting your money on your behalf.

So a capacity failure now **degrades the panel** instead:

```
PARTIAL PANEL: 2 of 4 configured seat(s) answered.
  lost reviewer-3     vendor-a/m3 — quota_exhausted
  lost reviewer-4     vendor-b/m4 — timeout
  A provider ran out of capacity and the run continued rather than discarding the
  seats that had already answered. …read every agreement count against the seats
  that ANSWERED, not the panel you configured.
```

**Nothing is retried, substituted or assumed.** No other model stands in, nothing is topped up, and
the review is not declared fine. The run reports a smaller panel, names which seats were lost and to
what, and lets the denominator speak. The findings that survive came from the seats that ran — as
true as it ever was, over a smaller number.

**Its presence is the signal.** `partialPanel` is absent on a full panel rather than zeroed, so you
never have to compare two counts to notice. The classification is the one already in the audit record
(`clihint`), read rather than re-derived — a second classifier that disagreed with the first would be
the difference between a delivered review and a discarded one.

**There is no minimum-seat floor**, deliberately. No threshold can be justified from anything: the
honest denominator is however many answered, reported plainly, and the panel echo already accounts
for every requested seat by name. A floor would be a number we made up standing where a fact belongs.

**An unrecognized signal halts.** The capacity set is small and explicit — quota and timeout — rather
than default-allow, so adding a failure mode to the classifier can never silently widen what a panel
will continue past.

## What was the agreement worth? (panel composition)

`agreementCount` is host arithmetic over which blind seats reported a finding. It is correct as
arithmetic and it does not say how *independent* those seats were — three seats agreeing means less
when two of them run the same model behind two different vendor CLIs, because their errors correlate.

So every result now carries what the panel **was**, and every panel finding carries what its
agreement was worth:

- `composition` — the seats as configured, `distinctModels`, and `independence`
  (`distinct_models` | `shared_model`) for the panel as a whole.
- per finding — `distinctModels` and `agreementIndependence` over the seats that actually agreed.

**No count is adjusted.** `agreementCount` means exactly what it always meant. `distinctModels` is
not a discounted version of it — it is a count of a *different* thing, computed the same way and
equally checkable. "3 seats, 2 distinct models" states the whole situation without anyone having to
interpret a weighting.

**Only the model string is asserted**, and that limit is the point. An adjusted figure — "3 seats,
effective agreement 2.1" — would need a model-*family* taxonomy: which model strings are the same
weights under different names, which vendor rebadges whom. None of that is observable here, and
publishing a number resting on a table we cannot verify would be the same failure as asserting a
dollar cost. Two seats naming the same model *are* the same model; that needs no taxonomy. Vendor,
base family and weight lineage are deliberately **not** claimed — the raw composition rides alongside
so a reader who knows that `provider-a/x` and `provider-b/y` are the same weights can see it, and
this simply refuses to assert it on their behalf.

**`shared_model` does not make a finding wrong.** It means the agreeing seats share priors, so the
extra agreement is worth less than the count suggests. The fixed `note` in the record says both of
those, because the two misreadings run in opposite directions.

It is the same vocabulary as exploremesh's canonicalizer `independence`, one scale up: there it
describes two proposers, here a whole panel.

## Composing a panel: the adapter is checked, the model is yours

`--reviewer adapter=<name>,model=<m>[,effort=<e>]` composes an ad-hoc panel. The two halves are held
to different standards, on purpose:

- **The adapter must be one your configuration defines.** It is fail-closed and stays that way: an
  adapter is a binary this machine executes, and it is the thing that carries trust and
  identity-evidence capability.
- **The model may be anything the adapter accepts.** A `modelCatalog` key (run `aimesh review list`
  to see them) resolves to that entry's pinned per-adapter argument and effort. Anything else is
  handed to the adapter **verbatim** and reported back as `modelSource: passthrough`.

Requiring the model *string* to be pre-registered established nothing the identity layer does not
establish independently — it verifies what answered, whatever the string said — while making real
panels unexpressible: composing one meant editing your global config mid-task, which is a bad thing
to have to do on someone else's behalf. exploremesh's `--explorer` has always worked this way; this
is review agreeing with it.

**A profile's own lanes are still held to the catalog.** The split is on who wrote the seat, not on
what the string looks like: a config file declares the vocabulary it is then read against, so an
unknown key there is a typo worth catching. If it passed through, every config typo would become a
silent call to a provider.

**What pass-through costs you.** Nothing about a pass-through seat's findings, agreement or
applyability differs — but your configuration cannot describe that model, so `list` will not show it,
nothing pins its effort or argument, and whether the name exists at all is the provider's answer
rather than ours. A typo therefore fails **mid-run, in the vendor's words**, after spending, instead
of being refused before you paid. Two things soften that:

- **The near-miss hint.** Catalog keys embed effort (`claude-opus-5-high`), while the flag documents
  model and effort separately — so the documented shape names a key that does not exist. When your
  string is a configured key minus its effort suffix, both the refusal (for a profile lane) and the
  pass-through report name it: *did you mean `claude-opus-5-high`?* The match is deliberately narrow
  — exactly one missing suffix segment — because a confident wrong name is worse than none.
- **`--dry-run`** still resolves the whole panel and spends nothing, so a composed panel can be
  priced and shape-checked before it runs.

## What did the rest of the panel do? (dissent)

An agreement count tells you who reported a finding. It does not tell you what the *other* seats did,
and until you know the denominator the count is half a fact: one seat out of five is a very different
thing to read than three out of three.

Every panel finding therefore carries a `consensus` label, and the run carries the tally:

- **`unanimous`** — every seat that completed reported it.
- **`majority`** — more of the completed seats reported it than did not.
- **`contested`** — a minority reported it, or the seats split evenly.

You see it in the run summary line, marked per finding in `review-summary.md`
(`2 of 5 seats (contested)`), and as `consensus` per finding plus `dissent` per run in `--json`,
ACP and MCP.

**A silent seat is not a seat that disagreed.** This is the whole caveat, and it is why the label is a
label rather than a score. The seats are blind: each is asked what it finds, not to vote on a list. A
seat that did not report a finding may have disagreed with it, may never have reached that file, may
have spent its round elsewhere, or may have stopped when its own set stabilized — those are
indistinguishable from here, and nothing guesses between them.

So `contested` says exactly one checkable thing: fewer of the seats that ran reported this than did
not. **It is not evidence against the finding**, and a contested finding is not less likely to be real
than a unanimous one. It is the one worth reading yourself rather than skimming.

**Nothing is gated on it.** No count is adjusted, nothing is dropped, downgraded, reordered or made
unapplyable. The tempting version — weight each finding by its support ratio and sort on it — needs
silence to mean disagreement, which is the one thing it does not mean, and it would bury a single-seat
finding that happens to be the serious one. That is the failure a panel exists to prevent.

Two cases carry no label at all, because there is nothing honest to say: a finding with no panel
behind it (an evidence hook, the cross-check, the verifier), and a run where fewer than two seats
completed — one seat cannot be unanimous with itself.

**What is not built:** an adversarial refutation stage, where a second pass asks a seat to argue each
surviving finding is wrong. It is deferred rather than refused. It spends a second round of real calls
on the findings that survived, and its distinctive claim — that "argue this is wrong" surfaces
something a second *blind seat* does not — is plausible and unmeasured. Making dissent visible is the
precondition for measuring it: until you can see what the panel already disagreed about, there is
nothing to compare a rebuttal against.

## Applying into a tree you were already working in

`--mode apply` writes into your working tree, and **it does not care whether that tree was clean.**
Working with uncommitted changes is the normal state of the work, not an anomaly, so a gate there
would fire on nearly every real invocation — and a precondition that fires constantly is one you
learn to override by reflex, which makes it worth nothing anyway.

**What you get instead is a precise undo.** `patches/changes.patch` is a complete reverse-appliable
delta of everything the run wrote, produced on the apply path as well as the patch path. So
`git apply -R` takes out exactly this run's edits and leaves yours alone. The blunt undo
(`git checkout -- .`) does take both — that is what the old refusal was really about — but the
precise one has always existed, and it is the one to reach for.

The tree's state **is** recorded: an apply into a dirty tree emits a `workspace_dirty` event with
the paths involved, so a run read later says plainly that the tree was not clean when it was
written to. Recorded, never enforced.

**A tree under no version control** has no "dirty" state to speak of, and it is the one that most
needs the patch, because there is no `git` to fall back on. So the run says so outright:

```
this tree is under no version control, so reverse-applying <run>/patches/changes.patch is the
ONLY way to undo what this run is about to write (`git apply -R <patch>`, or `patch -R -p1 < <patch>`)
```

That artifact used to land in the OS temp directory for exactly this tree — the least recoverable one
keeping its only recovery artifact in the most disposable place. It now gets a project state directory
created for it before the run. That is the one case where creating state unasked is right: we are
already about to write into this tree by explicit instruction, and declining to also write the means
of undoing that is restraint aimed at the wrong thing. A configured artifact directory is never
relocated — only the unconfigured fallback is.

What the write path *does* defend hard against is a **targeted file changing during the run**:
base-hash pins, verified twice, the second time inside the commit window. That is a different
question from whether the tree was already dirty, and it is the one that can actually corrupt a
write.

## Reviewing a tree the denylist protects (`--allow-protected-paths`)

Containment refuses `.git`, `.claude`, `.cursor`, `.codex`, `.gemini`, `.vscode`, `.idea` and
`.windsurf` as write destinations, and refuses a workspace root that sits inside one. The reason is
specific: `.git/hooks/**` and `.vscode/mcp.json` **execute code on the next command**, so an edit
there escapes the review that proposed it.

**`.aimesh` is not on that list.** Run artifacts live in `.aimesh/{review,explore}/runs/**`, and
reading a previous run is ordinary work that some modes exist to do — so a run directory can be
named as a workspace with no flag at all. It stays write-denied (nothing model-authored edits the
config beside those runs) and stays excluded from a walk (reviewing a *project* does not drag its
run artifacts into the payload). Naming a run directory and walking past one are different acts.

But those are also ordinary directories that people own, and `~/.claude/hooks` is somebody's actual
source tree. `--allow-protected-paths` admits such a root and permits writes inside it. It is an
operator act on every surface — a flag on `review run`, a launch flag on `review acp` / `review mcp`
— and for a sharper reason than the other waivers: a caller that could grant itself this could
arrange to run its own code on your machine later.

**It does not unlock secrets, and no flag does.** `.env*`, `.ssh`, `.gnupg`, `.aws`, `.azure`,
`.gcloud`, `.kube`, `.docker`, `.netrc`, `.npmrc`, `.pypirc`, `.pgpass`, `.git-credentials` and
private-key material stay refused for **reads as well as writes**. The asymmetry is deliberate: an
unwanted edit is recoverable from the journal and the patch artifact, while a secret that reached a
prompt has reached a vendor and cannot be recalled. Root confinement is untouched too — the waiver
widens which *names* are allowed inside a root, never which roots exist.

## Judging against a large specification

There is **no size limit on an authority document**. Whether a model can hold a 100 KiB
specification is the model's business, not this tool's, and the ceiling that used to live here
(64 KiB per document) meant a real spec — this repository's own `docs/mcp.md` is over 100 KiB —
could not be judged against at all.

The cost is real and worth knowing: authority is embedded in **every seat's prompt, on every
round**, so it multiplies. That is what `--dry-run` is for. It names the exact bytes, the files and
the destinations before anything is spent, which is a better answer than a refusal — you see the
number and decide, instead of being told no.

Nothing is ever silently shortened. A document is embedded exactly as declared; the only thing that
narrows one is `completeness: ranges`, which the **caller** asks for explicitly. What survives is a
cap on the **count** (`MaxDocs` = 8): how many separate things one run is judged against is a
question about the shape of the request rather than its size.

## Did the change break anything? (`--verify-cmd`)

Point a review at your project's own build and test commands and it will run them **on the containment
copy** — before any edit, and again after every edit — and record what happened:

```bash
aimesh review run . --mode apply --verify-cmd "go build ./..." --verify-cmd "go test ./..."
```

**Nothing model-authored is ever executed.** The commands come from your command line, and they run in
a copy of your own project. These are commands you already run on that tree, so the threat model is
exactly what it was. (Running reviewer-proposed reproducers would be a different thing entirely — it
inverts the containment guarantee, needs a real sandbox this project does not have, and is refused
until it does.)

**Read the `delta`, not the colour.**

| Delta | Meaning |
|---|---|
| `unchanged_pass` | Everything passed before and after. |
| `unchanged_fail` | The same commands failed both times. **This is an ordinary outcome** — the tree was already red and the change did not make it worse. |
| `fixed` | Something that failed before passes now. |
| `broken` | Something that passed before fails now. The one worth looking at. |
| `baseline_only` | A report run: it writes nothing, so there is no "after". |
| `not_comparable` | A command timed out or could not start, so the two passes do not describe the same thing. A fact about the budget or the host, not about your code. |

Absolute green is deliberately **not** the signal. Real repositories routinely have a red suite — a
flaky integration test, a known failure, a half-finished migration — and a tool that demanded
green-before would refuse to run on exactly the repositories most in need of review.

**It gates nothing, and that is structural rather than a promise.** The second pass runs *after* the
commit, so a failure could not have blocked it even if someone wanted it to. No finding is dropped,
downgraded or reordered by any of this, and the exit code does not change. A test suite does not know
whether a reviewer's finding is real, and it does not know whether the code it exercised is the code
that changed — so a pass does **not** mean the change is correct.

**Where the passes run, and why it is two different copies.** The baseline runs on a throwaway copy;
the second pass runs on the write copy only once the commit is done. Both are byte-identical to your
tree at the moment they are taken, so the comparison holds — but neither can leave anything behind. A
build writes: object files, binaries, `node_modules`, coverage directories. The commit writes back
every file in its copy that differs from your tree, so a verification pass in the wrong place would
commit build output into your repository, and no denylist would catch that reliably because build
artifacts look exactly like source.

On **report** runs the commands do not run unless you add `--verify-baseline`: there is no "after", so
you would pay the full cost of your suite for a single baseline fact. `--verify-timeout` bounds each
command (default 5m); at most 8 commands, since each is a real process run twice.

On the **agent surfaces** it is a launch flag — `aimesh review acp --verify-cmd …`, `aimesh review mcp
--verify-cmd …` — and this is the strictest case of that rule in the codebase rather than another
instance of it. Every other launch grant decides what the tool may *read* or *write*; this one decides
what it *executes*. A caller that could name a command would have arbitrary code execution on your
machine dressed as a review parameter, so the value comes from your command line and nowhere else.

## Does the finding point at real code? (citation grounding)

Every review checks, for every finding it reports, the part of the citation that can be checked by
reading files and **running nothing**: does the cited file exist under the reviewed root, is the file
long enough to contain the cited lines, does the named symbol appear in it at all.

It is always on. There is no flag, because there is nothing to consent to — it executes nothing,
starts no process, and reads only files this run already read to build the reviewer prompts.

**It labels; it never disposes.** A finding whose citation did not resolve is raised, adjudicated and
reported exactly like any other, at its own severity, in its own place in the list. That is not
leniency. A reviewer that named the wrong file may have found a real defect a line away, in a file it
half-remembered, or in a file the collector never showed it — and a host that cannot read the code is
in no position to decide which. The host knows one thing, that the pointer did not resolve, and
reporting that is the whole of what it is entitled to say. (Same rule as
[model identity](model-identity.md): recorded, never acted on.)

**`grounded` is a floor, not a verification.** It says the finding points at something real. It says
nothing about whether the code there does what the finding claims — no execution, no semantics, just
existence. The run-level tally carries a fixed `note` saying so, and that note travels with the numbers
into `--json`, the ACP echo and the MCP result, so a consumer that surfaces the ratio has the qualifier
in hand.

The per-finding `status` is one of:

| Status | Meaning |
|---|---|
| `grounded` | The file exists, and the location — if this pass understood it — resolved. |
| `no_citation` | The finding names no file. Ordinary, not a failure: plenty of good findings are about the change as a whole. Deliberately **not** counted in the `checked` denominator. |
| `file_missing` | The cited path is not under the reviewed root. This is the failure the check exists to catch. |
| `line_out_of_range` | The file exists and is shorter than the line the finding cites. |
| `symbol_absent` | The file exists and the named identifier appears nowhere in it. |
| `location_unchecked` | The file exists; the location was not a line reference or a single symbol, so it was **not examined**. It says nothing either way — do not read it as a problem. |
| `unreadable` | The path could not be read (a directory, a permission error, past the size bound). A fact about the host, not about the finding. |

That last-but-one row is a deliberate design choice rather than a gap. A `Location` is model-authored
free text, so it is often a sentence — *"the retry loop near the top"*. The pass will only treat a
location as a symbol when it is a single identifier-shaped token; anything wordier is reported
unchecked. It would rather say "I did not check this" than accuse a correct finding of pointing
nowhere, because an unchecked location costs a reader nothing while a false `symbol_absent`
discredits a real finding.

The human summary prints only when something failed to resolve — an all-grounded run has nothing to
act on, and a line every time would train you past the line that matters. `--json` carries the full
per-finding detail either way.

> **exploremesh does not do this**, and that asymmetry is deliberate. It has no workspace and reads no
> filesystem: its subject is the argument you handed it, not a repository. What it does instead is
> refuse to let an outside reference vanish — a finding citing `src/foo.rs` keeps that pointer under
> `unverifiedReferences`, labelled as something nothing here checked. See
> [explore.md](explore.md).

## Reviewing against an intent (`--authority`)

A review is far more useful when the reviewers know what the artifact was *supposed* to do. Point them at the **authority** — requirements, a design doc, an ADR, a spec — and they judge the artifact **against that stated intent** instead of against their own priors:

```bash
# judge the implementation against the design it is meant to fulfil
aimesh review run --report --authority docs/design.md .

# several documents, one of them content-pinned so it cannot change under you
aimesh review run --report \
  --authority docs/design.md \
  --authority ../specs/api-contract.md \
  --authority-hash design.md=$(shasum -a 256 docs/design.md | cut -d' ' -f1) \
  .
```

**Declaring part of a document.** A large spec often has one section a change is actually judged
against, and narrowing to it costs less and focuses the panel. Nothing forces you to — the whole
document is carried if you name it — but when you know the section, say so. Append a selector:

```bash
aimesh review run --report --authority 'docs/design.md#Storage layer' .
aimesh review run --report --authority docs/design.md:1012-3033 .
```

The first names a top-level section; the second is an explicit byte range (start inclusive, end
exclusive) — the form the budget refusal prints, so it can be pasted back verbatim. Both resolve, at
the flag boundary, into the same `ranges` declaration `--authority-manifest` takes: the run records
concrete byte offsets rather than a heading that might resolve differently on a later read, the
inclusion manifest says `complete: false` with its own embedded hash, and the omitted spans are
marked in the prompt. A heading that names nothing lists the sections that exist; one that names two
sections is refused rather than resolved to the first.

An authority document is **context, never a target.** It is never reviewed, never patched, never applied, and never placed in the containment copy — it is embedded in the prompt, so the write layer cannot reach it at all. A document's **name** defaults to its file base name (that is what `--authority-hash <name>=<sha256>` refers to).

For anything a flag cannot express — byte ranges, a media type, inline content — use a manifest file instead (the two forms do not mix):

```bash
aimesh review run --report --authority-manifest ./authority.json .
```

```json
{ "authority": [
  { "name": "design", "path": "docs/design.md", "mediaType": "text/markdown",
    "expectedHash": "sha256:…", "completeness": "requireFull" },
  { "name": "api", "path": "../specs/api-contract.md",
    "completeness": "ranges", "ranges": [ { "start": 0, "end": 4096 } ] }
] }
```

What it guarantees, and what it refuses:

| Guarantee | If violated |
|---|---|
| **Root-scoped reads.** Naming a path is consent to read *that path*; the non-overridable read denylist still applies, so a `.env` or key file is never admitted as "authority". | exit **6**, `scope_read_denied` / `scope_outside_root` |
| **Hash pinning.** `--authority-hash` is checked against the bytes read, so a document cannot change between a report run and the apply run acting on it. | exit **7**, `authority_hash_mismatch` |
| **No silent truncation.** A declared document is embedded exactly as declared, at any size — there is no byte budget, because how much context a model can take is the model's business. Partial inclusion happens **only** through declared `ranges`, and the manifest then records `complete: false` with both hashes. The one cap left is the **count**: 8 documents. | exit **7**, `authority_too_many_docs` |
| **Provenance split.** Inline `content` authority is **report-mode only** and never reaches the host adjudicator — otherwise a model could supply the intent, its peers "find deviations" from it, and the host would write the result with no human artifact in the loop. | exit **3**, `authority_inline_mode_invalid` |
| **The write-path rule.** An applied hunk must trace to evidence in the workspace copy. A finding supported only by authority text is **reported** and marked `applyable: false` (`applyRefusalReason: authority_only`) — never written. | — (reported, not applied) |

Every run that declares authority writes an **inclusion manifest** — `authority/inclusion-manifest.json` in the run directory, echoed in `run-state.json`, `review --json`, and the ACP prompt result — recording per document the source, the full-content hash, the hash of exactly the bytes embedded, the byte counts, and whether the inclusion was complete. "Which intent was this judged against" is therefore a recorded fact.

The same capability is available on the ACP surface as `_meta.reviewmesh.authority[]` with the identical schema and identical fail-closed refusals ([docs/acp.md](../docs/acp.md#authority-documents-via-_metareviewmesh)); the prompt rules and rendered block are in [docs/prompts.md](../docs/prompts.md#authority--context-inputs).

## Real models

Point lanes at real model CLIs with `aimesh review setup` — install and authenticate whichever you use (Claude Code, Codex, Ollama, Antigravity via `agy`, a Devin CLI gateway). reviewmesh **detects and safely probes** configured binaries and surfaces native CLI auth/trust/model errors, but it never logs in for you — browser/device login and folder-trust stay native to each CLI.

Each reviewer call records a **model-identity** status (`verified` / `self_reported` / `unknown` / `mismatch`) and surfaces it as a caveat on the run. It is reported, never enforced: a lane's findings are used whatever its identity says, because what a reviewer *found* is what decides whether it matters. See [docs/model-identity.md](../docs/model-identity.md) and the per-adapter recipes in [docs/adapters.md](../docs/adapters.md).

For sensitive material, configure local (Ollama) models for every role to keep reviews entirely on your machine — e.g. create the example profile with `aimesh review setup --profile fully-local-ollama`. See [docs/security.md](../docs/security.md).

## Configuration

Personal defaults live in user/global config `~/.aimesh/review/config.yaml` (written by `aimesh review setup`, which defaults to `--scope user`); a project `./.aimesh/review/config.yaml` is optional, for repo-specific overrides. Resolution order: invocation override → project → user/global → shipped defaults. Full reference: **[docs/configuration.md](../docs/configuration.md)**.

Key environment variables: `AIMESH_HOME` (config/home root, used for hermetic runs), `REVIEWMESH_OLLAMA_MODEL` (model tag for the local-Ollama example profile).

## Diagnostic codes

A halt reports a plain-English reason; audit files (`call-status.json` / `halt-record.json` / `events.jsonl`) also record a stable diagnostic code. You never need to memorize them — this is the catalogue.

| Code | Meaning |
|------|---------|
| Class A | Adapter/auth/binary problem (CLI not found, non-zero exit) |
| Class B | Provider quota exhausted *(reserved)* |
| Class C | Provider capacity/overload *(reserved)* |
| Class D | Requested model unavailable *(reserved)* |
| Class E | Silent model fallback, or a proven strong-evidence model-identity mismatch |
| Class F | Transient failure or cancellation |
| Class G | Output not parseable as schema-valid JSON (after one retry) |
| M5 | Containment breach: the isolated reviewer copy was modified |
| M6 | Path confinement: a read or write was refused by the workspace scope policy (outside every allowed root, or a protected path — `.env*`, key material, `.git`, local state/agent config) |

These map to process exit codes: `0` success · `1` findings (with `--fail-on-findings`/`--ci`) · `2` usage · `3` config · `4` adapter · `5` model/identity · `6` containment · `7` policy/cap · `8` internal.

## Surfaces

- **CLI** — the commands above.
- **ACP** — `aimesh review acp` runs as an [Agent Client Protocol](https://agentclientprotocol.com/) server over stdio (JSON-RPC 2.0; newline or `Content-Length` framing), for ACP-capable IDEs. Over ACP the caller is another *process*, not a person, so the consent a CLI path argument carries is given at launch instead: **`--root <dir>` (repeatable) names the trusted workspace roots a session may review**, and every path a request supplies — workspace, session cwd, authority documents — must resolve inside them. With no `--root` the launch working directory is adopted (an IDE starts the agent in the project you opened), unless it is a degenerate root such as `/` or your home directory, which is refused; `--no-default-root` requires explicit roots. See [docs/acp.md](../docs/acp.md#trusted-roots-reviewmesh-only-what-an-acp-session-is-allowed-to-read).
- **MCP** — `aimesh review mcp` runs as a [Model Context Protocol](https://modelcontextprotocol.io/) server over stdio, so an MCP-speaking agent can drive a governed review. Trusted roots work exactly as they do over ACP and are **mandatory** here too. Two tools form a run: **`review_report`** runs the cycle and **writes nothing**, returning the adjudicated findings with their per-seat provenance, the authority inclusion manifest, the identity caveats and the requested-vs-executed roster; **`review_remediate`** applies an already-adjudicated accepted set — and it is **doubly gated**. It is not even listed unless the operator launched with `--allow-remediate` (which grants the config capability `surfaces.capabilitiesBySurface.mcp: [allowRemediate]`, the thing that raises the shipped `mcp: report` ceiling), and every call must additionally pass `allowWrite: true` — a declaration the *calling model* makes, which is auditable and host-inspectable but is **not** human consent (the enforcement a model cannot supply is the launch flag, the config capability and root confinement). Its preferred `fromRun` form binds the reviewed root by device+inode, reconciles decisions to findings by ID, re-verifies the base hashes captured at review time — before the window **and again per destination inside the commit**, which is what catches a concurrent in-place save — journals the intended hunks durably before writing (a journal that cannot be written halts the run), applies each finding all-or-nothing, and always persists a receipt derived only from a commit that succeeded. See [docs/mcp.md](../docs/mcp.md#reviewmesh-mcp).

## Documentation

- [docs/configuration.md](../docs/configuration.md) — full configuration reference
- [docs/adapters.md](../docs/adapters.md) — adapter contract, per-provider recipes, adding an adapter
- [docs/prompts.md](../docs/prompts.md) — reviewer/adjudication prompt templates and JSON result contracts
- [docs/acp.md](../docs/acp.md) — the ACP surface and host-verification procedure
- [docs/mcp.md](../docs/mcp.md) — the MCP surface: tools, the remediation double opt-in, the write window
- [docs/security.md](../docs/security.md) — data flow, containment
- [docs/architecture.md](../docs/architecture.md) — where reviewmesh sits in the monorepo
