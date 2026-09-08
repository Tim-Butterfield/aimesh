// Package cli is the standalone CLI Client (Surface). It parses arguments,
// dispatches to a Manager / doctor, renders the outcome, and maps errors to the
// documented process exit codes (meshcore/fault; the public table is in docs/architecture.md).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/rootfile"
	"github.com/Tim-Butterfield/aimesh/internal/review/app"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	runmgr "github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/setup"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/review/utility/prompt"
	"github.com/Tim-Butterfield/aimesh/internal/review/version"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/clihint"
	mdoctor "github.com/Tim-Butterfield/aimesh/meshcore/doctor"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

const usage = `aimesh review — provider-diverse, governed AI review orchestrator

Usage:
  aimesh review <command> [flags]

Commands:
  run <path>         review an artifact/workspace. REPORTS by default and
                     writes nothing; --apply is what writes.
                     flags: --report | --patch | --apply | --mode, --profile,
                     --config, --set, --fail-on-findings, --ci, --json,
                     --authority, --authority-hash, --authority-manifest
                     (--json emits the machine-readable run projection on
                     stdout — findings, dispositions, identity caveats, the
                     authority inclusion manifest, and the halt record on a
                     halt; progress goes to stderr and exit codes are
                     unchanged)
                     authority/context: --authority <path> (repeatable) names
                     a document the review is judged AGAINST — requirements,
                     a design doc, a spec. It is context, never a target: it
                     is never reviewed, patched or applied, and a finding
                     supported only by authority text is reported but never
                     applied. --authority-hash <name>=<sha256> pins content;
                     --authority-manifest <file.json> declares ranges/media
                     types/inline content.
  doctor [<path>]    static readiness checks
                     flags: --probe, --probe-deep, --fix, --interactive,
                     --profile, --json
                     --probe is free ("<binary> --version"). --probe-deep
                     SPENDS: one real, bounded model invocation per required
                     adapter, in a throwaway isolated directory — the only
                     probe that can tell you whether a CLI does real work
                     where a run actually happens. It never answers a trust
                     or login prompt; it classifies and reports the fix.
                     guided repair: doctor --fix --interactive (prompts to
                     record an adapter binary path; default --fix prints
                     guidance only)
  list               list configured adapters (name, configured?/path, shell
                     vs ACP, declared identity evidence capability) + profiles
                     with their lanes (flag: --json for a machine-readable
                     projection)
  setup              write/update config; defaults to user/global
                     (~/.aimesh); --scope project for project overrides
                     (flags: --scope, --profile)
                     interactive wizard: setup --interactive (prompts for
                     profile, adapter paths, and lane models from the catalog;
                     confirms before writing)
                     promote user→project: setup --scope project --from user
                     [--yes] [--include-adapter-paths]
                     capture an adapter binary path: setup --adapter <name>
                     --path <full/path>
                     user-defined ACP adapters (no fixed catalog):
                       setup --acp detect --path <full/path> [--acp-arg <a> ...]
                       setup --acp add    --path <full/path> [--name <key>]
                         [--title <t>] [--acp-arg <a> ...]
                       setup --acp remove --name <key>
                     (detect/add LAUNCH the real CLI to confirm its launch
                     args; add/remove honour --scope)
  init               create a local, VCS-excluded .aimesh/ state directory
                     (shared adapter locations, run artifacts). Variants:
                     init (repo if inside one, else folder), repo init
                     (require a repo), folder init (require a non-repo dir)
  acp                run as an ACP agent server (JSON-RPC 2.0 over stdio;
                     flags: --framing, --root <dir> (repeatable: the trusted
                     workspace roots a session may review; default: the launch
                     working directory), --no-default-root,
                     --allow-broad-root, --turn-timeout)
  mcp                run as an MCP server (Model Context Protocol over stdio):
                     review_report (writes nothing) plus, only with
                     --allow-remediate, review_remediate (writes; every call
                     must also pass allowWrite: true). Trusted roots are
                     MANDATORY — same --root/--no-default-root/
                     --allow-broad-root model as acp. Flags: --framing,
                     --wait-seconds, --turn-timeout

Global flags:
  -h, --help         show this help
  --version          print version ( --version --json for machine-readable )

A command is required; running "aimesh review" with no command prints this
help and exits non-zero.
Exit codes: 0 success, 1 findings (with --fail-on-findings), 2 usage error,
  3 config, 4 adapter, 5 model/identity, 6 containment, 7 policy/cap OR a
  completed apply that refused a protected-path finding, 8 internal.
`

// Run is the CLI entry point. It returns the process exit code.
func Run(args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, usage)
		return int(fault.Usage)
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(out, usage)
		return int(fault.OK)
	case "--version":
		return runVersion(args[1:], out)
	case "doctor":
		return runDoctor(args[1:], out, errw)
	case "list":
		return runList(args[1:], out, errw)
	case "review":
		return runReview(args[1:], out, errw)
	case "setup":
		return runSetup(args[1:], out, errw)
	case "config":
		return runConfig(args[1:], out, errw)
	case "acp":
		return runACP(args[1:], out, errw)
	case "mcp":
		return runMCP(args[1:], out, errw)
	case "init":
		return runInit(args[1:], localstate.InitAuto, out, errw)
	case "repo":
		if len(args) >= 2 && args[1] == "init" {
			return runInit(args[2:], localstate.InitRepo, out, errw)
		}
		fmt.Fprintln(errw, "aimesh review: usage: aimesh review repo init")
		return int(fault.Usage)
	case "folder":
		if len(args) >= 2 && args[1] == "init" {
			return runInit(args[2:], localstate.InitFolder, out, errw)
		}
		fmt.Fprintln(errw, "aimesh review: usage: aimesh review folder init")
		return int(fault.Usage)
	default:
		fmt.Fprintf(errw, "aimesh review: unknown command %q\n\n%s", args[0], usage)
		return int(fault.Usage)
	}
}

// runInit creates the local, VCS-excluded `.aimesh/` state directory (shared adapter locations, run
// artifacts) — mirroring exploremesh's `init`/`repo init`/`folder init` and aikit's init. It seeds no
// config: adapter locations are written by `aimesh review setup --adapter <name> --path`.
func runInit(args []string, mode localstate.InitMode, out, errw io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh review init")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errw, "aimesh review init:", err)
		return int(fault.Internal)
	}
	res, err := localstate.Init(cwd, mode)
	if err != nil {
		fmt.Fprintln(errw, "aimesh review init:", err)
		return int(fault.Usage)
	}
	for _, a := range res.Actions {
		fmt.Fprintf(out, "  %s\n", a)
	}
	fmt.Fprintf(out, "aimesh review: initialized %s\n", res.Home)
	return int(fault.OK)
}

func runVersion(args []string, out io.Writer) int {
	info := version.Get()
	for _, a := range args {
		if a == "--json" {
			s, err := info.JSON()
			if err != nil {
				return int(fault.Internal)
			}
			fmt.Fprintln(out, s)
			return int(fault.OK)
		}
	}
	fmt.Fprintln(out, info.String())
	return int(fault.OK)
}

// doctorDeepProbeTimeout bounds the WHOLE `--probe-deep` pass. Each adapter's deep probe is already
// individually bounded; this is the outer guarantee that `doctor` terminates even with a large panel
// of slow CLIs, because a human is waiting on it.
const doctorDeepProbeTimeout = 10 * time.Minute

func runDoctor(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh review doctor")
	probe := fs.Bool("probe", false, "run a safe `<binary> --version` probe of the selected profile's adapters (no model call, no auth)")
	// --probe-deep is a SEPARATE flag rather than a stronger --probe because --probe is documented
	// everywhere as free. A flag that costs nothing today must not start costing money because a
	// better probe was invented; the spend gets its own name, and its own warning.
	probeDeep := fs.Bool("probe-deep", false, "additionally run the DEEP probe: one REAL, bounded model invocation per required adapter in a throwaway isolated directory, through the same path a run takes. SPENDS REAL TOKENS. It answers what --version cannot — whether the CLI does real work where a run actually happens. It NEVER answers an interactive trust/login prompt; it detects, classifies and reports the fix")
	fix := fs.Bool("fix", false, "offer guided repair for issues doctor finds")
	interactive := fs.Bool("interactive", false, "with --fix: interactively apply repairs (e.g. record an adapter binary path); default prints guidance only")
	profile := fs.String("profile", "", "also report this profile's readiness (e.g. fully-local-ollama)")
	asJSON := fs.Bool("json", false, "emit the machine-readable readiness projection on stdout instead of the human report; exit semantics unchanged")
	flagArgs, positionals := splitArgs(fs, args)
	if err := fs.Parse(flagArgs); err != nil {
		return int(fault.Usage)
	}
	if *interactive && !*fix {
		fmt.Fprintln(errw, "aimesh review doctor: --interactive applies only with --fix")
		return int(fault.Usage)
	}
	// --json is a READ projection. Pairing it with --fix would mean an interactive repair
	// flow writing prose into the same stream the projection occupies; refuse instead of
	// emitting something a parser cannot read.
	if *asJSON && (*fix || *interactive) {
		fmt.Fprintln(errw, "aimesh review doctor: --json cannot be combined with --fix/--interactive (the repair flow is interactive; --json is a read-only projection)")
		return int(fault.Usage)
	}
	workspace := ""
	if len(positionals) > 0 {
		workspace = positionals[0]
	}
	a, err := app.New(app.Options{})
	if err != nil {
		fmt.Fprintln(errw, "aimesh review doctor:", err)
		return int(fault.CodeOf(err))
	}
	// The deep pass is bounded as a WHOLE, not only per adapter: a large panel of slow CLIs must still
	// terminate, and `doctor` is a diagnostic a human is waiting on.
	dctx, dcancel := context.WithTimeout(context.Background(), doctorDeepProbeTimeout)
	defer dcancel()
	rep := a.DoctorWith(workspace, *profile, app.DoctorOptions{Probe: *probe, ProbeDeep: *probeDeep, Ctx: dctx})
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(mdoctor.Project(rep)); err != nil {
			fmt.Fprintln(errw, "aimesh review doctor:", err)
			return int(fault.Internal)
		}
		// Exit semantics are unchanged by the rendering: a failing report is still exit 3.
		if !rep.OK {
			return int(fault.Config)
		}
		return int(fault.OK)
	}
	fmt.Fprint(out, rep.String())
	if *probe || *probeDeep {
		fmt.Fprintln(out, "note: --probe runs a safe `<binary> --version` check (no model call, no auth); a timeout means the CLI likely needs its own native login or folder-trust setup.")
	}
	if *probeDeep {
		fmt.Fprintln(out, "note: --probe-deep additionally ran ONE REAL model invocation per required adapter (SPENDING TOKENS) in a throwaway, git-initialized directory — the same shape a run gets. It answers the question `--version` cannot: does this CLI do real work where the run happens?")
		fmt.Fprintln(out, "      No interactive prompt was answered on your behalf. A `probe-deep:` row that names folder_trust or login_required is telling you to complete that CLI's own setup once, yourself.")
	}
	if *fix {
		// Repair writes to the user/global scope (the normal place for personal config),
		// mirroring `setup`'s default. Interactive mode supplies a Stdin Prompter; otherwise
		// guidance only (RB-17).
		base, _ := os.Getwd()
		if h, herr := config.HomeDir(); herr == nil {
			base = h
		}
		var ask review.Prompter
		if *interactive {
			ask = prompt.NewStdin(os.Stdin, out)
		}
		res := a.SetupManager(out).Repair(base, rep, ask)
		// After an interactive repair that wrote config, re-run doctor (fresh app) so the
		// user sees the post-repair state.
		if *interactive && res.ConfigWritten {
			fmt.Fprintln(out, "Re-running doctor after repair:")
			if a2, derr := app.New(app.Options{}); derr == nil {
				// The re-run deliberately does NOT repeat a deep probe: a repair that recorded a binary
				// path has no bearing on whether the CLI trusts a directory, and spending a second time
				// to say so is not a courtesy.
				rep = a2.Doctor(workspace, *profile, *probe)
				fmt.Fprint(out, rep.String())
			}
		}
	}
	if !rep.OK {
		return int(fault.Config)
	}
	return int(fault.OK)
}

