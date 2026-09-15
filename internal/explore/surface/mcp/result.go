package mcp

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/runview"
)

// This file builds tool results: the structuredContent, which must match the tool's outputSchema, and a
// short text rendering. Every result fills both, since some clients show only one.

// slot is one panel seat as sent by the caller.
type slot struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// panelPick holds the panel a call requested and the plan that runs.
type panelPick struct {
	source     string // always "adhoc"
	reqSeats   []slot
	reqCollate *slot
	reqCanon   []slot // the caller's canonicalizers, if any
	plan       roster.Plan
	selected   int
	configured int
}

// echo renders the requested and executed panel. out is nil while the run has not finished.
func (p panelPick) echo(out *pipeline.Result) map[string]any {
	req := map[string]any{"source": p.source}
	if len(p.reqSeats) > 0 {
		req["explorers"] = seatMaps(p.reqSeats)
	}
	if p.reqCollate != nil {
		req["collator"] = seatMap(*p.reqCollate)
	}
	if len(p.reqCanon) > 0 {
		req["canonicalizers"] = seatMaps(p.reqCanon)
	}
	exec := map[string]any{
		"explorers":  planSeats(p.plan),
		"collator":   map[string]any{"adapter": p.plan.Collator.Adapter, "model": p.plan.Collator.Model, "effort": p.plan.Collator.Effort},
		"selected":   p.selected,
		"configured": p.configured,
	}
	exec["canonicalizers"] = canonSeats(p.plan.Canonicalizers)
	exec["canonicalizerSource"] = canonicalizerSourceOf(p, out)
	if out != nil && out.CanonicalizerIndependence != "" {
		exec["canonicalizerIndependence"] = out.CanonicalizerIndependence
	}
	if out != nil {
		exec["dropped"] = len(out.Dropped)
	}
	return map[string]any{"requested": req, "executed": exec}
}

// canonicalizerSourceOf returns `explicit` or `derived`, preferring the finished run's recorded provenance.
func canonicalizerSourceOf(p panelPick, out *pipeline.Result) string {
	if out != nil && out.CanonicalizerProvenance != "" {
		return out.CanonicalizerProvenance
	}
	if len(p.plan.Canonicalizers) > 0 {
		return pipeline.ProvenanceExplicit
	}
	return pipeline.ProvenanceDerived
}

func canonSeats(cs []roster.Explorer) []map[string]any {
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]any{"adapter": c.Adapter, "model": c.Model, "effort": c.Effort})
	}
	return out
}

func seatMap(s slot) map[string]any {
	return map[string]any{"adapter": s.Adapter, "model": s.Model, "effort": s.Effort}
}

func seatMaps(ss []slot) []map[string]any {
	out := make([]map[string]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, seatMap(s))
	}
	return out
}

func planSeats(p roster.Plan) []map[string]any {
	out := make([]map[string]any, 0, len(p.Explorers))
	for _, e := range p.Explorers {
		out = append(out, map[string]any{"adapter": e.Adapter, "model": e.Model, "effort": e.Effort})
	}
	return out
}

// governanceBlock returns runview.Governance with `countsEmitted` added. For a mode without counts it
// returns a block saying so, since the output schema requires the block.
func governanceBlock(out pipeline.Result) map[string]any {
	gov := runview.Governance(out)
	if gov == nil {
		return map[string]any{
			"countsEmitted": false,
			"note":          "this mode emits no host-computed counts — there is no tally to report, which is different from a tally of zero",
			"panelSize":     len(out.Envelopes),
		}
	}
	gov["countsEmitted"] = true
	return gov
}

// identityCaveatMaps converts runview.IdentityCaveats to maps. The slice is never nil; empty means every
// seat was verified.
func identityCaveatMaps(out pipeline.Result) []map[string]any {
	caveats := runview.IdentityCaveats(out)
	res := make([]map[string]any, 0, len(caveats))
	for _, c := range caveats {
		m := map[string]any{"role": c.Role, "identityStatus": c.Status}
		if c.Adapter != "" {
			m["adapter"] = c.Adapter
		}
		if c.Model != "" {
			m["model"] = c.Model
		}
		if c.Effort != "" {
			m["effort"] = c.Effort
		}
		if c.Evidence != "" {
			m["identityEvidence"] = c.Evidence
		}
		if c.Caveat != "" {
			m["caveat"] = c.Caveat
		}
		res = append(res, m)
	}
	return res
}

