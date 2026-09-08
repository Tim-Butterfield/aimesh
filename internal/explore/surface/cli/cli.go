// Package cli is exploremesh's command-line surface: `explore` runs a fan-out→collate exploration
// and prints/persists the synthesis; `doctor` reports adapter readiness (reusing meshcore/doctor).
// Adapters are discovered from meshcore (see internal/registry): a roster naming a real adapter runs
// the real CLI. The explicit `fake` key is the HIDDEN internal test harness — it resolves only under
// the internal gate (tests/golden runs) and is otherwise an unrecognized name. Every unrecognized
// name is FAIL-CLOSED — a configuration error surfaced before any spend, never a silent substitute
// (registry.Build returns it as `unknown`; `explore`/`acp` refuse and `doctor` fails a check).
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

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	mdoctor "github.com/Tim-Butterfield/aimesh/meshcore/doctor"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"

	"github.com/Tim-Butterfield/aimesh/internal/explore/capture"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
	"github.com/Tim-Butterfield/aimesh/internal/explore/version"
)

// Run dispatches a subcommand and returns a process exit code.
//
// `usage` PRINTS and nothing more; every call site below decides its own exit code, because printing
// the help text is not by itself a verdict. Asking for help succeeds (0); being given no command, or
// an unknown one, is a usage error (2). Folding the two together is how `exploremesh --help` came to
// exit 2 while `reviewmesh --help` exited 0.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return int(fault.Usage)
	}
	switch args[0] {
	case "explore":
		return runExplore(args[1:], stdout, stderr)
	case "list":
		return runList(args[1:], stdout, stderr)
	case "export":
		return runExport(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "acp":
		return runACP(args[1:], stdout, stderr)
	case "mcp":
		return runMCP(args[1:], stdout, stderr)
	case "setup":
		return runSetup(args[1:], stdout, stderr)
	case "init":
		return runInit(args[1:], localstate.InitAuto, stdout, stderr)
	case "repo":
		if len(args) >= 2 && args[1] == "init" {
			return runInit(args[2:], localstate.InitRepo, stdout, stderr)
		}
		fmt.Fprintln(stderr, "aimesh explore: usage: exploremesh repo init")
		return int(fault.Usage)
	case "folder":
		if len(args) >= 2 && args[1] == "init" {
			return runInit(args[2:], localstate.InitFolder, stdout, stderr)
		}
		fmt.Fprintln(stderr, "aimesh explore: usage: exploremesh folder init")
		return int(fault.Usage)
	case "-h", "--help", "help":
		// Asking for help is not a usage error. It goes to STDOUT and exits 0 — the same contract
		// `reviewmesh --help` has always had, and the one `foo --help | less` and every CI script
		// that gates on the exit code assume.
		usage(stdout)
		return int(fault.OK)
	case "--version":
		return runVersion(args[1:], stdout)
	default:
		fmt.Fprintf(stderr, "aimesh explore: unknown command %q\n", args[0])
		usage(stderr)
		return int(fault.Usage)
	}
}

// usage prints the complete command/flag surface. The mode list is rendered FROM the registry
// (mode.Names) rather than spelled out, so help can never drift from the modes the binary actually has.
//
// It returns nothing on purpose: the exit code belongs to the CALLER, which knows whether the text is
// an answer (`--help` → 0) or an explanation of a refusal (no command / unknown command → 2).
func usage(w io.Writer) {
	fmt.Fprintf(w, `exploremesh — N-explorer research/exploration on meshcore

usage:
  exploremesh explore --purpose <text> --criteria <a,b,...> [--mode <name>] [--profile <name> | --roster <path>]
                      [--count <n>|all] [--artifact <path|->] [--prior-context <text>] [--json] [--dump-run] [--debug]
                      [--max-parallel <n>]   (how many explorer CLIs run at once; default: the whole panel)
                      [--canonicalizer adapter=<n>,model=<m>[,effort=<e>] --canonicalizer ...]   (exactly 2, or none)
  exploremesh explore --purpose <text> --criteria <a,b,...> --explorer adapter=<n>,model=<m>[,effort=<e>] --explorer ... --collator adapter=<n>,model=<m>[,effort=<e>]
  exploremesh explore --mode compare --options <a,b,c> --criterion name=<n>,direction=higher_is_better|lower_is_better[,role=dimension|filter][,weight=<w>] --criterion ...
  exploremesh explore --mode forecast --target <what> --unit <unit> --horizon <when> [--conditioning <event>]
  exploremesh export --sqlite <out.db> --run <run-dir> [--verify] [--force] [--json]
  exploremesh list [--json]
  exploremesh doctor [--profile <name> | --roster <path>] [--probe] [--probe-deep] [--json]
  exploremesh acp [--framing newline|content-length] [--turn-timeout <dur>] [--roster <path>]
  exploremesh mcp [--wait-seconds <n>] [--turn-timeout <dur>]
                  [--no-capture] [--roster <path>]
  exploremesh setup --adapter <name> --path <p>          |  setup --remove-adapter <name>
  exploremesh setup --acp detect --path <p> [--acp-arg <a> ...]
  exploremesh setup --acp add --path <p> [--name <k>] [--title <t>] [--acp-arg <a> ...]
  exploremesh setup --acp remove --name <k>
  exploremesh setup --profile <name> --explorer adapter=<n>,model=<m>[,effort=<e>] --explorer ...
                    --collator adapter=<n>,model=<m> [--canonicalizer ... --canonicalizer ...] [--default-mode <m>]
  exploremesh setup --delete-profile <name> --yes
  exploremesh init            (repo if inside one, else plain folder)
  exploremesh repo init       (require a Git/Mercurial repo; .aimesh is VCS-excluded)
  exploremesh folder init     (require a non-repo directory)

commands:
  explore   blind fan-out over the explorer roster, then collate the responses into a synthesis;
            --mode selects the app-owned exploration mode (default map, which is formulation-free).
            --artifact supplies the thing under review (a path, or - for stdin); --mode challenge
            requires it. Adjudicative modes (challenge, shortlist, ai-collab) run 2 rounds and report
            HOST-computed counts/rankings with both denominators and honest WITHHELD labels.
            --profile selects a named profile, --roster a raw roster file (mutually exclusive), and
            --count runs the top-N explorers by the profile's preference order (never clamped).
            Ad-hoc: pass >=2 --explorer + a --collator (structured adapter=,model=[,effort=] specs) to
            run a one-off roster by identifier, bypassing --roster/--profile (all mutually exclusive).
            --canonicalizer names the identities that propose the canonicalization for a canonicalizing
            mode: exactly TWO (the merge-agreement rule holds a merge only when both propose it) or none,
            in which case the host derives them — slot a from the collator, slot b from the first explorer
            by PREFERENCE order that differs from it. One entry is refused: it does not say which slot it
            fills. It overrides the profile's canonicalizers, and every adapter must already be configured.
            --dump-run records the run (envelopes, raw outputs, prompts, a versioned manifest) under
            $EXPLOREMESH_ARTIFACT_DIR; --debug prints a per-adapter-call diagnostic to stderr.
            FIXED-SPACE modes declare their space UP FRONT and run ONE blind round with no canonicalizer:
            --mode compare takes --options + repeatable --criterion (each with a direction and a
            dimension/filter role; a weight is what licenses a scalar ranking — without weights the result
            is the Pareto/trade-off view and says so). --mode forecast takes --target/--unit/--horizon.
  export    build the DERIVED SQLite evidence database from a captured run directory (--dump-run wrote it).
            The run directory stays the system of record; the database is a rebuildable view over it, so
            --verify re-exports and compares (the derivability invariant). Optional: nothing else needs it.
            This is exploremesh's ONLY write to a path you type, and it is governed: an existing --sqlite
            destination is refused unless --force, a directory/symlink/device destination is refused rather
            than followed, a protected path (~/.aimesh/**, .git/**, .env*, key material, IDE/agent config)
            is refused outright and NO flag lifts that (exit 6), and the database is published by an atomic
            rename so an interrupted export cannot replace a valid database with a truncated one
  list      report the configured adapters (name, configured?/path, shell vs ACP, declared identity
            evidence capability), the resolved roster, the configured profiles (ordered explorers +
            collator + canonicalizers + default mode), and the available modes (--json for a projection)
  doctor    report explorer/collator adapter readiness for the FULL configured panel (no --count).
            Static by default (no process is started). --probe additionally runs a SAFE, bounded,
            no-model readiness probe of each required adapter (a shell recipe's "<binary> --version",
            an ACP instance's handshake) and reports the stage it reached and any classified blocker
            signal — it spends no tokens and never authenticates. --probe-deep goes further and SPENDS:
            ONE real, bounded model invocation per required adapter, in a throwaway, git-initialized
            directory — the shape every run actually gets. It is the only probe that can answer whether
            a CLI does real work where the run happens (gemini-cli passed --version and then failed
            every real call with exit 55 until --skip-trust reached its recipe). It NEVER answers an
            interactive trust or login prompt: it detects, classifies and reports the fix you perform.
            --json emits the machine-readable projection (ok + checks[] + probes[], with "deep" marking
            the rows that spent) instead of the human report; the exit code is the same either way
  setup     configure exploration: record or clear a shell adapter's binary path, detect/add/remove a
            user-defined ACP adapter, and create or delete a profile. Exactly ONE action per
            invocation, through the governed manager seams — every write is validated by Go before it
            lands. Adapter + ACP entries persist to the shared ~/.aimesh/adapters.yaml; profiles to the
            project profiles.yaml inside a repo, else the user one. --acp detect and --acp add LAUNCH
            THE REAL CLI to confirm its launch args (that is the only way to know it speaks ACP);
            --delete-profile requires --yes. A no-flag run always binds to the profile named "default"
            (as in reviewmesh) — save the panel you want as "default"; there is no set-default
  acp       run as a local ACP (Agent Client Protocol) agent over stdio so another tool can drive one
            exploration; the task PURPOSE comes from the prompt and the CRITERIA from _meta.exploremesh,
            which may also SELECT the panel (profile: <name>, count: <n>|"all"), COMPOSE an ad-hoc one
            (panel: {explorers[], collator}) from the configured adapters, and capture the run (dumpRun)
  mcp       run as a local MCP (Model Context Protocol) server over stdio so an MCP agent can drive
            explorations. Tools: explore / explore_challenge / explore_compare / explore_forecast (each
            SPENDS: it launches the configured model CLIs), plus read-only list / doctor / run_status /
            run_result. Calls are JOB-SHAPED — a run that outlives --wait-seconds is handed back as
            {runId, state:"running"} and fetched with run_result. Concurrency within one run is set per call with maxParallel.
            Every run is captured to a run directory unless --no-capture. A call SELECTS
            or COMPOSES from the configured adapters; it can never change any configuration
  init      create a local .aimesh/ state directory (adapter locations, run artifacts), VCS-excluded

modes:
  %s

global flags:
  -h, --help  show this help on stdout and exit 0 (asking for help is not a usage error)
  --version   print the version ( --version --json for the machine-readable form )

Adapters are discovered from meshcore: a roster naming a real adapter (claude-code, codex-cli, ollama,
agy-cli, devin-cli, gemini-cli, cursor-cli) runs the real CLI, and a user-defined ACP instance from the
shared ~/.aimesh/adapters.yaml runs that agent. Any other name is a configuration error, refused before
any spend — it is never silently substituted.

Exit codes (the SHARED aimesh halt taxonomy — the same table reviewmesh uses; docs/architecture.md):
  0 success, 1 gated findings / schema-invalid output, 2 usage error, 3 config, 4 adapter,
  5 model/identity, 6 containment, 7 policy/cap, 8 internal. A code exploremesh does not currently
  produce is reserved, never reused.
`, strings.Join(mode.Names(), ", "))
}