func runSetup(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh review setup")
	scope := fs.String("scope", "user", "config scope to write: user (~/.aimesh/review, the normal default) | project (<repo-root>/.aimesh/review)")
	profile := fs.String("profile", "", "profile to configure (e.g. fully-local-ollama)")
	adapter := fs.String("adapter", "", "record a binary path for this adapter (e.g. claude-code); use with --path")
	path := fs.String("path", "", "full path to the adapter binary (use with --adapter)")
	interactive := fs.Bool("interactive", false, "run the interactive setup wizard (prompts for profile + adapter paths; confirms before writing)")
	from := fs.String("from", "", "promote config from this scope into --scope project (only supported value: user)")
	yes := fs.Bool("yes", false, "with --from: confirm the promotion non-interactively")
	includePaths := fs.Bool("include-adapter-paths", false, "with --from: also promote machine-specific adapter binary paths (skipped by default)")
	// The user-defined ACP adapter surface. It had NO CLI caller before — the detect/save/remove seams
	// were reachable only from the web UI, so a headless install could not configure an ACP adapter at all.
	acpAction := fs.String("acp", "", "user-defined ACP adapter action: detect (probe only) | add | remove")
	acpName := fs.String("name", "", "with --acp add: the instance key (default: derived from the binary); with --acp remove: the key to remove")
	acpTitle := fs.String("title", "", "with --acp add: the display title (default \"ACP: <binary>\")")
	var acpArgs acpArgFlags
	fs.Var(&acpArgs, "acp-arg", "with --acp detect/add: one launch argument to try (repeatable; default --acp)")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	if *interactive && (*adapter != "" || *path != "" || *profile != "") {
		fmt.Fprintln(errw, "aimesh review setup: --interactive cannot be combined with --adapter/--path/--profile")
		return int(fault.Usage)
	}
	// ACP guards: the action is its own surface, so it composes with --scope only.
	if *acpAction != "" {
		if *interactive || *from != "" || *adapter != "" || *profile != "" {
			fmt.Fprintln(errw, "aimesh review setup: --acp cannot be combined with --interactive/--from/--adapter/--profile")
			return int(fault.Usage)
		}
		switch *acpAction {
		case "detect", "add":
			if *path == "" {
				fmt.Fprintf(errw, "aimesh review setup: --acp %s requires --path (the ACP-capable CLI binary to launch)\n", *acpAction)
				return int(fault.Usage)
			}
		case "remove":
			if *acpName == "" {
				fmt.Fprintln(errw, "aimesh review setup: --acp remove requires --name (the saved instance key; `list` names them)")
				return int(fault.Usage)
			}
		default:
			fmt.Fprintf(errw, "aimesh review setup: unknown --acp action %q (want detect, add or remove)\n", *acpAction)
			return int(fault.Usage)
		}
	} else if len(acpArgs) > 0 || *acpName != "" || *acpTitle != "" {
		fmt.Fprintln(errw, "aimesh review setup: --name/--title/--acp-arg apply only with --acp")
		return int(fault.Usage)
	}
	// Promotion (`--from`) guards.
	if *from != "" {
		if *from != "user" {
			fmt.Fprintln(errw, "aimesh review setup: --from only supports \"user\" (promotes user/global → project)")
			return int(fault.Usage)
		}
		if *scope != "project" {
			fmt.Fprintln(errw, "aimesh review setup: --from user requires --scope project")
			return int(fault.Usage)
		}
		if *adapter != "" || *path != "" || *profile != "" {
			fmt.Fprintln(errw, "aimesh review setup: --from cannot be combined with --adapter/--path/--profile")
			return int(fault.Usage)
		}
	} else if *yes || *includePaths {
		fmt.Fprintln(errw, "aimesh review setup: --yes/--include-adapter-paths apply only with --from")
		return int(fault.Usage)
	}
	if *scope != "user" && *scope != "project" {
		fmt.Fprintln(errw, "aimesh review setup: --scope must be user or project")
		return int(fault.Usage)
	}
	// usage guards for the path-capture surface (--acp detect/add supply their own --path)
	if *path != "" && *adapter == "" && *acpAction == "" {
		fmt.Fprintln(errw, "aimesh review setup: --path requires --adapter (e.g. aimesh review setup --adapter claude-code --path /full/path/to/claude)")
		return int(fault.Usage)
	}
	if *adapter != "" && *path == "" {
		fmt.Fprintln(errw, "aimesh review setup: --adapter requires --path (e.g. aimesh review setup --adapter "+*adapter+" --path /full/path/to/binary)")
		return int(fault.Usage)
	}

	a, err := app.New(app.Options{})
	if err != nil {
		fmt.Fprintln(errw, "aimesh review setup:", err)
		return int(fault.CodeOf(err))
	}
	// scope → base dir: user = ~/.aimesh/review (the normal place for personal defaults),
	// project = <repo-root>/.aimesh/review (optional, override-only). ProjectConfigPath(base)
	// yields <base>/.aimesh/review/config.yaml for both.
	base, _ := os.Getwd()
	if *scope == "user" {
		h, herr := config.HomeDir()
		if herr != nil {
			fmt.Fprintln(errw, "aimesh review setup: cannot resolve home dir for --scope user:", herr)
			return int(fault.Config)
		}
		base = h
	}
	mgr := a.SetupManager(out)
	// Adapter binary paths persist to the scope's shared .aimesh/adapters.yaml (scope-anchored: user →
	// AIMESH_HOME, project → repo root). The config.yaml (profiles/lanes) write target still derives
	// from baseDir; WriteScope governs only the shared path target, and the two agree by construction.
	mgr.WriteScope = *scope
	// The ACP flow renders its own probe/save/remove outcome (and detect writes nothing at all), so it
	// returns directly rather than folding into the shared write-and-report switch below.
	if *acpAction != "" {
		return runSetupACP(mgr, *acpAction, *acpName, *acpTitle, *path, acpArgs, out, errw)
	}
	var serr error
	var res setup.Result
	switch {
	case *from != "":
		// user→project promotion. Interactive supplies a confirm prompt; otherwise --yes is required.
		var ask review.Prompter
		if *interactive {
			ask = prompt.NewStdin(os.Stdin, out)
		}
		res, serr = mgr.PromoteUserToProject(base, *includePaths, *yes, ask)
	case *interactive:
		// Interactive wizard: the CLI is the interactive surface, so supply a Stdin Prompter.
		res, serr = mgr.SetupWizard(base, prompt.NewStdin(os.Stdin, out))
	case *adapter != "":
		_, serr = mgr.SetAdapterPath(base, *adapter, *path)
	case *profile == "fully-local-ollama":
		_, serr = mgr.SetupLocalOllama(base, os.Getenv("REVIEWMESH_OLLAMA_MODEL"), nil)
	default:
		// non-interactive default: writes config idempotently (never clobbers an existing one)
		_, serr = mgr.Setup(base, nil)
	}
	if serr != nil {
		fmt.Fprintln(errw, "aimesh review setup: "+serr.Error()+diagSuffix(serr))
		return int(fault.CodeOf(serr))
	}
	// After an interactive write, re-run the static doctor checks so the user sees the
	// result. Use a FRESH app so doctor loads the just-written config (the original `a`
	// snapshotted config at construction, before the wizard wrote).
	if *interactive && res.ConfigWritten {
		fmt.Fprintln(out, "Re-running doctor to verify the new configuration:")
		if a2, derr := app.New(app.Options{}); derr == nil {
			fmt.Fprint(out, a2.Doctor("", "", false).String())
		}
	}
	return int(fault.OK)
}

// acpArgFlags collects a repeatable --acp-arg (order preserved). Launch args are passed one flag at a
// time rather than comma-split: an argument may legitimately contain a comma, and a mangled launch line
// fails looking like a broken CLI rather than a broken flag.
type acpArgFlags []string

func (a *acpArgFlags) String() string     { return strings.Join(*a, " ") }
func (a *acpArgFlags) Set(v string) error { *a = append(*a, v); return nil }

// acpProbeTimeout bounds `setup --acp detect|add`: both launch the REAL candidate CLI (each attempt is
// itself watchdog-bounded inside acpagent), so this caps total wall time. It matches the web UI's budget
// for the same two operations, so the two surfaces behave identically on a slow-starting CLI.
const acpProbeTimeout = 90 * time.Second

// runSetupACP drives the user-defined generic ACP adapter flow over the SetupManager's existing seams —
// the CLI half that was missing: `detect` probes a candidate binary read-only, `add` validates then
// persists the instance to the scope's shared adapters.yaml, and `remove` deletes it (blocked while a
// profile lane still references it). There is no fixed ACP catalog: any ACP-capable CLI can be named,
// which is exactly why detect/add have to launch it to confirm the launch args.
func runSetupACP(mgr *setup.Manager, action, name, title, path string, args []string, out, errw io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), acpProbeTimeout)
	defer cancel()

	switch action {
	case "detect":
		res, err := mgr.DetectACPAdapter(ctx, path, args)
		if err != nil {
			fmt.Fprintln(errw, "aimesh review setup:", err)
			return int(fault.CodeOf(err))
		}
		fmt.Fprintf(out, "ACP probe: %s\n", path)
		if !res.OK {
			// Reported as a failure so a script can branch on it, WITHOUT declaring the CLI unusable:
			// mid-login and folder-trust are the common causes and both are fixed in that CLI, not here.
			fmt.Fprintf(out, "  handshake: FAILED — %s\n", res.Detail)
			fmt.Fprintln(out, "  next step: complete the CLI's own login / folder-trust setup, then re-run detect (or `setup --acp add` anyway and re-save later).")
			return int(fault.Adapter)
		}
		fmt.Fprintln(out, "  handshake: OK")
		fmt.Fprintf(out, "  launch args: %s\n", strings.Join(res.Args, " "))
		if res.Model != "" {
			fmt.Fprintf(out, "  reported model: %s\n", res.Model)
		}
		if res.SuggestedTitle != "" {
			fmt.Fprintf(out, "  suggested title: %s\n", res.SuggestedTitle)
		}
		fmt.Fprintf(out, "  save it: reviewmesh setup --acp add --path %s\n", path)
		return int(fault.OK)

	case "add":
		// A handshake failure does NOT block the save (the manager warns instead), so a CLI that is
		// mid-login can still be recorded and re-saved later to capture its model.
		res, err := mgr.SaveACPAdapter(ctx, name, title, path, args)
		if err != nil {
			fmt.Fprintln(errw, "aimesh review setup: "+err.Error()+diagSuffix(err))
			return int(fault.CodeOf(err))
		}
		for _, m := range res.Messages {
			fmt.Fprintln(out, m)
		}
		return int(fault.OK)

	default: // "remove" (validated by the caller)
		res, err := mgr.RemoveACPAdapter(name)
		if err != nil {
			fmt.Fprintln(errw, "aimesh review setup: "+err.Error()+diagSuffix(err))
			return int(fault.CodeOf(err))
		}
		if res.Blocked {
			// Still referenced by a profile lane (or simply not a saved ACP adapter): the manager reports
			// this as a refusal, not an error, and the lanes are listed so the user knows what to change.
			fmt.Fprintln(errw, "aimesh review setup: "+res.Message)
			for _, u := range res.UsedBy {
				fmt.Fprintf(errw, "  in use by: %s / %s lane\n", u.ProfileDisplay, u.Role)
			}
			return int(fault.Config)
		}
		fmt.Fprintln(out, res.Message)
		return int(fault.OK)
	}
}

