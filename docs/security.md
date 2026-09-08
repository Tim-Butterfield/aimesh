# Security & privacy

Both `aimesh` applications send content to AI models and run external model CLIs on your machine. This page describes what leaves your machine, the guarantees meshcore enforces underneath both apps, the app-level trust model (containment), and how to report a vulnerability.

## What leaves your machine

A run sends the **content being reviewed or explored (and any needed context)** to whichever model backs each configured role, through the selected adapter. In reviewmesh, the host's own adjudication call also uses a model. Content reaches the provider behind whatever adapter you configure:

| Adapter | Where content goes |
|---|---|
| `devin-cli` | the Devin CLI gateway (routes to the underlying provider for the chosen model) |
| `claude-code` | Anthropic |
| `codex-cli` | OpenAI |
| `agy-cli` | Google (Antigravity) |
| `gemini-cli` | Google |
| `cursor-cli` | Cursor (which routes to the provider behind the chosen Cursor model slug) |
| a user-defined `acpAdapters` instance | whichever provider backs the CLI you pointed it at — an ACP instance is *your* configuration, so you choose the destination |
| `ollama` | **local** — stays on your machine; nothing is sent to a third party |

Because both apps are provider-diverse, a single run may send content to **more than one provider** — one per lane in reviewmesh, one per explorer/collator in exploremesh. The resolved role/explorer → adapter → model mapping is announced once at the start of a run, so you can see exactly where content will go before it goes.

**Ask the run itself, rather than reading this table against a roster.** `aimesh review run --dry-run` spends nothing and reports the destinations of the *resolved* panel, grouped by destination:

```
  content goes to — the reviewed files and any authority documents, in full:
    → Anthropic
      via reviewer, reviewer-2
    → OpenAI
      via reviewer-3, cross_check
    · this machine (local — nothing sent)
      via reviewer-4
```

It is computed from what the run actually resolved, so an ad-hoc `--reviewer` panel with no profile is covered too, and a fully local panel is stated as one sentence rather than a list. A lane executing in-process sends nothing and does not appear. An adapter this tool does not own — a user-defined `acpAdapters` instance — is reported as an **unknown** destination rather than omitted, and counts as leaving the machine: it points at a binary you chose, so you are the one who knows where it sends things. Each destination is declared by the adapter recipe that runs the binary (`meshcore/model/shell`), which is why this table and that output cannot drift apart.

Neither app adds telemetry or sends content anywhere other than the providers behind your configured adapters. Neither app stores your provider credentials — each adapter is a thin wrapper over a real CLI, and that CLI owns its own authentication (login, device flow, folder trust). A failed call surfaces the CLI's own auth/trust error; the apps never authenticate on your behalf.

## Fully local / offline reviews

Which model fills which role is **entirely your configuration** — nothing in reviewmesh reserves the host/adjudicator role for a cloud model. So you can run an entirely local review by pointing every role, including the host/`author_remediator`, at local (`ollama`) models. Nothing then leaves your machine. This is the recommended posture for sensitive or air-gapped material; create the example profile with `aimesh review setup --profile fully-local-ollama` and set `REVIEWMESH_OLLAMA_MODEL` to the tag you have pulled. See [configuration.md](configuration.md).

Two honest caveats on the local posture. Ollama's evidence tier is `invocation_tag` — strong enough to be `verified`, but it proves *which local tag was run*, not what that tag contains. And a fully-local panel is a **single-provider** panel: it removes the network, not the correlated-error risk that provider diversity exists to reduce.

## Containment & path safety

Confinement is five layers, each answering a different question. They are independent: none of them is load-bearing alone.

### 1. Which roots are allowed at all (`meshcore/scope`)

Every path-taking surface funnels through **one** `scope.Resolver` per run, built from the roots a **human** authorized. Where that consent comes from differs by surface, and the difference is the point:

- **CLI** — the path you type (`aimesh review run ./service`) is the consent, so it is the root.
- **ACP and MCP** — the caller is a peer *process* (or, over MCP, a *model*), so consent is given at launch instead: `aimesh review acp --root <dir>` / `aimesh review mcp --root <dir>` (repeatable), or the launch working directory **when there is evidence it is the project**. That evidence is a **project marker** — `.git`, `.hg`, `go.mod`, `package.json`, `Cargo.toml`, `pyproject.toml`, `Makefile` and friends. A cwd with no marker is **not adopted**: the server still starts, but every filesystem path is refused with `scope_no_roots_configured` until `--root <project-dir>` is given, and `--allow-inferred-root` adopts it anyway. (`inlineWorkspace` consumes no trusted root, so it works with zero roots.) The rule is **identical on every protocol revision**. It replaced an era-conditional refusal that was keyed on the wrong variable: it fired on the newest MCP revision because that revision removes the client's ability to narrow the server — but a *legacy* client that simply declines the roots capability does not narrow it either, and that case was granted the unnarrowed cwd silently. Asking whether the directory looks like the work answers both, and the marker also catches the over-grant the degenerate rule cannot see (`~/projects` is neither home nor home's parent, yet adopting it exposes every project you own). One resolver implementation backs both surfaces. An **over-broad root is refused either way** — `/`, your home directory or its parent, a system tree, a shared tree (`/Users/Shared`, `\Users\Public`) or a mount parent (`/mnt`, `/media`, `/Volumes`, `/srv`) — so `--root /` is not a way around the rule; the Windows entries are drive-letter agnostic, and every root is judged on what it **canonicalizes to**, not on how it is spelled. An operator who genuinely wants a broad explicit root must say so with `--allow-broad-root`, which is never implied and never waives the denylist. Every path in a request, including authority document paths, must resolve **inside** that set; a request can only narrow it, never widen it. Over MCP, whatever the client narrows the server with — `roots/list` on the legacy era, the `roots` **tool argument** on either — is **intersected** with the server's, never unioned. See [acp.md → Trusted roots](acp.md#trusted-roots-reviewmesh-only-what-an-acp-session-is-allowed-to-read) and [mcp.md → Trusted roots are mandatory](mcp.md#trusted-roots-are-mandatory).

With no roots the resolver fails **closed**: every path is refused, with the machine reason `scope_no_roots_configured`. "No roots" is never "unrestricted".

### 2. What is refused even inside an allowed root (the denylist)

Authorizing a project does not authorize everything in it. The denylist has **two families, and they are not equally strong.**

**Secrets — never overridable, reads *and* writes.** `.env*` (prefix-matched, so `.envrc` and `.env-local` are covered), credential directories (`.ssh`, `.gnupg`, `.aws`, `.azure`, `.gcloud`, `.kube`, `.docker`), plaintext-credential files (`.netrc`, `.npmrc`, `.pypirc`, `.pgpass`, `.git-credentials`) and private key material (`id_*`, `*.pem`, `*.key`, `*.p12`, …). A secret never reaches a prompt, and **no flag on any surface changes this.** The reason it is absolute where the other family is not: a read is what puts the bytes into a prompt, a prompt goes to a vendor, and no later intent recalls it.

**Protected configuration — writes, waivable by the operator only.** `.git/**`, this tool's own state/config (`.aimesh`, plus the legacy `.reviewmesh`/`.exploremesh` locations, which nothing writes any more but which an earlier install may have left on disk holding real configuration) and agent/IDE client configuration (`.mcp.json`, `.cursor`, `.claude`, `.codex`, `.gemini`, `.vscode`, `.windsurf`, `.idea`). These are refused because `.git/hooks/**` and `.vscode/mcp.json` execute code on the next command — that is what makes "configuration stays human-only" true at the filesystem layer rather than at the parameter layer. They are also ordinary directories people own, so **`--allow-protected-paths`** (a flag on `review run`, a *launch* flag on `review acp` / `review mcp`, never a request field) admits such a root and permits writes inside it. A caller cannot grant it to itself, which matters more here than for any other waiver: a caller that could would be arranging to run its own code on your machine later.

Every rule is matched against **every component** of a path, so a denied directory really means its whole subtree (`x/.env/secret.txt` is a secret), and each component is compared with trailing dots/spaces stripped (Windows drops those, so `.git /config` names the real `.git`). Both families apply to the root itself: a directory that is a secret path can never be made a trusted root, and a protected-configuration one can only become a root under the operator waiver above. **Neither family is reachable from a request parameter on any surface**, and the waiver widens which *names* are allowed inside a root — never which roots exist.

**What was excluded is reported.** When a review drops the client-config family from the payload, each dropped path is recorded as a **withheld caveat** (`workspace_excluded`) — surfaced on the CLI, in `--json`, on both agent surfaces, and in the `--dry-run` shape, so "no findings in `.claude/`" can be read as "it was never shown" rather than "it was clean". A directory is reported once and stands for its subtree. This matters more than it used to: those directories now hold rules, prompts and hooks that are genuinely source.

Two deliberate silences. **Routine build/artifact exclusions** (`node_modules`, `dist`, `build`, `vendor`, `coverage`, `.cache`, `tmp`, `.git`) are *not* reported — they are in every repository and nobody asks for findings in them, so reporting them every run would bury the line that matters. And **secret paths are never named in a caveat**: a caveat is reported to the caller, and over MCP that caller is a model, so a list of where this machine keeps its credentials is exactly the inventory the rule exists to withhold. The secret families are fixed and documented, so what is never shown is knowable without being handed where it lives.

