package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"

	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/runview"
)

// This file builds what a tool call HANDS BACK: the structuredContent (which must validate against the
// declared outputSchema) and the short human rendering that rides `content`.
//
// Both channels are populated for every result, including failures. Some clients show the model only the
// text; some pass only the structured payload to a validator. A result that lived in one channel would be
// invisible to half of them, which is the same failure mode as reporting a count without its denominator.

// slot is one panel seat on the wire.
type slot struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// panelPick is the panel a call asked for AND the panel that will execute — kept together because the
// echo is only meaningful as a pair.
type panelPick struct {
	source     string // "default" | "profile" | "adhoc"
	profile    string
	count      int // 0 when the caller named none
	reqSeats   []slot
	reqCollate *slot
	// reqCanon is the caller's `canonicalizers` argument, when it supplied one. It is on the pick rather
	// than folded into the plan alone so the echo can say what was ASKED FOR next to what ran.
	reqCanon   []slot
	plan       roster.Plan
	selected   int
	configured int
}

// echo renders the requested-vs-executed panel block. It is REQUIRED in the output schema: a count that
// was narrowed, a profile that resolved somewhere unexpected, or a seat that dropped mid-run is visible
// only by comparing the two halves.
func (p panelPick) echo(out *pipeline.Result) map[string]any {
	req := map[string]any{"source": p.source}
	if p.profile != "" {
		req["profile"] = p.profile
	}
	if p.count > 0 {
		req["count"] = p.count
	}
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
	// WHICH identities held the merge-agreement rule, and WHO CHOSE THEM. The provenance comes from the run
	// itself once one has finished (the pipeline records it); before that, from the resolved panel.
	exec["canonicalizers"] = canonSeats(p.plan.Canonicalizers)
	exec["canonicalizerSource"] = canonicalizerSourceOf(p, out)
	// WHAT THE PAIR IS WORTH, once a run has resolved it. `shared_model` says both canonicalizers ran one
	// model behind two adapters, which makes every merge-agreement — and so every corroboration count in
	// this result — weaker evidence than the same count from two different models. It is reported rather
	// than refused, so reporting it is the whole safeguard.
	if out != nil && out.CanonicalizerIndependence != "" {
		exec["canonicalizerIndependence"] = out.CanonicalizerIndependence
	}
	if out != nil {
		exec["dropped"] = len(out.Dropped)
	}
	return map[string]any{"requested": req, "executed": exec}
}

// canonicalizerSourceOf reports how the canonicalizers were (or will be) chosen: `explicit` when a caller
// or a profile named them, `derived` when the host picks them. A finished run's OWN provenance wins — it is
// the recorded fact, and the two can only differ if the resolution changed under us.
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

// governanceBlock is runview's shared governance projection, made UNCONDITIONAL for MCP. A mode that
// emits no host-computed counts states that fact (`countsEmitted: false`) instead of omitting the block:
// the schema can then require it, and "this mode does not count" stops being indistinguishable from "the
// count was dropped on the way out".
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

// identityCaveatMaps projects the shared caveat list. It is always a non-nil slice: an empty array is the
// positive statement that every seat's identity was verified.
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
	// Whether the blind round 1 was COLD. The caller cannot turn this on and did not ask for it, which is
	// precisely why the result has to say it: without the key, a caller comparing two runs of one question
	// has no way to tell that the second was shown the first's conclusions.
	// A DRY RUN finished the call without exploring anything. It is marked on the payload rather than left
	// to be inferred from an empty result, because the inference a caller would otherwise make — "the panel
	// ran and produced nothing" — is the opposite of what happened. `dryRun` is the flag to branch on;
	// `shape` is its machine-readable content, and the two always travel together.
	if out.Shape != nil {
		structured["dryRun"] = true
		structured["shape"] = runview.Shape(out)
		return structured, renderShape(rec, *out.Shape)
	}
	return structured, renderComplete(structured, out)
}

// renderShape is the text channel's version of the dry run: the total, the stages behind it, and the two
// things a reader must not assume — that the count is a guess, and that anything was proven reachable.
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

// haltResult builds the terminal payload for a run that HALTED. It rides `isError: true` on a successful
// JSON-RPC response — a JSON-RPC error's `data` is routinely flattened or dropped by clients, and losing
// the taxonomy is losing exactly the field the model needs in order to react correctly.
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

// refusalResult builds the payload for a call refused BEFORE any spend — an admission limit, a
// fail-closed panel resolution, an unknown run id. It is the same taxonomy shape as a halt, so a caller
// branches on `reasonCode` without caring whether the refusal came before or after the money.
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

// reasonOf returns the stable machine reason code, NEVER an empty string. `reasonCode` is the field the
// docs tell a caller to branch on, and the output schema marks it required — so a failure that arrived
// without a typed reason must still answer with a code rather than with "". An empty machine-readable
// field forces every consumer to special-case it, and it reads like "no error" to anything that tests
// truthiness.
//
// meshcore's fault.ReasonOf already carries the heavy half: any non-nil error yields a code-shaped
// fallback from the per-exit-code table (`internal_error` for something nobody classified). This is the
// last guard, for the one input that answers "". It is character-for-character reviewmesh's reasonOf,
// so the two MCP surfaces cannot drift apart on what an unclassified halt says.
func reasonOf(err error) string {
	if r := fault.ReasonOf(err); r != "" {
		return r
	}
	return "unclassified"
}

// maxFailureItems + maxFailureText bound the failure breakdown that reaches a third-party inference log.
const (
	maxFailureItems = 16
	maxFailureText  = 2000
)

// capFailure bounds the halt breakdown: a long message is truncated with an explicit marker and a long
// drop list is cut with a stated remainder. Truncation is always ANNOUNCED — a silently shortened failure
// report is worse than a long one, because it reads complete.
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

// renderComplete is the SHORT human rendering of a finished run. It leads with the governance facts a
// reader must not miss (withheld claims, quorum, weak identity) rather than burying them under the
// headline, because the text channel is all some clients ever show the model.
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

// runningResult is what a call returns when the run outlived its inline wait budget: the run id, the
// state, and the panel that IS executing — enough for the caller to report honestly what it started
// before it has an answer.
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

// sortedNames is a small helper for deterministic listings.
func sortedNames(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
