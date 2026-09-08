package evidence

// This file implements the DERIVABILITY INVARIANT (design §9): the evidence database is a DERIVED artifact,
// so exporting the same run directory twice must produce the same thing. That is a checkable property, and
// this is the check.
//
// It is verified at two strengths, deliberately:
//
//   - CONTENT: a canonical dump of every table, every row, every column, in a fixed order. This is the
//     invariant that MATTERS — it says the export is a pure function of the run directory.
//   - BYTES: the SHA-256 of the file itself. The export pins the page size and the journal mode and inserts
//     in a fixed order with no timestamps, so this holds too. It is checked as well as the dump because a
//     byte difference the dump cannot see would mean something non-deterministic slipped into the file.
//
// The dump is also the human-readable form of "what is in here", which is why Verify returns it rather than
// only a boolean.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CanonicalDump renders every table of an evidence database as deterministic text: one line per row, with
// the columns in declared order, the rows sorted, and the tables in the fixed exportTables order. It is the
// comparison surface of the derivability invariant — two dumps that match mean two exports carry identical
// content, whatever the file bytes happen to look like.
func CanonicalDump(dbPath string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)&mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var b strings.Builder
	for _, table := range exportTables {
		lines, derr := dumpTable(db, table)
		if derr != nil {
			return "", derr
		}
		fmt.Fprintf(&b, "== %s (%d row(s))\n", table, len(lines))
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

// dumpTable renders one table's rows as sorted `col=value` lines. Sorting the RENDERED rows (rather than
// relying on a primary-key ORDER BY that differs per table) keeps the dump independent of both insert order
// and SQLite's row layout — which is exactly what a content comparison should be.
func dumpTable(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query("SELECT * FROM " + table)
	if err != nil {
		return nil, fmt.Errorf("dump %s: %w", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if serr := rows.Scan(ptrs...); serr != nil {
			return nil, serr
		}
		parts := make([]string, 0, len(cols))
		for i, c := range cols {
			parts = append(parts, c+"="+renderValue(vals[i]))
		}
		out = append(out, strings.Join(parts, "\t"))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// renderValue renders one scanned column deterministically. NULL is a distinct rendering from an empty
// string — in a STRICT schema with typed columns those two mean different things, and a dump that conflated
// them could not detect a change between them.
func renderValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "<NULL>"
	case []byte:
		return string(t)
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

// FileDigest is the SHA-256 of a database file — the byte-level half of the derivability check.
func FileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify re-exports runDir into a scratch database of its OWN and compares BOTH the canonical dump and the
// file digest against an existing export. It is the runnable form of "the database's derivability is itself
// a checkable invariant" (§9): a caller can hand an auditor a database and the run directory it came from,
// and the auditor can check that one really does produce the other.
//
// The scratch database lives in a temporary directory this function CREATES and REMOVES, rather than beside
// dbPath where it used to. Two reasons, both of them about the check not being able to hurt what it is
// checking: a scratch path derived from the destination can collide with a real file the user owns, and a
// check that leaves anything next to the artifact it verified is not a read-only check. Because the scratch
// path is unobservable and disposable, it is built directly — the destination guard has nothing to protect
// there, and Export's no-clobber rule would only be an obstacle to a file nobody else can see.
func Verify(runDir, dbPath string) error {
	rec, rerr := readRun(runDir)
	if rerr != nil {
		return fmt.Errorf("verify: rebuilding from %s: %w", runDir, rerr)
	}
	scratchDir, err := os.MkdirTemp("", "exploremesh-verify-")
	if err != nil {
		return fmt.Errorf("verify: create a scratch directory: %w", err)
	}
	defer os.RemoveAll(scratchDir)
	scratchPath := filepath.Join(scratchDir, "verify.db")
	if _, berr := build(runDir, rec, scratchPath); berr != nil {
		return fmt.Errorf("verify: rebuilding from %s: %w", runDir, berr)
	}
	haveDump, err := CanonicalDump(dbPath)
	if err != nil {
		return err
	}
	wantDump, err := CanonicalDump(scratchPath)
	if err != nil {
		return err
	}
	if haveDump != wantDump {
		return fmt.Errorf("verify: %s does not match a fresh export of %s — the database is NOT derivable from the run directory (content differs)", dbPath, runDir)
	}
	haveSum, err := FileDigest(dbPath)
	if err != nil {
		return err
	}
	wantSum, err := FileDigest(scratchPath)
	if err != nil {
		return err
	}
	if haveSum != wantSum {
		return fmt.Errorf("verify: %s has identical CONTENT to a fresh export of %s but different bytes (%s vs %s) — the export is not byte-deterministic", dbPath, runDir, haveSum[:12], wantSum[:12])
	}
	return nil
}