// runConfig hosts governed local-config maintenance. Today: `config clean-model-keys` renames
// generated model-catalog keys off their adapter-key prefix (and repoints the lanes that use them)
// through the same SetupManager→SetupEngine→ConfigAccess path the web UI uses. Preview by default;
// `--apply` writes after taking a timestamped backup.
func runConfig(args []string, out, errw io.Writer) int {
	if len(args) == 0 || args[0] != "clean-model-keys" {
		fmt.Fprintln(errw, "aimesh review config: known subcommand is `clean-model-keys` (preview; --apply to write)")
		return int(fault.Usage)
	}
	fs := flag.NewFlagSet("config clean-model-keys", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh review config clean-model-keys")
	apply := fs.Bool("apply", false, "apply the rename plan (default: preview only). A timestamped backup is taken first.")
	if err := fs.Parse(args[1:]); err != nil {
		return int(fault.Usage)
	}
	a, err := app.New(app.Options{})
	if err != nil {
		fmt.Fprintln(errw, "aimesh review config:", err)
		return int(fault.CodeOf(err))
	}
	mgr := a.SetupManager(out)
	plan := mgr.PlanModelKeyCleanup()
	if len(plan.Renames) == 0 {
		fmt.Fprintln(out, "No generated model keys with adapter-key prefixes to clean.")
		for _, s := range plan.Skipped {
			fmt.Fprintf(out, "  skipped: %s — %s\n", s.Key, s.Reason)
		}
		return int(fault.OK)
	}
	fmt.Fprintf(out, "Planned model-key renames (%d):\n", len(plan.Renames))
	for _, r := range plan.Renames {
		fmt.Fprintf(out, "  %s → %s   [%s]  used by: %s\n", r.OldKey, r.NewKey, r.Label, strings.Join(r.UsedBy, ", "))
	}
	for _, s := range plan.Skipped {
		fmt.Fprintf(out, "  skipped: %s — %s\n", s.Key, s.Reason)
	}
	if !*apply {
		fmt.Fprintln(out, "\nPreview only — re-run with --apply to write (a timestamped backup is taken first).")
		return int(fault.OK)
	}
	res, err := mgr.ApplyModelKeyCleanup(setup.ModelKeyCleanupConfirm())
	if err != nil {
		fmt.Fprintln(errw, "aimesh review config:", err)
		return int(fault.CodeOf(err))
	}
	for _, msg := range res.Messages {
		fmt.Fprintln(out, msg)
	}
	return int(fault.OK)
}

func runReview(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh review run <path>")
	report := fs.Bool("report", false, "report mode: findings only, nothing is written. THIS IS THE DEFAULT — a run that names no mode reports.")
	patch := fs.Bool("patch", false, "patch mode: emit a diff of the accepted findings and write nothing to your tree")
	apply := fs.Bool("apply", false, "apply mode: WRITE the accepted findings into your working tree. Never the default — a run only writes when you say this. `<run>/patches/changes.patch` is a complete reverse-appliable delta of what was written, so `git apply -R` undoes exactly this run's edits.")
	mode := fs.String("mode", "", "report|patch|apply (default report)")
	profile := fs.String("profile", "", "runtime profile")
	cfgPath := fs.String("config", "", "explicit config file (JSON)")
	failOn := fs.Bool("fail-on-findings", false, "exit 1 if the review ends with findings (CI gating)")
	ci := fs.Bool("ci", false, "non-interactive CI mode: ci surface (report), gate exit 1 on findings, no prompts/writes")
	maxParallel := fs.Int("max-parallel", 0, "how many reviewer seats may invoke their model CLI at once (default: the whole panel in parallel). Lower it when this machine cannot host that many provider CLIs at once — each is a real subprocess, and a local model also loads weights — or to stay under a provider rate limit. It bounds PARALLELISM only: every seat still reviews, so only the wall clock changes")
	verifyReadiness := fs.Bool("verify-readiness", false, "before dispatching anything, ask EVERY configured agent whether it can do real work: one bounded, one-token invocation per distinct adapter/model in a throwaway directory, all at once. It catches what `--version` cannot — a CLI that is installed but not logged in, blocked on folder trust, or handed a model the account cannot use — so a panel does not pay for its first seat's whole prompt and then halt on its second. It SPENDS one call per agent, which is why it is opt-in; `--dry-run` prices those calls and performs none of them. It cannot preflight quota: a window can empty between the probe and the call")
	dryRun := fs.Bool("dry-run", false, "resolve everything, spend nothing: print the panel this run would convene, the lanes it would call, and the model-call range it is bounded by, then stop before the first model call. Every configuration error a real run would hit — an unresolvable seat, a duplicate panel identity, a missing binary, an oversized authority document — is reported here for free. Combine with the mode you mean (`--apply --dry-run` prices an apply run); the dry run itself never writes")
	includeHostReview := fs.Bool("include-host-review", false, "report-only: add a host self-review pass (author_self_review findings); invalid with patch/apply")
	var verifyCmds setFlags
	fs.Var(&verifyCmds, "verify-cmd", "your project's own build/test `command`, run on the CONTAINMENT COPY and RECORDED (repeatable, max "+strconv.Itoa(runmgr.MaxVerifyCommands)+"). Nothing model-authored is ever executed: these are commands you already run, in a copy of your own tree. On patch/apply they run TWICE — before any edit and after every edit — and the result is a `delta`. The delta is the signal, never absolute green: a repository with a red suite is an ordinary one, and what matters is whether your commands answered differently afterwards. IT GATES NOTHING — a failure does not invalidate a finding, does not stop the commit and does not change the exit code (the second pass runs after the commit, so it structurally cannot).")
	verifyTimeout := fs.Duration("verify-timeout", runmgr.DefaultVerifyTimeout, "per-command wall-clock budget for --verify-cmd. A command that hits it is recorded as timed out, which makes the two passes NOT COMPARABLE rather than red — a wall clock is not a defect in your code.")
	// SCOPE. Three baselines, and the two that work in ANY directory come first deliberately: not
	// all usage is repo-focused, and `folder init` is a peer of `repo init` rather than a fallback.
	var scopePaths setFlags
	fs.Var(&scopePaths, "path", "review only this workspace-relative `path` or glob (repeatable). A directory selects everything under it. Works in any directory. A selector matching NO file is refused rather than falling back to the whole tree — that fallback would review everything at full cost while you believed you had narrowed it.")
	changedSince := fs.String("changed-since", "", "review only files modified within this `window` — a duration (\"2h\", \"90m\") or an RFC3339 timestamp. Works in ANY directory, including one under no version control, which is what makes \"what did I just change\" answerable outside a repository. Coarser than a diff (a touched-but-unchanged file is selected) and coarse in the safe direction.")
	vcsRef := fs.String("changed-vs", "", "review only files changed against a version-control `baseline`: diff (working tree vs HEAD), staged, or any ref/commit. REQUIRES a repository — in a plain directory this is refused rather than ignored, and the refusal points you at --path and --changed-since, which work everywhere.")
	allowProtectedRun := fs.Bool("allow-protected-paths", false, "review and write inside a tree that is, or sits under, a protected configuration path — .git, .claude, .cursor, .codex, .gemini, .vscode, .idea, .windsurf. Refused by default because .git/hooks/** and .vscode/mcp.json execute code on the next command, so an edit there escapes the review that proposed it. Pass this when the tree IS the thing you want reviewed (your own hooks, your own agent config). It does NOT unlock secrets: .env*, .ssh, .aws, .netrc and key material stay refused for reading as well as writing, because a read puts them in a prompt and a prompt reaches a vendor. `.aimesh` is NOT on this list — its run artifacts are readable without any flag.")
	verifyBaseline := fs.Bool("verify-baseline", false, "run --verify-cmd on a report run too. Off by default: a report writes nothing, so there is no 'after' and you pay the full cost of your suite for a single baseline fact. On patch/apply the commands run without this flag, because there a comparison exists.")
	asJSON := fs.Bool("json", false, "emit the machine-readable run projection on stdout (findings + dispositions + identity caveats + halt record); progress/hints go to stderr; exit codes are unchanged")
	debug := fs.Bool("debug", false, "print a per-adapter-call diagnostic (resolved argv, exit code, identity, captured stderr/stdout) to stderr — for troubleshooting an adapter/provider")
	var sets setFlags
	fs.Var(&sets, "set", "per-lane override: role.adapter=NAME or role.model=NAME (repeatable)")
	var reviewerSpecs setFlags
	fs.Var(&reviewerSpecs, "reviewer", "ad-hoc BLIND REVIEWER PANEL seat as adapter=<name>,model=<m>[,effort=<e>] (repeatable, order preserved; 1.."+fmt.Sprint(review.MaxReviewerSeats)+" seats). Every seat reviews blind and in parallel; agreement counts are computed by the host. Mutually exclusive with --profile: a panel is composed OR selected, never half of each. The ADAPTER must be one your configuration defines; the MODEL may be a modelCatalog key (`list` shows them) or any string your adapter accepts, which is passed through verbatim and reported as such.")
	var selectFPs setFlags
	// The backquoted word is what Go's flag package prints as the value name, so it has to come
	// first and has to be the thing a user types. Backquoting an example command instead makes the
	// help render as `-select --report --json`.
	fs.Var(&selectFPs, "select", "SELECTIVE APPLY: write only the accepted finding whose HOST-COMPUTED `fingerprint` is given (repeatable). Take the value from a prior --report --json run's findings[].fingerprint — NEVER a finding's id, which is model-authored and renumbered, so keying a write set on one would let the model steer which finding you selected. Omitted applies everything the run's own adjudication authorizes. A fingerprint that names no finding in THIS run is dropped and reported; if none matches, the run is refused rather than writing nothing silently. Valid only with --patch/--apply.")
	var authPaths, authHashes setFlags
	fs.Var(&authPaths, "authority", "authority/context document the review is judged AGAINST (requirements, design doc, spec) — repeatable. It is context, never a target: never reviewed, never patched, never applied. The document is read root-scoped (a .env or key file is refused) and embedded in full, or the run fails closed — it is never silently truncated. To declare PART of one, append a section (`spec.md#The Relevant Part`) or an explicit byte range (`spec.md:1012-3033`, start inclusive, end exclusive); either resolves to the same recorded `ranges` declaration the manifest takes, so the inclusion manifest states exactly which bytes were embedded.")
	fs.Var(&authHashes, "authority-hash", "pin an --authority document's content: <name>=<sha256> (name defaults to the file's base name; repeatable). A mismatch HALTS, so a document cannot change between a report run and the apply run that acts on it.")
	authManifest := fs.String("authority-manifest", "", "read the full authority declaration from a JSON file (array of {name,path|content,mediaType,expectedHash,completeness,ranges}) — the way to declare byte ranges, media types, or inline content. Cannot be combined with --authority/--authority-hash.")
	// support both `review --report PATH` and `review PATH --report` by separating
	// flag tokens (and their values) from positionals before parsing.
	flagArgs, positionals := splitArgs(fs, args)
	if err := fs.Parse(flagArgs); err != nil {
		return int(fault.Usage)
	}
	path := ""
	if len(positionals) > 0 {
		path = positionals[0]
	}
	if path == "" {
		fmt.Fprintln(errw, "aimesh review: a <path> is required")
		return int(fault.Usage)
	}
	m, err := resolveMode(*mode, *report, *patch, *apply)
	if err != nil {
		fmt.Fprintln(errw, "aimesh review:", err)
		return int(fault.Usage)
	}
	// --include-host-review is report-only. Reject it early whenever an apply/patch mode was
	// EXPLICITLY requested — independent of --ci (so `--ci --apply --include-host-review` is
	// still a usage error, not silently forced to report). A bare flag (no explicit mode) is
	// left to the manager's authoritative guard on the EFFECTIVE mode. CI (no explicit mode)
	// composes fine. The manager re-checks before any model call regardless.
	if *includeHostReview && (m == review.ModePatch || m == review.ModeApply) {
		fmt.Fprintln(errw, "aimesh review: --include-host-review is only valid in report mode")
		return int(fault.Usage)
	}
	// A DRY RUN REVIEWS NOTHING, so it finds nothing, so a findings gate over one always passes.
	// Accepting the combination would turn `--ci --dry-run` into a green CI job that priced a
	// review instead of running it — the single most dangerous way this flag could be misread.
	// --ci sets --fail-on-findings itself, so naming either one is caught here.
	if *dryRun && (*failOn || *ci) {
		fmt.Fprintln(errw, "aimesh review: --dry-run cannot be combined with --fail-on-findings or --ci — a dry run reviews nothing, so a gate over its (always empty) findings would report success for a review that never happened")
		return int(fault.Usage)
	}
	// A ledger path with the feature off names a file nothing would read or write. Accepting it
	// would be an inert knob that looks configured — the same failure `--select` on a report run is
	// refused for.
	// The ceiling is checked HERE, before any spend, so an over-long command list is a usage error at
	// the flag rather than a surprise after a panel has been paid for.
	verifyCommands, vcerr := runmgr.ValidateVerifyCommands(verifyCmds)
	if vcerr != nil {
		fmt.Fprintln(errw, "aimesh review:", vcerr)
		return int(fault.Usage)
	}
	// The same inert-knob rule the ledger flag follows: a baseline opt-in with nothing to run, or a
	// timeout bounding nothing, would look configured and do nothing.
	if len(verifyCommands) == 0 && (*verifyBaseline || *verifyTimeout != runmgr.DefaultVerifyTimeout) {
		fmt.Fprintln(errw, "aimesh review: --verify-baseline / --verify-timeout need at least one --verify-cmd — without a command there is nothing to run, time out, or take a baseline of")
		return int(fault.Usage)
	}
	// SELECTIVE APPLY is a filter over what gets WRITTEN, so it is refused on a mode that writes
	// nothing rather than accepted and silently inert. It is also refused when every entry is
	// blank: an empty narrowing filter names zero findings and must never fall back to "everything".
	selection, serr := parseSelection(selectFPs, m)
	if serr != nil {
		fmt.Fprintln(errw, "aimesh review:", serr)
		return int(fault.Usage)
	}
	adapterOv, modelOv, perr := parseSets(sets)
	if perr != nil {
		fmt.Fprintln(errw, "aimesh review:", perr)
		return int(fault.Usage)
	}
	// AD-HOC PANEL vs PROFILE: composing a panel and selecting a profile that carries one are
	// two ways to answer the same question, so naming both is a usage error rather than a silent
	// precedence rule the user has to know (the same rule exploremesh applies to
	// --explorer vs --roster/--profile).
	reviewerPanel, rperr := parseReviewerPanel(reviewerSpecs)
	if rperr != nil {
		fmt.Fprintln(errw, "aimesh review:", rperr)
		return int(fault.Usage)
	}
	if len(reviewerPanel) > 0 {
		if *profile != "" {
			fmt.Fprintln(errw, "aimesh review: --reviewer (ad-hoc panel) cannot be combined with --profile — compose a panel or select one, not both")
			return int(fault.Usage)
		}
		if adapterOv[review.RoleReviewer] != "" || modelOv[review.RoleReviewer] != "" {
			fmt.Fprintln(errw, "aimesh review: --set reviewer.* cannot be combined with --reviewer — a per-ROLE override cannot address one of N seats; put the value in the seat spec instead")
			return int(fault.Usage)
		}
	}
	// Authority documents. The flag form covers the common case (whole documents, optionally
	// hash-pinned); the manifest file is the escape hatch for the parts a flag cannot express
	// (byte ranges, media types, inline content). They are mutually exclusive so there is
	// never a question of which one won.
	authDocs, aerr := parseAuthority(authPaths, authHashes, *authManifest)
	if aerr != nil {
		fmt.Fprintln(errw, "aimesh review:", aerr)
		return int(fault.Usage)
	}

	surface := "cli"
	if *ci {
		// CI is report-only and gated, regardless of flags or config: force report at
		// the CLI boundary (so a project config can't widen `ci` to apply) and gate.
		surface = "ci"
		m = review.ModeReport
		*failOn = true
	}

	// The human-given path is the CONSENT boundary for this run: the CLI passing it is
	// what makes a filesystem root allowed at all. Validating it here (through the same
	// resolver the manager confines writes with) turns a bad path into an immediate,
	// machine-coded refusal instead of a failure discovered mid-run.
	if code, cerr := checkWorkspacePath(path); cerr != nil {
		fmt.Fprintln(errw, "aimesh review: "+cerr.Error())
		return code
	}
	// Structural authority checks (no I/O) run here so a malformed declaration — or inline
	// content in a write-capable mode — is refused before any config load or spend. The
	// Manager re-checks on the EFFECTIVE mode; this is the fast, surface-local echo of it.
	if verr := authority.Validate(authDocs, m); verr != nil {
		fmt.Fprintln(errw, "aimesh review: "+verr.Error()+diagSuffix(verr))
		return int(fault.CodeOf(verr))
	}

	a, err := app.New(app.Options{ConfigPath: *cfgPath})
	if err != nil {
		fmt.Fprintln(errw, "aimesh review:", err)
		return int(fault.CodeOf(err))
	}
	ctx := context.Background()
	if *debug {
		ctx = shell.WithDebug(ctx, errw)
	}
	outcome, runErr := a.Manager().RunContext(ctx, runmgr.Request{
		Workspace: path, Mode: m, Surface: surface, Profile: *profile,
		AdapterOverride: adapterOv, ModelOverride: modelOv,
		ReviewerPanel:     reviewerPanel,
		IncludeHostReview: *includeHostReview,
		Authority:         authDocs,
		VerifyCommands:    verifyCommands,
		VerifyTimeout:     *verifyTimeout,
		VerifyBaseline:    *verifyBaseline,
		// THE OPERATOR WAIVERS. The human typing these IS the consent, exactly as the human-typed
		// workspace path is: they are choosing, on their own machine and for this run, to widen what
		// containment admits or what a blind reviewer's prompt carries. That is why each of them is
		// a flag here and a LAUNCH flag on acp/mcp, and never a request field on any surface.
		AllowProtectedPaths: *allowProtectedRun,
		Scope: runmgr.Scope{
			Paths: scopePaths, ChangedSince: strings.TrimSpace(*changedSince), VCSRef: strings.TrimSpace(*vcsRef),
		},
		MaxParallel:     *maxParallel,
		DryRun:          *dryRun,
		VerifyReadiness: *verifyReadiness,
		// The narrowing selection travels UNEXAMINED into the one governed write path (D8-A). The
		// CLI resolves no fingerprint of its own, so `--select` cannot come to mean something here
		// that `select` does not mean over MCP or ACP.
		Select: selection,
	})
	// With --json, stdout carries the projection and NOTHING else: progress, hints and
	// the error line stay on stderr so the stream stays parseable by a caller that only
	// reads stdout. Exit codes are identical either way — the flag changes the rendering,
	// never the outcome.
	progress := out
	if *asJSON {
		progress = errw
	}
	if outcome.RunDir != "" {
		fmt.Fprintf(progress, "run: %s\n", outcome.RunDir)
	}
	if runErr != nil {
		fmt.Fprintln(errw, "aimesh review: "+runErr.Error()+diagSuffix(runErr))
		printFailureHint(errw, outcome.Failure)
		// ...and every OTHER seat that failed. The line above names the seat that decided the halt;
		// this names the ones the caller also paid for and would otherwise have to dig out of the
		// run directory.
		printSeatFailures(errw, outcome.Panel)
		if *asJSON {
			// A halt still emits the projection (with the halt record populated), so an
			// automated caller receives the taxonomy rather than a bare exit code.
			if werr := writeRunJSON(out, outcome, m, runErr); werr != nil {
				fmt.Fprintln(errw, "aimesh review:", werr)
				return int(fault.Internal)
			}
		}
		return int(fault.CodeOf(runErr))
	}
	// A DRY RUN's entire output is the shape. It returns here rather than falling through so it
	// can never print a findings/dispositions summary of a review that did not happen — the
	// counts would all be zero and every one of those zeros would be a lie by omission.
	if outcome.Shape != nil {
		if *asJSON {
			if werr := writeRunJSON(out, outcome, m, nil); werr != nil {
				fmt.Fprintln(errw, "aimesh review:", werr)
				return int(fault.Internal)
			}
			return int(fault.OK)
		}
		printShape(out, *outcome.Shape)
		return int(fault.OK)
	}
	if *asJSON {
		if werr := writeRunJSON(out, outcome, m, nil); werr != nil {
			fmt.Fprintln(errw, "aimesh review:", werr)
			return int(fault.Internal)
		}
		if s, ok := outcome.Scope.(*runmgr.ScopeSummary); ok {
			printScope(errw, s)
		}
		if p, ok := outcome.PartialPanel.(*runmgr.PartialPanel); ok {
			printPartialPanel(errw, p)
		}
		printIdentityCaveats(errw, outcome.IdentityCaveats)
		printWithheld(errw, outcome.Withheld)
		printGrounding(errw, outcome.Grounding)
		printVerification(errw, outcome.Verification)
		printComposition(errw, outcome.Composition)
		printDissent(errw, outcome.Dissent)
		printSelection(errw, outcome.Selection)
		printRefusals(errw, outcome.Refusals)
		if code := refusalCode(outcome.Refusals); code != int(fault.OK) {
			return code
		}
		return gatingCode(*failOn, len(outcome.Findings))
	}
	fmt.Fprintf(out, "review complete: mode=%s, findings=%d, status=%s\n", outcome.Mode, len(outcome.Findings), outcome.Status)
	if p, ok := outcome.PartialPanel.(*runmgr.PartialPanel); ok {
		printPartialPanel(out, p)
	}
	printIdentityCaveats(out, outcome.IdentityCaveats)
	printWithheld(out, outcome.Withheld)
	printGrounding(out, outcome.Grounding)
	printVerification(out, outcome.Verification)
	printComposition(out, outcome.Composition)
	printDissent(out, outcome.Dissent)
	printSelection(out, outcome.Selection)
	printRefusals(out, outcome.Refusals)
	if code := refusalCode(outcome.Refusals); code != int(fault.OK) {
		return code
	}
	return gatingCode(*failOn, len(outcome.Findings))
}

// parseSelection validates `--select` at the flag boundary (D8-A).
//
// Two rules, both fail-closed, both here rather than mid-run so a caller pays nothing to learn them:
//
//  1. A selection is meaningful only in a WRITE mode. `--select` on `--report` names a filter over a
//     write that will not happen, so it is a usage error rather than an accepted no-op.
//  2. A selection that is present and blank names ZERO findings, and must never fall back to
//     "apply everything" — the same fail-closed discipline the root intersection applies.
//
// It deliberately does NOT check that a fingerprint LOOKS like one. The write path matches selectors
// against the run's own fingerprints and reports the ones that matched nothing; a shape check here
// would be a check pretending to be a guarantee, and it would reject a value the write path might
// legitimately match if the fingerprint format ever changed.
func parseSelection(specs setFlags, m review.Mode) ([]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	if m != review.ModePatch && m != review.ModeApply {
		return nil, fmt.Errorf("--select narrows what is WRITTEN, and %s mode writes nothing — use it with --patch or --apply, or drop it", m)
	}
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--select was given with no value. An empty narrowing filter names zero findings — it does not mean \"apply everything\". Omit --select to apply the whole accepted set, or pass the `fingerprint` of each finding you want written")
	}
	return out, nil
}

