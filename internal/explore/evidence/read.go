package evidence

// This file READS a captured run directory into memory. It is deliberately the only half of the export that
// knows about files, and it reads the persisted JSON — never the live in-memory types — because the system
// of record is the run directory (design §9). Two consequences worth stating:
//
//   - A MISSING MANIFEST MEANS AN UNFINISHED RUN. The manifest is written last and renamed into place, so
//     its absence is the capture layer's own signal that the directory is not a complete record. The export
//     refuses rather than exporting a partial one.
//   - THE APPEND-ONLY LEDGER IS READ FROM ITS JSONL, not from the confirmation record's embedded copy. The
//     JSONL is the append-only artifact §9 designates; the embedded copy exists for a different purpose.
//
// Where a persisted shape happens to be an exported Go type that round-trips cleanly (schema.RawTask,
// govern.Report, govern.Decision, canon.LedgerRow, …) the reader uses it: the export must consume exactly
// the bytes the capture layer writes, and re-declaring those shapes here would create two definitions of one
// wire format that could drift apart silently. Where the persisted shape CANNOT round-trip — round.Round has
// unexported fields and marshals through a custom encoder — a local wire struct mirrors the encoder's output.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/capture"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/round"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// Artifact file names inside a run directory (the capture layer's own vocabulary, restated here as the
// export's read contract).
const (
	fileManifest         = "manifest.json"
	fileTask             = "task.json"
	fileFormulation      = "formulation.json"
	fileRounds           = "rounds.json"
	fileMergeLedger      = "merge-ledger.jsonl"
	fileMergeLedgerProv  = "merge-ledger-provisional.jsonl"
	fileConfirmation     = "confirmation.json"
	fileGovernanceClaims = "governance-claims.json"
	fileDecision         = "decision.json"
	filePreflight        = "preflight.json"
	fileMediations       = "mediations.json"
	fileDegraded         = "degraded.json"
)

// roundWire mirrors round.Round's custom JSON encoding (its fields are unexported so the immutable blind
// baseline cannot be rewritten — which also means it has no decoder, hence this).
type roundWire struct {
	Index       int               `json:"index"`
	ID          string            `json:"id"`
	Blind       bool              `json:"blind"`
	PayloadHash string            `json:"payloadHash"`
	Envelopes   []schema.Envelope `json:"envelopes"`
	Carried     *round.Artifact   `json:"carried,omitempty"`
}

// runRecord is a whole captured run, read into memory. Every field is exactly what the run directory holds;
// nothing here is derived, defaulted or repaired — the derivation happens in evidence.go, over this.
type runRecord struct {
	Dir          string
	Manifest     capture.ManifestV1
	Task         schema.RawTask
	HasTask      bool
	Formulation  schema.Formulation
	Rounds       []roundWire
	Ledger       []canon.LedgerRow // the CONFIRMED (or only) revision
	Provisional  []canon.LedgerRow // the superseded revision, retained on disk
	Confirmation *canon.Confirmation
	Governance   *govern.Report
	Decision     *govern.Decision
	Preflight    []pipeline.PreflightRecord
	Mediations   []round.Mediation
	Degraded     *schema.DegradedOutput
}

// readRun loads a captured run directory. It fails on a missing/unreadable manifest (an unfinished run) and
// on a malformed artifact — an export that silently skipped a file it could not parse would produce a
// database that looks complete and is not.
func readRun(dir string) (*runRecord, error) {
	rec := &runRecord{Dir: dir}
	if err := readJSONFile(dir, fileManifest, &rec.Manifest, true); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("run directory %s has no %s — the manifest is written LAST and renamed into place, so its absence means the run did not finish writing and there is nothing complete to export", dir, fileManifest)
		}
		return nil, err
	}
	if rec.Manifest.RunID == "" {
		return nil, fmt.Errorf("run directory %s: the manifest carries no runId", dir)
	}
	if err := readJSONFile(dir, fileTask, &rec.Task, false); err != nil {
		return nil, err
	}
	rec.HasTask = rec.Task.Purpose != ""
	if err := readJSONFile(dir, fileFormulation, &rec.Formulation, false); err != nil {
		return nil, err
	}
	if err := readJSONFile(dir, fileRounds, &rec.Rounds, false); err != nil {
		return nil, err
	}
	if err := readJSONFile(dir, filePreflight, &rec.Preflight, false); err != nil {
		return nil, err
	}
	if err := readJSONFile(dir, fileMediations, &rec.Mediations, false); err != nil {
		return nil, err
	}
	var err error
	if rec.Ledger, err = readLedger(dir, fileMergeLedger); err != nil {
		return nil, err
	}
	if rec.Provisional, err = readLedger(dir, fileMergeLedgerProv); err != nil {
		return nil, err
	}
	if rec.Confirmation, err = readOptional[canon.Confirmation](dir, fileConfirmation); err != nil {
		return nil, err
	}
	if rec.Governance, err = readOptional[govern.Report](dir, fileGovernanceClaims); err != nil {
		return nil, err
	}
	if rec.Decision, err = readOptional[govern.Decision](dir, fileDecision); err != nil {
		return nil, err
	}
	if rec.Degraded, err = readOptional[schema.DegradedOutput](dir, fileDegraded); err != nil {
		return nil, err
	}
	// A run with no recorded round still exports (a halt before the fan-out is a real, exportable record);
	// but a run whose rounds file exists and is empty while envelopes were captured is a corrupt directory.
	if len(rec.Rounds) == 0 && len(rec.Manifest.Envelopes) > 0 {
		return nil, fmt.Errorf("run directory %s: the manifest lists %d envelope(s) but %s records no round — the directory is inconsistent",
			dir, len(rec.Manifest.Envelopes), fileRounds)
	}
	return rec, nil
}

// readJSONFile decodes one artifact. When required is false a missing file leaves the target untouched (the
// artifact simply did not apply to this mode); a PRESENT but malformed file is always an error.
func readJSONFile(dir, name string, v any, required bool) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if !required && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if jerr := json.Unmarshal(b, v); jerr != nil {
		return fmt.Errorf("run directory %s: %s is malformed: %w", dir, name, jerr)
	}
	return nil
}

// readOptional decodes an artifact that only some modes produce, returning nil when it is absent.
func readOptional[T any](dir, name string) (*T, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var v T
	if jerr := json.Unmarshal(b, &v); jerr != nil {
		return nil, fmt.Errorf("run directory %s: %s is malformed: %w", dir, name, jerr)
	}
	return &v, nil
}

// readLedger reads an append-only merge-ledger JSONL, one row per line, IN FILE ORDER. Order is part of the
// artifact: the revision hash is a chain over the rows in exactly this sequence, so reordering them would
// break the very property the ledger exists to provide.
func readLedger(dir, name string) ([]canon.LedgerRow, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var rows []canon.LedgerRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20) // a ledger row is small; the cap only bounds a corrupt file
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var row canon.LedgerRow
		if jerr := json.Unmarshal(sc.Bytes(), &row); jerr != nil {
			return nil, fmt.Errorf("run directory %s: %s line %d is malformed: %w", dir, name, line, jerr)
		}
		rows = append(rows, row)
	}
	if serr := sc.Err(); serr != nil {
		return nil, fmt.Errorf("run directory %s: reading %s: %w", dir, name, serr)
	}
	return rows, nil
}