// runVersion prints the build version — the single "exploremesh <version>" line by default, or the full
// machine-readable metadata object for --version --json. It mirrors reviewmesh's `--version` exactly.
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

// resolveAdapters loads the layered adapter config effective for the cwd: shell binary-path overrides
// (user + root-anchored project scope) and the user-defined ACP adapter instances. A missing config
// yields empty results (binaries looked up on PATH); a present-but-malformed config is a surfaced error.
//
// The two load failures are CLASSIFIED here rather than at the print boundary: meshcore's stores return
// plain errors (they know nothing of any app's exit table), but this surface knows that an unreadable or
// malformed adapters.yaml is a CONFIGURATION fault (exit 3) and not an internal one. A failed Getwd is
// genuinely internal (exit 8).
func resolveAdapters() (map[string]string, map[string]acpagent.Instance, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, fault.Wrap(fault.Internal, "resolve the working directory", err)
	}
	paths, err := adapterlocations.ResolvePaths(cwd)
	if err != nil {
		return nil, nil, fault.Wrap(fault.Config, "read the shared adapter locations", err)
	}
	acpInsts, err := registry.ResolveACPInstances(cwd)
	if err != nil {
		return nil, nil, fault.Wrap(fault.Config, "read the configured ACP adapters", err)
	}
	return paths, acpInsts, nil
}

// resolvedRun is the outcome of resolving a run's plan from the roster/profile/count flags: the executable
// plan, the resolved profile's defaultMode (the mode a run uses when --mode is omitted; "" for a raw
// --roster file), a human sourceLabel for messages, and the selected vs full explorer counts (so a caller
// can print the SELECTED panel and warn on a --count subset).
type resolvedRun struct {
	plan        roster.Plan
	defaultMode string
	sourceLabel string
	selected    int
	full        int
}

// errProfileUnconfigured marks the shipped-unconfigured-profile refusal so doctor can render it as a
// failing readiness CHECK (mirroring reviewmesh's "[FAIL] profile: default ready") instead of an error.
// It is a *fault.Fault so the sentinel carries its own exit class: a wrapper built with %w keeps both
// errors.Is(err, errProfileUnconfigured) AND fault.CodeOf(err) == fault.Config working.
var errProfileUnconfigured = fault.New(fault.Config, "profile is unconfigured")

// resolveRun resolves the run plan by the precedence --roster (a raw single roster file) > --profile (a
// named profile from the profiles file) > the default profile, then applies --count (top-N by preference).
// count is the raw flag value: "" or "all" selects every explorer, an integer selects the top-N (out of
// range / <2 is a clear error — never clamped, §7). --roster and --profile are mutually exclusive (the
// caller enforces this before calling).
func resolveRun(rosterPath, profileName, count string) (resolvedRun, error) {
	src, mode, label, err := resolveSource(rosterPath, profileName)
	if err != nil {
		return resolvedRun{}, err
	}
	// An UNCONFIGURED profile (the shipped `default` — no explorers, no collator) cannot run. This is
	// the deliberate fresh-install posture: nothing — not even a deterministic demo — runs until the
	// user configures a panel. Refused here with guidance, before SelectTopN turns it into a
	// confusing count error. (resolveSource stays error-free for this case so `ui` can open on it.)
	if src.IsZero() && rosterPath == "" {
		return resolvedRun{}, fmt.Errorf("%w: %s has no explorers or collator configured — it ships unconfigured; configure a panel with `aimesh explore setup --profile default --explorer adapter=<n>,model=<m> --explorer ... --collator adapter=<n>,model=<m>`", errProfileUnconfigured, label)
	}
	full := len(src.Explorers)
	n := full
	switch strings.TrimSpace(count) {
	case "", "all":
		n = full
	default:
		v, perr := strconv.Atoi(strings.TrimSpace(count))
		if perr != nil {
			return resolvedRun{}, fault.New(fault.Usage, fmt.Sprintf("invalid --count %q — want an integer or \"all\"", count))
		}
		n = v
	}
	plan, err := src.SelectTopN(n)
	if err != nil {
		return resolvedRun{}, err
	}
	return resolvedRun{plan: plan, defaultMode: mode, sourceLabel: label, selected: n, full: full}, nil
}

// resolveSource resolves the SOURCE roster (its ordered explorers = preference order) + the profile
// defaultMode + a human label, by the precedence --roster > --profile > default profile. A raw --roster
// file carries no defaultMode. profile.Resolve discovers the profiles.yaml (project preferred over user),
// else a legacy roster.yaml migrated to a `default` profile, else the built-in unconfigured default.
func resolveSource(rosterPath, profileName string) (r roster.Roster, defaultMode, label string, err error) {
	if rosterPath != "" {
		loaded, lerr := roster.Load(rosterPath)
		if lerr != nil {
			return roster.Roster{}, "", "", lerr
		}
		return loaded, "", fmt.Sprintf("roster file %s", rosterPath), nil
	}
	cwd, cerr := os.Getwd()
	if cerr != nil {
		return roster.Roster{}, "", "", fault.Wrap(fault.Internal, "resolve the working directory", cerr)
	}
	set, serr := profile.Resolve(cwd)
	if serr != nil {
		return roster.Roster{}, "", "", serr
	}
	if profileName != "" {
		p, ok := set.Get(profileName)
		if !ok {
			return roster.Roster{}, "", "", fault.New(fault.Config, fmt.Sprintf("no profile named %q — configured profiles: %s", profileName, strings.Join(set.Names(), ", ")))
		}
		return p.Roster(), p.DefaultMode, fmt.Sprintf("profile %q", profileName), nil
	}
	p, derr := set.Default()
	if derr != nil {
		return roster.Roster{}, "", "", derr
	}
	return p.Roster(), p.DefaultMode, fmt.Sprintf("profile %q (default)", set.DefaultProfile), nil
}

// printSelected states the panel a run will ACTUALLY execute, before any spend: its source (profile or
// roster file), the selected explorers in attribution order, and the collator. When --count selected a
// subset it also says how many configured explorers were left out — explorer order is SELECTION priority,
// so reordering the full list can change WHICH explorers a subset selects (design §7) and a run must never
// leave that implicit. It writes to the diagnostic stream so it never contaminates --json stdout.
func printSelected(w io.Writer, run resolvedRun) {
	fmt.Fprintf(w, "panel: %d of %d explorer(s) from %s\n", run.selected, run.full, run.sourceLabel)
	for _, e := range run.plan.Explorers {
		fmt.Fprintf(w, "  %s\n", slotDisplay(listSlot{Adapter: e.Adapter, Model: e.Model, Effort: e.Effort}))
	}
	c := run.plan.Collator
	fmt.Fprintf(w, "  collator: %s\n", slotDisplay(listSlot{Adapter: c.Adapter, Model: c.Model, Effort: c.Effort}))
	// The CANONICALIZER identities, when they were named explicitly. When they were not, the host derives
	// them from the collator + the panel's preference order at run time — so stating "derived" here is the
	// honest thing to print, rather than pre-computing a choice the mode may not even make (a fixed-space
	// mode runs no canonicalizer at all).
	if len(run.plan.Canonicalizers) > 0 {
		fmt.Fprintln(w, "  canonicalizers (explicit):")
		for _, cz := range run.plan.Canonicalizers {
			fmt.Fprintf(w, "    %s\n", slotDisplay(listSlot{Adapter: cz.Adapter, Model: cz.Model, Effort: cz.Effort}))
		}
	} else {
		fmt.Fprintln(w, "  canonicalizers: derived (slot a: the collator; slot b: the first explorer by preference order that differs from it)")
	}
	if run.selected < run.full {
		fmt.Fprintf(w, "  note: --count selected the top %d by preference order; %d configured explorer(s) were NOT run\n",
			run.selected, run.full-run.selected)
	}
}

// loadPlan resolves the executable plan for a surface that takes only an optional --roster (doctor, acp):
// an explicit raw roster file, else the DEFAULT profile (profiles.yaml → a migrated legacy roster.yaml →
// the built-in unconfigured default), with every explorer selected. It is profile-aware, so those
// surfaces bind to the SAME config a no-flag `explore` does (§F10).
func loadPlan(rosterPath string) (roster.Plan, error) {
	run, err := resolveRun(rosterPath, "", "")
	if err != nil {
		return roster.Plan{}, err
	}
	return run.plan, nil
}

