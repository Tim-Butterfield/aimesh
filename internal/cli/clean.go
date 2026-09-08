package cli

// `aimesh clean` — remove prior run artifacts.
//
// It is a SHARED command rather than a per-domain one because the thing being cleaned is shared: both
// domains write under one state root, and where they write depends on whether `init` has been run,
// not on which domain ran. A user asking "what is this taking up, and how do I get rid of it" is
// asking one question, and answering it twice under two nouns would be the tool's internal structure
// leaking into the request.
//
// TWO PROPERTIES DECIDE ITS SHAPE:
//
//  1. IT SWEEPS BOTH LOCATIONS. Output lands in the project `.aimesh/` when the tree was init'ed and
//     in the OS temp directory when it was not. Cleaning only the first would leave every artifact of
//     every uninitialized run exactly where the user cannot see it.
//  2. IT NEVER PICKS A RETENTION POLICY. With no selector it reports an inventory and stops. Deleting
//     review artifacts is deleting evidence — the run record holds the findings, the decisions, the
//     patch and, for an apply, the undo — so what to keep is the user's judgement and the command
//     refuses to guess it.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// cleanComponents are the state components whose runs this command manages. It is the fixed pair the
// tool actually writes, named here rather than discovered, so a stray directory in the state root is
// never swept as though it were a component's runs.
var cleanComponents = []string{"review", "explore"}

// cleanReport is the `--json` projection: every location considered, whether it existed, and every
// run in it with what happened to it.
type cleanReport struct {
	SchemaVersion int             `json:"schemaVersion"`
	DryRun        bool            `json:"dryRun"`
	Selector      string          `json:"selector"`
	Locations     []cleanLocation `json:"locations"`
	RemovedRuns   int             `json:"removedRuns"`
	FreedBytes    int64           `json:"freedBytes"`
	RemainingRuns int             `json:"remainingRuns"`
}

type cleanLocation struct {
	Root      string    `json:"root"`
	Component string    `json:"component"`
	Exists    bool      `json:"exists"`
	Runs      []cleanRt `json:"runs"`
}

type cleanRt struct {
	Name     string `json:"name"`
	Bytes    int64  `json:"bytes"`
	Modified string `json:"modified,omitempty"`
	Removed  bool   `json:"removed,omitempty"`
	Skipped  string `json:"skipped,omitempty"`
}

func runClean(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh clean")
	all := fs.Bool("all", false, "remove every prior run in both locations (except any still within the in-progress grace period)")
	keep := fs.Int("keep", 0, "keep the N most recent runs per location and remove the rest")
	olderThan := fs.String("older-than", "", "remove runs last modified longer ago than this (a Go duration such as 168h, or a day count such as 7d)")
	dryRun := fs.Bool("dry-run", false, "report exactly what would be removed and remove nothing")
	asJSON := fs.Bool("json", false, "emit a machine-readable projection")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}

	sel := localstate.CleanSelector{All: *all, Keep: *keep}
	if strings.TrimSpace(*olderThan) != "" {
		d, err := parseCleanDuration(*olderThan)
		if err != nil {
			fmt.Fprintf(errw, "aimesh clean: %v\n", err)
			return int(fault.Usage)
		}
		sel.OlderThan = d
	}
	// TWO SELECTORS IS AMBIGUOUS, not a conjunction. "--keep 3 --older-than 7d" could mean either
	// rule, and picking one silently would delete on a policy the user did not write.
	if n := selectorCount(sel); n > 1 {
		fmt.Fprintln(errw, "aimesh clean: name ONE of --all, --keep or --older-than — two selectors do not combine, and guessing which one wins would delete on a rule you did not write")
		return int(fault.Usage)
	}

	now := time.Now()
	rep := cleanReport{SchemaVersion: 1, DryRun: *dryRun, Selector: selectorLabel(sel)}
	for _, component := range cleanComponents {
		for _, root := range localstate.CleanTargets(component) {
			loc, err := localstate.CleanRuns(root, component, sel, *dryRun, now)
			if err != nil {
				fmt.Fprintf(errw, "aimesh clean: %v\n", err)
				return int(fault.Internal)
			}
			removed, freed := loc.Removed()
			total, _ := loc.Total()
			rep.RemovedRuns += removed
			rep.FreedBytes += freed
			rep.RemainingRuns += total - removed
			rep.Locations = append(rep.Locations, projectLocation(loc))
		}
	}

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(errw, "aimesh clean:", err)
			return int(fault.Internal)
		}
		return int(fault.OK)
	}
	printClean(out, rep, sel)
	return int(fault.OK)
}

