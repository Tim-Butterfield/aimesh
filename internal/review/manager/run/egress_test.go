package run

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// TestEgress_GroupsByDestinationNotBySeat. Four seats behind one provider is ONE disclosure —
// listing it per seat would make a diverse panel look like four leaks and a single-provider panel
// look like one, which inverts the fact the reader is trying to establish.
func TestEgress_GroupsByDestinationNotBySeat(t *testing.T) {
	rows := shapeEgress([]review.ShapeSeat{
		{SeatID: "reviewer", Adapter: "claude-code"},
		{SeatID: "reviewer-2", Adapter: "claude-code"},
		{SeatID: "reviewer-3", Adapter: "codex-cli"},
	}, nil)
	if len(rows) != 2 {
		t.Fatalf("got %d destinations, want 2 (two Anthropic seats are one destination): %+v", len(rows), rows)
	}
	byDest := map[string][]string{}
	for _, r := range rows {
		byDest[r.Destination] = r.Via
	}
	if via := byDest["Anthropic"]; len(via) != 2 || via[0] != "reviewer" || via[1] != "reviewer-2" {
		t.Errorf("Anthropic via = %v, want both seats in run order", via)
	}
	if via := byDest["OpenAI"]; len(via) != 1 {
		t.Errorf("OpenAI via = %v, want one seat", via)
	}
}

// TestEgress_AnInProcessLaneSendsNothing: attributing an egress to a step that never leaves the
// machine is precisely backwards, and the host adjudicator is usually that step.
func TestEgress_AnInProcessLaneSendsNothing(t *testing.T) {
	rows := shapeEgress(
		[]review.ShapeSeat{{SeatID: "reviewer", Adapter: "ollama"}},
		[]review.ShapeLane{{Role: review.RoleAuthorRemediator, Execution: "host", Adapter: "claude-code"}},
	)
	for _, r := range rows {
		for _, v := range r.Via {
			if v == string(review.RoleAuthorRemediator) {
				t.Errorf("an in-process lane was reported as egress to %q", r.Destination)
			}
		}
	}
}

// TestEgress_FullyLocalIsRecognisable. The offline posture is the one docs/security.md recommends
// for sensitive material, so a consumer must be able to establish it without string-matching a
// destination name.
func TestEgress_FullyLocalIsRecognisable(t *testing.T) {
	rows := shapeEgress([]review.ShapeSeat{
		{SeatID: "reviewer", Adapter: "ollama"},
		{SeatID: "reviewer-2", Adapter: "ollama"},
	}, nil)
	if len(rows) != 1 || !rows[0].Local {
		t.Fatalf("a fully local panel = %+v, want one local destination", rows)
	}
	if EgressLeavesTheMachine(rows) {
		t.Error("a fully local panel reports content leaving the machine")
	}
}

// TestEgress_UnknownIsReportedAndCountsAsLeaving is the load-bearing one.
//
// A user-defined ACP instance points at a binary the OPERATOR chose, so this tool cannot say where it
// sends anything. Omitting the row would read as "nothing goes there", and treating unknown as local
// would answer the offline question wrongly in the unsafe direction.
func TestEgress_UnknownIsReportedAndCountsAsLeaving(t *testing.T) {
	rows := shapeEgress([]review.ShapeSeat{
		{SeatID: "reviewer", Adapter: "some-user-defined-acp-instance"},
	}, nil)
	if len(rows) != 1 {
		t.Fatalf("an unknown adapter must still produce a row: %+v", rows)
	}
	if rows[0].Known {
		t.Error("an adapter this tool does not own was reported as known")
	}
	if rows[0].Local {
		t.Error("an unknown destination was reported as local — that is the unsafe reading")
	}
	if !EgressLeavesTheMachine(rows) {
		t.Error("an unknown destination must count as leaving the machine")
	}
	// The label has to tell the operator that THEY are the one who knows.
	if !strings.Contains(rows[0].Destination, "you pointed it") {
		t.Errorf("the unknown label does not say who knows: %q", rows[0].Destination)
	}
}

// TestEgress_RemoteBeforeLocal: a reader scanning for what left the machine should meet those rows
// first, and the order must not depend on which seat happened to be configured first.
func TestEgress_RemoteBeforeLocal(t *testing.T) {
	rows := shapeEgress([]review.ShapeSeat{
		{SeatID: "reviewer", Adapter: "ollama"}, // local, configured FIRST
		{SeatID: "reviewer-2", Adapter: "claude-code"},
	}, nil)
	if len(rows) != 2 {
		t.Fatalf("got %+v", rows)
	}
	if rows[0].Local {
		t.Errorf("the local destination sorted first: %+v", rows)
	}
}

// TestEgress_AGatewayIsNotPresentedAsTheProvider. devin-cli routes onward, and naming it as the
// final recipient would overstate what this tool actually knows.
func TestEgress_AGatewayIsNotPresentedAsTheProvider(t *testing.T) {
	rows := shapeEgress([]review.ShapeSeat{{SeatID: "reviewer", Adapter: "devin-cli"}}, nil)
	if len(rows) != 1 {
		t.Fatalf("got %+v", rows)
	}
	if rows[0].Note == "" {
		t.Error("a gateway destination carries no qualification about routing onward")
	}
}

// TestEgress_NoModelLanesMeansNoClaim: a shape with nothing to invoke asserts nothing rather than
// asserting an empty destination.
func TestEgress_NoModelLanesMeansNoClaim(t *testing.T) {
	if rows := shapeEgress(nil, nil); rows != nil {
		t.Errorf("egress = %+v, want none", rows)
	}
}