// completeResult builds the terminal payload for a run that finished cleanly.
func completeResult(rec *record, raw schema.RawTask, pick panelPick, out pipeline.Result, capturedID string) (map[string]any, string) {
	structured := map[string]any{
		"runId":                rec.ID,
		"state":                StateComplete,
		"tool":                 rec.Tool,
		"mode":                 runview.EffectiveMode(out.Mode),
		"purpose":              raw.Purpose,
		"criteria":             raw.Criteria,
		"priorContextSupplied": strings.TrimSpace(raw.PriorContext) != "",
		"captured":             capturedID != "",
		"panel":                pick.echo(&out),
		"governance":           governanceBlock(out),
		"identityCaveats":      identityCaveatMaps(out),
	}
	if detail := runview.ModeDetail(out); detail != nil {
		structured["result"] = detail
	}
	// A dry run is flagged explicitly so it is not mistaken for a run that found nothing.
	if out.Shape != nil {
		structured["dryRun"] = true
		structured["shape"] = runview.Shape(out)
		return structured, renderShape(rec, *out.Shape)
	}
	return structured, renderComplete(structured, out)
}

// renderShape renders a dry run as text: the call total, the stages, and the fact that no model was
// contacted.
func renderShape(rec *record, sh pipeline.Shape) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dry run (mode %s, run %s): nothing was spent.\n", sh.Mode, rec.ID)
	fmt.Fprintf(&b, "It would make %d model call(s) exactly — %d explorer(s) over %d round(s), dual=%t confirm=%t.\n",
		sh.ModelCalls, len(sh.Explorers), sh.Rounds, sh.Policy.Dual, sh.Policy.Confirm)
	for _, c := range sh.Calls {
		label := c.Phase
		if c.Round > 0 {
			label = fmt.Sprintf("%s round %d", c.Phase, c.Round)
		}
		fmt.Fprintf(&b, "  %s: %d\n", label, c.Calls)
	}
	fmt.Fprintf(&b, "The round-1 prompt is %d bytes, identical for every explorer (payload sha256 %s); it is in structuredContent.shape.payload.\n",
		sh.Payload.PromptBytes, sh.Payload.PayloadHash)
	b.WriteString("The count is exact rather than a range: an exploration's round count is fixed by its mode contract. Only a halt makes it fewer.\n")
	b.WriteString("The identity pre-flight was NOT run — it is a real call per governed role. Nothing here says the collator answered.")
	return b.String()
}

// haltResult builds the payload for a halted or cancelled run, returned as an `isError` tool result.
func haltResult(rec *record, pick panelPick, raw schema.RawTask, out pipeline.Result, err error, capturedID string, cancelled bool) (map[string]any, string) {
	state := StateHalted
	if cancelled {
		state = StateCancelled
	}
	structured := map[string]any{
		"runId":           rec.ID,
		"state":           state,
		"tool":            rec.Tool,
		"mode":            runview.EffectiveMode(rec.Mode),
		"captured":        capturedID != "",
		"exitCode":        int(fault.CodeOf(err)),
		"haltClass":       runview.HaltClass(err),
		"reasonCode":      reasonOf(err),
		"failure":         capFailure(runview.Failure(raw, out, err)),
		"panel":           pick.echo(&out),
		"identityCaveats": identityCaveatMaps(out),
	}
	if cancelled {
		structured["haltClass"] = "cancelled"
		structured["reasonCode"] = "run_cancelled"
	}
	text := fmt.Sprintf("Exploration %s (run %s): %s [haltClass=%v reasonCode=%v exitCode=%v]",
		state, rec.ID, err.Error(), structured["haltClass"], structured["reasonCode"], structured["exitCode"])
	if n := len(out.Dropped); n > 0 {
		text += fmt.Sprintf("\n%d explorer(s) were dropped; see failure.dropped for each reason.", n)
	}
	return structured, text
}

