package cli

// This file implements `aimesh explore export --sqlite <out.db> --run <run-dir>`, the optional evidence
// export. It is the only caller of package evidence, so no run writes a database implicitly; the run
// directory remains the system of record.
//
// It is the one place explore writes to a path the user typed. The destination checks (scope confinement,
// the protected-path denylist, no overwrite without --force, and an atomic replace) live in
// internal/evidence/dest.go, so no caller can bypass them; this file only supplies the flags.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/explore/evidence"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// runExport builds the derived SQLite evidence database from a captured run directory.
func runExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cliflags.Style(fs, "aimesh explore export")
	sqlitePath := fs.String("sqlite", "", "path of the SQLite evidence database to write (required). An EXISTING file is refused unless --force: the database is derived, but the file is yours. Protected paths (~/.aimesh/**, .git/**, .env*, key material, IDE/agent config) are refused outright, and --force does not lift that")
	runDir := fs.String("run", "", "path of the captured run directory to export (required; the one `explore --dump-run` printed)")
	verify := fs.Bool("verify", false, "after exporting, REBUILD from the same run directory into a scratch file of its own and compare — the derivability invariant, checked in content and in bytes")
	force := fs.Bool("force", false, "REPLACE an existing --sqlite destination (and clear the stale SQLite sidecars beside it). Never lifts the protected-path denylist")
	asJSON := fs.Bool("json", false, "print the export summary as JSON instead of a human table")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	if strings.TrimSpace(*sqlitePath) == "" || strings.TrimSpace(*runDir) == "" {
		fmt.Fprintln(stderr, "aimesh explore: export requires both --sqlite <out.db> and --run <run-dir>")
		return int(fault.Usage)
	}
	sum, err := evidence.Export(*runDir, *sqlitePath, evidence.Options{Force: *force})
	if err != nil {
		fmt.Fprintf(stderr, "aimesh explore export: %v\n", err)
		return codeOf(err)
	}
	if *verify {
		// Verify rebuilds into its own temporary directory, so nothing is left beside the database.
		if verr := evidence.Verify(*runDir, *sqlitePath); verr != nil {
			fmt.Fprintf(stderr, "aimesh explore export: %v\n", verr)
			return codeOf(verr)
		}
		fmt.Fprintf(stderr, "verified: %s is byte-identical to a fresh export of %s (the database is derivable from the run directory)\n", *sqlitePath, *runDir)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sum)
		return int(fault.OK)
	}
	fmt.Fprintf(stdout, "Exported %s → %s\n", sum.RunDir, sum.DBPath)
	fmt.Fprintf(stdout, "  exploration %s (mode %s), evidence schema v%d, %d row(s)\n",
		sum.ExplorationID, sum.Mode, sum.SchemaVersion, sum.TotalRows)
	fmt.Fprintln(stdout, "  the run directory remains the system of record; this database is a DERIVED, rebuildable view of it")
	tables := make([]string, 0, len(sum.Rows))
	for t, n := range sum.Rows {
		if n > 0 {
			tables = append(tables, t)
		}
	}
	sort.Strings(tables)
	for _, t := range tables {
		fmt.Fprintf(stdout, "    %-28s %d\n", t, sum.Rows[t])
	}
	return int(fault.OK)
}