// printSelection states what a narrowing selection did on the human channel. The UNMATCHED list is
// the reason this exists: a mistyped or stale fingerprint writes nothing, and a caller that is not
// told would read a smaller-than-expected apply as a smaller-than-expected finding set.
func printSelection(w io.Writer, sel *review.ApplySelection) {
	if !sel.Selective() {
		return
	}
	fmt.Fprintf(w, "selective apply: %d fingerprint(s) requested, %d matched this run's accepted set.\n",
		len(sel.Requested), len(sel.Matched))
	if len(sel.Unmatched) == 0 {
		return
	}
	fmt.Fprintf(w, "SELECTOR MATCHED NOTHING: %d fingerprint(s) name no finding in this run — they wrote nothing, and nothing was fetched for them.\n",
		len(sel.Unmatched))
	for _, u := range sel.Unmatched {
		fmt.Fprintf(w, "  - %s\n", u)
	}
}

// refusalCode is the CLI's answer to a PARTIAL REFUSAL: exit 7, and no halt record.
//
// The exit code is the CLI's only machine-readable "not clean" channel, and CI scripts key on it,
// so exit 0 would violate D8-C's load-bearing clause on the one surface where violating it is
// cheapest. The other candidates are all wrong for this case: exit 1 (`Findings`) is the GATING
// code and fires only under `--fail-on-findings`/`--ci`; exit 6 (`Containment`) means a
// containment BREACH, and nothing was breached — the denylist held, which is exactly why the
// finding was refused; and a new code 9 would widen a range `docs/schema/halt-record.schema.json`
// pins at 0–8 for one case.
//
// So exit 7 (`fault.Policy`), with the reason `apply_refused_protected_path`. Two consequences,
// both deliberate: `fault.Policy` is documented as "halted by policy or cap, or completed with a
// policy refusal that must not read as clean", and NO HALT RECORD IS WRITTEN, because there was
// no halt. A reader who expects "exit 7 ⇒ halt record" will find that surprising exactly once,
// which is why it is stated here and in docs/architecture.md rather than discovered.
//
// The two OTHER apply-refusal reasons (`authority_only`, `no_workspace_evidence`)
// continue to exit 0 — they describe findings that were never eligible to
// be applied, not a finding the model targeted at a path we refuse to touch. That asymmetry is
// intended; changing it would be a separate, defensible decision.
func refusalCode(refusals []review.ApplyRefusal) int {
	if len(refusals) == 0 {
		return int(fault.OK)
	}
	return int(fault.Policy)
}

