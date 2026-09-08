package run

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
)

// The caller's W3C trace context reaches the RUN RECORD, and says what it does not measure.
//
// HONEST NOTE ON THIS TEST'S STRENGTH, because the repo's rule is that a new test must fail against
// the code it was written for. This one CANNOT fail at the prior HEAD — it references
// `Request.Trace` and `review.Trace`, which did not exist, so at HEAD it fails to COMPILE rather
// than to assert. That is a weaker proof, and it is stated rather than glossed. Two things make up
// for it:
//
//  1. The end-to-end proof over the real wire lives in exploremesh, where the same feature can be
//     exercised without any new symbol: `TestSubprocess_ModernTraceContextReachesTheRunRecord` sends
//     `_meta.traceparent` to the shipped binary and reads the run directory back. That test compiles
//     unchanged at HEAD and FAILS there, because the transport already parsed the key and nothing
//     recorded it.
//  2. The MUTATION CHECKS below, which are the real defence for this file. Each was performed by
//     reverting one line and confirming the failure named beside it.
//
// MUTATIONS VERIFIED (revert the change, run this test, get the named failure):
//   - delete `st["trace"] = o.Trace` in runState  → "run-state.json carries no trace block"
//   - drop `Trace: req.Trace` from the RunOutcome literal in RunContext → same failure, which is why
//     the outcome carries it rather than the halt writer taking a request it does not have
//   - change `Measures: MeasuresCorrelationOnly` to "" → "measures = \"\"": the record would carry a
//     correlation id with no statement of what it accounts for, which is the one thing this block
//     must never do silently
func TestRunState_CarriesTheCallersTraceContext(t *testing.T) {
	rec := &callRecorder{}
	m := recorderManager(t, rec)
	ws, _ := makeWorkspace(t)

	const (
		parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		state  = "rojo=00f067aa0ba902b7"
		bag    = "userId=alice"
	)
	outcome, err := m.Run(Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "mcp", Profile: "recorder",
		Trace: review.NewTrace(parent, state, bag),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	st := readRunState(t, outcome.RunDir)
	trace, ok := st["trace"].(map[string]any)
	if !ok {
		t.Fatalf("run-state.json carries no trace block; the caller's trace context was accepted and dropped:\n%v", st)
	}
	for _, want := range []struct{ key, value string }{
		{"traceparent", parent}, {"tracestate", state}, {"baggage", bag},
	} {
		if got, _ := trace[want.key].(string); got != want.value {
			t.Errorf("trace.%s = %q, want the caller's own %q carried verbatim", want.key, got, want.value)
		}
	}
	if got, _ := trace["measures"].(string); got != review.MeasuresCorrelationOnly {
		t.Errorf("measures = %q, want %q — a run record that carries a trace id without saying what it accounts for invites a reader to assume the run is metered, and no run here records tokens or cost",
			got, review.MeasuresCorrelationOnly)
	}

	// It also rides the OUTCOME, so a surface projects the same value the record holds rather than
	// re-reading the run directory for it.
	if outcome.Trace == nil || outcome.Trace.TraceParent != parent {
		t.Errorf("outcome.Trace = %+v, want the request's own", outcome.Trace)
	}
}

// A run whose caller sent no trace context writes NO `trace` key. The absence is load-bearing: it is
// what keeps every CLI run's record byte-identical to one written before the field existed, which is
// what let this ship without moving run-state's schemaVersion.
func TestRunState_NoTraceContextWritesNoTraceBlock(t *testing.T) {
	rec := &callRecorder{}
	m := recorderManager(t, rec)
	ws, _ := makeWorkspace(t)

	outcome, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "recorder"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	st := readRunState(t, outcome.RunDir)
	if _, present := st["trace"]; present {
		t.Errorf("a run whose caller sent no trace context wrote a trace block anyway: %v", st["trace"])
	}
}

// The written artifact validates against its PUBLISHED schema, with and without a trace block.
//
// `docs/schema/run-state.schema.json` is `additionalProperties: false`, so adding a key to the
// artifact without adding it to the schema would turn the schema into a false description of what
// this repo writes — the exact defect `schemaartifact_test.go` exists to prevent for a different
// artifact. This is that check for run-state.
func TestRunState_ValidatesAgainstItsPublishedSchema(t *testing.T) {
	root := repoRootFromPackage(t)
	schema, cerr := jsonschema.CompileFile(filepath.Join(root, "docs", "schema", "run-state.schema.json"))
	if cerr != nil {
		t.Fatalf("compile run-state.schema.json: %v", cerr)
	}
	for _, tc := range []struct {
		name  string
		trace *review.Trace
	}{
		{"with a trace context", review.NewTrace("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "", "")},
		{"without one", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &callRecorder{}
			m := recorderManager(t, rec)
			ws, _ := makeWorkspace(t)
			outcome, err := m.Run(Request{
				Workspace: ws, Mode: review.ModeReport, Surface: "mcp", Profile: "recorder", Trace: tc.trace,
			})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			b := []byte(read(t, filepath.Join(outcome.RunDir, "run-state.json")))
			if verr := schema.ValidateJSON(b); verr != nil {
				t.Fatalf("run-state.json does not validate against docs/schema/run-state.schema.json — the schema no longer describes what this repo writes: %v\n%s", verr, b)
			}
		})
	}
}

func readRunState(t *testing.T, runDir string) map[string]any {
	t.Helper()
	var st map[string]any
	if err := json.Unmarshal([]byte(read(t, filepath.Join(runDir, "run-state.json"))), &st); err != nil {
		t.Fatalf("decode run-state.json: %v", err)
	}
	return st
}