// refusalResult builds the payload for a call refused before any spend, such as an unknown run id. It has
// the same fields as a halt.
func refusalResult(runID string, err error) (map[string]any, string) {
	structured := map[string]any{
		"state":      StateHalted,
		"exitCode":   int(fault.CodeOf(err)),
		"haltClass":  runview.HaltClass(err),
		"reasonCode": reasonOf(err),
	}
	structured["runId"] = runID
	return structured, err.Error()
}

// reasonOf returns err's reason code, or "unclassified" when there is none. The output schema requires a
// non-empty reasonCode. It matches the review MCP surface's reasonOf.
func reasonOf(err error) string {
	if r := fault.ReasonOf(err); r != "" {
		return r
	}
	return "unclassified"
}

// Limits on the failure breakdown returned to the client.
const (
	maxFailureItems = 16
	maxFailureText  = 2000
)

// capFailure truncates a long failure message and a long drop list, marking each truncation.
func capFailure(f map[string]any) map[string]any {
	if msg, ok := f["message"].(string); ok && len(msg) > maxFailureText {
		f["message"] = msg[:maxFailureText] + fmt.Sprintf("… [truncated, %d bytes total]", len(msg))
	}
	if dropped, ok := f["dropped"].([]map[string]any); ok && len(dropped) > maxFailureItems {
		f["dropped"] = dropped[:maxFailureItems]
		f["droppedOmitted"] = len(dropped) - maxFailureItems
	}
	return f
}

// renderComplete renders a finished run as short text, including withheld claims, quorum and identity
// caveats.
func renderComplete(structured map[string]any, out pipeline.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Exploration complete (mode %v, run %v).\n", structured["mode"], structured["runId"])
	fmt.Fprintf(&b, "Panel: %d explorer(s) executed, %d dropped.\n", len(out.Envelopes), len(out.Dropped))
	if detail, ok := structured["result"].(map[string]any); ok {
		if s, ok := detail["summary"].(string); ok && s != "" {
			fmt.Fprintf(&b, "Summary: %s\n", s)
		}
	}
	gov, _ := structured["governance"].(map[string]any)
	if counts, _ := gov["countsEmitted"].(bool); counts {
		fmt.Fprintf(&b, "Governance (host-computed): %v claim(s), %v withheld, %v contested; quorum met: %v.\n",
			gov["claims"], gov["claimsWithheld"], gov["claimsContested"], gov["quorumMet"])
		fmt.Fprintf(&b, "Counts are computed by the host over the blind responses — report them as given.\n")
	} else {
		fmt.Fprintf(&b, "Governance: this mode emits no host-computed counts.\n")
	}
	caveats, _ := structured["identityCaveats"].([]map[string]any)
	if len(caveats) == 0 {
		b.WriteString("Identity: every seat verified.\n")
	} else {
		fmt.Fprintf(&b, "Identity caveats: %d seat(s) could NOT be verified — ", len(caveats))
		parts := make([]string, 0, len(caveats))
		for _, c := range caveats {
			parts = append(parts, fmt.Sprintf("%v %v/%v (%v)", c["role"], c["adapter"], c["model"], c["identityStatus"]))
		}
		b.WriteString(strings.Join(parts, "; ") + ". Each of these seats contributed normally — identity is reported, not enforced.\n")
	}
	b.WriteString("The full machine-readable result is in structuredContent.")
	return b.String()
}

// runningResult builds the payload for a run still running after the inline wait: its id, state and panel.
func runningResult(rec *record, pick panelPick, waited int) (map[string]any, string) {
	structured := map[string]any{
		"runId":         rec.ID,
		"state":         StateRunning,
		"tool":          rec.Tool,
		"mode":          rec.Mode,
		"waitedSeconds": waited,
		"panel":         pick.echo(nil),
	}
	text := fmt.Sprintf("Run %s is still running after %ds (%d explorer(s) in the panel). Poll explore_run_status with this runId, then fetch explore_run_result. Do NOT start another run for the same question — re-issuing with the same idempotencyKey returns this run.",
		rec.ID, waited, len(pick.plan.Explorers))
	return structured, text
}
