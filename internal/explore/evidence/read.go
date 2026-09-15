package evidence

// This file reads a captured run directory into memory. It is the only part of the export that reads files,
// and it reads the persisted JSON, since the run directory is the system of record.
//
//   - A missing manifest means an unfinished run: the manifest is written last, so the export refuses.
//   - The merge ledger is read from its append-only JSONL file, not from the copy embedded in the
//     confirmation record.
//
// Persisted shapes are decoded into the Go types that wrote them, so there is one definition of each
// format. round.Round has unexported fields and a custom encoder, so roundWire mirrors its encoding.

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

// Artifact file names inside a run directory, as the capture package writes them.
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

// roundWire mirrors round.Round's JSON encoding. round.Round keeps its fields unexported and has no decoder.
type roundWire struct {
	Index       int               `json:"index"`
	ID          string            `json:"id"`
	Blind       bool              `json:"blind"`
	PayloadHash string            `json:"payloadHash"`
	Envelopes   []schema.Envelope `json:"envelopes"`
	Carried     *round.Artifact   `json:"carried,omitempty"`
}

// runRecord is a captured run read into memory, exactly as the run directory holds it. Derivation happens in
// evidence.go.
type runRecord struct {
	Dir          string
	Manifest     capture.ManifestV1
	Task         schema.RawTask
	HasTask      bool
	Formulation  schema.Formulation
	Rounds       []roundWire
	Ledger       []canon.LedgerRow // the confirmed (or only) revision
	Provisional  []canon.LedgerRow // the superseded revision, retained on disk
	Confirmation *canon.Confirmation
	Governance   *govern.Report
	Decision     *govern.Decision
	Preflight    []pipeline.PreflightRecord
	Mediations   []round.Mediation
	Degraded     *schema.DegradedOutput
}

// readRun loads a captured run directory. A missing or unreadable manifest, or a malformed artifact, is an
// error rather than a skipped file.
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
	// A run that halted before the fan-out has no round and still exports; envelopes without a round mean
	// the directory is inconsistent.
	if len(rec.Rounds) == 0 && len(rec.Manifest.Envelopes) > 0 {
		return nil, fmt.Errorf("run directory %s: the manifest lists %d envelope(s) but %s records no round — the directory is inconsistent",
			dir, len(rec.Manifest.Envelopes), fileRounds)
	}
	return rec, nil
}

// readJSONFile decodes one artifact into v. When required is false a missing file leaves v unchanged; a
// malformed file is always an error.
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

// readLedger reads a merge-ledger JSONL file, one row per line, in file order. The order matters because
// the revision hash chains over the rows in sequence.
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