### 3. How files are actually touched (`meshcore/workspace`)

Checking a path and then re-opening it by name is racy by construction — a component swapped to a symlink in between is followed, and the path that was *judged* and the object that is *read* become two different things. So every read and write of **content under review** — the isolated workspace copy, the live tree it commits back to, and the authority/context documents embedded in prompts — goes through an `os.Root` handle opened on the directory that authorized it: each component is resolved against that open root and cannot leave it, the final component is opened without following symlinks, and what is validated is the **descriptor that was opened** (an `fstat`), never the path string that named it. That makes the check and the use the same act.

The handle itself is **bound by identity**: the approved directory is stat-ed before the open, the opened root is stat-ed through its own handle, and the two must be the same file (`os.SameFile` — device+inode on unix, volume serial + file index on Windows). `os.Root` hardens traversal *inside* a boundary; it does not decide *which* boundary was opened, so without this binding an attacker who swaps the approved directory for a symlink in that window gets every subsequent operation faithfully confined to **their** tree.

*What this does not cover:* reviewmesh's own configuration and state — its config files (including the backup it takes before rewriting one) and the run artifacts it wrote itself (under `.aimesh/review/runs/`, or the OS temp directory) — are still read by path. They are the tool's own files, not content under review, and a model never names them.

- **Symlinks and other reparse points are never followed.** On Windows this specifically includes junctions and mount points, detected via the reparse-point file attribute (not `os.ModeSymlink` alone, which can miss them) — the check runs before any directory short-circuit, so a junction/mount-point directory can't slip past it.
- **Hardlinked regular files are withheld — and the omission is recorded.** An innocuous in-repo name may be a second name for `~/.ssh/id_rsa`, so a regular file with a link count above one is never copied and never reaches a prompt. It is *not* a halt: an in-tree `cp -al` tree or a dedup store is ordinary, and halting the run over one would let anything that can write inside a trusted tree stop every review at will. Instead the file is withheld and a **caveat** naming the path and the rule is carried in the run record (`run-state.json` → `withheld[]`), in the `--json` projection (`withheld[]`, alongside `identityCaveats`), on the ACP result (`_meta.reviewmesh.withheld`), and printed in the CLI summary. That is the part that makes withholding safe: a reviewer cannot object to a file it was never shown, so an unrecorded withhold would turn a containment rule into a blind spot, with the file's absence from the findings reading as approval.
- **A write never truncates in place**: the destination is unlinked first (a failed unlink halts) and re-created exclusively with no-follow, and a commit must write to exactly the canonical path the guard approved — so an intermediate symlinked directory cannot redirect it.
- **Excluded directories are never copied, diffed, committed, or accepted as a target**, matched case-insensitively, per path component, and across path separators. A name earns its place by being present on machines this tool has never seen — these are ecosystem conventions and widely deployed client tooling, never a particular user's local setup. Two families: internal/generated trees (`.git`, `tmp`, `node_modules`, `vendor`, `dist`, `build`, `coverage`, `.cache`) and agent/IDE client configuration (`.vscode`, `.idea`, `.windsurf`, `.claude`, `.cursor`, `.codex`, `.gemini`, and this tool's own `.aimesh` plus the legacy `.reviewmesh`/`.exploremesh`) — the second family is excluded from the *copy* as well as write-denied, because a file that is copied is a file that can be shown to a model, proposed as an edit, and accepted by a human reading a diff. `.DS_Store`, `.mcp.json` and `claude_desktop_config.json` are excluded file names, matched per component too. Choosing a **descendant** of a content-protected one as the workspace root is refused rather than silently stripping the protection: exclusion is judged relative to the root, so a root of `/trusted/.vscode` would otherwise make `mcp.json` an ordinary relative file.
- `..` and absolute-path edits are refused.

A containment refusal is always a **recorded fact**: exit 6, halt class `M6`, the refusing rule's own machine reason code in `halt-record.json` — never a silently shortened prompt or an empty result.

### 4. Where the model process runs (adapter cwd)

Reviewers read a disposable, isolated **copy** of the workspace, never the live tree; any write detected in that copy is a **containment-breach halt (M5)**, and the mutation evidence (`mutation-diff.patch`) is retained for forensics before the copy is discarded. Only the host writes the live workspace, only in `apply` mode, and only within the review's own scope; `report` produces findings only, and `patch` emits a diff for you to apply yourself.