// unionPlan folds every profile's explorers + collator into base, producing a synthetic plan used ONLY to
// build an adapter registry that covers every profile a surface can select at run time (the ACP agent).
// The registry is keyed by adapter NAME, so a union changes nothing about how registry.Build resolves or
// fails closed — it only widens the set of names checked. The result is NEVER executed as a panel.
func unionPlan(base roster.Plan, set profile.Set) roster.Plan {
	out := roster.Plan{Explorers: append(roster.AttributionOrdered(nil), base.Explorers...), Collator: base.Collator}
	for _, name := range set.Names() {
		p := set.Profiles[name]
		out.Explorers = append(out.Explorers, p.Explorers...)
		// A collator's adapter must resolve too; Build resolves by name, so carrying it as an explorer
		// entry registers the same adapter (the synthetic plan is never run).
		out.Explorers = append(out.Explorers, roster.Explorer{Adapter: p.Collator.Adapter, Model: p.Collator.Model, Effort: p.Collator.Effort})
		// Same for a profile's CANONICALIZER adapters: a call may select that profile, so its canonicalizer
		// adapters must be resolvable at startup or the run halts at pre-flight over a name the server could
		// have bound.
		out.Explorers = append(out.Explorers, p.Canonicalizers...)
	}
	return out
}

// namesPlan builds a SYNTHETIC plan whose only purpose is to make registry.Build resolve a set of adapter
// NAMES (the registry is keyed by name; models are irrelevant to resolution). It is never executed as a
// panel — it is how a surface that accepts an ad-hoc composition binds the full configured adapter set at
// startup, so the composition can be checked against it without re-deriving adapter resolution rules.
func namesPlan(names []string) roster.Plan {
	out := roster.Plan{}
	for _, n := range names {
		out.Explorers = append(out.Explorers, roster.Explorer{Adapter: n, Model: "-"})
	}
	return out
}

func runExplore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("explore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cliflags.Style(fs, "aimesh explore run [question]")
	purpose := fs.String("purpose", "", "the exploration purpose. May instead be given as the positional argument: `aimesh explore run \"how should we shard this?\" --criteria ...`")
	criteria := fs.String("criteria", "", "comma-separated criteria the response must satisfy (REQUIRED, and never invented for you: criteria are what the explorers are judged against, so a run that let the tool choose them would be grading its own homework)")
	modeFlag := fs.String("mode", "", "exploration mode; unknown names are rejected. default: the profile's defaultMode, else map. known modes: "+strings.Join(mode.Names(), ", "))
	priorContext := fs.String("prior-context", "", "optional prior context (e.g. a prior exploration's result)")
	artifactPath := fs.String("artifact", "", "path to the ARTIFACT UNDER REVIEW (or - for stdin) — required by --mode challenge, ignored by modes that do not review one")
	// The FIXED-SPACE declarations (design §3 Compare + Forecast rows): the option set / criteria /
	// estimation target the user fixes BEFORE any explorer speaks. They are what make those modes need no
	// canonicalizer, so they are required by those modes and ignored by every other.
	optionSet := fs.String("options", "", "comma-separated DECLARED OPTION SET — required by --mode compare, which evaluates exactly these options")
	var criterionSpecList criterionSpecs
	fs.Var(&criterionSpecList, "criterion", "declared comparison criterion as name=<n>,direction=higher_is_better|lower_is_better[,role=dimension|filter][,weight=<w>] (repeatable) — required by --mode compare. A weight on EVERY scored criterion is what licenses a scalar ranking; without weights the result is the Pareto/trade-off view")
	target := fs.String("target", "", "the DECLARED estimation target (what is being estimated) — required by --mode forecast")
	unit := fs.String("unit", "", "the unit every estimate must be given in — required by --mode forecast")
	horizon := fs.String("horizon", "", "the period the estimate is for — required by --mode forecast")
	conditioning := fs.String("conditioning", "", "optional conditioning event for --mode forecast (\"assume this holds\")")
	rosterPath := fs.String("roster", "", "optional raw roster file (YAML/JSON); mutually exclusive with --profile")
	profileName := fs.String("profile", "", "named profile from the profiles file (default: the default profile); mutually exclusive with --roster")
	countFlag := fs.String("count", "", "select the top-N explorers by preference order (an integer, or \"all\"; default: all). Out-of-range fails; <2 is rejected")
	var explorerSpecs slotSpecs
	fs.Var(&explorerSpecs, "explorer", "ad-hoc explorer as adapter=<name>,model=<m>[,effort=<e>] (repeatable; needs >=2 with a --collator; mutually exclusive with --roster)")
	collatorSpec := fs.String("collator", "", "ad-hoc collator as adapter=<name>,model=<m>[,effort=<e>] (used with >=2 --explorer)")
	var canonicalizerSpecs slotSpecs
	fs.Var(&canonicalizerSpecs, "canonicalizer", "canonicalizer identity as adapter=<name>,model=<m>[,effort=<e>] (repeatable; supply exactly 2 or none). Two INDEPENDENT identities propose the canonicalization and a merge holds only if both propose it. Omitted: the host derives them (slot a = the collator, slot b = the first explorer by PREFERENCE order that differs from it). Overrides the profile's canonicalizers")
	asJSON := fs.Bool("json", false, "print the full result as JSON instead of a human summary")
	dryRun := fs.Bool("dry-run", false, "resolve everything, spend nothing: print the panel this exploration would convene, every stage it would call, the exact model-call total, and the exact round-1 prompt every explorer would receive — then stop BEFORE the identity pre-flight, which is this run's first model call. Every configuration error a real run would hit for free is reported here, including a dual-canonicalizer plan that cannot yield two independent identities. Unlike a review's dry run it does NOT prove the collator answered: proving that would mean calling it")
	dumpRun := fs.Bool("dump-run", false, "record the run (envelopes, raw outputs, prompts, a versioned manifest) under $EXPLOREMESH_ARTIFACT_DIR (default: the project-local .aimesh/explore/runs/ when a .aimesh state dir exists — run `init` once — else a subdirectory of the OS temp dir) for replay/fixtures")
	debug := fs.Bool("debug", false, "print a per-adapter-call diagnostic (resolved argv, exit code, identity, captured stderr/stdout) to stderr — for troubleshooting an adapter/provider")
	maxParallel := fs.Int("max-parallel", 0, "how many explorers may invoke their model CLI at once (default: the whole panel in parallel). Lower it when this machine cannot host that many provider CLIs at once — each is a real subprocess, and a local model also loads weights — or to stay under a provider rate limit. It bounds PARALLELISM only: every explorer still runs, so only the wall clock changes")
	// SPLIT BEFORE PARSING, so the advertised positional does not silently disarm every flag after
	// it. Go's parser stops at the first non-flag token, so `explore run "a question" --criteria x`
	// parsed ZERO flags: the criteria were dropped without a word and the run was then refused for
	// having none — blaming the user for a flag they had typed.
	flagArgs, positionals := cliflags.SplitArgs(fs, args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	// THE POSITIONAL IS THE PURPOSE. The usage line has always advertised `[question]` and nothing
	// ever read it, so the documented shorthand refused every time it was used.
	if len(positionals) > 0 {
		joined := strings.TrimSpace(strings.Join(positionals, " "))
		switch {
		case strings.TrimSpace(*purpose) != "" && joined != "":
			// Two purposes is not a thing to guess between: picking one silently would run an
			// exploration the user did not ask for, and they pay for it.
			fmt.Fprintf(stderr, "aimesh explore: the purpose was given twice — as --purpose %q and as the positional %q. Give it once.\n", *purpose, joined)
			return int(fault.Usage)
		case joined != "":
			*purpose = joined
		}
	}
	// Ad-hoc by-identifier run (design §8): >=2 --explorer + a --collator build a one-off roster, bypassing
	// --roster/--profile (all mutually exclusive). --roster and --profile are also mutually exclusive.
	adHoc := len(explorerSpecs) > 0 || strings.TrimSpace(*collatorSpec) != ""
	if adHoc && (*rosterPath != "" || *profileName != "") {
		fmt.Fprintln(stderr, "aimesh explore: --explorer/--collator (ad-hoc roster) cannot be combined with --roster or --profile")
		return int(fault.Usage)
	}
	if *rosterPath != "" && *profileName != "" {
		fmt.Fprintln(stderr, "aimesh explore: --roster and --profile are mutually exclusive")
		return int(fault.Usage)
	}

	// Resolve the plan + the profile's defaultMode (precedence ad-hoc > --roster > --profile > default),
	// applying --count (top-N by preference) BEFORE any spend so an out-of-range count fails clearly.
	var run resolvedRun
	var err error
	if adHoc {
		r, berr := buildAdHocRoster(explorerSpecs, *collatorSpec)
		if berr != nil {
			fmt.Fprintf(stderr, "aimesh explore: %v\n", berr)
			return codeOf(berr)
		}
		full := len(r.Explorers)
		n := full
		if c := strings.TrimSpace(*countFlag); c != "" && c != "all" {
			v, perr := strconv.Atoi(c)
			if perr != nil {
				fmt.Fprintf(stderr, "aimesh explore: invalid --count %q — want an integer or \"all\"\n", *countFlag)
				return int(fault.Usage)
			}
			n = v
		}
		plan, perr := r.SelectTopN(n) // enforces unique triples / non-empty fields + count range
		if perr != nil {
			fmt.Fprintf(stderr, "aimesh explore: %v\n", perr)
			return codeOf(perr)
		}
		run = resolvedRun{plan: plan, sourceLabel: "ad-hoc explorers", selected: n, full: full}
	} else {
		run, err = resolveRun(*rosterPath, *profileName, *countFlag)
		if err != nil {
			fmt.Fprintf(stderr, "aimesh explore: %v\n", err)
			return codeOf(err)
		}
	}
	// The explicit CANONICALIZER identities (design §4), resolved BEFORE any spend. An explicit
	// --canonicalizer pair REPLACES whatever the resolved profile carries — same precedence as every other
	// flag over its profile value — and a malformed pair (one entry, two identical identities) is a usage
	// error here rather than a halt after a panel has been paid for.
	if len(canonicalizerSpecs) > 0 {
		cs, cerr := buildCanonicalizers(canonicalizerSpecs)
		if cerr != nil {
			fmt.Fprintf(stderr, "aimesh explore: %v\n", cerr)
			return codeOf(cerr)
		}
		run.plan.Canonicalizers = cs
	}
	plan := run.plan

	// Resolve the effective --mode BEFORE spending: an explicit --mode wins; otherwise the resolved
	// profile's defaultMode supplies it; otherwise it stays empty (the pipeline defaults to map). A
	// non-empty resolved mode must be a registered mode (a usage error listing the known modes).
	modeName := strings.TrimSpace(*modeFlag)
	if modeName == "" {
		modeName = strings.TrimSpace(run.defaultMode)
	}
	if modeName != "" {
		if _, ok := mode.Lookup(modeName); !ok {
			fmt.Fprintf(stderr, "aimesh explore: unknown mode %q (known modes: %s)\n", modeName, strings.Join(mode.Names(), ", "))
			return int(fault.Usage)
		}
	}
	// The ARTIFACT UNDER REVIEW (design §3 Challenge row): read from a file, or from stdin for `-`, BEFORE any
	// spend. Read failures and an empty artifact are usage errors here rather than a halt mid-run.
	artifact, aerr := readArtifact(*artifactPath, os.Stdin)
	if aerr != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", aerr)
		return int(fault.Usage)
	}
	// The declared comparison criteria are parsed BEFORE the task is assembled, so a malformed spec is a
	// usage error at the flag rather than a mode-contract failure later.
	compareCriteria, cerr := parseCriteria(criterionSpecList)
	if cerr != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", cerr)
		return int(fault.Usage)
	}
	raw := schema.RawTask{
		Purpose:      strings.TrimSpace(*purpose),
		Criteria:     splitCSV(*criteria),
		PriorContext: strings.TrimSpace(*priorContext),
		Mode:         modeName,
		Artifact:     artifact,

		Options:           splitCSV(*optionSet),
		CompareCriteria:   compareCriteria,
		Target:            strings.TrimSpace(*target),
		Unit:              strings.TrimSpace(*unit),
		Horizon:           strings.TrimSpace(*horizon),
		ConditioningEvent: strings.TrimSpace(*conditioning),
	}
	if err := raw.Validate(); err != nil {
		// TEACH THE WHOLE COMMAND, not just the missing field. Both of these are required, both are
		// the FIRST thing a new user meets, and a bare "empty purpose" leaves them to reconstruct
		// the shape from the help text. Criteria in particular are refused rather than invented on
		// purpose — they are what the explorers are judged against — so the message says why.
		fmt.Fprintf(stderr, "aimesh explore: %v\n\n"+
			"An exploration needs a PURPOSE (what to explore) and CRITERIA (what a good answer must satisfy):\n"+
			"  aimesh explore run \"how should we shard the write path?\" --criteria correctness,operational-cost\n"+
			"  aimesh explore run --purpose \"...\" --criteria a,b --mode map\n\n"+
			"The criteria are never chosen for you: they are the standard every explorer is judged against, so a\n"+
			"run that let the tool pick them would be marking its own work.\n", err)
		return int(fault.Usage)
	}
	// The MODE's own task requirement (e.g. challenge needs an artifact), checked before the panel is touched
	// so a missing input costs nothing. The pipeline re-checks it; this is the friendly, early message.
	if err := mode.ValidateTask(modeName, raw); err != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", err)
		return int(fault.Usage)
	}

	// Print the SELECTED panel (attribution order) + warn when a --count subset drops explorers from the
	// full profile — a reorder of the full list can change WHICH explorers a subset selects (design §7).
	printSelected(stderr, run)
	paths, acpInsts, err := resolveAdapters()
	if err != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", err)
		return codeOf(err)
	}
	reg, unknown := registry.Build(plan, paths, acpInsts, 0)
	if len(unknown) > 0 {
		// Fail closed BEFORE spending: an unrecognized adapter is a config error, not a silent substitute.
		// A by-identifier composition (--explorer/--collator/--canonicalizer) names the configured adapters to
		// choose from — the same set `list` surfaces — because there the caller wrote the name on the command
		// line and can correct it directly.
		if adHoc || len(canonicalizerSpecs) > 0 {
			fmt.Fprintf(stderr, "aimesh explore: unknown adapter(s) in --explorer/--collator/--canonicalizer: %s — configured adapters are: %s\n", strings.Join(unknown, ", "), strings.Join(configuredAdapterNames(acpInsts), ", "))
		} else {
			fmt.Fprintf(stderr, "aimesh explore: unknown adapter(s) in the roster: %s — configure them (setup) or fix the roster\n", strings.Join(unknown, ", "))
		}
		return int(fault.Config)
	}

	ctx := context.Background()
	if *debug {
		ctx = shell.WithDebug(ctx, stderr)
	}
	if *maxParallel < 0 {
		fmt.Fprintln(stderr, "aimesh explore: --max-parallel must be at least 1 (omit it to run the whole panel in parallel)")
		return int(fault.Usage)
	}
	// --dump-run captures an EXPLORATION's artifacts — envelopes, raw model outputs, prompts, a versioned
	// manifest. A dry run produces none of them, so the combination is refused rather than quietly writing a
	// run directory that records an exploration nobody performed.
	if *dryRun && *dumpRun {
		fmt.Fprintln(stderr, "aimesh explore: --dry-run cannot be combined with --dump-run — a dry run makes no model calls, so there are no envelopes, raw outputs or later-round prompts to capture; the shape goes to stdout (--json for the machine-readable form)")
		return int(fault.Usage)
	}
	res, rerr := pipeline.Run(ctx, reg, plan, raw, pipeline.Options{MaxParallel: *maxParallel, DryRun: *dryRun}, nil)

	// Opt-in capture, on BOTH success and halt paths (a halted run is a valuable fixture). The
	// `run:` line goes to STDERR so it never contaminates `--json` stdout.
	if *dumpRun {
		// The artifact directory is resolved by internal/capture, so the CLI, the ACP surface
		// (`_meta.exploremesh.dumpRun`) and the MCP job registry all write their runs to the same place.
		dir := capture.ArtifactDir()
		if run, aerr := audit.NewRun(dir, "", time.Now()); aerr != nil {
			fmt.Fprintf(stderr, "aimesh explore: dump-run: %v\n", aerr)
		} else {
			status, faultMsg := "complete", ""
			if rerr != nil {
				status, faultMsg = "halted", rerr.Error()
			}
			// The DECLARED TASK travels with the dump: §9's derived export is built from the run directory
			// alone, and a fixed-space result cannot be reproduced without the space it was computed over.
			if derr := capture.Dump(capture.Input{Run: run, Plan: plan, Result: res, Status: status, Fault: faultMsg, Task: raw}); derr != nil {
				fmt.Fprintf(stderr, "aimesh explore: dump-run failed: %v\n", derr)
			} else {
				fmt.Fprintf(stderr, "run: %s\n", run.Dir)
			}
		}
	}

	if rerr != nil {
		// A halt still carries partial state; report it clearly. The exit code is the HALT'S OWN CLASS
		// (the pipeline builds its halts as *fault.Fault values), so a halted `explore` is now
		// distinguishable by class instead of collapsing to the old undifferentiated 1.
		fmt.Fprintf(stderr, "aimesh explore: exploration halted: %v\n", rerr)
		printPartial(stderr, res)
		return codeOf(rerr)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return int(fault.OK)
	}
	if res.Shape != nil {
		printShape(stdout, *res.Shape)
		return int(fault.OK)
	}
	printSummary(stdout, res)
	return int(fault.OK)
}