// printRefusals states the refusal on the human channel, naming each path and the fact that the
// rest of the run proceeded. Silence here would recreate the failure the original halt existed to
// prevent.
func printRefusals(w io.Writer, refusals []review.ApplyRefusal) {
	if len(refusals) == 0 {
		return
	}
	fmt.Fprintf(w, "REFUSED (%s): %d finding(s) target a protected path and were NOT applied; nothing was written to them. Every other accepted finding was applied.\n",
		runmgr.ReasonApplyRefusedProtectedPath, len(refusals))
	for _, r := range refusals {
		fmt.Fprintf(w, "  - %s (%s)\n", r.File, r.Reason)
	}
	fmt.Fprintln(w, "  Re-running will refuse them identically — edit those paths by hand if they need changing. Exit code 7; this is not a halt, and no halt record was written.")
}

// checkWorkspacePath validates the human-given path against the SAME confinement rules
// the run will enforce on writes, with the path itself as the allowed root. It catches
// the cases a later failure would report less clearly: an unreadable/unresolvable path,
// and a path that is itself read-denied (pointing a review at a secret file).
func checkWorkspacePath(path string) (int, error) {
	res, err := scope.New(path)
	if err != nil {
		return int(fault.Usage), fmt.Errorf("workspace path %q cannot be resolved: %w", path, err)
	}
	if _, err := res.ResolveRead(path); err != nil {
		return int(fault.Config), err
	}
	return int(fault.OK), nil
}

// writeRunJSON encodes the run projection. The taxonomy values (exit code, machine reason,
// signal) are resolved HERE, from the fault, and handed to the pure projection builder —
// the projection package never depends on the exit-code mapping.
func writeRunJSON(out io.Writer, outcome review.RunOutcome, requested review.Mode, runErr error) error {
	view := runview.Build(runview.Input{
		Outcome: outcome, RequestedMode: requested, Err: runErr,
		ExitCode:   int(fault.CodeOf(runErr)),
		ReasonCode: fault.ReasonOf(runErr),
		Signal:     fault.SignalOf(runErr),
	})
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(view)
}

// printShape renders a dry run's disclosure: the panel that would be convened, the lanes that
// would be called, and the model-call range the run is bounded by.
//
// It states the RANGE rather than a single number, and says why in one line, because a reader
// given "21 calls" for a run that will almost certainly make five would learn to distrust the
// figure — and a reader given "5" for a run that can make 21 would be ambushed. It also names
// what neither bound counts (a retry after a schema-invalid response), since an undisclosed
// exclusion is how a cost estimate becomes an untrue one.
func printShape(out io.Writer, s review.RunShape) {
	fmt.Fprintf(out, "dry run: nothing was spent. This is what %s mode would do.\n\n", s.Mode)

	parallel := "all at once"
	if s.MaxParallel > 0 && s.MaxParallel < len(s.Seats) {
		parallel = fmt.Sprintf("%d at once", s.MaxParallel)
	}
	fmt.Fprintf(out, "  blind panel — %d seat(s), %s\n", len(s.Seats), parallel)
	width := 0
	for _, seat := range s.Seats {
		width = max(width, len(seat.SeatID))
	}
	for _, seat := range s.Seats {
		fmt.Fprintf(out, "    %-*s  %s / %s%s\n", width, seat.SeatID, seat.Adapter, seat.Model, effortSuffix(seat.Effort))
	}

	if len(s.Lanes) > 0 {
		fmt.Fprintln(out, "  other lanes")
		lw := 0
		for _, l := range s.Lanes {
			lw = max(lw, len(l.Role))
		}
		for _, l := range s.Lanes {
			// "host" execution is in-process and free; saying so here is the difference between a
			// reader budgeting for a lane and correctly ignoring it.
			cost := ""
			if l.Execution == "host" {
				cost = "  (in-process, no model call)"
			}
			fmt.Fprintf(out, "    %-*s  %s / %s%s%s\n", lw, l.Role, l.Adapter, l.Model, effortSuffix(l.Effort), cost)
		}
	}
	if s.DeterministicHost {
		fmt.Fprintln(out, "  adjudication is deterministic (no author_remediator model) — it costs nothing")
	}

	fmt.Fprintf(out, "  outer cycles: up to %d\n", s.OuterCycles)
	fmt.Fprintf(out, "  panel rounds: %d, shared across the panel (not per seat)\n\n", s.PanelRounds)

	printShapePayload(out, s.Payload)
	printShapeEgress(out, s.Egress)

	if s.ReadinessProbes > 0 {
		fmt.Fprintf(out, "  readiness probes: %d (one per distinct adapter/model, before anything is dispatched)\n", s.ReadinessProbes)
	}
	fmt.Fprintf(out, "  model calls: %d minimum, %d maximum\n", s.MinModelCalls, s.MaxModelCalls)
	fmt.Fprintln(out, "    The floor is a run that converges on its first cycle with nothing contested;")
	fmt.Fprintln(out, "    the ceiling is every configured cap multiplied out. Neither counts a retry")
	fmt.Fprintln(out, "    after a schema-invalid response.")
	if s.Writes {
		fmt.Fprintf(out, "\n  %s mode writes. This dry run did not.\n", s.Mode)
	}
	fmt.Fprintln(out, "\nDrop --dry-run to run it.")
}

// printShapePayload renders WHAT THE RUN WOULD CARRY: every file each reviewer would be shown, in
// full, and the only thing that keeps a file out of that set.
//
// It is the whole workspace minus containment, so the byte total is a measurement and not a
// budget — and it is the number to read before convening a large panel, because it is sent once
// per seat per round. A WITHHELD file is an omission containment chose, and it is printed because
// an omission nobody is told about is indistinguishable from a file that never existed. The file
// list itself stays in run-shape.json, where it can be long without burying the summary.
func printShapePayload(out io.Writer, p review.ShapePayload) {
	fmt.Fprintf(out, "  workspace: %d file(s), %s — every one shown to every reviewer, in full\n", p.Files, humanBytes(p.Bytes))
	for _, w := range p.Withheld {
		fmt.Fprintf(out, "    withheld by containment: %s\n", w)
	}
	fmt.Fprintln(out, "    Full list: run-shape.json.")
	fmt.Fprintln(out)
}