Because reviewer CLIs are increasingly *agentic* — they open files themselves rather than only reading the prompt — reviewmesh also runs **every** model call (reviewer, cross-check, verifier, host adjudication, remediation) with its working directory set to that isolated copy. A CLI that resolves "the current project" from its cwd therefore lands in the copy, not in the tree that invoked reviewmesh. Where a recipe exposes a read-sandbox / allowed-directory flag, the copy is passed there too (for example `claude --add-dir`).

> **Residual risk — stated plainly.** A working directory is not a sandbox. An agentic CLI that reads an **absolute** path, follows `$HOME`, or shells out can still reach files outside the copy, and reviewmesh cannot prevent that from the outside — the CLI is your binary, running with your credentials. What holds regardless is the rest of this page: the reviewed content in the prompt is denylist-filtered, the copy is disposable and read-only to reviewers with **M5 mutation detection** on it, and every live write goes through the scope guard and the `os.Root` write path. If your threat model includes a hostile or compromised reviewer CLI, confine it with an OS-level mechanism (container, VM, sandbox profile) — and prefer a local `ollama` lane for sensitive material.

### 5. Remediation over MCP: what the double opt-in does, and does not, protect

`aimesh review mcp` is the only surface whose caller is a **model** and one of whose tools **writes**. It is gated twice: `review_remediate` is not even *listed* unless the config store grants the `mcp` surface the `allowRemediate` capability (which `aimesh review mcp --allow-remediate` sets), and every individual call must additionally pass `allowWrite: true`. Neither gate is an exception above the policy ceiling — the ceiling is computed *from* that config, so granting the capability **raises** it rather than stepping around it, and `surfaces.defaultModeBySurface.mcp: report` stays authoritative.

**`allowWrite` is not human consent — and this page will not imply that it is.** The value is supplied by the **calling model**, in the same tool call it is authorizing. It is never seen, typed, or approved by a person on its way through; the teaching error that a malformed call receives even shows the corrected call *including* `"allowWrite": true`, because the alternative — refusing to say what a well-formed call looks like — would only make the model guess. What `allowWrite` genuinely is: (a) an **auditable declaration**, recorded in the call, that this request was a write request and not a read that drifted; and (b) a **surface an argument-inspecting host can show a human** before it forwards the call — the MCP host, not reviewmesh, is where a per-call human prompt can exist. An out-of-band approval token (issued to a human, presented by the call, verified by the server) would be a real per-call consent mechanism; it is a legitimate future design and is deliberately **not** built here, because shipping a half-version of it would be worse than shipping none.

**So where does the honest enforcement live?** In three things a model cannot supply: the operator's **launch flag** (`--allow-remediate`), the **config capability** it grants (which is what raises the ceiling), and **root confinement** established before any request existed. Everything below is host-enforced in code.

**What the double opt-in actually buys.** It is a *coordination* mechanism with the host's permission layer and with the operator's intent. Not advertising a tool means a model cannot be talked into calling it and a client cannot present it to a user as available; requiring a literal per-call `allowWrite` means a write is never a default the model drifted into, and leaves a record saying so. Both are honest signals — and both are **hints**, exactly like the MCP annotations beside them.

**What it does not protect against, stated plainly.** It is *not* an authorization boundary against a confused or hostile client model. A model that has been told to write, on a server whose operator enabled writing, will pass `allowWrite: true`. What holds regardless is everything else on this page:

- **Root confinement** (§1). Every request path — the workspace and every authority document — is judged against roots a human established at **launch**, before any request existed. A request may narrow them and can never widen them; with no root, every path is refused. Whatever an MCP client narrows the server with — `roots/list` (legacy era) or the `roots` tool argument (either era) — is **intersected**, never unioned. On `2026-07-28` an *inferred* launch cwd is not a root at all without `--allow-inferred-root` (§1).
- **The denylist** (§2), enforced at the **write** layer. A remediation aimed at `~/.aimesh/**`, an app or MCP client config, `.git/config`, `.env*` or key material is a containment **halt** (exit 6, `M6`) — not a quiet skip, because a skip would let a run that tried to rewrite `.env` finish reporting success. The secret half of it holds no matter what any surface was launched with; the protected-configuration half is what `--allow-protected-paths` waives, and only an operator can pass it.
- **The `os.Root` write path** (§3): edits land in a disposable copy first, and only `apply` commits, through the same guard as every other reviewmesh write.
- **The write-path rule.** Every applied hunk must trace to evidence in the workspace copy, so a finding supported only by authority text — including authority a client model supplied inline — is reportable and **never** applyable. The same rule quarantines a finding supported only by weak-identity seats.
- **The shown-files gate, applied to every edit target.** Only files a reviewer was actually shown can be edited, and a two-phase `fromRun` remediation inherits that set from the run that produced the decisions rather than re-deriving it at write time. Each individual hunk must additionally target **its own finding's file** and a file the review **base-hashed**: scope answers *where* a write may land, never *what was reviewed*, so a model-authored hunk aimed at some other in-root file is a halt.
- **Identity binding on the reviewed root.** The decision set carries the reviewed directory's device+inode and canonical path, captured when the review ran. If the path later names a different directory — a swapped checkout, a re-pointed mount — the remediation halts. A path string is not a repository, and a substitute whose targeted files happen to hash the same would otherwise pass every content check.
- **Decisions bound by finding ID.** Findings and decisions are reconciled by ID, never by array position, and a set that does not reconcile is refused whole. A reordered or truncated record would otherwise authorize a *different* finding than the one a human read.
- **The pre-existing, inspectable decision set, re-verified twice.** The preferred `fromRun` form applies findings that were adjudicated in an earlier call and can be read by a human first. Base hashes captured then are re-verified before the write window opens **and again inside the commit, per destination, immediately before it is replaced** — the second check is what catches a concurrent in-place save, which keeps the file's inode and so passes an identity check.

Every one of those is enforced by the host, in code, on the write path — none of them depends on the calling model's cooperation.

**Recorded, always.** The write window is journal → apply → receipt. The journal is written and **fsynced before the first edit**, and a journal that cannot be made durable **halts the run** rather than proceeding unrecorded; it records each intended hunk's replacement digest plus a digest of the whole intent, so "what was this about to write" is answerable and not merely "how many bytes". A finding is **all-applied or none-applied** — its hunks are staged so a partial failure leaves nothing behind. The receipt's applied set, file list and `committed` flag are derived **only from a commit that actually succeeded**, and the receipt is persisted (durably) on every path, including a cancelled call. Durability is what makes that last case answerable at all, because a cancelled call's outcome cannot be delivered on the cancelled request: on `2025-06-18` — the legacy revision; both eras are implemented today — a server **SHOULD NOT** send a response and a client is told to ignore a late one anyway, and under `2026-07-28` over stdio it **MUST NOT** send any further message for that request at all. Under `2025-06-18` the receipt is additionally emitted as a log notification; `2026-07-28` stdio closes that channel too, which is why the durable on-disk receipt — reachable by lookup — is the guarantee and the notification is only a convenience. A receipt that cannot be persisted is a terminal failure of the run.

> **Residual risk — stated plainly.** There is **no filesystem lock**. The commit's content re-verification narrows the unprotected window from the minutes a model call takes to the few syscalls between reading a destination and replacing it; a writer inside *that* window is neither prevented nor detected. And a crash between the commit and the receipt leaves the tree written with no receipt: the run directory is made reconcilable for exactly that case — `remediation/journal.json` (what was intended) and `remediation/commit-attempt.json`, fsynced before the first live write (that a commit was entered), so an interrupted window has a signature rather than being indistinguishable from a run that did nothing.

### 6. What an operator should configure