// printShape renders a dry run's disclosure: the panel, every stage that would be called, the exact
// model-call total, and the exact prompt the blind round would carry.
//
// It states ONE call total rather than a range, unlike reviewmesh's, and the line under it says why: an
// exploration's round count is fixed by its mode contract, so there is nothing left to estimate. It also
// says what the number does NOT include, because an undisclosed exclusion is how a cost estimate becomes an
// untrue one — and it says plainly that the pre-flight was not run, since the temptation with a dry run is
// to read it as proof the panel works.
func printShape(out io.Writer, s pipeline.Shape) {
	fmt.Fprintf(out, "dry run: nothing was spent. This is what %s mode would do.\n\n", s.Mode)

	parallel := "all at once"
	if s.MaxParallel > 0 && s.MaxParallel < len(s.Explorers) {
		parallel = fmt.Sprintf("%d at once", s.MaxParallel)
	}
	fmt.Fprintf(out, "  blind panel — %d explorer(s), %s\n", len(s.Explorers), parallel)
	for _, e := range s.Explorers {
		fmt.Fprintf(out, "    %s\n", slotDisplay(listSlot{Adapter: e.Adapter, Model: e.Model, Effort: e.Effort}))
	}
	fmt.Fprintf(out, "  collator: %s\n", slotDisplay(listSlot{Adapter: s.Collator.Adapter, Model: s.Collator.Model, Effort: s.Collator.Effort}))
	for _, c := range s.Canonicalizers {
		fmt.Fprintf(out, "  %s: %s  (%s)\n", c.Role,
			slotDisplay(listSlot{Adapter: c.Identity.Adapter, Model: c.Identity.Model, Effort: c.Identity.Effort}),
			s.CanonicalizerProvenance)
	}
	// The best moment to say this is here, before the panel is paid for: changing the pair still costs
	// nothing, and after the run the same fact only explains a count the reader already has.
	if s.CanonicalizerIndependence == pipeline.IndependenceSharedModel {
		fmt.Fprintln(out, "    NOTE: both canonicalizers run the same model, reached through different adapters.")
		fmt.Fprintln(out, "    A merge held by their agreement is weaker evidence than one held across two")
		fmt.Fprintln(out, "    different models — same priors, correlated errors — and every corroboration")
		fmt.Fprintln(out, "    count in the result rests on it. Name a second model with --canonicalizer to change it.")
	}

	fmt.Fprintf(out, "  rounds: %d (fixed by the mode contract)\n", s.Rounds)
	fmt.Fprintf(out, "  policy: dual=%t confirm=%t ballot=%t, terminal=%s\n\n", s.Policy.Dual, s.Policy.Confirm, s.Policy.Ballot, s.Policy.Terminal)

	fmt.Fprintln(out, "  stages, in order:")
	for _, c := range s.Calls {
		label := c.Phase
		if c.Round > 0 {
			label = fmt.Sprintf("%s (round %d)", c.Phase, c.Round)
		}
		fmt.Fprintf(out, "    %-22s %2d call(s) — %s\n", label, c.Calls, c.Detail)
	}
	fmt.Fprintf(out, "\n  model calls: %d exactly.\n", s.ModelCalls)
	fmt.Fprintln(out, "    Not a range: an exploration's round count is fixed by its mode contract, and every")
	fmt.Fprintln(out, "    other multiplier — panel size, dual canonicalization, the confirmation round — is")
	fmt.Fprintln(out, "    settled before the run starts. Only a HALT makes it fewer. It does not count a")
	fmt.Fprintln(out, "    retry after a schema-invalid response.")

	fmt.Fprintf(out, "\n  round-1 payload: %s, identical for every explorer (sha256 %s)\n",
		humanBytes(s.Payload.PromptBytes), short(s.Payload.PayloadHash))
	fmt.Fprintf(out, "    response schema: %s\n", strings.Join(s.Payload.SchemaFields, ", "))
	if s.Rounds > 1 {
		fmt.Fprintln(out, "    Rounds 2+ carry the pooled confirmed-canonical uniques, which are derived from what")
		fmt.Fprintln(out, "    the panel says in round 1 — they cannot be shown here because they do not exist yet.")
	}
	fmt.Fprintln(out, "    Full prompt: --json.")

	fmt.Fprintln(out, "\n  The identity pre-flight was NOT run: it is a real model call per governed role, and it")
	fmt.Fprintln(out, "  is the first thing a real run spends on. So this says nothing left between here and that")
	fmt.Fprintln(out, "  call is a configuration question — not that the collator answered.")
	fmt.Fprintln(out, "\nDrop --dry-run to run it.")
}

