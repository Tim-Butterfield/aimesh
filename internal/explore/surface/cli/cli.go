// Package cli implements the `aimesh explore` commands: run an exploration, list configuration, export a
// run, check readiness, configure adapters and profiles, and serve ACP or MCP.
//
// A roster naming a known adapter runs that CLI. The `fake` adapter resolves only under the test harness
// gate. Any other name is a configuration error reported before a model is called.
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
	"slices"
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
	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/pathexpand"
)

// Run dispatches a subcommand and returns a process exit code. Asking for help exits 0; a missing or
// unknown command is a usage error.
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
		fmt.Fprintln(stderr, "aimesh explore: usage: aimesh explorerepo init")
		return int(fault.Usage)
	case "folder":
		if len(args) >= 2 && args[1] == "init" {
			return runInit(args[2:], localstate.InitFolder, stdout, stderr)
		}
		fmt.Fprintln(stderr, "aimesh explore: usage: aimesh explorefolder init")
		return int(fault.Usage)
	case "-h", "--help", "help":
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

// usage writes the help text to w, listing modes from the registry. Callers choose the exit code.
func usage(w io.Writer) {
	fmt.Fprintf(w, `aimesh explore — N-explorer research/exploration on meshcore

usage:
  aimesh exploreexplore --purpose <text> --criteria <a,b,...> [--mode <name>] [--profile <name> | --roster <path>]
                         [--count <n>|all] [--artifact <path|->] [--prior-context <text>] [--json] [--dump-run] [--debug]
                         [--max-parallel <n>]   (how many explorer CLIs run at once; default: the whole panel)
                         [--canonicalizer adapter=<n>,model=<m>[,effort=<e>] --canonicalizer ...]   (exactly 2, or none)
  aimesh exploreexplore --purpose <text> --criteria <a,b,...> --explorer adapter=<n>,model=<m>[,effort=<e>] --explorer ... --collator adapter=<n>,model=<m>[,effort=<e>]
  aimesh exploreexplore --mode compare --options <a,b,c> --criterion name=<n>,direction=higher_is_better|lower_is_better[,role=dimension|filter][,weight=<w>] --criterion ...
  aimesh exploreexplore --mode forecast --target <what> --unit <unit> --horizon <when> [--conditioning <event>]
  aimesh exploreexport --sqlite <out.db> --run <run-dir> [--verify] [--force] [--json]
  aimesh explorelist [--json]
  aimesh exploredoctor [--profile <name> | --roster <path>] [--probe] [--probe-deep] [--json]
  aimesh exploreacp [--adapter <name>[=<path>] ...] [--framing newline|content-length] [--turn-timeout <dur>]
  aimesh exploremcp [--adapter <name>[=<path>] ...] [--wait-seconds <n>] [--turn-timeout <dur>] [--no-capture]
  aimesh exploresetup --adapter <name> --path <p>          |  setup --remove-adapter <name>
  aimesh exploresetup --acp detect --path <p> [--acp-arg <a> ...]
  aimesh exploresetup --acp add --path <p> [--name <k>] [--title <t>] [--acp-arg <a> ...]
  aimesh exploresetup --acp remove --name <k>
  aimesh exploresetup --profile <name> --explorer adapter=<n>,model=<m>[,effort=<e>] --explorer ...
                       --collator adapter=<n>,model=<m> [--canonicalizer ... --canonicalizer ...] [--default-mode <m>]
  aimesh exploresetup --delete-profile <name> --yes
  aimesh exploreinit            (repo if inside one, else plain folder)
  aimesh explorerepo init       (require a Git/Mercurial repo; .aimesh is VCS-excluded)
  aimesh explorefolder init     (require a non-repo directory)

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
            This is the only write aimesh explore makes to a path you type, and it is governed: an existing --sqlite
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
            a CLI does real work where the run happens. It never answers an interactive trust or login
            prompt: it detects, classifies and reports the fix you perform.
            --json emits the machine-readable projection (ok + checks[] + probes[], with "deep" marking
            the rows that spent) instead of the human report; the exit code is the same either way
  setup     configure exploration: record or clear a shell adapter's binary path, detect/add/remove a
            user-defined ACP adapter, and create or delete a profile. Exactly ONE action per
            invocation, through the governed manager seams — every write is validated by Go before it
            lands. Adapter + ACP entries persist to the shared ~/.aimesh/adapters.yaml; profiles to the
            project profiles.yaml inside a repo, else the user one. --acp detect and --acp add LAUNCH
            THE REAL CLI to confirm its launch args (that is the only way to know it speaks ACP);
            --delete-profile requires --yes. A no-flag run always binds to the profile named "default"
            (as in aimesh review) — save the panel you want as "default"; there is no set-default
  acp       run as a local ACP (Agent Client Protocol) agent over stdio so another tool can drive one
            exploration; the task PURPOSE comes from the prompt and the CRITERIA from _meta.exploremesh,
            which also COMPOSES the panel (panel: {explorers[], collator}) from the adapters named at
            launch (--adapter or AIMESH_ADAPTERS) and may capture the run (dumpRun). It reads no saved
            configuration
  mcp       run as a local MCP (Model Context Protocol) server over stdio so an MCP agent can drive
            explorations. Tools: explore / explore_challenge / explore_compare / explore_forecast (each
            SPENDS: it launches the configured model CLIs), plus read-only list / doctor / run_status /
            run_result. Calls are JOB-SHAPED — a run that outlives --wait-seconds is handed back as
            {runId, state:"running"} and fetched with run_result. Concurrency within one run is set per call with maxParallel.
            Every run is captured to a run directory unless --no-capture. Each call COMPOSES its panel
            from the adapters named at launch (--adapter or AIMESH_ADAPTERS); it reads no saved
            configuration and can never change any
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

// runVersion prints the version line, or the version metadata as JSON when args include --json.
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

// resolveAdapters loads the adapter binary paths and ACP instances configured for the working directory.
// Missing files yield empty results; unreadable or malformed ones are configuration faults.
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

// resolvedRun is a resolved run plan: the plan, the profile's default mode ("" for a roster file), a label
// naming the source, and the selected and total explorer counts.
type resolvedRun struct {
	plan        roster.Plan
	defaultMode string
	sourceLabel string
	selected    int
	full        int
}

// errProfileUnconfigured reports that the profile has no panel. doctor turns it into a failing check.
var errProfileUnconfigured = fault.New(fault.Config, "profile is unconfigured")

// resolveRun resolves the plan from --roster, else --profile, else the default profile, then selects the
// top count explorers by preference ("" or "all" selects every one). The caller rejects --roster combined
// with --profile.
func resolveRun(rosterPath, profileName, count string) (resolvedRun, error) {
	src, mode, label, err := resolveSource(rosterPath, profileName)
	if err != nil {
		return resolvedRun{}, err
	}
	// An unconfigured profile cannot run; refuse it here with setup guidance.
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

// resolveSource returns the roster, default mode and a source label from --roster, else --profile, else
// the default profile of the discovered profile set (see profile.Resolve).
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

// printSelected writes the panel a run will execute to w: its source, the selected explorers, the collator,
// the canonicalizers, and how many configured explorers --count left out.
func printSelected(w io.Writer, run resolvedRun) {
	fmt.Fprintf(w, "panel: %d of %d explorer(s) from %s\n", run.selected, run.full, run.sourceLabel)
	for _, e := range run.plan.Explorers {
		fmt.Fprintf(w, "  %s\n", slotDisplay(listSlot{Adapter: e.Adapter, Model: e.Model, Effort: e.Effort}))
	}
	c := run.plan.Collator
	fmt.Fprintf(w, "  collator: %s\n", slotDisplay(listSlot{Adapter: c.Adapter, Model: c.Model, Effort: c.Effort}))
	// Derived canonicalizers are chosen at run time, and only by modes that canonicalize.
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

// namesPlan returns a plan with one placeholder explorer per name, so registry.Build can resolve a set of
// adapter names. It is never run.
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
	// Fixed-space declarations, required by the modes that use them.
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
	// Separate positionals first: the flag package stops parsing at the first non-flag argument, which
	// would ignore flags placed after the question.
	flagArgs, positionals := cliflags.SplitArgs(fs, args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	// Positional arguments form the purpose.
	if len(positionals) > 0 {
		joined := strings.TrimSpace(strings.Join(positionals, " "))
		switch {
		case strings.TrimSpace(*purpose) != "" && joined != "":
			fmt.Fprintf(stderr, "aimesh explore: the purpose was given twice — as --purpose %q and as the positional %q. Give it once.\n", *purpose, joined)
			return int(fault.Usage)
		case joined != "":
			*purpose = joined
		}
	}
	// --explorer/--collator, --roster and --profile are mutually exclusive.
	adHoc := len(explorerSpecs) > 0 || strings.TrimSpace(*collatorSpec) != ""
	if adHoc && (*rosterPath != "" || *profileName != "") {
		fmt.Fprintln(stderr, "aimesh explore: --explorer/--collator (ad-hoc roster) cannot be combined with --roster or --profile")
		return int(fault.Usage)
	}
	if *rosterPath != "" && *profileName != "" {
		fmt.Fprintln(stderr, "aimesh explore: --roster and --profile are mutually exclusive")
		return int(fault.Usage)
	}

	// Resolve the plan: ad-hoc specs, else --roster, else --profile, else the default profile.
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
		plan, perr := r.SelectTopN(n)
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
	// --canonicalizer replaces the profile's canonicalizers.
	if len(canonicalizerSpecs) > 0 {
		cs, cerr := buildCanonicalizers(canonicalizerSpecs)
		if cerr != nil {
			fmt.Fprintf(stderr, "aimesh explore: %v\n", cerr)
			return codeOf(cerr)
		}
		run.plan.Canonicalizers = cs
	}
	plan := run.plan

	// The mode is --mode, else the profile's default mode, else empty (the pipeline default).
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
	artifact, aerr := readArtifact(*artifactPath, os.Stdin)
	if aerr != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", aerr)
		return int(fault.Usage)
	}
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
		// Show a complete example command, not just the missing field.
		fmt.Fprintf(stderr, "aimesh explore: %v\n\n"+
			"An exploration needs a PURPOSE (what to explore) and CRITERIA (what a good answer must satisfy):\n"+
			"  aimesh explore run \"how should we shard the write path?\" --criteria correctness,operational-cost\n"+
			"  aimesh explore run --purpose \"...\" --criteria a,b --mode map\n\n"+
			"The criteria are never chosen for you: they are the standard every explorer is judged against, so a\n"+
			"run that let the tool pick them would be marking its own work.\n", err)
		return int(fault.Usage)
	}
	if err := mode.ValidateTask(modeName, raw); err != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", err)
		return int(fault.Usage)
	}

	printSelected(stderr, run)
	paths, acpInsts, err := resolveAdapters()
	if err != nil {
		fmt.Fprintf(stderr, "aimesh explore: %v\n", err)
		return codeOf(err)
	}
	reg, unknown := registry.Build(plan, paths, acpInsts, 0)
	if len(unknown) > 0 {
		// For names typed on the command line, list the configured adapters to choose from.
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
	// A dry run performs no exploration, so there is nothing to capture.
	if *dryRun && *dumpRun {
		fmt.Fprintln(stderr, "aimesh explore: --dry-run cannot be combined with --dump-run — a dry run makes no model calls, so there are no envelopes, raw outputs or later-round prompts to capture; the shape goes to stdout (--json for the machine-readable form)")
		return int(fault.Usage)
	}
	res, rerr := pipeline.Run(ctx, reg, plan, raw, pipeline.Options{MaxParallel: *maxParallel, DryRun: *dryRun}, nil)

	// Capture completed and halted runs. The `run:` line goes to stderr to keep --json output clean.
	if *dumpRun {
		dir := capture.ArtifactDir()
		if run, aerr := audit.NewRun(dir, "", time.Now()); aerr != nil {
			fmt.Fprintf(stderr, "aimesh explore: dump-run: %v\n", aerr)
		} else {
			status, faultMsg := "complete", ""
			if rerr != nil {
				status, faultMsg = "halted", rerr.Error()
			}
			if derr := capture.Dump(capture.Input{Run: run, Plan: plan, Result: res, Status: status, Fault: faultMsg, Task: raw}); derr != nil {
				fmt.Fprintf(stderr, "aimesh explore: dump-run failed: %v\n", derr)
			} else {
				fmt.Fprintf(stderr, "run: %s\n", run.Dir)
			}
		}
	}

	if rerr != nil {
		// Report the partial state and exit with the halt's own fault class.
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

// printShape writes a dry run's shape: the panel, each stage with its call count, the total, and the
// round-1 payload, along with what the total excludes and the fact that no model was contacted.
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

// humanBytes formats n as bytes below 1 KB and as kilobytes otherwise.
func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}

// short returns the first 12 characters of a hash.
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

// doctorProbeJSON is one adapter's probe result. Signal is a machine-readable blocker, Detail is prose
// that must not be parsed, and Stage is how far the probe got.
type doctorProbeJSON struct {
	Adapter string `json:"adapter"`
	OK      bool   `json:"ok"`
	Stage   string `json:"stage,omitempty"`
	Signal  string `json:"signal,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// Deep marks a result from the deep probe, which made a real model call.
	Deep bool `json:"deep,omitempty"`
}

// doctorJSON is the `doctor --json` output. OK is false exactly when the command exits with a
// configuration error.
type doctorJSON struct {
	OK     bool              `json:"ok"`
	Checks []doctorCheckJSON `json:"checks"`
	Probes []doctorProbeJSON `json:"probes,omitempty"`
}

// doctorProbeTimeout bounds the whole --probe pass; each probe also has its own timeout.
const doctorProbeTimeout = 90 * time.Second

// doctorDeepProbeTimeout bounds the whole --probe-deep pass; each probe also has its own timeout.
const doctorDeepProbeTimeout = 10 * time.Minute

// emitDoctor writes the report as text or JSON and returns the exit code: a configuration error when the
// report fails.
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

// printProbes writes each probe's stage and signal, which the report's checks do not include.
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
	// A separate flag, because --probe must stay free.
	probeDeep := fs.Bool("probe-deep", false, "additionally run the DEEP probe: one REAL, bounded model invocation per required adapter in a throwaway isolated directory, through the same path a run takes. SPENDS REAL TOKENS. It answers what `--version` cannot — whether the CLI does real work where a run actually happens. It NEVER answers an interactive trust/login prompt; it detects, classifies and reports the fix")
	asJSON := fs.Bool("json", false, "emit the machine-readable projection (ok + checks[] + probes[]) instead of the human report; the exit code is unchanged")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	if *rosterPath != "" && *profileName != "" {
		fmt.Fprintln(stderr, "aimesh explore: --roster and --profile are mutually exclusive")
		return int(fault.Usage)
	}
	// Check the full configured panel.
	run, err := resolveRun(*rosterPath, *profileName, "")
	if errors.Is(err, errProfileUnconfigured) {
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
	slices.Sort(names)

	var rep mdoctor.Report
	rep.Checks = append(rep.Checks, mdoctor.Check{Name: "roster: explorers >= 2", OK: len(plan.Explorers) >= 2, Detail: fmt.Sprintf("%d explorers", len(plan.Explorers))})
	rep.Checks = append(rep.Checks, canonicalizerCheck(plan, run.defaultMode))
	for _, u := range unknown {
		rep.Checks = append(rep.Checks, mdoctor.Check{Name: "adapter: " + u, OK: false, Detail: "unknown adapter — not a CLI recipe or a configured ACP instance; configure it (setup) or fix the roster"})
	}
	rep.Checks = append(rep.Checks, mdoctor.AdapterAvailability(adapters, names, required)...)

	// Probes start processes, so they run only when requested, under a shared timeout, through
	// meshcore's doctor.ProbeAdapters.
	var probes []doctorProbeJSON
	if *probe || *probeDeep {
		ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
		recorded := map[string]model.ProbeResult{}
		rep.Checks = append(rep.Checks, mdoctor.ProbeAdapters(ctx, recordProbes(adapters, recorded), names, required)...)
		cancel()
		probes = probeProjection(rep.Checks, recorded)
		// The deep pass runs after the cheap probes and uses each seat's real model argument.
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

// probeRecorder wraps a probeable adapter to record its model.ProbeResult while doctor.ProbeAdapters
// drives the probes, so each adapter is probed once.
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

// recordProbes returns adapters with each model.Prober wrapped to record its result into into. Other
// adapters are returned unwrapped, so they are still reported as not probeable.
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

// canonicalizerCheck reports the panel's canonicalizers, explicit or derived. It fails only when no
// independent second canonicalizer can be derived and the profile's default mode needs two; otherwise that
// condition is reported in the detail.
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
	// Every explorer shares the collator's model, so a dual-canonicalizer mode would halt.
	detail += " — but every explorer shares the collator's model, so no independent second canonicalizer can be derived." +
		" A dual-canonicalizer mode (challenge, shortlist, ai-collab) will HALT. Name two with --canonicalizer, or add an explorer on a different model"
	spec, ok := mode.Resolve(defaultMode)
	return mdoctor.Check{Name: name, OK: !(ok && spec.Canonicalization.Dual), Detail: detail}
}

// planDeepSeats returns every explorer and the collator with its model argument, for the deep probe.
// meshcore probes each adapter once.
func planDeepSeats(plan roster.Plan) []mdoctor.DeepSeat {
	seats := make([]mdoctor.DeepSeat, 0, len(plan.Explorers))
	for _, e := range plan.Explorers {
		seats = append(seats, mdoctor.DeepSeat{Adapter: e.Adapter, ModelArg: e.Model, Effort: e.Effort})
	}
	seats = append(seats, mdoctor.DeepSeat{Adapter: plan.Collator.Adapter, ModelArg: plan.Collator.Model, Effort: plan.Collator.Effort})
	return seats
}

// deepProbeRecorder wraps a deep-probeable adapter to record its model.ProbeResult.
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

// recordDeepProbes returns adapters with each model.DeepProber wrapped to record its result into into.
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

// deepProjection builds deep probe rows from the `probe-deep: <adapter>` checks and the recorded results.
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

// probeProjection builds probe rows from the `probe: <adapter>` checks, in check order, adding the stage
// and signal of each recorded result.
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

// runACP serves explorations as an ACP agent over stdio until `exit` or EOF. Adapters come only from the
// launch arguments; each prompt supplies its task and panel. Sessions are saved under
// <AIMESH_HOME>/.aimesh/explore/acp-sessions so they can be resumed; if the home directory cannot be
// resolved, resume is unavailable.
func runACP(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh explore acp")
	framing := fs.String("framing", acp.FramingNewline, "wire framing: newline | content-length")
	turnTimeout := fs.Duration("turn-timeout", 10*time.Minute, "total wall-clock budget for one exploration turn")
	adapters := launchflags.RegisterAdapters(fs)
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	set, err := adapters.Resolve(pathexpand.OS())
	if err != nil {
		fmt.Fprintf(errw, "aimesh explore acp: %v\n", err)
		return codeOf(err)
	}
	if set.Empty() {
		fmt.Fprintln(errw, noAdapterNotice("acp"))
	}
	reg, err := launchRegistry(set)
	if err != nil {
		fmt.Fprintf(errw, "aimesh explore acp: %v\n", err)
		return codeOf(err)
	}
	srv := &acp.Server{
		Explorer:    acp.NewPipelineExplorer(reg),
		Adapters:    set,
		Framing:     *framing,
		TurnTimeout: *turnTimeout,
	}
	if home, herr := roster.HomeDir(); herr == nil {
		srv.Sessions = acp.NewFileSessionStore(filepath.Join(home, localstate.HomeDirName, roster.ComponentName, "acp-sessions"))
	}
	if err := srv.Serve(os.Stdin, out); err != nil {
		fmt.Fprintf(errw, "aimesh explore acp: %v\n", err)
		return int(fault.Internal)
	}
	return int(fault.OK)
}

// runInit creates the local `.aimesh/` state directory, excluded from version control. It writes no
// configuration.
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
		// The usual cause is running the command in the wrong directory, a usage error.
		fmt.Fprintf(stderr, "aimesh explore init: %v\n", err)
		return int(fault.Usage)
	}
	for _, a := range res.Actions {
		fmt.Fprintf(stdout, "  %s\n", a)
	}
	fmt.Fprintf(stdout, "aimesh explore: initialized %s\n", res.Home)
	return int(fault.OK)
}

