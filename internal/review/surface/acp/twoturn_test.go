package acp

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// THE ACP TWO-TURN WRITE CONTRACT (migration design §9.4, decision D5).
//
// ACP has no lookup surface — not one of its methods accepts a run identifier or returns anything
// about a prior run, and `session/resume` deliberately replays nothing. So a write whose response is
// lost would leave a host holding nothing at all. The answer is the same two-phase rule MCP has,
// expressed in ACP's own unit: turn 1 reports and its RESPONSE hands back the run handle plus a
// fingerprint per accepted finding; turn 2 writes and must carry that handle.
//
// The `select` tests below pin both directions of the narrowing filter: a non-empty `select` is
// HONOURED on a write turn, and an explicitly-empty list is REFUSED rather than let through a
// `len(...) > 0` guard into a full-set write — which is the exact widening the declaration exists to
// prevent, arriving by the one input easiest to leave unchecked.

func TestACPWrite_ApplyWithoutARunHandleIsRefusedBeforeAnyWork(t *testing.T) {
	msg, data, ok := requireRunHandle(review.ModeApply, "", nil)
	if ok {
		t.Fatal("an apply turn with no `fromRun` must be refused: a turn whose response is lost would leave the host holding no handle to a run that may have written")
	}
	if data["reasonCode"] != "write_without_run_handle" {
		t.Fatalf("reasonCode = %v — a host must be able to branch on this without parsing the message", data)
	}
	// The refusal has to teach the sequence, not merely reject: the caller is a model or a host that
	// cannot guess a two-turn protocol from a "no".
	for _, want := range []string{"report", "runDir", "fromRun"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the teaching error must name %q; got %q", want, msg)
		}
	}
}

func TestACPWrite_ReportAndPatchTurnsNeedNoRunHandle(t *testing.T) {
	// A report turn writes nothing. A patch turn writes a diff artifact into the run directory it is
	// itself creating, so neither can leave a caller holding nothing — demanding a handle from them
	// would be ceremony, and ceremony is what teaches callers to paste values that mean nothing.
	for _, m := range []review.Mode{review.ModeReport, review.ModePatch} {
		if _, _, ok := requireRunHandle(m, "", nil); !ok {
			t.Fatalf("mode %q must not require a run handle", m)
		}
	}
}

func TestACPWrite_ApplyWithARunHandleProceeds(t *testing.T) {
	if _, _, ok := requireRunHandle(review.ModeApply, "/runs/prior-report-run", nil); !ok {
		t.Fatal("an apply turn carrying the handle from a completed report turn must proceed")
	}
}

// TestACPWrite_SelectIsHonouredOnAWriteTurn — D8-A. `select` is a REAL filter here, so acceptance is
// what must be pinned: a server that declared it and dropped it would silently widen a host's write
// set, which is why the field can never be merely declared.
func TestACPWrite_SelectIsHonouredOnAWriteTurn(t *testing.T) {
	if _, _, ok := requireRunHandle(review.ModeApply, "/runs/prior-report-run", []string{"sha1:abc"}); !ok {
		t.Fatal("a `select` list on an apply turn carrying a run handle must be accepted — selective apply is implemented")
	}
	// A patch turn produces a write PRODUCT (the diff), so narrowing it is meaningful there too.
	if _, _, ok := requireRunHandle(review.ModePatch, "", []string{"sha1:abc"}); !ok {
		t.Fatal("`select` must be accepted on a patch turn: a diff is a write product, and narrowing it is meaningful")
	}
}

// TestACPWrite_EmptySelectIsRefusedNotWidened carries the danger the whole filter exists for:
// an empty narrowing filter names ZERO findings, and the one
// outcome a caller can neither detect nor survive is having that read as "apply everything".
func TestACPWrite_EmptySelectIsRefusedNotWidened(t *testing.T) {
	for _, empty := range [][]string{{}, {""}, {"  ", ""}} {
		msg, data, ok := requireRunHandle(review.ModeApply, "/runs/prior-report-run", empty)
		if ok {
			t.Fatalf("an empty `select` (%v) must be refused, never treated as \"apply everything\"", empty)
		}
		if data["reasonCode"] != "select_empty" {
			t.Fatalf("reasonCode = %v — a host must branch on this without parsing the message", data)
		}
		if !strings.Contains(msg, "ZERO findings") {
			t.Fatalf("the refusal must say what an empty filter names; got %q", msg)
		}
	}
}

// TestACPWrite_SelectOnAReportTurnIsRefused — a report turn writes nothing, so a filter over what it
// writes could not have had an effect. Accepting it would report success for a narrowing that did
// nothing, which is the same class of lie as dropping the filter.
func TestACPWrite_SelectOnAReportTurnIsRefused(t *testing.T) {
	msg, data, ok := requireRunHandle(review.ModeReport, "", []string{"sha1:abc"})
	if ok {
		t.Fatal("`select` on a report turn must be refused: there is no write for it to narrow")
	}
	if data["reasonCode"] != "select_without_write" {
		t.Fatalf("reasonCode = %v", data)
	}
	if !strings.Contains(msg, "writes nothing") {
		t.Fatalf("the refusal must say why; got %q", msg)
	}
}

func TestACPWrite_AcceptedFingerprintsRideTheReportTurn(t *testing.T) {
	// This is the channel that makes selective apply expressible on ACP at all: a caller cannot select
	// on host-computed values it was never told, and with no lookup surface there is no other moment
	// to tell it.
	out := review.RunOutcome{
		Findings: []review.Finding{
			{ID: "F-001", Kind: "bug", File: "a.go", Title: "boom"},
			{ID: "F-002", Kind: "bug", File: "b.go", Title: "quarantined"},
			{ID: "F-003", Kind: "bug", File: "c.go", Title: "rejected"},
		},
		Decisions: []review.Decision{
			{FindingID: "F-001", Valid: true, State: review.StateReportedValid},
			{FindingID: "F-002", Valid: true, State: review.StateReportedValid,
				Applyable: boolPtr(false), ApplyRefusalReason: "no_workspace_evidence"},
			{FindingID: "F-003", Valid: false, State: review.StateReportedValid},
		},
	}
	rows := acceptedFingerprints(out)
	if len(rows) != 1 {
		t.Fatalf("accepted = %v — only findings the write path would actually consider may be offered as selectors", rows)
	}
	fp, _ := rows[0]["fingerprint"].(string)
	if fp == "" || !strings.HasPrefix(fp, "sha1:") {
		t.Fatalf("fingerprint = %q, want the host-computed identity", fp)
	}
	// THE SECURITY PROPERTY: the selector is never the model-authored Finding.ID. Keying a write set
	// on a model-controlled identifier would let a model relabel findings until "apply only this one"
	// selected something else.
	if fp == "F-001" || strings.Contains(fp, "F-001") {
		t.Fatalf("the selector must not be the model-authored finding id, got %q", fp)
	}
	if rows[0]["file"] != "a.go" {
		t.Fatalf("the row must name the file a human would recognise, got %v", rows[0])
	}
}

func boolPtr(b bool) *bool { return &b }