func projectLocation(loc localstate.CleanLocation) cleanLocation {
	o := cleanLocation{Root: loc.Root, Component: loc.Component, Exists: loc.Exists, Runs: []cleanRt{}}
	for _, r := range loc.Runs {
		row := cleanRt{Name: r.Name, Bytes: r.Bytes, Removed: r.Removed, Skipped: r.Skipped}
		if !r.Modified.IsZero() {
			row.Modified = r.Modified.UTC().Format(time.RFC3339)
		}
		o.Runs = append(o.Runs, row)
	}
	return o
}

func selectorCount(s localstate.CleanSelector) int {
	n := 0
	for _, set := range []bool{s.All, s.Keep > 0, s.OlderThan > 0} {
		if set {
			n++
		}
	}
	return n
}

func selectorLabel(s localstate.CleanSelector) string {
	switch {
	case s.All:
		return "all"
	case s.Keep > 0:
		return "keep " + strconv.Itoa(s.Keep)
	case s.OlderThan > 0:
		return "older than " + s.OlderThan.String()
	default:
		return "" // inventory
	}
}

// parseCleanDuration accepts a Go duration and additionally `<n>d` for days, because retention is
// spoken in days and `168h` is the kind of arithmetic a user should not have to do to delete a file.
func parseCleanDuration(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid --older-than %q (want a day count like 7d, or a Go duration like 168h)", raw)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid --older-than %q (want a day count like 7d, or a Go duration like 168h)", raw)
	}
	return d, nil
}

// printClean renders the human report. With no selector it is an INVENTORY plus the three ways to
// select — a `clean` that deletes nothing has to say what it saw and how to act on it, or it reads
// as a command that failed.
func printClean(w io.Writer, rep cleanReport, sel localstate.CleanSelector) {
	for _, loc := range rep.Locations {
		if !loc.Exists {
			fmt.Fprintf(w, "%s (%s): no runs directory\n", loc.Root, loc.Component)
			continue
		}
		count, bytes := 0, int64(0)
		for _, r := range loc.Runs {
			count++
			bytes += r.Bytes
		}
		fmt.Fprintf(w, "%s (%s): %d run(s), %s\n", loc.Root, loc.Component, count, humanBytes(bytes))
		for _, r := range loc.Runs {
			switch {
			case r.Removed:
				fmt.Fprintf(w, "  removed  %-28s %s\n", r.Name, humanBytes(r.Bytes))
			case rep.DryRun && r.Skipped == "":
				fmt.Fprintf(w, "  WOULD REMOVE %-24s %s\n", r.Name, humanBytes(r.Bytes))
			case r.Skipped == localstate.SkipInProgress:
				fmt.Fprintf(w, "  kept     %-28s %s — modified within the last %s, so it may be a run in progress\n",
					r.Name, humanBytes(r.Bytes), localstate.CleanGrace)
			case r.Skipped == localstate.SkipRemoveFailed:
				fmt.Fprintf(w, "  FAILED   %-28s %s — could not be removed\n", r.Name, humanBytes(r.Bytes))
			case r.Skipped == localstate.SkipNotADirectory:
				fmt.Fprintf(w, "  ignored  %-28s (not a run directory)\n", r.Name)
			}
		}
	}
	if !sel.Selective() {
		fmt.Fprintln(w, "\nNothing was removed: no selector was given, and this command does not choose a retention")
		fmt.Fprintln(w, "policy for you — a run directory holds that run's findings, decisions, patch and, for an")
		fmt.Fprintln(w, "apply, its undo. Select with one of:")
		fmt.Fprintln(w, "  --keep <n>          keep the n most recent runs per location")
		fmt.Fprintln(w, "  --older-than 7d     remove runs older than a day count or a Go duration")
		fmt.Fprintln(w, "  --all               remove every prior run")
		fmt.Fprintln(w, "Add --dry-run to any of them to see exactly what would go.")
		return
	}
	verb := "Removed"
	if rep.DryRun {
		verb = "Would remove"
	}
	fmt.Fprintf(w, "\n%s %d run(s), %s. %d remaining.\n", verb, rep.RemovedRuns, humanBytes(rep.FreedBytes), rep.RemainingRuns)
	// Said once, at the end, where it is a reason to run this rather than a warning about having
	// run it: the artifacts hold verbatim copies of everything the models were shown.
	fmt.Fprintln(w, "Run artifacts hold verbatim copies of everything the models were shown, so this frees disk")
	fmt.Fprintln(w, "and removes those copies. It cannot be undone.")
}

// humanBytes renders a size the way a person reads one.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit && exp < 3; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