// printShapeEgress says where this run's content would go, grouped by destination.
//
// It prints DIRECTLY UNDER the payload, because the two only mean something together: the payload
// says how much of your tree is in every prompt, and this says who receives it. Either alone invites
// the wrong conclusion — a big payload sounds alarming until you see it never leaves the machine, and
// a small one sounds harmless until you see it reaches three providers.
func printShapeEgress(out io.Writer, rows []review.ShapeEgress) {
	if len(rows) == 0 {
		return
	}
	if !runmgr.EgressLeavesTheMachine(rows) {
		// The fully-local posture gets a positive statement rather than a list. It is the one case
		// where the answer is short enough to be a sentence, and the sentence is the point.
		fmt.Fprintln(out, "  content goes: NOWHERE — every seat and lane runs locally, nothing leaves this machine")
		fmt.Fprintln(out)
		return
	}
	fmt.Fprintln(out, "  content goes to — the reviewed files and any authority documents, in full:")
	for _, r := range rows {
		via := strings.Join(r.Via, ", ")
		switch {
		case !r.Known:
			fmt.Fprintf(out, "    ? %s\n      via %s\n", r.Destination, via)
		case r.Local:
			fmt.Fprintf(out, "    · %s (local — nothing sent)\n      via %s\n", r.Destination, via)
		default:
			line := "    → " + r.Destination
			if r.Note != "" {
				line += " (" + r.Note + ")"
			}
			fmt.Fprintf(out, "%s\n      via %s\n", line, via)
		}
	}
	fmt.Fprintln(out)
}

// humanBytes renders a byte count for a person reading a cost disclosure. It stays exact under a
// kilobyte, because "0.1 KB" is a worse answer than "97 B" for the file counts this prints.
func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}

func effortSuffix(effort string) string {
	if effort == "" {
		return ""
	}
	return "  effort=" + effort
}

// printFailureHint renders an actionable next step when a lane failure carries a classified CLI
// signal (folder-trust / login / model / update / timeout) — turning an opaque exit code into
// guidance.
//
// It renders the signal the MANAGER already classified and persisted (LaneFailure.Signal), rather
// than re-classifying the captured output here: the guidance a human reads and the `signal` a
// machine reads out of halt-record.json are then the same fact, not two derivations that can
// disagree. It falls back to classifying the excerpts only for a failure that predates the
// manager's classification (e.g. one assembled by a caller).
func printFailureHint(errw io.Writer, f *review.LaneFailure) {
	if f == nil {
		return
	}
	sig := clihint.Signal(f.Signal)
	if sig == "" {
		// NO Prompt: a LaneFailure carries the captured output but not the text that was sent, and
		// this branch exists only for a failure this process did not issue. Claiming a prompt we do
		// not have would be worse than classifying without one — see meshcore/clihint on why the
		// echo matters. The manager sets Signal for every failure it produces, so the accurate
		// path is the normal one and this is the degraded fallback it is documented to be.
		sig = clihint.ForFailure(clihint.Failure{
			Stderr: f.StderrExcerpt, Stdout: f.StdoutExcerpt, ExitCode: f.ExitCode, Adapter: f.Adapter,
		})
	}
	var hint string
	switch sig {
	case clihint.FolderTrust:
		hint = "the adapter blocked on a folder-trust prompt — complete its own folder-trust setup once (in its CLI), then re-run."
	case clihint.LoginRequired:
		hint = "the adapter is not authenticated — run its login in its own CLI, then re-run."
	case clihint.ModelInvalid:
		hint = "the requested model is unavailable for this adapter/account — set a valid model in the profile, then re-run."
	case clihint.UpdatePrompt:
		hint = "the adapter is waiting on an update — update its CLI, then re-run."
	case clihint.Timeout:
		hint = "the adapter timed out with no output — it may be waiting on login or a folder-trust prompt; run it directly once to finish setup, then re-run."
	}
	if hint != "" {
		fmt.Fprintln(errw, "  next step: "+hint)
	}
}

// hintFor maps a classified signal to its remediation sentence. Shared by the run-level hint and the
// per-seat enumeration so one seat and the run can never be given different advice for the same signal.
func hintFor(sig clihint.Signal) string {
	switch sig {
	case clihint.QuotaExhausted:
		// The only signal whose next step is to do NOTHING for a while. It is worded to stop the
		// reflex every other signal invites — going to look for a fault — and it does not name a
		// duration: aimesh cannot see the plan or its window, so the provider's own message (carried
		// beside this line as the seat's detail) is the only honest source for when to come back.
		return "the provider refused on a rate/usage limit — nothing here is misconfigured; wait for the window it names, then re-run."
	case clihint.FolderTrust:
		return "complete its own folder-trust setup once (in its CLI), then re-run."
	case clihint.LoginRequired:
		return "not authenticated — run its login in its own CLI, then re-run."
	case clihint.ModelInvalid:
		return "the requested model is unavailable for this adapter/account — set a valid model, then re-run."
	case clihint.UpdatePrompt:
		return "waiting on an update — update its CLI, then re-run."
	case clihint.Timeout:
		return "timed out with no output — run it directly once to finish setup, then re-run."
	}
	return ""
}

// printSeatFailures enumerates EVERY seat that failed, with its own cause and its own next step.
//
// The run-level hint above reports the first-by-index seat only, which is the correct halt contract
// and the wrong thing to show a human on its own: a panel dispatches all its seats concurrently, so a
// halted run has usually PAID for every seat's failure. Measured 2026-08-11, a two-seat panel failed
// on an unusable model and a nonexistent model — two separate fixes — and printed one of them.
//
// It prints nothing when fewer than two seats failed: the run-level hint already covers that case, and
// repeating it under a heading would be noise.
func printSeatFailures(errw io.Writer, panel []review.SeatStatus) {
	var failed []review.SeatStatus
	for _, s := range panel {
		if s.Status == "halted" && (s.Detail != "" || s.ReasonCode != "") {
			failed = append(failed, s)
		}
	}
	if len(failed) < 2 {
		return
	}
	fmt.Fprintf(errw, "%d of %d reviewer seat(s) failed, for different reasons — every one is listed so a single pass fixes them all:\n", len(failed), len(panel))
	for _, s := range failed {
		fmt.Fprintf(errw, "  - %s (%s / %s): %s\n", s.SeatID, s.Adapter, s.Model, s.Detail)
		if h := hintFor(clihint.Signal(s.Signal)); h != "" {
			fmt.Fprintf(errw, "      next step: %s\n", h)
		}
	}
}

// printIdentityCaveats surfaces model-identity caveats in the CLI review summary: the run PASSED (the
// adapter ran and returned schema-valid output) but a lane's exact model was weak/self-reported or not
// verified — the shared identity policy passes with a caveat rather than halting. A proven mismatch
// would have halted instead. Each line is model-string/adapter-key only (no paths/secrets).
func printIdentityCaveats(out io.Writer, caveats []review.IdentityCaveat) {
	if len(caveats) == 0 {
		return
	}
	fmt.Fprintf(out, "identity caveats (%d): exact model not fully verified — the run still passed\n", len(caveats))
	for _, c := range caveats {
		state := "not verified"
		if c.Status == review.VerifSelfReported {
			state = "weak · self-reported"
		}
		reported := "not reported"
		if c.ReportedModel != "" {
			reported = c.ReportedModel
		}
		fmt.Fprintf(out, "  - %s lane (%s): requested %q, reported %q — %s\n",
			c.Role, c.Adapter, c.RequestedModel, reported, state)
	}
}

// printWithheld surfaces files a CONTAINMENT rule kept out of the reviewed set. The run
// PASSED, and that is exactly why this has to be said out loud: a reviewer cannot object to a
// file it was never shown, so an unmentioned withhold turns a containment rule into a silent
// blind spot in a report a human is about to trust. Paths are workspace-relative; the machine
// reason code is printed alongside the rule so the line matches the run record.
func printWithheld(out io.Writer, withheld []review.WithheldFile) {
	if len(withheld) == 0 {
		return
	}
	fmt.Fprintf(out, "withheld from review (%d): these files were NOT shown to the reviewers — their absence from the findings means nothing\n", len(withheld))
	for _, w := range withheld {
		line := fmt.Sprintf("  - %s [%s]", w.Path, w.Reason)
		if w.Rule != "" {
			line += ": " + w.Rule
		}
		if w.Detail != "" {
			line += " (" + w.Detail + ")"
		}
		fmt.Fprintln(out, line)
	}
}

// printGrounding renders the CITATION-GROUNDING tally for a human.
//
// It prints ONLY when something failed to resolve, which is a deliberate asymmetry. An all-grounded
// run has nothing a reader needs to act on, and a line reporting it every time would train them past
// the line that matters. When it does print, it leads with the caveat rather than the ratio: the
// numbers invite "so those findings are wrong", and they do not mean that.
func printGrounding(out io.Writer, g *review.GroundingSummary) {
	if g == nil || g.Unresolved == 0 {
		return
	}
	fmt.Fprintf(out, "citation grounding: %d of %d cited finding(s) point at something that is NOT in the reviewed tree.\n", g.Unresolved, g.Checked)
	for _, status := range []string{runmgr.GroundedFileMissing, runmgr.GroundedLineOutOfRange, runmgr.GroundedSymbolAbsent, runmgr.GroundedUnreadable} {
		if n := g.ByStatus[status]; n > 0 {
			fmt.Fprintf(out, "  %-18s %d\n", status, n)
		}
	}
	fmt.Fprintln(out, "  Nothing was dropped or downgraded. A reviewer that named the wrong file may still have")
	fmt.Fprintln(out, "  found a real defect a line away — this says the pointer did not resolve, not that the")
	fmt.Fprintln(out, "  finding is wrong. Findings carry the detail; --json has it per finding.")
}