// humanBytes renders a byte count for a person reading a cost disclosure. It stays exact under a kilobyte,
// because "0.4 KB" is a worse answer than "412 B" for a short prompt.
func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}

// short clips a hex digest to its leading bytes for a human line — enough to compare two runs at a glance,
// while the full value stays in --json.
func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

// doctorCheckJSON is one readiness check in the `doctor --json` projection.
type doctorCheckJSON struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// doctorProbeJSON is one adapter's --probe outcome, projected with the TYPED model.ProbeResult fields
// kept apart: `signal` is the machine-readable classified blocker a caller may branch on, `detail` is
// human prose that must never be parsed, and `stage` says how far the probe got (resolve / spawn /
// initialize / session_new / version) — the single most useful field when a probe fails, and the one
// meshcore's Check projection has to fold away because a Check carries only a name/ok/detail.
type doctorProbeJSON struct {
	Adapter string `json:"adapter"`
	OK      bool   `json:"ok"`
	Stage   string `json:"stage,omitempty"`
	Signal  string `json:"signal,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// Deep marks a row produced by the DEEP probe: a real, bounded model invocation in a throwaway
	// isolated directory, which SPENT tokens. A consumer that treats readiness data as free must be
	// able to tell it apart from the `--version` row beside it.
	Deep bool `json:"deep,omitempty"`
}

// doctorJSON is the `doctor --json` projection: the overall verdict, the ordered checks, and (only with
// --probe) the per-adapter probe results. It is additive by construction — a consumer reads `ok` for the
// verdict and the arrays for the detail — and it says exactly what the exit code says (`ok:false` ⇔ the
// config exit code), so a script never has to choose which one to trust.
type doctorJSON struct {
	OK     bool              `json:"ok"`
	Checks []doctorCheckJSON `json:"checks"`
	Probes []doctorProbeJSON `json:"probes,omitempty"`
}

// doctorProbeTimeout bounds the WHOLE --probe pass. Each adapter's probe is already individually
// watchdogged (a shell recipe's `--version` under 10s, an ACP handshake under its own budget), so this
// is the outer guarantee that `doctor --probe` terminates even with a large panel of slow CLIs.
const doctorProbeTimeout = 90 * time.Second

// doctorDeepProbeTimeout bounds the WHOLE --probe-deep pass. Each adapter's deep probe is itself
// bounded (a real invocation under a 2-minute budget), so this is the outer guarantee that the command
// terminates even with a large panel of slow CLIs — a human is waiting on a diagnostic.
const doctorDeepProbeTimeout = 10 * time.Minute

// emitDoctor renders a finished report on the caller's chosen surface and returns the exit code. The
// exit SEMANTICS are identical for text and --json: a failing report is a configuration fault (3), the
// same class reviewmesh's `doctor` exits on. --json writes ONLY the projection to stdout.
func emitDoctor(stdout io.Writer, rep mdoctor.Report, probes []doctorProbeJSON, asJSON, probed bool) int {
	if asJSON {
		out := doctorJSON{OK: rep.OK, Checks: make([]doctorCheckJSON, 0, len(rep.Checks)), Probes: probes}
		for _, c := range rep.Checks {
			out.Checks = append(out.Checks, doctorCheckJSON{Name: c.Name, OK: c.OK, Detail: c.Detail})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		fmt.Fprint(stdout, rep.String())
		if probed {
			printProbes(stdout, probes)
		}
	}
	if !rep.OK {
		return int(fault.Config)
	}
	return int(fault.OK)
}

// printProbes renders the per-adapter probe detail under the report. The report itself already carries a
// pass/fail `probe: <adapter>` check per adapter (meshcore composed it); this block adds the two fields a
// Check cannot carry — the STAGE reached and the classified SIGNAL — because "failed" without "at which
// stage" is the difference between a missing binary and a CLI sitting on a login prompt.
func printProbes(w io.Writer, probes []doctorProbeJSON) {
	fmt.Fprintln(w, "Probe detail (the `--version` rows are safe: no model call, no token spend, no authentication):")
	if len(probes) == 0 {
		fmt.Fprintln(w, "  (no required adapter was probeable — nothing was started)")
		return
	}
	deepSeen := false
	for _, p := range probes {
		state := "ok  "
		if !p.OK {
			state = "FAIL"
		}
		kind := ""
		if p.Deep {
			kind, deepSeen = " (DEEP — a real invocation; tokens were spent)", true
		}
		line := fmt.Sprintf("  [%s] %s%s", state, p.Adapter, kind)
		if p.Stage != "" {
			line += " — stage " + p.Stage
		}
		if p.Signal != "" {
			line += "; signal " + p.Signal
		}
		if p.Detail != "" {
			line += "; " + p.Detail
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "  note: a `--version` probe proves the CLI starts and answers — it does NOT prove provider auth,")
	fmt.Fprintln(w, "        model availability or model identity. A timeout usually means that CLI still needs its own")
	fmt.Fprintln(w, "        native login or folder-trust setup; complete it in that CLI, then re-run.")
	if deepSeen {
		fmt.Fprintln(w, "  note: a DEEP row came from ONE real invocation in a throwaway, git-initialized directory — the")
		fmt.Fprintln(w, "        shape every run actually gets. No interactive trust or login prompt was answered for you;")
		fmt.Fprintln(w, "        a folder_trust / login_required signal is asking YOU to complete that CLI's setup once.")
	}
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cliflags.Style(fs, "aimesh explore doctor")
	rosterPath := fs.String("roster", "", "optional raw roster file to check; mutually exclusive with --profile")
	profileName := fs.String("profile", "", "named profile to check (default: the default profile); mutually exclusive with --roster")
	probe := fs.Bool("probe", false, "additionally run a SAFE, bounded, no-model readiness probe of each required adapter (a shell recipe's `<binary> --version`, an ACP instance's handshake) — no token spend, no authentication")
	// A SEPARATE flag, not a stronger --probe: --probe is documented everywhere as free, and a flag
	// that costs nothing today must not start costing money because a better probe was invented.
	probeDeep := fs.Bool("probe-deep", false, "additionally run the DEEP probe: one REAL, bounded model invocation per required adapter in a throwaway isolated directory, through the same path a run takes. SPENDS REAL TOKENS. It answers what `--version` cannot — whether the CLI does real work where a run actually happens. It NEVER answers an interactive trust/login prompt; it detects, classifies and reports the fix")
	asJSON := fs.Bool("json", false, "emit the machine-readable projection (ok + checks[] + probes[]) instead of the human report; the exit code is unchanged")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	if *rosterPath != "" && *profileName != "" {
		fmt.Fprintln(stderr, "aimesh explore: --roster and --profile are mutually exclusive")
		return int(fault.Usage)
	}
	// Check the FULL configured panel (no --count): doctor reports readiness of everything a run could
	// select, not of one subset.
	run, err := resolveRun(*rosterPath, *profileName, "")
	if errors.Is(err, errProfileUnconfigured) {
		// The shipped-unconfigured posture is a READINESS finding, not a usage error — render it as a
		// failing check (mirroring reviewmesh's "[FAIL] profile: default ready" on a fresh install).
		var rep mdoctor.Report
		rep.Checks = append(rep.Checks, mdoctor.Check{Name: "profile: configured", OK: false, Detail: err.Error()})
		rep.Finalize()
		return emitDoctor(stdout, rep, nil, *asJSON, false)
	}
	if err != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", err)
		return codeOf(err)
	}
	plan := run.plan
	paths, acpInsts, err := resolveAdapters()
	if err != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", err)
		return codeOf(err)
	}
	reg, unknown := registry.Build(plan, paths, acpInsts, 0)
	adapters := map[string]model.Adapter{}
	var names []string
	required := map[string]bool{}
	for name, a := range reg {
		adapters[name] = a
		names = append(names, name)
		required[name] = true
	}
	// stable order
	sortStrings(names)

	var rep mdoctor.Report
	rep.Checks = append(rep.Checks, mdoctor.Check{Name: "roster: explorers >= 2", OK: len(plan.Explorers) >= 2, Detail: fmt.Sprintf("%d explorers", len(plan.Explorers))})
	rep.Checks = append(rep.Checks, canonicalizerCheck(plan, run.defaultMode))
	// Fail-closed: an unrecognized adapter is a failing check with guidance, never a silent fake.
	for _, u := range unknown {
		rep.Checks = append(rep.Checks, mdoctor.Check{Name: "adapter: " + u, OK: false, Detail: "unknown adapter — not a CLI recipe or a configured ACP instance; configure it (setup) or fix the roster"})
	}
	rep.Checks = append(rep.Checks, mdoctor.AdapterAvailability(adapters, names, required)...)

	// OPT-IN probe. It is the one thing `doctor` does that starts a process, so it happens only behind
	// --probe, is bounded by a ctx the whole pass shares, and drives EVERY adapter through meshcore's
	// doctor.ProbeAdapters — the same primitive reviewmesh wires (internal/utility/doctor/doctor.go), so
	// neither app can drift in which adapters it probes or how a result becomes a check.
	var probes []doctorProbeJSON
	if *probe || *probeDeep {
		ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
		recorded := map[string]model.ProbeResult{}
		rep.Checks = append(rep.Checks, mdoctor.ProbeAdapters(ctx, recordProbes(adapters, recorded), names, required)...)
		cancel()
		probes = probeProjection(rep.Checks, recorded)
		// The DEEP pass, only on its own explicit opt-in. The cheap ladder above ran first on purpose:
		// an unresolvable binary is named before anything is spent discovering that the expensive way.
		// Seats come from the PLAN, so each adapter is exercised with the model argument a real run
		// would pass — meshcore then spends at most once per adapter.
		if *probeDeep {
			dctx, dcancel := context.WithTimeout(context.Background(), doctorDeepProbeTimeout)
			deepRecorded := map[string]model.ProbeResult{}
			deepChecks := mdoctor.ProbeAdaptersDeep(dctx, recordDeepProbes(adapters, deepRecorded), planDeepSeats(plan))
			dcancel()
			rep.Checks = append(rep.Checks, deepChecks...)
			probes = append(probes, deepProjection(deepChecks, deepRecorded)...)
		}
	}
	rep.Finalize()
	return emitDoctor(stdout, rep, probes, *asJSON, *probe || *probeDeep)
}

// probeRecorder wraps a probeable adapter so meshcore's doctor.ProbeAdapters stays the SINGLE probe
// driver (it decides which adapters are probed, threads the ctx, and composes the checks) while this
// surface still captures the TYPED model.ProbeResult on the way past. Without it the only way to report
// a probe's Stage would be to re-implement ProbeAdapters app-side or to probe every adapter twice — one
// duplicating meshcore's rules, the other starting every CLI two times for one `doctor --probe`.
type probeRecorder struct {
	model.Adapter
	prober model.Prober
	name   string
	into   map[string]model.ProbeResult
}

func (p *probeRecorder) Probe(ctx context.Context) model.ProbeResult {
	r := p.prober.Probe(ctx)
	p.into[p.name] = r
	return r
}

// recordProbes returns a registry in which every PROBEABLE adapter is wrapped to record its typed
// result into `into`. An adapter without the capability is passed through untouched, so ProbeAdapters
// still sees it as un-probeable and reports it honestly as "no probe" rather than being handed a
// wrapper that falsely claims the interface.
func recordProbes(adapters map[string]model.Adapter, into map[string]model.ProbeResult) map[string]model.Adapter {
	out := make(map[string]model.Adapter, len(adapters))
	for name, a := range adapters {
		if pr, ok := a.(model.Prober); ok {
			out[name] = &probeRecorder{Adapter: a, prober: pr, name: name, into: into}
			continue
		}
		out[name] = a
	}
	return out
}

// probeProjection pairs each `probe: <adapter>` check meshcore emitted with the typed result recorded
// for that adapter. The CHECK list is authoritative about WHICH adapters were probed and in what order
// (that is ProbeAdapters' decision, not this surface's); the recorded map supplies Stage/Signal. An
// adapter with no recorded entry was reported as un-probeable, and its check detail is carried verbatim.
// canonicalizerCheck reports the RESOLVED canonicalizer identities for the configured panel — explicitly
// named, or the pair the host would derive. It is a readiness check rather than a footnote because a run's
// merge-agreement rule is only as good as the two identities holding it, and a `doctor` that reported the
// explorers and the collator but stayed silent about the canonicalizers left the one seat whose selection
// had no visible source.
//
// It FAILS only when a no-flag run would actually halt: no independent second canonicalizer can be derived
// AND the profile's default mode is a dual-canonicalizer one. Otherwise the same condition is reported as a
// caveat, because a `map`-only panel is genuinely ready and a doctor that went red over a mode the user
// never runs would turn a real signal into noise.
func canonicalizerCheck(plan roster.Plan, defaultMode string) mdoctor.Check {
	const name = "roster: canonicalizers"
	if len(plan.Canonicalizers) > 0 {
		var names []string
		for _, cz := range plan.Canonicalizers {
			names = append(names, slotDisplay(listSlot{Adapter: cz.Adapter, Model: cz.Model, Effort: cz.Effort}))
		}
		return mdoctor.Check{Name: name, OK: true, Detail: "explicit — " + strings.Join(names, ", ")}
	}
	detail := "derived — a: the collator; b: the first explorer by preference order that differs from it"
	for _, ex := range plan.Preferred {
		if ex.Adapter == plan.Collator.Adapter && ex.Model == plan.Collator.Model {
			continue
		}
		return mdoctor.Check{Name: name, OK: true, Detail: detail + " (b would be " +
			slotDisplay(listSlot{Adapter: ex.Adapter, Model: ex.Model, Effort: ex.Effort}) + ")"}
	}
	// Every explorer shares the collator's model: a canonicalizing mode with a DUAL policy halts rather than
	// manufacture a second opinion from the same weights. Say so before a run pays to find out.
	detail += " — but every explorer shares the collator's model, so no independent second canonicalizer can be derived." +
		" A dual-canonicalizer mode (challenge, shortlist, ai-collab) will HALT. Name two with --canonicalizer, or add an explorer on a different model"
	spec, ok := mode.Resolve(defaultMode)
	return mdoctor.Check{Name: name, OK: !(ok && spec.Canonicalization.Dual), Detail: detail}
}

// planDeepSeats is the deep probe's seat list: every explorer plus the collator, in plan order, each
// carrying the model argument that seat would really be invoked with. meshcore collapses it to one
// probe per ADAPTER — the question ("does this CLI work in a throwaway directory?") is a property of
// the CLI, and probing five seats of one adapter would spend five times to learn one fact.
func planDeepSeats(plan roster.Plan) []mdoctor.DeepSeat {
	seats := make([]mdoctor.DeepSeat, 0, len(plan.Explorers))
	for _, e := range plan.Explorers {
		seats = append(seats, mdoctor.DeepSeat{Adapter: e.Adapter, ModelArg: e.Model, Effort: e.Effort})
	}
	seats = append(seats, mdoctor.DeepSeat{Adapter: plan.Collator.Adapter, ModelArg: plan.Collator.Model, Effort: plan.Collator.Effort})
	return seats
}

// deepProbeRecorder is probeRecorder's counterpart for the DEEP capability: meshcore stays the single
// driver (it decides which adapters are probed and enforces once-per-adapter), while this surface
// captures the typed result on the way past so `--json` can report the stage and signal a Check drops.
type deepProbeRecorder struct {
	model.Adapter
	deep model.DeepProber
	name string
	into map[string]model.ProbeResult
}

func (d *deepProbeRecorder) ProbeDeep(ctx context.Context, spec model.DeepProbeSpec) model.ProbeResult {
	r := d.deep.ProbeDeep(ctx, spec)
	d.into[d.name] = r
	return r
}

// recordDeepProbes wraps every DEEP-probeable adapter to record its typed result. An adapter without
// the capability is passed through untouched, so meshcore still reports it honestly as un-probeable
// rather than being handed a wrapper that falsely claims the interface.
func recordDeepProbes(adapters map[string]model.Adapter, into map[string]model.ProbeResult) map[string]model.Adapter {
	out := make(map[string]model.Adapter, len(adapters))
	for name, a := range adapters {
		if dp, ok := a.(model.DeepProber); ok {
			out[name] = &deepProbeRecorder{Adapter: a, deep: dp, name: name, into: into}
			continue
		}
		out[name] = a
	}
	return out
}

// deepProjection pairs each `probe-deep: <adapter>` check with the typed result recorded for it, and
// marks the row `deep` so a consumer can tell a row that SPENT from one that did not.
func deepProjection(checks []mdoctor.Check, recorded map[string]model.ProbeResult) []doctorProbeJSON {
	var out []doctorProbeJSON
	for _, c := range checks {
		name, isDeep := strings.CutPrefix(c.Name, "probe-deep: ")
		if !isDeep {
			continue
		}
		row := doctorProbeJSON{Adapter: name, OK: c.OK, Detail: c.Detail, Deep: true}
		if r, ok := recorded[name]; ok {
			row.OK, row.Stage, row.Signal, row.Detail = r.OK, r.Stage, string(r.Signal), r.Detail
		}
		out = append(out, row)
	}
	return out
}

func probeProjection(checks []mdoctor.Check, recorded map[string]model.ProbeResult) []doctorProbeJSON {
	var out []doctorProbeJSON
	for _, c := range checks {
		name, isProbe := strings.CutPrefix(c.Name, "probe: ")
		if !isProbe {
			continue
		}
		row := doctorProbeJSON{Adapter: name, OK: c.OK, Detail: c.Detail}
		if r, ok := recorded[name]; ok {
			row.OK, row.Stage, row.Signal, row.Detail = r.OK, r.Stage, string(r.Signal), r.Detail
		}
		out = append(out, row)
	}
	return out
}

// runACP runs exploremesh as a local ACP agent over stdio: it resolves the roster + adapters exactly as
// `explore`/`doctor` do (explicit --roster, else the discovered/user roster, else the built-in unconfigured default),
// builds the registry, and serves the ACP protocol until `exit`/EOF. The exploration TASK arrives per
// `session/prompt` (purpose from the prompt, criteria from `_meta.exploremesh`); the CONFIG is the bound
// profile set — a prompt may only SELECT within it (`_meta.exploremesh.profile`/`count`), never supply a
// new roster. A durable session store under <AIMESH_HOME>/.exploremesh/acp-sessions enables session
// resume across a restart (best-effort — resume is simply unsupported if the home cannot be resolved).
func runACP(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh explore acp")
	framing := fs.String("framing", acp.FramingNewline, "wire framing: newline | content-length")
	turnTimeout := fs.Duration("turn-timeout", 10*time.Minute, "total wall-clock budget for one exploration turn")
	rosterPath := fs.String("roster", "", "optional roster file (YAML/JSON); default is the discovered profile set (profiles.yaml, a migrated legacy roster.yaml, else the unconfigured default)")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	// The DEFAULT panel (an explicit raw --roster, else the default profile) — what a prompt naming
	// neither a profile nor a count runs.
	plan, err := loadPlan(*rosterPath)
	if err != nil {
		fmt.Fprintf(errw, "aimesh explore acp: %v\n", err)
		return codeOf(err)
	}
	// Bind the PROFILE SET a `session/prompt` may select from (design §7). An explicit --roster is a
	// single ANONYMOUS roster: no named profiles, so `_meta.exploremesh.profile` naming anything is
	// invalid-params. Otherwise it is the same set a no-flag `explore` binds to.
	var set profile.Set
	if *rosterPath == "" {
		cwd, cerr := os.Getwd()
		if cerr != nil {
			fmt.Fprintf(errw, "aimesh explore acp: %v\n", cerr)
			return int(fault.Internal)
		}
		if set, err = profile.Resolve(cwd); err != nil {
			fmt.Fprintf(errw, "aimesh explore acp: %v\n", err)
			return codeOf(err)
		}
	}
	paths, acpInsts, err := resolveAdapters()
	if err != nil {
		fmt.Fprintf(errw, "aimesh explore acp: %v\n", err)
		return codeOf(err)
	}
	// Build the registry over EVERY bound profile's adapters, not just the default panel's: any profile
	// is selectable per prompt, so an unconfigured adapter in one of them must fail closed BEFORE serving
	// rather than mid-turn. With one profile (or an explicit --roster) this is exactly the default plan.
	reg, unknown := registry.Build(unionPlan(plan, set), paths, acpInsts, 0)
	if len(unknown) > 0 {
		// Fail closed BEFORE serving: an unrecognized adapter is a config error, not a silent substitute.
		fmt.Fprintf(errw, "aimesh explore acp: unknown adapter(s) in the roster: %s — configure them (setup) or fix the roster\n", strings.Join(unknown, ", "))
		return int(fault.Config)
	}
	// WIDEN the registry to every CONFIGURED adapter (not just the ones a bound profile happens to name),
	// because a prompt may now COMPOSE an ad-hoc panel (`_meta.exploremesh.panel`). The widening is
	// name-resolution only — no process starts and nothing is spent — and it is what makes
	// compose-not-configure a real boundary rather than an accident of which profiles exist: the prompt
	// may select any adapter the OPERATOR configured, and nothing outside that set.
	adapterNames := configuredAdapterNames(acpInsts)
	wide, _ := registry.Build(namesPlan(adapterNames), paths, acpInsts, 0)
	for name, a := range wide {
		if _, have := reg[name]; !have {
			reg[name] = a
		}
	}
	srv := &acp.Server{
		Explorer:    acp.NewPipelineExplorer(reg),
		Plan:        plan,
		Profiles:    set,
		Adapters:    adapterNames,
		Framing:     *framing,
		TurnTimeout: *turnTimeout,
	}
	// Durable session store (AIMESH_HOME-aware) → enables ACP v1 `session/resume` across an agent
	// restart. If the home cannot be resolved, resume is simply unsupported (not fatal).
	if home, herr := roster.HomeDir(); herr == nil {
		srv.Sessions = acp.NewFileSessionStore(filepath.Join(home, localstate.HomeDirName, roster.ComponentName, "acp-sessions"))
	}
	if err := srv.Serve(os.Stdin, out); err != nil {
		fmt.Fprintf(errw, "aimesh explore acp: %v\n", err)
		return int(fault.Internal)
	}
	return int(fault.OK)
}

// runInit creates the local, VCS-excluded `.aimesh/` state directory (adapter locations, run
// artifacts, containment copies) — mirroring aikit's `init`/`repo init`/`folder init`. It seeds no
// config: adapter locations are written by the explicit adapter-config step.
func runInit(args []string, mode localstate.InitMode, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cliflags.Style(fs, "aimesh explore init")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", err)
		return int(fault.Internal)
	}
	res, err := localstate.Init(cwd, mode)
	if err != nil {
		// `repo init` outside a repo (and `folder init` inside one) is the dominant failure here: the
		// command was pointed at the wrong directory, which is a usage error — the same class
		// `reviewmesh init` returns for the identical refusal.
		fmt.Fprintf(stderr, "aimesh explore init: %v\n", err)
		return int(fault.Usage)
	}
	for _, a := range res.Actions {
		fmt.Fprintf(stdout, "  %s\n", a)
	}
	fmt.Fprintf(stdout, "aimesh explore: initialized %s\n", res.Home)
	return int(fault.OK)
}

// maxArtifactBytes bounds the artifact a single run may read in. It is generous for a document under review
// and small enough that `--artifact -` on the wrong stream cannot swallow a pipe forever.
const maxArtifactBytes = 1 << 20 // 1 MiB

// readArtifact loads the ARTIFACT UNDER REVIEW from a path, or from `stdin` when the path is `-` (design §3
// Challenge row). An empty flag yields an empty artifact — whether that is acceptable is the MODE's decision
// (ModeSpec.ValidateTask), not this reader's. A supplied-but-empty artifact IS an error here: the user asked
// for a review of something and handed over nothing, which is a mistake worth naming rather than running.
func readArtifact(path string, stdin io.Reader) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(io.LimitReader(stdin, maxArtifactBytes+1))
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("--artifact %s: %w", path, err)
	}
	if len(b) > maxArtifactBytes {
		return "", fmt.Errorf("--artifact %s: artifact exceeds the %d-byte limit for one review", path, maxArtifactBytes)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("--artifact %s: the artifact is empty — there is nothing to review", path)
	}
	return string(b), nil
}

// printSummary renders the human result. It deliberately does NOT print the formulation source: every
// mode is formulation-free (there is no collator-formulate leg), so the line could only
// ever print one value while reading like a live distinction between formulation regimes. The source is
// still RECORDED (formulation.json, the derived export, the ACP echo) — it is a governance fact about
// the run, just not a per-run choice worth reporting to the terminal.
func printSummary(w io.Writer, res pipeline.Result) {
	fmt.Fprintf(w, "Exploration complete.\n")
	if res.CollatorCaveat != "" {
		fmt.Fprintf(w, "  collator caveat: %s\n", res.CollatorCaveat)
	}
	if res.CanonicalizerCaveat != "" {
		fmt.Fprintf(w, "  canonicalizer caveat: %s\n", res.CanonicalizerCaveat)
	}
	fmt.Fprintf(w, "  explorers: %d in panel, %d dropped\n", len(res.Envelopes), len(res.Dropped))
	for _, d := range res.Dropped {
		fmt.Fprintf(w, "    dropped %s — %s\n", identity(d.Explorer), d.Reason)
	}
	if res.Output == nil {
		return
	}
	// The terminal output is per-mode: render each mode's own detail, defaulting to the generic
	// one-line Summary() for any mode without a bespoke renderer.
	switch out := res.Output.(type) {
	case schema.CollatorOutput:
		printMapSynthesis(w, out)
	case schema.SynthesizeOutput:
		printSynthesizeOutput(w, out)
	case schema.CatalogOutput:
		printCatalogOutput(w, res, out)
	case mode.ChallengeOutput:
		printChallengeOutput(w, res, out)
	case mode.ShortlistOutput:
		printShortlistOutput(w, res, out)
	case mode.CompareOutput:
		printCompareOutput(w, res, out)
	case mode.ForecastOutput:
		printForecastOutput(w, res, out)
	default:
		fmt.Fprintf(w, "\nResult:\n  %s\n", res.Output.Summary())
	}
}

// recordedWord states whether this run's own result was appended. "was NOT recorded" is deliberately
// loud: it means the next iteration will run cold, and an operator who thinks memory is accumulating
// when it is not will read the next run's novelty as a result rather than as a missing input.
func recordedWord(recorded bool) string {
	if recorded {
		return "was recorded for the next one"
	}
	return "was NOT recorded"
}

// printChallengeOutput renders the Challenge mode's severity-triaged register. It prints every entry's
// HOST-computed corroboration with BOTH denominators and its LABEL — including a WITHHELD one, spelled out
// with its sensitivity range. Hiding a withheld label behind a bare count is the single most tempting
// dishonesty this whole design exists to prevent, so the renderer states it first and states it plainly.
func printChallengeOutput(w io.Writer, res pipeline.Result, out mode.ChallengeOutput) {
	fmt.Fprintf(w, "\nChallenge: %s\n", out.Summary())
	if out.ArtifactDigest != "" {
		fmt.Fprintf(w, "  artifact under review: sha256:%s\n", out.ArtifactDigest[:16])
	}
	printGovernanceHeader(w, res)
	fmt.Fprintln(w, "\nFinding register (most severe first; counts are HOST-computed over the blind round-1 responses):")
	for _, e := range out.Register {
		fmt.Fprintf(w, "  [%s] %s\n", strings.ToUpper(string(e.Severity)), e.Statement)
		fmt.Fprintf(w, "      %s — %s / %s\n", labelDisplay(e.Corroboration.Label), e.Corroboration.KOfPanel, e.Corroboration.KOfRespondents)
		if s := e.Corroboration.Sensitivity; s != nil {
			fmt.Fprintf(w, "      CONTESTED PARTITION: conditional, range %d..%d — %s\n", s.Low, s.High, s.Note)
		}
		for _, f := range e.Findings {
			fmt.Fprintf(w, "      · %s ← %s [%s]\n", f.Statement, identity(f.Explorer), f.Severity)
			if f.FailureScenario != "" {
				fmt.Fprintf(w, "          fails when: %s\n", f.FailureScenario)
			}
		}
		for _, d := range e.Deepening {
			fmt.Fprintf(w, "      » cross-review %s ← %s: %s\n", d.Stance, identity(d.Explorer), d.Depth)
		}
	}
	if len(out.SurvivingStrengths) > 0 {
		fmt.Fprintln(w, "\nSurviving strengths (attacked in cross-review and refuted, with nobody deepening):")
		for _, s := range out.SurvivingStrengths {
			fmt.Fprintf(w, "  - %s: %s\n", s.Statement, s.Reason)
		}
	}
	printNarrative(w, out.CollatorNarrative)
}

// printShortlistOutput renders the Shortlist mode's host-tallied ranking. It leads with the honest decision
// rendering (never "the winner is"), prints each entry's label — WITHHELD ones included, with their range —
// and always shows the rejects with the host's reason, so the candidates the panel nominated are never lost.
func printShortlistOutput(w io.Writer, res pipeline.Result, out mode.ShortlistOutput) {
	fmt.Fprintf(w, "\nShortlist: %s\n", out.Summary())
	fmt.Fprintf(w, "\n%s\n", out.Decision)
	fmt.Fprintf(w, "  method: %s; decision inputs frozen as %s (hashed BEFORE the ballot was solicited)\n", out.Method, out.DecisionInputsHash)
	if !out.QuorumMet {
		fmt.Fprintln(w, "  QUORUM NOT MET — every ranked label is withheld")
	}
	if out.TieOutcome != "" {
		fmt.Fprintf(w, "  TIE: %s\n", out.TieOutcome)
	}
	printGovernanceHeader(w, res)
	fmt.Fprintln(w, "\nFrozen criteria (origin / aggregation method):")
	for _, c := range out.Criteria {
		fmt.Fprintf(w, "  - %s [%s / %s]\n", c.Name, c.Origin, c.AggregationMethod)
	}
	fmt.Fprintln(w, "\nRanked (host tally over the confirmed candidate universe):")
	for _, e := range out.Ranked {
		fmt.Fprintf(w, "  %d. %s — %s (score %d, %d approval(s))\n", e.Rank, e.Name, labelDisplay(e.Claim.Label), e.Score, e.Approvals)
		fmt.Fprintf(w, "      placed by %s / %s\n", e.Claim.KOfPanel, e.Claim.KOfRespondents)
		if s := e.Claim.Sensitivity; s != nil {
			fmt.Fprintf(w, "      CONTESTED PARTITION: conditional, range %d..%d — %s\n", s.Low, s.High, s.Note)
		}
		if e.EmergentSalience != nil {
			// Printed side by side deliberately: `emergent` (who NAMED it, blind) and `voted` (who PREFERRED it)
			// are different measurements, and a reader must be able to see both rather than substitute one.
			fmt.Fprintf(w, "      emergent salience (blind round 1, a DIFFERENT measurement): %s — %s\n",
				e.EmergentSalience.KOfPanel, labelDisplay(e.EmergentSalience.Label))
		}
		for _, p := range e.Provenance {
			fmt.Fprintf(w, "      · nominated as %q by %s\n", p.RawNomination, identity(p.Explorer))
		}
	}
	if len(out.Rejects) > 0 {
		fmt.Fprintln(w, "\nNot shortlisted (carried with the host's reason — a nominated candidate is never dropped):")
		for _, r := range out.Rejects {
			fmt.Fprintf(w, "  %d. %s — %s\n", r.Rank, r.Name, r.Reason)
		}
	}
	printNarrative(w, out.CollatorNarrative)
}

// printGovernanceHeader prints the governance pins every adjudicative result rides on: the confirmed partition
// revision, the frozen counting-policy hash, the rules version and any contested mappings left unsettled.
func printGovernanceHeader(w io.Writer, res pipeline.Result) {
	if res.Canonicalization != nil {
		fmt.Fprintf(w, "  partition revision: %s (surjectivity holds; %d ledger row(s))\n",
			res.Canonicalization.PartitionRevisionHash, res.Canonicalization.Ledger.Len())
	} else if isFixedSpace(res) {
		// A FIXED-SPACE result has no partition, and the header says so rather than omitting the line: an
		// absent partition row and an unstated one read the same way to someone scanning output (design §9).
		fmt.Fprintln(w, "  partition revision: NONE — fixed space: the space was declared before any explorer spoke, so no canonicalizer ran and no entity resolution was performed")
	}
	if res.Governance != nil {
		fmt.Fprintf(w, "  %s\n", res.Governance.Summary())
		fmt.Fprintf(w, "  frozen counting policy: %s\n", res.Governance.Panel.PolicyHash)
	}
	if res.Confirmation != nil && !res.Confirmation.Settled() {
		fmt.Fprintf(w, "  UNSETTLED: %d contested mapping(s) survived the confirmation round — dependent counts are conditional\n",
			len(res.Confirmation.Contested))
	}
}

// isFixedSpace reports whether a run's claims are FIXED-SPACE ones — read from the claims themselves (they
// carry the explicit no-partition statement) rather than from the mode name, so the rendering follows the
// governance record instead of a label.
func isFixedSpace(res pipeline.Result) bool {
	if res.Governance == nil {
		return false
	}
	for _, c := range res.Governance.Claims {
		if c.PartitionRevisionHash == govern.FixedSpaceNoPartition {
			return true
		}
	}
	return false
}

// printNarrative prints the quarantined model prose, LABELED as such so nothing in it can be read as a
// governance value (design §0 F-C).
func printNarrative(w io.Writer, narrative []govern.Narrative) {
	if len(narrative) == 0 {
		return
	}
	fmt.Fprintln(w, "\nCollator narrative (MODEL PROSE — carries no governance value):")
	for _, n := range narrative {
		fmt.Fprintf(w, "  - [%s] %s: %s\n", n.Phase, identity(n.Source), n.Prose)
	}
}

// labelDisplay renders a governance label, SHOUTING a withheld one. A withheld label that reads like a
// definitive one is worse than no label at all.
func labelDisplay(l govern.Label) string {
	if l.Definitive() {
		return string(l)
	}
	return "WITHHELD (" + string(l) + ")"
}

// printMapSynthesis renders the Map mode's terminal CollatorOutput. Each finding shows the HOST-VALIDATED
// `envelope#k` citations behind it (C1) — or, when none of the collator's citations resolved, the host's
// UNCITED label. The label is printed rather than hidden because an unsourced conclusion presented like a
// sourced one is the failure the citation pass exists to make visible.
func printMapSynthesis(w io.Writer, out schema.CollatorOutput) {
	fmt.Fprintf(w, "\nSynthesis:\n  %s\n", out.SynthesisSummary)
	if len(out.Findings) > 0 {
		fmt.Fprintln(w, "\nFindings:")
		for _, f := range out.Findings {
			fmt.Fprintf(w, "  - %s (confidence %.2f)\n", f.Statement, f.Confidence)
			if f.Uncited {
				fmt.Fprintln(w, "      UNCITED — no citation resolved to a response in this panel")
			} else {
				fmt.Fprintf(w, "      sources: %s\n", strings.Join(f.Sources, ", "))
			}
			// Printed for the terminal reader specifically. A driver reads this off the machine
			// surface; someone reading a synthesis in a terminal would otherwise see a finding that
			// points at a file and no indication that nothing here looked at it.
			if len(f.UnverifiedReferences) > 0 {
				fmt.Fprintf(w, "      UNVERIFIED reference(s): %s\n", strings.Join(f.UnverifiedReferences, ", "))
				fmt.Fprintln(w, "        Named outside this panel. exploremesh reads no filesystem, so nothing here")
				fmt.Fprintln(w, "        checked that they exist or say what the finding says they say.")
			}
		}
	}
	if len(out.DisagreementRegister) > 0 {
		fmt.Fprintln(w, "\nDisagreements:")
		for _, d := range out.DisagreementRegister {
			fmt.Fprintf(w, "  - %s: %s (residual risk: %s)\n", d.Subject, d.Resolution, d.ResidualRisk)
		}
	}
}

// printSynthesizeOutput renders the Synthesize mode's composed artifact + provenance + minority report.
func printSynthesizeOutput(w io.Writer, out schema.SynthesizeOutput) {
	fmt.Fprintf(w, "\nComposed answer:\n  %s\n", out.Artifact)
	if out.Rationale != "" {
		fmt.Fprintf(w, "\nRationale:\n  %s\n", out.Rationale)
	}
	if len(out.ComponentProvenance) > 0 {
		fmt.Fprintln(w, "\nComponent provenance (grafted elements):")
		for _, p := range out.ComponentProvenance {
			fmt.Fprintf(w, "  - %s ← %s\n", p.Component, identity(p.FromExplorer))
		}
	}
	if len(out.MinorityReport) > 0 {
		fmt.Fprintln(w, "\nMinority report (rejected alternatives):")
		for _, m := range out.MinorityReport {
			fmt.Fprintf(w, "  - %s [%s]: %s\n", m.Alternative, identity(m.FromExplorer), m.WhyRejected)
		}
	}
}

// printCatalogOutput renders the Catalog mode's canonical clusters + PROPOSED dimensions. It surfaces the
// partition revision hash (the ledger the partition was computed at) and marks single-source members as a
// carried minority.
func printCatalogOutput(w io.Writer, res pipeline.Result, out schema.CatalogOutput) {
	fmt.Fprintf(w, "\nCatalog: %s\n", out.Summary())
	if res.Canonicalization != nil {
		fmt.Fprintf(w, "  partition revision: %s (surjectivity holds; %d ledger row(s))\n",
			res.Canonicalization.PartitionRevisionHash, res.Canonicalization.Ledger.Len())
	}
	fmt.Fprintln(w, "\nClusters:")
	for _, c := range out.Clusters {
		fmt.Fprintf(w, "  - %s\n", c.Name)
		for _, m := range c.Members {
			tag := ""
			if m.SingleSource {
				tag = " [single-source]"
			}
			fmt.Fprintf(w, "      %s ← %s%s\n", m.RawNomination, identity(m.SourceExplorer), tag)
		}
	}
	if len(out.ProposedDimensions) > 0 {
		fmt.Fprintln(w, "\nProposed dimensions (NOT decision criteria):")
		for _, d := range out.ProposedDimensions {
			fmt.Fprintf(w, "  - %s\n", d)
		}
	}
	if strings.TrimSpace(out.CoverageNotes) != "" {
		fmt.Fprintf(w, "\nCoverage notes:\n  %s\n", out.CoverageNotes)
	}
}

func printPartial(w io.Writer, res pipeline.Result) {
	fmt.Fprintf(w, "  (partial) %d envelopes, %d dropped\n", len(res.Envelopes), len(res.Dropped))
}

func identity(id schema.ExplorerIdentity) string {
	return fmt.Sprintf("(%s, %s, %s)", id.Adapter, id.Model, id.Effort)
}

func splitCSV(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
