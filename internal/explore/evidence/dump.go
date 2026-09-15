package evidence

// This file checks that the evidence database is derivable: exporting the same run directory twice gives
// the same result. Verify compares two things: a canonical dump of every row, which shows the content is a
// function of the run directory, and the file's SHA-256, which catches non-determinism the dump cannot see.

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
// the columns in declared order, the rows sorted, and the tables in exportTables order. Two matching dumps
// mean two exports hold identical content.
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

// dumpTable renders one table's rows as sorted `col=value` lines. Sorting the rendered rows keeps the dump
// independent of insert order and row layout.
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

// renderValue renders one scanned column deterministically, rendering NULL differently from an empty
// string so a change between them is visible.
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

// FileDigest returns the hex SHA-256 of the file at path.
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

// Verify re-exports runDir into a scratch database and compares its canonical dump and file digest with the
// database at dbPath, so anyone holding both can check that one produces the other. The scratch database
// is built in a temporary directory that Verify creates and removes, so the check writes nothing beside
// dbPath and cannot collide with a user's file.
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