// printScope says that this review looked at part of the tree.
//
// It prints unconditionally when present and BEFORE the findings, because it changes what the whole
// result means: a narrowed review is silent about everything it was not shown, and that silence must
// not read as approval.
func printScope(out io.Writer, s *runmgr.ScopeSummary) {
	if s == nil {
		return
	}
	fmt.Fprintf(out, "SCOPE: %d of %d file(s) reviewed", s.Selected, s.Available)
	switch {
	case s.VCSRef != "":
		fmt.Fprintf(out, " (changed vs %s)", s.VCSRef)
	case s.ChangedSince != "":
		fmt.Fprintf(out, " (modified within %s)", s.ChangedSince)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  This says nothing about the rest of the tree — no finding there means nobody looked.")
}

// printPartialPanel says that this review ran with fewer seats than were configured.
//
// It prints FIRST among the qualifiers, and unconditionally when present, because it changes the
// denominator every other number in the output is read against. Its absence is the ordinary case, so
// nothing is printed for a full panel.
func printPartialPanel(out io.Writer, p *runmgr.PartialPanel) {
	if p == nil {
		return
	}
	fmt.Fprintf(out, "PARTIAL PANEL: %d of %d configured seat(s) answered.\n", p.Answered, p.Configured)
	for _, l := range p.Lost {
		fmt.Fprintf(out, "  lost %-14s %s/%s — %s\n", l.SeatID, l.Adapter, l.Model, l.Signal)
	}
	fmt.Fprintln(out, "  A provider ran out of capacity and the run continued rather than discarding the")
	fmt.Fprintln(out, "  seats that had already answered. Nothing about the surviving findings changed —")
	fmt.Fprintln(out, "  but read every agreement count against the seats that ANSWERED, not the panel you")
	fmt.Fprintln(out, "  configured. Re-run when capacity returns if the full panel matters.")
}

// printComposition tells a human what their panel's agreement counts were worth.
//
// It prints ONLY when a model is shared, on the same asymmetry printGrounding uses: a panel of
// distinct models has nothing to act on, and a line every run would train a reader past the one that
// matters. It names WHICH model is doubled — an operator who has to go read the roster to find out
// has been told half a fact.
func printComposition(out io.Writer, c *review.PanelComposition) {
	if c == nil {
		return
	}
	// A PASS-THROUGH seat is reported on its own asymmetry: it is absent from the ordinary run, and
	// when present it is the one thing `list` cannot answer questions about afterwards.
	for _, s := range c.Seats {
		if s.ModelSource != runmgr.ModelSourcePassThrough {
			continue
		}
		line := fmt.Sprintf("seat %d (%s) ran model %q, which your configuration does not define", s.Index, s.Adapter, s.Model)
		if s.PassThroughHint != "" {
			line += fmt.Sprintf(" — did you mean %q?", s.PassThroughHint)
		}
		fmt.Fprintln(out, line+".")
		fmt.Fprintln(out, "  The adapter was checked; the model string was passed to it verbatim. Nothing about")
		fmt.Fprintln(out, "  this seat's findings differs — but `list` cannot describe this model, and whether it")
		fmt.Fprintln(out, "  exists at all was the provider's answer, not ours.")
	}
	if c.Independence != runmgr.IndependenceSharedModel {
		return
	}
	fmt.Fprintf(out, "panel composition: %d seat(s), %d distinct model(s) — %s appears more than once.\n",
		len(c.Seats), c.DistinctModels, strings.Join(runmgr.SharedModels(c.Seats), ", "))
	fmt.Fprintln(out, "  Where those seats agree, their errors correlate: they share priors, so the extra")
	fmt.Fprintln(out, "  agreement is worth less than the count suggests. No count was adjusted, and this")
	fmt.Fprintln(out, "  does not make any finding wrong. --json carries it per finding.")
}

// printDissent tells a human how much of this result their panel actually agreed on.
//
// It prints ONLY when something is contested, on the same asymmetry printGrounding and
// printComposition use: a result the panel agreed on throughout has nothing a reader needs to act on,
// and a line every run would train them past the one that matters.
//
// It leads with what silence is NOT, before the number. The number invites "so those findings are
// doubtful", and it does not mean that — the seats are blind, so a seat that did not report a finding
// may equally never have reached that file.
func printDissent(out io.Writer, d *review.DissentSummary) {
	if d == nil || d.Contested == 0 {
		return
	}
	fmt.Fprintf(out, "panel dissent: %d of %d panel finding(s) are contested — fewer of the seats that ran\n", d.Contested, d.Panelled)
	fmt.Fprintln(out, "  reported them than did not. A silent seat is not a seat that disagreed: seats are blind,")
	fmt.Fprintln(out, "  and a seat may have disagreed, never reached that file, or stopped when its own set")
	fmt.Fprintln(out, "  stabilized. Nothing was dropped, downgraded or reordered — read those findings rather")
	fmt.Fprintln(out, "  than skim them. review-summary.md marks each one; --json has it per finding.")
}

// printVerification renders the bounded-execution record for a human.
//
// It leads with the DELTA and states the two limits before the numbers, because both misreadings are
// easy and they run in opposite directions: green invites "the change is fine", red invites "the
// findings were wrong". Neither follows.
func printVerification(out io.Writer, v *review.VerificationReport) {
	if v == nil {
		return
	}
	fmt.Fprintf(out, "verification (your own commands, on the containment copy): %s\n", v.Delta)
	switch v.Delta {
	case runmgr.DeltaBroken:
		fmt.Fprintln(out, "  SOMETHING THAT PASSED BEFORE FAILS NOW. This did not block the commit and did not")
		fmt.Fprintln(out, "  invalidate any finding — it ran after the write, deliberately. Look at it yourself.")
	case runmgr.DeltaUnchangedFail:
		fmt.Fprintln(out, "  The same command(s) failed before and after: the tree was already red and this run")
		fmt.Fprintln(out, "  did not make it worse. That is an ordinary outcome, not an error.")
	case runmgr.DeltaNotComparable:
		fmt.Fprintln(out, "  A command timed out or could not start, so the two passes do not describe the same")
		fmt.Fprintln(out, "  thing. That is a fact about the budget or this host, not about your code.")
	case runmgr.DeltaBaselineOnly:
		fmt.Fprintln(out, "  Baseline only — a report run writes nothing, so there is no 'after' to compare.")
	}
	for i, b := range v.Before {
		line := fmt.Sprintf("  %-8s %s", verifyMark(b), b.Command)
		if i < len(v.After) {
			line = fmt.Sprintf("  %-8s → %-8s %s", verifyMark(b), verifyMark(v.After[i]), b.Command)
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, "  A pass does not mean the change is correct — your commands may not cover what changed.")
}

// verifyMark is one command's outcome as a short word. `timeout` and `unstartable` are their own
// marks rather than folded into `FAIL`, because neither is a statement about the code.
func verifyMark(r review.VerificationResult) string {
	switch {
	case r.TimedOut:
		return "timeout"
	case r.Unstartable != "":
		return "no-start"
	case r.OK:
		return "pass"
	default:
		return "FAIL"
	}
}

// registerVerifyFlags declares the BOUNDED-EXECUTION flags on an AGENT-surface launch command (acp,
// mcp) and returns the resolver. It is one function rather than two copies because the help text is
// the consent an operator is giving, and two copies of a consent notice drift into two different
// promises.
//
// The wording differs from the `review` command's on one point that matters: here the operator is
// agreeing IN ADVANCE, for every run a peer or a model will later request. That is why these are
// launch flags at all — a caller that could name a command to execute would have arbitrary code
// execution on this machine dressed as a review parameter.
//
// It returns a struct rather than a tuple because these are OPERATOR POLICY settings and the set will
// keep growing — every one of them is something a caller must not be able to decide for itself, and a
// four-value return was already at the limit of what reads.
type writePolicy struct {
	VerifyCommands      []string
	VerifyTimeout       time.Duration
	VerifyBaseline      bool
	AllowProtectedPaths bool
}

func registerWritePolicyFlags(fs *flag.FlagSet, surface string) func(io.Writer) (writePolicy, bool) {
	var cmds setFlags
	fs.Var(&cmds, "verify-cmd", "your project's own build/test `command`, run on the CONTAINMENT COPY and RECORDED for every review this "+surface+" runs (repeatable, max "+strconv.Itoa(runmgr.MaxVerifyCommands)+"). YOU are agreeing to this in advance, for runs a caller will request later: a caller can neither name a command nor cause one to run, because a peer that could would have arbitrary code execution here. Nothing model-authored is ever executed. The result GATES NOTHING — it is reported with a note saying so.")
	timeout := fs.Duration("verify-timeout", runmgr.DefaultVerifyTimeout, "per-command wall-clock budget for --verify-cmd.")
	baseline := fs.Bool("verify-baseline", false, "run --verify-cmd on report runs too (they write nothing, so there is no 'after' — you pay the full cost of your suite for one baseline fact).")
	allowProtected := fs.Bool("allow-protected-paths", false, "permit reviews and writes inside a tree that is, or sits under, a protected configuration path — .git, .claude, .cursor, .codex, .gemini, .vscode, .idea, .windsurf. YOU are agreeing to this in advance, for runs a caller will request later, and it is the sharpest of these grants: .git/hooks/** and .vscode/mcp.json execute code on the next command, so a caller that could grant itself this could arrange to run its own code on your machine later. It does NOT unlock secrets — .env*, .ssh, .aws, .netrc and key material stay refused for reading as well as writing. `.aimesh` is NOT on this list: run artifacts are readable without any flag.")
	return func(errw io.Writer) (writePolicy, bool) {
		out, err := runmgr.ValidateVerifyCommands(cmds)
		if err != nil {
			fmt.Fprintf(errw, "aimesh review %s: %v\n", surface, err)
			return writePolicy{}, false
		}
		if len(out) == 0 && (*baseline || *timeout != runmgr.DefaultVerifyTimeout) {
			fmt.Fprintf(errw, "aimesh review %s: --verify-baseline / --verify-timeout need at least one --verify-cmd\n", surface)
			return writePolicy{}, false
		}
		return writePolicy{
			VerifyCommands: out, VerifyTimeout: *timeout, VerifyBaseline: *baseline,
			AllowProtectedPaths: *allowProtected,
		}, true
	}
}

// setFlags collects repeatable --set values.
type setFlags []string

func (s *setFlags) String() string { return strings.Join(*s, ",") }
func (s *setFlags) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseSets turns --set values ("role.adapter=NAME" / "role.model=NAME") into the
// per-lane override maps the resolver consumes.
func parseSets(sets []string) (adapters, models map[review.Role]string, err error) {
	for _, s := range sets {
		kv := strings.SplitN(s, "=", 2)
		if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
			return nil, nil, fmt.Errorf("invalid --set %q (want role.adapter=NAME or role.model=NAME)", s)
		}
		dot := strings.SplitN(kv[0], ".", 2)
		if len(dot) != 2 {
			return nil, nil, fmt.Errorf("invalid --set key %q (want role.adapter or role.model)", kv[0])
		}
		role, field, val := review.Role(dot[0]), dot[1], kv[1]
		switch field {
		case "adapter":
			if adapters == nil {
				adapters = map[review.Role]string{}
			}
			adapters[role] = val
		case "model":
			if models == nil {
				models = map[review.Role]string{}
			}
			models[role] = val
		default:
			return nil, nil, fmt.Errorf("invalid --set field %q (want adapter or model)", field)
		}
	}
	return adapters, models, nil
}

// parseAuthority turns the CLI's authority flags into the declaration the core consumes.
//
// Two forms, deliberately mutually exclusive:
//   - `--authority <path>` (repeatable) + `--authority-hash <name>=<sha256>` — the common
//     case: whole documents, optionally content-pinned. The document's NAME defaults to its
//     base name, which is what a hash pin refers to.
//   - `--authority-manifest <file.json>` — the full declaration (byte ranges, media types,
//     inline content), for anything a flag cannot express.
//
// It performs SHAPE parsing only; the semantic rules (exactly-one-of, uniqueness, the
// report-mode-only inline split, the budget, hash verification, root scoping) all live in
// internal/engine/authority so every surface gets identical answers.
func parseAuthority(paths, hashes []string, manifestPath string) ([]review.AuthorityDoc, error) {
	if manifestPath != "" {
		if len(paths) > 0 || len(hashes) > 0 {
			return nil, fmt.Errorf("--authority-manifest cannot be combined with --authority/--authority-hash (declare everything in the manifest instead)")
		}
		return readAuthorityManifest(manifestPath)
	}
	if len(paths) == 0 {
		if len(hashes) > 0 {
			return nil, fmt.Errorf("--authority-hash applies only with --authority (it pins a declared document by name)")
		}
		return nil, nil
	}
	pins := map[string]string{}
	for _, h := range hashes {
		kv := strings.SplitN(h, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" || strings.TrimSpace(kv[1]) == "" {
			return nil, fmt.Errorf("invalid --authority-hash %q (want <name>=<sha256>, where <name> is the authority document's name — by default its file base name)", h)
		}
		pins[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	docs := make([]review.AuthorityDoc, 0, len(paths))
	used := map[string]bool{}
	for _, p := range paths {
		// A value may carry a SECTION or a BYTE RANGE selector; see authorityref.go. Both resolve
		// here into the same explicit `ranges` declaration the manifest takes, so nothing downstream
		// — the run record, the inclusion manifest, any surface — sees the shorthand.
		ref, rerr := parseAuthorityRef(p)
		if rerr != nil {
			return nil, rerr
		}
		name := filepath.Base(filepath.Clean(ref.Path))
		doc, derr := resolveAuthorityRef(ref, pins[name])
		if derr != nil {
			return nil, derr
		}
		docs = append(docs, doc)
		used[name] = true
	}
	// An unmatched pin is a typo, and a typo'd pin silently pins NOTHING — which is the one
	// failure mode a content pin exists to prevent. Refuse instead.
	for name := range pins {
		if !used[name] {
			return nil, fmt.Errorf("--authority-hash names %q, which is not one of the declared --authority documents (names default to the file base name)", name)
		}
	}
	return docs, nil
}

// readAuthorityManifest loads the JSON authority declaration. It accepts either a bare array
// or `{"authority": [...]}`, and rejects unknown fields — a mistyped key in a governance
// declaration must fail loudly, never be silently ignored.
func readAuthorityManifest(path string) ([]review.AuthorityDoc, error) {
	res, err := scope.New(path)
	if err != nil {
		return nil, fmt.Errorf("--authority-manifest %q cannot be resolved: %w", path, err)
	}
	abs, derr := res.ResolveRead(path)
	if derr != nil {
		return nil, fmt.Errorf("--authority-manifest: %w", derr)
	}
	// Read through the resolved root, not by re-opening the resolved STRING: this file names
	// the documents (and their hash pins) the whole run is judged against, so a swap between
	// the resolution and the read would substitute the governance declaration itself.
	b, rerr := rootfile.Read(res.Roots(), abs)
	if rerr != nil {
		return nil, fmt.Errorf("--authority-manifest %q: %w", path, rerr)
	}
	trimmed := strings.TrimSpace(string(b))
	if strings.HasPrefix(trimmed, "[") {
		var docs []review.AuthorityDoc
		if jerr := decodeStrict(trimmed, &docs); jerr != nil {
			return nil, fmt.Errorf("--authority-manifest %q is not a valid authority array: %w", path, jerr)
		}
		return docs, nil
	}
	var wrapper struct {
		Authority []review.AuthorityDoc `json:"authority"`
	}
	if jerr := decodeStrict(trimmed, &wrapper); jerr != nil {
		return nil, fmt.Errorf("--authority-manifest %q is not a valid authority manifest: %w", path, jerr)
	}
	return wrapper.Authority, nil
}

// decodeStrict decodes JSON refusing unknown fields.
func decodeStrict(s string, v any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// diagSuffix appends the stable diagnostic halt code (e.g. " (Class E)" / " (M5)")
// to a user-facing error, so the plain-English message leads and the code follows.
func diagSuffix(err error) string {
	var f *fault.Fault
	if errors.As(err, &f) && f.Halt != "" {
		if strings.HasPrefix(f.Halt, "M") {
			return " (" + f.Halt + ")"
		}
		return " (Class " + f.Halt + ")"
	}
	return ""
}

// gatingCode returns exit 1 when gating is on and the review ended with findings,
// else 0 — findings exit 0 unless gating is explicitly enabled, so a review that merely
// reports never fails a caller's build by default.
func gatingCode(failOnFindings bool, findings int) int {
	if failOnFindings && findings > 0 {
		return int(fault.Findings)
	}
	return int(fault.OK)
}

func resolveMode(mode string, report, patch, apply bool) (review.Mode, error) {
	n := 0
	for _, b := range []bool{report, patch, apply} {
		if b {
			n++
		}
	}
	if n > 1 || (n == 1 && mode != "") {
		return "", fmt.Errorf("specify at most one of --report/--patch/--apply/--mode")
	}
	switch {
	case mode != "":
		switch review.Mode(mode) {
		case review.ModeReport, review.ModePatch, review.ModeApply:
			return review.Mode(mode), nil
		}
		return "", fmt.Errorf("invalid --mode %q", mode)
	case report:
		return review.ModeReport, nil
	case patch:
		return review.ModePatch, nil
	case apply:
		return review.ModeApply, nil
	default:
		return "", nil // let the surface default apply
	}
}

func runACP(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh review acp")
	framing := fs.String("framing", acp.FramingNewline, "wire framing: newline | content-length")
	// TRUSTED ROOTS. On the CLI the human types the path and that IS the consent; over ACP
	// the caller is a peer process, so the consent has to be given BEFORE any request — here,
	// at launch. Everything a session later names must resolve inside these roots.
	var roots setFlags
	fs.Var(&roots, "root", "trusted workspace root an ACP session may review (repeatable; default: the launch working directory)")
	noDefaultRoot := fs.Bool("no-default-root", false, "do not adopt the launch working directory as a trusted root (explicit --root only)")
	// An over-broad root (`/`, a home directory, a system/shared tree) is refused for an
	// explicit --root just as it is for the launch cwd. This flag keeps that a refusal an
	// operator can OVERRIDE BY SAYING SO, rather than a rule the documented flag silently
	// walked past; it never waives the non-overridable read denylist.
	allowBroadRoot := fs.Bool("allow-broad-root", false, "permit an explicit --root that is normally refused as over-broad (/, a home directory, a system/shared tree)")
	// The turn budget. Without it an ACP prompt could run until the host gave up — leaving a panel
	// of model CLIs spending with nobody waiting for them. Every other aimesh agent surface has
	// bounded a turn since it shipped; this closes the recorded parity gap.
	turnTimeout := fs.Duration("turn-timeout", acp.DefaultTurnTimeout, "total wall-clock budget for one prompt/review turn")
	// CROSS-RUN DISPOSITION MEMORY is an OPERATOR opt-in here, exactly as it is a human opt-in on
	// `reviewmesh review`. It is not a `_meta` field: on this surface the caller is a peer process,
	// so the consent has to be given before any request — the same rule `--root` follows.
	resolveWritePolicy := registerWritePolicyFlags(fs, "acp")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	policy, pok := resolveWritePolicy(errw)
	if !pok {
		return int(fault.Usage)
	}
	// The provenance is discarded here on purpose: ACP has no protocol-revision split, so an inferred
	// cwd is exactly as trusted on this surface as it always was. It is the MCP surface's modern era
	// that must distinguish them (see runMCP).
	trustedRoots, _, rerr := acp.ResolveTrustedRoots(acp.RootOptions{Surface: "acp", Explicit: roots, NoDefault: *noDefaultRoot, AllowBroadRoot: *allowBroadRoot})
	if rerr != nil {
		fmt.Fprintln(errw, "aimesh review acp: "+rerr.Error())
		return int(fault.CodeOf(rerr))
	}
	a, err := app.New(app.Options{})
	if err != nil {
		fmt.Fprintln(errw, "aimesh review acp:", err)
		return int(fault.CodeOf(err))
	}
	// degrade-vs-fail policy when a requested mode exceeds the effective ceiling
	// (config default is true); the ACP surface enforces host-capability mode gating.
	degrade := a.Cfg.Surfaces.DegradeWhenModeUnavailable == nil || *a.Cfg.Surfaces.DegradeWhenModeUnavailable
	// Write-authority POLICY ceiling for the ACP surface (`surfaces.defaultModeBySurface.acp`;
	// the shipped seed is `report` — a live workspace write over ACP is an explicit config
	// opt-in). The resolver caps the mode by the same policy; passing it to the server makes the
	// cap visible to the host instead of a silent downgrade. An absent or unrecognized value
	// fails CLOSED at `report`: the ACP surface never widens itself on a malformed policy.
	policyCeiling := review.ModeReport
	switch m := review.Mode(a.Cfg.Surfaces.DefaultModeBySurface["acp"]); m {
	case review.ModeReport, review.ModePatch, review.ModeApply:
		policyCeiling = m
	}
	srv := &acp.Server{
		Manager: a.Manager(),
		Framing: *framing,
		Roots:   trustedRoots,
		Caps: review.SurfaceCaps{
			WorkspaceRoot: true, FileRead: true, FileWrite: true,
			DiffContext: true, ArtifactDir: true, Interactive: false,
		},
		DegradeWhenModeUnavailable: degrade,
		PolicyCeiling:              policyCeiling,
		TurnTimeout:                *turnTimeout,
		// Set ONLY by the web ACP-validation harness (it exports this env when spawning the child); a
		// normal `reviewmesh acp` invocation leaves it unset → no synthetic host-adjudication probe.
		ValidateHostAdjudication: os.Getenv("REVIEWMESH_ACP_VALIDATE_HOSTADJ") == "1",
		VerifyCommands:           policy.VerifyCommands,
		VerifyTimeout:            policy.VerifyTimeout,
		VerifyBaseline:           policy.VerifyBaseline,
		AllowProtectedPaths:      policy.AllowProtectedPaths,
	}
	// Durable session store (AIMESH_HOME-aware) → enables ACP v1 `session/resume` across an
	// agent restart. If the home cannot be resolved, resume is simply unsupported (not fatal).
	if home, herr := config.HomeDir(); herr == nil {
		srv.Sessions = acp.NewFileSessionStore(filepath.Join(config.ComponentDir(home), "acp-sessions"))
	}
	if err := srv.Serve(os.Stdin, out); err != nil {
		fmt.Fprintln(errw, "aimesh review acp:", err)
		return int(fault.Internal)
	}
	return int(fault.OK)
}

// splitArgs is meshcore/cliflags.SplitArgs. It lives behind a local name because both apps hit the
// same two failures — a flag's value misread as the operand, and an operand that makes Go's parser
// stop and silently drop every flag after it — and one implementation is how they stay fixed
// together. See that package for what the rule is and why it is asked of the flag set.
func splitArgs(fs *flag.FlagSet, args []string) (flags, positionals []string) {
	return cliflags.SplitArgs(fs, args)
}
