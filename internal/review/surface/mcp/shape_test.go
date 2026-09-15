package mcp_test

import (
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// A dry run's shape reaches an MCP caller, as on the CLI; without it a planned status and empty
// findings would look like a review that found nothing.
func TestDryRun_TheShapeReachesTheWire(t *testing.T) {
	ws := workspaceFixture(t)
	// The manager's dry-run stop is tested in the run package; this covers the projection.
	planned := &review.RunOutcome{
		Status: "planned",
		Shape: &review.RunShape{
			Mode: review.ModeReport, Surface: "mcp",
			Seats:         []review.ShapeSeat{{SeatID: "reviewer", Adapter: "fake", Model: "fake-model"}},
			MinModelCalls: 1, MaxModelCalls: 6,
			Payload: review.ShapePayload{Files: 2, Bytes: 97, Paths: []string{"a.go", "b.go"}},
			Egress: []review.ShapeEgress{
				{Destination: "this machine", Local: true, Known: true, Via: []string{"reviewer"}},
			},
		},
	}
	c := serve(t, newServer(t, &fakeReviewer{outcome: planned}, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	res := c.tool(t, "review_report", map[string]any{"workspace": ws, "dryRun": true})
	if res.rpc != nil {
		t.Fatalf("a dry run must not error: %+v", res.rpc)
	}
	if got, _ := res.structured["status"].(string); got != "planned" {
		t.Fatalf("status = %q, want \"planned\"", got)
	}
	shape, ok := res.structured["shape"].(map[string]any)
	if !ok {
		t.Fatalf("no shape on a dry run — the caller paid a round trip and learned nothing: %v", keysOf(res.structured))
	}
	// The fields that make a dry run useful.
	for _, k := range []string{"seats", "minModelCalls", "maxModelCalls", "payload"} {
		if _, present := shape[k]; !present {
			t.Errorf("shape carries no %q", k)
		}
	}
	// A range, since a review iterates until adjudication converges.
	lo, hi := shape["minModelCalls"], shape["maxModelCalls"]
	if lo == nil || hi == nil {
		t.Error("the call bounds must both be present — one end of a range is not a price")
	}
	// The empty findings mean nothing was looked at.
	if f, _ := res.structured["findings"].([]any); len(f) != 0 {
		t.Errorf("a dry run reported %d finding(s) — it convenes nobody", len(f))
	}
	// Egress reaches the wire, so a calling model can see who would receive the content.
	eg, ok := shape["egress"].([]any)
	if !ok || len(eg) == 0 {
		t.Fatalf("the shape carries no egress: %v", shape["egress"])
	}
	row, _ := eg[0].(map[string]any)
	if row["local"] != true || row["known"] != true {
		t.Errorf("egress row = %v, want the local/known flags a consumer branches on", row)
	}
}

// A real run carries no shape; the key's presence means planned, not performed.
func TestDryRun_ARealRunCarriesNoShape(t *testing.T) {
	ws := workspaceFixture(t)
	c := serve(t, newServer(t, &fakeReviewer{}, func(s *mcp.Server) { s.Ceiling = []string{ws} }))
	res := c.tool(t, "review_report", map[string]any{"workspace": ws})
	if res.rpc != nil {
		t.Fatalf("unexpected error: %+v", res.rpc)
	}
	if _, present := res.structured["shape"]; present {
		t.Error("a real run carried a shape — a shape is a prediction, and this run has facts")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