// maxArtifactBytes bounds the size of an artifact read by --artifact.
const maxArtifactBytes = 1 << 20 // 1 MiB

// readArtifact reads the artifact under review from path, or from stdin when path is `-`. An empty path
// returns "" (the mode decides whether that is allowed); an empty artifact is an error.
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

// printSummary writes the human-readable result: caveats, dropped explorers and the mode's output.
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

// printChallengeOutput writes the Challenge register: each entry's corroboration with both denominators and
// its label, including withheld labels and their ranges.
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

// printShortlistOutput writes the Shortlist ranking: the decision rendering, each entry's label and range,
// and the rejected candidates with the host's reasons.
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

// printGovernanceHeader writes the partition revision, governance summary, counting-policy hash and any
// unsettled contested mappings.
func printGovernanceHeader(w io.Writer, res pipeline.Result) {
	if res.Canonicalization != nil {
		fmt.Fprintf(w, "  partition revision: %s (surjectivity holds; %d ledger row(s))\n",
			res.Canonicalization.PartitionRevisionHash, res.Canonicalization.Ledger.Len())
	} else if isFixedSpace(res) {
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

// isFixedSpace reports whether res has fixed-space claims.
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

// printNarrative writes the model narrative, labeled as model prose.
func printNarrative(w io.Writer, narrative []govern.Narrative) {
	if len(narrative) == 0 {
		return
	}
	fmt.Fprintln(w, "\nCollator narrative (MODEL PROSE — carries no governance value):")
	for _, n := range narrative {
		fmt.Fprintf(w, "  - [%s] %s: %s\n", n.Phase, identity(n.Source), n.Prose)
	}
}

// labelDisplay returns a label for display, prefixing a withheld label with "WITHHELD".
func labelDisplay(l govern.Label) string {
	if l.Definitive() {
		return string(l)
	}
	return "WITHHELD (" + string(l) + ")"
}

// printMapSynthesis writes the Map synthesis. Each finding shows its validated citations or an UNCITED
// label, and any references outside the panel.
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

// printSynthesizeOutput writes the composed answer, its provenance and the minority report.
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

// printCatalogOutput writes the Catalog clusters, marking single-source members, with the partition
// revision and proposed dimensions.
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