- Run `aimesh review acp` / `aimesh review mcp` with an explicit `--root <project-dir>` (repeatable) rather than relying on the launch-cwd default, especially when a host may start the agent from an arbitrary directory. Name the **narrowest** directory that contains the work; do not reach for `--allow-broad-root` to make a refusal go away, because the refusal is telling you the root grants more than the review needs.
- Keep the ACP write-authority ceiling at its shipped `report` (`surfaces.defaultModeBySurface.acp`) unless a live workspace write over ACP is genuinely wanted.
- Leave `aimesh review mcp` **without** `--allow-remediate` unless you specifically want an agent to be able to change your files; a review server that only reports is the strictly safer default, and `review_remediate` is then not exposed at all. When you do enable it, prefer `output: "patch"` and apply the diff yourself.
- Keep run artifacts out of version control: they land under `.aimesh/` (which `aimesh init` registers in the VCS exclude file) or the OS temp directory, never a relative path inside the checkout — see [Audit-artifact sensitivity](#audit-artifact-sensitivity).
- Point `adapters.<name>.path` at a pinned, trusted binary, and treat a user-defined `acpAdapters` entry (path **and** launch args) as code you run.

### 7. Platform matrix — where each guarantee actually holds

Everything above is written in the present tense, and on unix it is verified by the standard gate. **On Windows it is not.** Windows is coded for and cross-compiles (`make windows-build`), but it is not part of `make gate`, no adapter has been exercised against a real CLI there, and no assertion on this page has been executed on a real NTFS volume. Rather than leave that as one sentence covering a dozen different claims, here is the per-guarantee answer.

Three statuses are used, and they mean different things:

- **Holds** — implemented and covered by a test that runs on that platform.
- **Branch tested only** — the platform-specific code path is exercised on *any* platform by substituting the variable that selects it (the `streamAliasing` / `reparseAttr` / `foldRootPaths` pattern), so the branch's **logic** is tested. The behaviour of the real filesystem underneath it is **not**.
- **Unknowable / weaker** — the platform cannot supply the signal the rule needs, and the code says so rather than guessing.

| Guarantee | unix (macOS / Linux) | Windows | Where |
|---|---|---|---|
| Root confinement (`scope.Resolver`), fail-closed with no roots | Holds | Holds (platform-independent logic) | `meshcore/scope` |
| Over-broad-root refusal (`/`, `~`, system, shared, mount parents) | Holds | Partly branch tested — the Windows arm is entered and asserted not to misclassify an ordinary directory, but the **drive-letter-agnostic positive match is not reachable off Windows** (see caveat 2 below) | `reviewmesh/internal/surface/acp/roots.go` |
| Case-insensitive path comparison — allowed roots, the boundary picker for an identity-bound read, the reviewed-root identity check, the commit destination check, and the MCP root intersection | Holds (folding is **off**, which is the correct unix behaviour, and that is now asserted in each place) | Branch tested only — five substitutable vars (`scope.foldPaths`, `workspace.foldPaths`, `rootfile.foldPaths`, `review.foldPaths`, `acp.windowsPaths`, plus the MCP surface's `foldRootPaths`) let the folding branch run on any platform | `meshcore/scope`, `meshcore/workspace`, `reviewmesh/internal/access/rootfile`, `reviewmesh/internal/manager/review`, `reviewmesh/internal/surface/{acp,mcp}` |
| Denylist, matched per path component — the secret family non-overridable, the protected-configuration family waivable by the operator only | Holds | Holds for the matching itself, and for the waiver's boundary (that it reaches protected config and never a secret, and never widens root confinement); **branch tested only** for the Windows-specific trailing-dot/space stripping and NTFS stream (`file.txt:$DATA`) aliasing | `meshcore/scope`, `meshcore/workspace` |
| `os.Root` traversal confinement | Holds | Expected to hold (it is the standard library's own guarantee), **not exercised here** | `meshcore/workspace` |
| Root handle bound by identity (`os.SameFile`) | Holds (device+inode) | Expected to hold (volume serial + file index), **not exercised here** | `meshcore/workspace/root.go` |
| Symlinks never followed on the final component (`O_NOFOLLOW`) | Holds | **Weaker**: `noFollowFlag` is `0` — Windows has no `O_NOFOLLOW` in `syscall`. The rule is carried instead by the reparse-point checks below | `meshcore/workspace/hardlink_{unix,other}.go` |
| Junctions / mount points / other reparse points refused | n/a (symlinks are covered by `os.ModeSymlink`) | Branch tested only — `reparseAttr` is substituted so the attribute-detection branch runs on any platform; never run against a real junction | `meshcore/workspace/fsguard.go`, `reparse_windows.go` |
| Hardlinked regular files withheld | Holds (`Nlink` from `stat`) | **Unknowable**: `linkCount` reports "not knowable" and the rule does not fire. A hardlink to a protected file outside the tree is **not** withheld on Windows | `meshcore/workspace/hardlink_{unix,other}.go` |
| Parent-directory `fsync` after a durable write | Holds | **Best-effort**: the directory `fsync` is not meaningful on Windows; a crash window is correspondingly wider | `meshcore/workspace` |
| Write path: unlink-then-create-exclusive, canonical-path check | Holds | Expected to hold, **not exercised here**; note Windows refuses to unlink an open file, so a concurrently-open destination fails the write rather than replacing it | `meshcore/workspace/workspace.go` |
| Journal → apply → receipt, receipt persisted on every path | Holds | Expected to hold (no platform-specific code), **not exercised here** | `reviewmesh/internal/manager/review` |
| MCP root intersection (startup ∩ client), fail-closed when empty | Holds | Branch tested only (case folding, as above). The **separator** half is *not* covered on unix: `path/filepath` is selected at compile time and is not substitutable | `reviewmesh/internal/surface/mcp` |

**What could not be covered, stated plainly.** Four things:

1. **No real-filesystem verification on Windows, at all.** Every "branch tested only" row proves the *decision* the code makes given a platform answer; none proves the platform gives that answer. Only running the suite on Windows can close that, and it is not run here.
2. **Anything that depends on `path/filepath` is not branch-testable.** The separator, `filepath.Clean`'s semantics and `filepath.VolumeName` are chosen by **build constraint**, not by a variable, so a `\`-separated fixture on unix is one opaque path component. Two concrete consequences: the separator half of every path rule is uncovered, and `matchSystemRoot`'s **drive-letter-agnostic positive match** — the rule whose earlier hardcoded `C:` let `D:\Windows` through — cannot be asserted off Windows at all. Substituting the *case-folding* variable covers what can be covered; this cannot be, short of a Windows build.
3. **The hardlink rule genuinely does not exist on Windows.** That is not an untested branch, it is an absent guarantee, and it is the one row in the table an operator should read as a real difference in protection rather than a coverage gap.
4. **The executable-bit readiness checks are not branch tested.** Four call sites (`meshcore/model/shell`, `meshcore/model/acpagent`, `reviewmesh/internal/manager/setup`, `exploremesh/internal/manager`) skip the `mode&0111` requirement on Windows via an inline `runtime.GOOS !=`. They are deliberately left as-is: they decide whether an adapter is *reported ready*, not what may be read or written, so they carry no containment guarantee — but they are Windows-conditional paths with no branch test, and this note is here so that is a stated fact rather than an omission.

`GOOS=windows go build ./...` is kept green by `make windows-build`, so the Windows branches at least compile on every change. That is the honest extent of it.

exploremesh has **no workspace and no patch/apply** — exploration never edits a file you own, so layers 1–3 do not apply to it; it runs every model call in a fresh isolated work directory of its own. It writes exactly two things, and neither is model-directed: its own **run artifacts** under `$EXPLOREMESH_ARTIFACT_DIR` (default: the project-local `.aimesh/explore/runs/` when a `.aimesh/` state dir exists, else the OS temp dir — never a relative path inside your checkout), and, only when a human runs `aimesh explore export --sqlite <path>`, the derived evidence database at the path that human typed. That export goes through the **same scope guard and the STRICT denylist** as every other write in the repo — strict meaning it is built with no waiver at all, so `~/.aimesh/**`, `.git/**`, `.env*`, key material and IDE/agent client config are refused **whatever flags are passed** (exit 6). `--allow-protected-paths` is a reviewmesh flag and reaches nothing here: exploremesh has no workspace to point at a protected tree. An existing destination needs `--force` (exit 7), a directory or symlink destination is refused rather than followed, and the database is published by an atomic rename so an interrupted export cannot truncate a valid one.

## Model identity (`meshcore/verify`)

Knowing which model actually answered is worth recording. A successful command exit is not proof of it, so every call is classified by an **evidence tier** — `envelope` (strongest) > `trace` > `cli_status` > `invocation_tag` > `self_report` > `none` — into a status: `verified`, `self_reported`, `unknown`, or `mismatch`.

**Identity is recorded, never enforced.** No classification — including a proven `mismatch` — drops a response, relegates it, or halts a run. Both apps apply this identically, on every surface.

This is a **deliberate limit on what this mechanism claims**, not a gap in it. An adapter can be *asked* to use a model but cannot be made to *prove* it did, and the only channels available are provider-sourced envelopes, local invocation tags, model self-reports, and — as an echo test against the real CLI demonstrated for `codex-cli`'s startup banner — plain echoes of the argument we passed in. Gating on a signal in that last category would have refused honest adapters while rubber-stamping the one adapter that told us whatever we asked to hear. Treat identity as **provenance metadata for the reader**, not as an access-control decision.

Consequently, do **not** rely on model identity as a security control. It cannot detect a compromised adapter binary, a provider-side substitution, or a malicious CLI — and it was never able to. The controls that do carry weight here are elsewhere on this page: workspace containment, the read-only reviewer sandbox, egress being a property of which adapters you configure, and the audit trail.

- A **self-report** (asking the model to name itself) is a weak fallback: a matching self-report is recorded as `self_reported`, and is **never** silently shown as `verified`.
- A registered adapter's declared identity **ceiling** caps classification, so a `self_report`/`none` adapter can never be shown as `verified` no matter what it reports. A recipe with no identity parser records **no** model rather than assuming the requested one.
- Every surface (CLI, ACP, MCP, the run manifest) carries the per-seat classification, so an unverifiable seat never reads like a verified one.

See [model-identity.md](model-identity.md) for evidence-tier detail, the echo test, and the identity-capture workflow.

## No network listener

aimesh opens no socket. There is no web workbench, no local HTTP server, and no capability token to
protect — configuration is CLI-only, and the agent surfaces (ACP, MCP) speak JSON-RPC over **stdio**
to a process the caller already launched. The only network traffic aimesh causes is whatever the
model CLIs you configured make on their own behalf.

## Audit-artifact sensitivity

Each run writes an audit trail to a run directory: findings, decisions, per-call status, an events log, and — on a halt — a halt record, plus a mutation diff on a containment breach.

**Assume the run directory holds a complete copy of everything the models were shown.** This is stronger than "may contain snippets" and it is the accurate statement: each seat's `prompt.md` embeds the reviewed content and every authority document **in full**, so a panel of five seats writes roughly five copies, and a seat's `stderr.txt` can hold the same text again. Two consequences worth planning for:

- **A file with credentials in it is now in the run directory N times.** The [non-overridable denylist](#2-what-is-refused-even-inside-an-allowed-root-the-non-overridable-denylist) keeps recognised secret files out of a prompt entirely, but it recognises *locations and filenames*, not a key pasted into ordinary source. Treat a run directory as being exactly as sensitive as the most sensitive thing reviewed.
- **Repo-wide `grep` will hit these copies.** They contain the reviewed text verbatim, including the version as it stood at review time — a search for a renamed symbol returns the old name from every prompt copy. Exclude the run directory when searching.
- **A seat's `stderr.txt` holds part of the prompt again.** Provider CLIs echo what they were given: measured across one real multi-seat run, **31% of all stderr bytes were verbatim prompt lines**, and one call's stderr was 85% its own prompt. The raw streams are kept **unedited on purpose** — `stdout.txt` and `stderr.txt` are what the process actually emitted, and an artifact trimmed on a heuristic about redundancy is exactly the one you cannot trust when something has gone wrong in a way nobody predicted. The answer to the disk cost is `aimesh clean`, which removes whole runs, rather than editing the evidence inside one.

**Where they go** (`localstate.RunDir`; an explicit override wins over both): `<project .aimesh>/<component>/runs/` when that state directory already exists — `init` creates it and registers it with the VCS exclude file, so writing there cannot dirty a checkout — otherwise `<os temp>/aimesh/<component>/runs/`, outside your tree entirely. It is never a relative path resolved against the process working directory: a run started inside a repository must not create a tree inside that repository.

They are written locally only. Neither app sends these artifacts anywhere — no telemetry, nothing beyond the providers behind your configured adapters.

**Removing them** is `aimesh clean`, which sweeps **both** locations — an uninitialized tree's artifacts accumulate in the temp fallback, which is the harder of the two to notice. With no selector it reports an inventory and removes nothing; retention is stated by you with `--keep <n>`, `--older-than <7d|168h>` or `--all`, and `--dry-run` shows exactly what would go. It only ever removes direct children of a runs directory, never the directory itself or anything beside it, and it spares a run modified in the last two minutes because a review in progress is writing into its own run directory and cannot be asked whether it has finished.

## Supply chain & external adapters

Both apps drive **external CLIs** you install and authenticate; they execute those binaries on your machine. Two areas deserve care:

- **User-defined ACP adapter instances.** An `acpAdapters` entry in `~/.aimesh/adapters.yaml` names a **binary path and launch arguments you supply**, and both apps will execute it. That is the one place ordinary configuration becomes code execution, so treat an `acpAdapters` entry like any other command you run: point it at a pinned, trusted binary, and review the `args` before saving. (There is deliberately **no** user-configurable shell-command adapter — a `generic-shell` argv-template adapter is design-only and not implemented; see [adapters.md](adapters.md#the-self-report-wrapper-shared-mechanism). Code-owned shell recipes accept a binary `path` override but never a caller-supplied argv.)
- **External / community adapters.** Install them from sources you trust; an adapter is ordinary code in your `PATH`. Neither app verifies the provenance of the binary that produced an answer — only the identity of the model that answered.

## Reporting a vulnerability

Please report security issues **privately**, not in public issues or pull requests. Use GitHub's private vulnerability reporting (the repository's Security → Report a vulnerability form). Include steps to reproduce and the affected version/commit. See [SECURITY.md](../SECURITY.md) at the repository root for the full policy.

## See also

- [configuration.md](configuration.md) — config files, precedence, environment variables
- [review.md](review.md) — commands, diagnostic codes, surfaces
- [explore.md](explore.md) — the explorer/collator model, identity policy
- [adapters.md](adapters.md) — the adapter contract and per-provider recipes
- [model-identity.md](model-identity.md) — evidence tiers and identity capture
- [architecture.md](architecture.md) — the meshcore boundary and governance guarantees
