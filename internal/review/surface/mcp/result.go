package mcp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// This file builds tool results: the structuredContent, which must validate against the declared
// outputSchema, and the short text rendering in content. Both are populated for every result,
// including failures, because some clients show the model only one of them.
//
// Two rules are enforced here:
//
//   - No host paths: the projection's runDir is dropped.
//   - Nothing is re-derived: agreement counts, refusals, severities and dispositions are copied from
//     the run's adjudication.

// seatSpecMap renders one requested seat.
func seatSpecMap(s review.SeatSpec) map[string]any {
	m := map[string]any{"adapter": s.Adapter, "model": s.Model}
	if s.Effort != "" {
		m["effort"] = s.Effort
	}
	return m
}

// panelPick is the panel a call requested, kept to pair with the executed roster.
type panelPick struct {
	source    string // "adhoc": every panel is composed by the call
	reviewers []review.SeatSpec
	roles     map[review.Role]review.SeatSpec // cross_check / verifier / author_remediator
}

// echo renders the requested and executed roster. The output schema requires it, since a halted seat
// is visible only by comparing the two.
func (p panelPick) echo(executed []runview.PanelSeat) map[string]any {
	req := map[string]any{"source": p.source}
	if len(p.reviewers) > 0 {
		seats := make([]map[string]any, 0, len(p.reviewers))
		for _, s := range p.reviewers {
			seats = append(seats, seatSpecMap(s))
		}
		req["reviewers"] = seats
	}
	for role, seat := range p.roles {
		req[string(role)] = seatSpecMap(seat)
	}
	exec := make([]map[string]any, 0, len(executed))
	for _, s := range executed {
		row := map[string]any{
			"seatId": s.SeatID, "index": s.Index, "adapter": s.Adapter, "model": s.Model,
			"status": s.Status, "rounds": s.Rounds, "findings": s.Findings,
		}
		if s.Effort != "" {
			row["effort"] = s.Effort
		}
		if s.IdentityTier != "" {
			row["identityTier"] = s.IdentityTier
		}
		if s.ReasonCode != "" {
			row["reasonCode"] = s.ReasonCode
		}
		exec = append(exec, row)
	}
	return map[string]any{"requested": req, "executed": exec}
}

// findingMaps projects the run view's findings with their per-seat provenance, as the host computed
// it.
func findingMaps(view runview.View) ([]map[string]any, int, int) {
	out := make([]map[string]any, 0, len(view.Findings))
	accepted, quarantined := 0, 0
	for _, f := range view.Findings {
		row := map[string]any{
			// The fingerprint is how a caller selects findings for review_remediate, and review_report is the only
			// place it is disclosed. It is not the model-authored id, which a model could relabel.
			"fingerprint": f.Fingerprint,
			"id":          f.ID, "severity": f.Severity, "kind": f.Kind,
			"summary": f.Summary, "disposition": f.Disposition,
		}
		if f.File != "" {
			row["file"] = f.File
		}
		if f.Location != "" {
			row["location"] = f.Location
		}
		if len(f.Sources) > 0 {
			row["sources"] = f.Sources
		}
		if f.Applyable != nil {
			row["applyable"] = *f.Applyable
			if !*f.Applyable {
				quarantined++
			}
		}
		if f.ApplyRefusalReason != "" {
			row["applyRefusalReason"] = f.ApplyRefusalReason
		}
		if len(f.SupportingSeats) > 0 {
			seats := make([]map[string]any, 0, len(f.SupportingSeats))
			for _, s := range f.SupportingSeats {
				seats = append(seats, map[string]any{
					"seatId": s.SeatID, "adapter": s.Adapter, "model": s.Model, "identityTier": s.IdentityTier,
				})
			}
			row["supportingSeats"] = seats
		}
		if f.AgreementCount > 0 {
			row["agreementCount"] = f.AgreementCount
		}
		if len(f.DissentingSeats) > 0 {
			row["dissentingSeats"] = f.DissentingSeats
		}
		// Qualify the agreement count with how independent the seats were and how many stayed silent.
		if f.DistinctModels > 0 {
			row["distinctModels"] = f.DistinctModels
		}
		if f.AgreementIndependence != "" {
			row["agreementIndependence"] = f.AgreementIndependence
		}
		if f.Consensus != "" {
			row["consensus"] = f.Consensus
		}
		if f.Disposition == string(review.StateReportedValid) && f.ApplyRefusalReason == "" &&
			(f.Applyable == nil || *f.Applyable) {
			accepted++
		}
		out = append(out, row)
	}
	return out, accepted, quarantined
}

// verificationResultMaps projects one verification pass explicitly, so the wire shape matches the
// schema and optional fields are absent rather than zero.
func verificationResultMaps(rs []review.VerificationResult) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		m := map[string]any{
			"command": r.Command, "ok": r.OK, "exitCode": r.ExitCode, "durationMs": r.DurationMs,
		}
		if r.TimedOut {
			m["timedOut"] = true
		}
		if r.Unstartable != "" {
			m["unstartable"] = r.Unstartable
		}
		// Include the output tail so a caller can act on a failure; it is bounded at the source.
		if r.Output != "" {
			m["output"] = r.Output
		}
		out = append(out, m)
	}
	return out
}

func identityCaveatMaps(view runview.View) []map[string]any {
	out := make([]map[string]any, 0, len(view.IdentityCaveats))
	for _, c := range view.IdentityCaveats {
		row := map[string]any{"role": c.Role, "status": c.Status}
		if c.Adapter != "" {
			row["adapter"] = c.Adapter
		}
		if c.RequestedModel != "" {
			row["requestedModel"] = c.RequestedModel
		}
		if c.ReportedModel != "" {
			row["reportedModel"] = c.ReportedModel
		}
		if c.Evidence != "" {
			row["evidence"] = c.Evidence
		}
		out = append(out, row)
	}
	return out
}

func withheldMaps(view runview.View) []map[string]any {
	out := make([]map[string]any, 0, len(view.Withheld))
	for _, w := range view.Withheld {
		row := map[string]any{"path": w.Path, "reason": w.Reason}
		if w.Rule != "" {
			row["rule"] = w.Rule
		}
		if w.Detail != "" {
			row["detail"] = w.Detail
		}
		if w.Stage != "" {
			row["stage"] = w.Stage
		}
		out = append(out, row)
	}
	return out
}

func authorityMaps(view runview.View) []map[string]any {
	out := make([]map[string]any, 0, len(view.Authority))
	for _, a := range view.Authority {
		row := map[string]any{
			"name": a.Name, "source": a.Source, "fullHash": a.FullHash,
			"embeddedHash": a.EmbeddedHash, "bytesEmbedded": a.BytesEmbedded,
			"bytesTotal": a.BytesTotal, "complete": a.Complete,
		}
		if a.MediaType != "" {
			row["mediaType"] = a.MediaType
		}
		if len(a.Ranges) > 0 {
			ranges := make([]map[string]any, 0, len(a.Ranges))
			for _, r := range a.Ranges {
				ranges = append(ranges, map[string]any{"start": r.Start, "end": r.End})
			}
			row["ranges"] = ranges
		}
		out = append(out, row)
	}
	return out
}

// completeResult builds the terminal payload of a finished review. runId is the server's opaque id;
// the run directory is omitted.
func completeResult(rec *record, pick panelPick, view runview.View, inline bool, remediable bool) (map[string]any, string, int) {
	findings, accepted, quarantined := findingMaps(view)
	counts := map[string]any{
		"findings":            view.Counts.Findings,
		"bySeverity":          view.Counts.BySeverity,
		"byDisposition":       view.Counts.ByDisposition,
		"identityCaveats":     view.Counts.IdentityCaveats,
		"withheld":            view.Counts.Withheld,
		"panelSeats":          view.Counts.PanelSeats,
		"panelSeatsCompleted": view.Counts.PanelSeatsCompleted,
		"accepted":            accepted,
		"quarantined":         quarantined,
	}
	source := "path"
	if inline {
		source = "inline"
	}
	structured := map[string]any{
		"runId":           rec.ID,
		"state":           StateComplete,
		"tool":            rec.Tool,
		"mode":            view.Mode,
		"status":          view.Status,
		"workspaceSource": source,
		"remediable":      remediable,
		"findings":        findings,
		"counts":          counts,
		"identityCaveats": identityCaveatMaps(view),
		"authority":       authorityMaps(view),
		"withheld":        withheldMaps(view),
		"panel":           pick.echo(view.Panel),
	}
	if view.RequestedMode != "" {
		structured["requestedMode"] = view.RequestedMode
	}
	// The citation-grounding tally, with its note on what grounding does not establish. root is omitted:
	// no host paths on the wire.
	if g := view.Grounding; g != nil {
		structured["grounding"] = map[string]any{
			"checked":    g.Checked,
			"grounded":   g.Grounded,
			"unresolved": g.Unresolved,
			"noCitation": g.NoCitation,
			"byStatus":   g.ByStatus,
			"note":       g.Note,
		}
	}
	// Present only when a capacity failure cost the run a seat, so agreement counts are read against the
	// panel that answered.
	if p := view.PartialPanel; p != nil {
		structured["partialPanel"] = p
	}
	// Present when the review was narrowed, so a clean result is not read as covering the whole tree.
	if s := view.Scope; s != nil {
		structured["scope"] = s
	}
	// Panel composition rides every blind-panel result, since shared models weaken agreement counts.
	if c := view.Composition; c != nil {
		structured["composition"] = map[string]any{
			"seats":          c.Seats,
			"distinctModels": c.DistinctModels,
			"independence":   c.Independence,
			"note":           c.Note,
		}
	}
	// Dissent rides with composition. Its note explains that a silent seat did not disagree.
	if d := view.Dissent; d != nil {
		structured["dissent"] = map[string]any{
			"panelled":  d.Panelled,
			"unanimous": d.Unanimous,
			"majority":  d.Majority,
			"contested": d.Contested,
			"note":      d.Note,
		}
	}
	// A dry run's shape; without it a planned status and empty findings would look like a review that
	// found nothing. Paths are workspace-relative.
	if sh := view.Shape; sh != nil {
		structured["shape"] = sh
	}
	if v := view.Verification; v != nil {
		structured["verification"] = map[string]any{
			"commands": v.Commands,
			"before":   verificationResultMaps(v.Before),
			"after":    verificationResultMaps(v.After),
			"delta":    v.Delta,
			"note":     v.Note,
		}
	}
	return structured, renderComplete(structured, view, accepted, quarantined), accepted
}

// haltResult builds the terminal payload for a halted run. It is an isError result on a successful
// JSON-RPC response, since clients often drop a JSON-RPC error's data.
func haltResult(rec *record, pick panelPick, view runview.View, err error, cancelled bool) (map[string]any, string) {
	state := StateHalted
	if cancelled {
		state = StateCancelled
	}
	structured := map[string]any{
		"runId":           rec.ID,
		"state":           state,
		"tool":            rec.Tool,
		"mode":            view.Mode,
		"exitCode":        int(fault.CodeOf(err)),
		"haltClass":       haltClassOf(err, view),
		"reasonCode":      reasonOf(err),
		"panel":           pick.echo(view.Panel),
		"identityCaveats": identityCaveatMaps(view),
	}
	if cancelled {
		structured["haltClass"], structured["reasonCode"] = "cancelled", "run_cancelled"
	}
	if h := view.HaltRecord; h != nil && h.Failure != nil {
		structured["failure"] = map[string]any{
			"role": h.Failure.Role, "adapter": h.Failure.Adapter, "model": h.Failure.Model,
			"exitCode": h.Failure.ExitCode, "haltClass": h.Failure.HaltClass,
			"reasonCode": h.Failure.ReasonCode, "signal": h.Failure.Signal,
		}
	}
	text := fmt.Sprintf("Review %s (run %s): %s [haltClass=%v reasonCode=%v exitCode=%v]",
		state, rec.ID, capText(err.Error()), structured["haltClass"], structured["reasonCode"], structured["exitCode"])
	return structured, text
}

// refusalResult builds the payload for a call refused before any spend, such as a path outside the
// call's scope, a stale decision set or a missing write confirmation. It has the same taxonomy shape
// as a halt.
func refusalResult(runID string, err error) (map[string]any, string) {
	return map[string]any{
		"runId":      runID,
		"state":      StateHalted,
		"exitCode":   int(fault.CodeOf(err)),
		"haltClass":  haltClassOf(err, runview.View{}),
		"reasonCode": reasonOf(err),
	}, capText(err.Error())
}

// runningResult returns the payload for a run that outlived its inline wait.
func runningResult(rec *record, pick panelPick, waited int) (map[string]any, string) {
	structured := map[string]any{
		"runId": rec.ID, "state": StateRunning, "tool": rec.Tool,
		"mode": rec.Mode, "waitedSeconds": waited,
	}
	if rec.Tool == toolReport {
		structured["panel"] = pick.echo(nil)
	}
	text := fmt.Sprintf("Run %s (%s) is still running after %ds. Poll review_run_status with this runId, then fetch review_run_result. Do NOT start another run for the same request — re-issuing with the same idempotencyKey returns this run.",
		rec.ID, rec.Tool, waited)
	return structured, text
}

// remediateResult builds the receipt payload. The receipt is the same object persisted to the run
// record.
func remediateResult(rec *record, out run.RemediateOutcome, err error, patch *proto.Resource) (map[string]any, string, bool) {
	receipt := receiptMap(out.Receipt, patch)
	structured := map[string]any{
		"runId": rec.ID, "tool": rec.Tool, "mode": string(out.Mode), "receipt": receipt,
		// Every remediation result, halted ones included, carries the outcome and counts. isError is set
		// independently from refused.
		"outcome": out.Outcome(),
		"counts":  map[string]any{"applied": out.Applied(), "refused": out.Refused()},
	}
	if n := out.Refused(); n > 0 {
		structured["refusals"] = refusalMaps(out.Refusals)
	}
	// The selection rides every path, including a halt, so a caller can see which selectors matched
	// nothing.
	if out.Selection.Selective() {
		structured["selection"] = selectionMap(out.Selection)
	}
	if out.SourceRunID != "" {
		structured["sourceRunId"] = out.SourceRunID
	}
	if len(out.Withheld) > 0 {
		rows := make([]map[string]any, 0, len(out.Withheld))
		for _, w := range out.Withheld {
			rows = append(rows, map[string]any{
				"path": w.Path, "reason": w.Reason, "rule": w.Rule, "detail": w.Detail, "stage": w.Stage,
			})
		}
		structured["withheld"] = rows
	}
	if err != nil {
		state := StateHalted
		if out.Cancelled {
			state = StateCancelled
		}
		structured["state"] = state
		structured["exitCode"] = int(fault.CodeOf(err))
		structured["haltClass"] = haltClassOf(err, runview.View{})
		structured["reasonCode"] = reasonOf(err)
		return structured, renderReceipt(out, err, patch), true
	}
	structured["state"] = StateComplete
	// A partial refusal is a completed write that must not read as clean. state stays "complete" and
	// isError signals it; a caller that checks finds the applied count.
	if out.Refused() > 0 {
		structured["reasonCode"] = run.ReasonApplyRefusedProtectedPath
		return structured, renderReceipt(out, nil, patch), true
	}
	return structured, renderReceipt(out, nil, patch), false
}

// refusalMaps projects protected-path refusals, keyed by host-computed fingerprint rather than the
// model-authored finding id.
func refusalMaps(rs []review.ApplyRefusal) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		out = append(out, map[string]any{
			"fingerprint": r.Fingerprint, "file": r.File, "reason": r.Reason,
		})
	}
	return out
}

// selectionMap projects a selection. All three lists are always present, empty included.
func selectionMap(s *review.ApplySelection) map[string]any {
	return map[string]any{
		"requested": append([]string{}, s.Requested...),
		"matched":   append([]string{}, s.Matched...),
		"unmatched": append([]string{}, s.Unmatched...),
	}
}

func receiptMap(r run.Receipt, patch *proto.Resource) map[string]any {
	m := map[string]any{
		"runId": r.RunID, "mode": r.Mode, "status": r.Status,
		"baseHashesVerified": r.BaseHashesVerified,
		"intended":           hunkMaps(r.Intended),
		"applied":            appliedMaps(r.Applied),
		"notApplied":         appliedMaps(r.NotApplied),
		"files":              append([]string{}, r.Files...),
		// commitAttempted and committed differ: committed false can mean nothing was tried or a rollback
		// happened.
		"commitAttempted": r.CommitAttempted,
		"committed":       r.Committed,
	}
	if r.IntentSHA256 != "" {
		m["intentSha256"] = r.IntentSHA256
	}
	if r.SourceRunID != "" {
		m["sourceRunId"] = r.SourceRunID
	}
	if r.ReasonCode != "" {
		m["reasonCode"] = r.ReasonCode
	}
	// A link and a hash, never the content, since a truncated patch would read as complete. patchResource
	// makes the link fetchable.
	if r.PatchArtifact != "" {
		m["patchArtifact"], m["patchSha256"] = r.PatchArtifact, r.PatchSHA256
		if patch != nil && patch.URI != "" {
			m["patchResource"] = patch.URI
		}
	}
	return m
}

func hunkMaps(hs []run.IntendedHunk) []map[string]any {
	out := make([]map[string]any, 0, len(hs))
	for _, h := range hs {
		row := map[string]any{"findingId": h.FindingID, "file": h.File, "bytes": h.Bytes, "origin": h.Origin}
		// The replacement's digest, never its text.
		if h.ReplacementSHA256 != "" {
			row["replacementSha256"] = h.ReplacementSHA256
		}
		if h.Anchor != "" {
			row["anchor"] = capExcerpt(h.Anchor)
		}
		if h.Occurrence != 0 {
			row["occurrence"] = h.Occurrence
		}
		out = append(out, row)
	}
	return out
}

func appliedMaps(as []run.AppliedFinding) []map[string]any {
	out := make([]map[string]any, 0, len(as))
	for _, a := range as {
		row := map[string]any{"findingId": a.FindingID, "state": a.State}
		if a.File != "" {
			row["file"] = a.File
		}
		if a.Reason != "" {
			row["reason"] = a.Reason
		}
		out = append(out, row)
	}
	return out
}

// haltClassOf resolves the taxonomy class for a failure: the run's halt class, then the fault's, then
// a stable name for the fault code. It never returns "".
func haltClassOf(err error, view runview.View) string {
	if view.HaltRecord != nil && view.HaltRecord.HaltClass != "" {
		return view.HaltRecord.HaltClass
	}
	var f *fault.Fault
	if errors.As(err, &f) && f.Halt != "" {
		return f.Halt
	}
	switch fault.CodeOf(err) {
	case fault.Usage:
		return "usage"
	case fault.Config:
		return "config"
	case fault.Adapter:
		return "adapter"
	case fault.Model:
		return "identity"
	case fault.Containment:
		return "containment"
	case fault.Policy:
		return "policy"
	default:
		return "internal"
	}
}

// reasonOf returns the machine reason code for err, never "".
func reasonOf(err error) string {
	if r := fault.ReasonOf(err); r != "" {
		return r
	}
	return "unclassified"
}

// --- human renderings ---

// maxFailureText bounds a message that reaches a third-party inference log.
const maxFailureText = 2000

// maxExcerpt bounds a journal anchor echoed to the caller, since it is workspace content.
const maxExcerpt = 160

func capText(s string) string {
	if len(s) <= maxFailureText {
		return s
	}
	return s[:maxFailureText] + fmt.Sprintf("… [truncated, %d bytes total]", len(s))
}

func capExcerpt(s string) string {
	if len(s) <= maxExcerpt {
		return s
	}
	return s[:maxExcerpt] + "…"
}

// renderComplete leads with the governance facts, since some clients show the model only text.
func renderComplete(structured map[string]any, view runview.View, accepted, quarantined int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review complete (mode %v, status %v, run %v). It wrote NOTHING to the workspace.\n",
		structured["mode"], structured["status"], structured["runId"])
	fmt.Fprintf(&b, "Panel: %d seat(s) requested, %d completed. Findings: %d (%d accepted, %d quarantined as never-applyable).\n",
		view.Counts.PanelSeats, view.Counts.PanelSeatsCompleted, view.Counts.Findings, accepted, quarantined)
	b.WriteString("Agreement counts are computed by the HOST over the blind seats — report them as given.\n")
	if n := len(view.IdentityCaveats); n == 0 {
		b.WriteString("Identity: every lane verified.\n")
	} else {
		parts := make([]string, 0, n)
		for _, c := range view.IdentityCaveats {
			parts = append(parts, fmt.Sprintf("%s (%s/%s: %s)", c.Role, c.Adapter, c.RequestedModel, c.Status))
		}
		fmt.Fprintf(&b, "Identity caveats: %d lane(s) NOT strongly verified — %s. A weak-identity lane is not a peer of a verified one.\n",
			n, strings.Join(parts, "; "))
	}
	if n := len(view.Authority); n > 0 {
		incomplete := 0
		for _, a := range view.Authority {
			if !a.Complete {
				incomplete++
			}
		}
		fmt.Fprintf(&b, "Judged against %d authority document(s); %d embedded incompletely.\n", n, incomplete)
	} else {
		b.WriteString("No authority documents were declared: this artifact was judged on its own terms.\n")
	}
	if n := len(view.Withheld); n > 0 {
		fmt.Fprintf(&b, "WITHHELD: %d file(s) were never shown to the reviewers — their absence from the findings means nothing.\n", n)
	}
	if remediable, _ := structured["remediable"].(bool); remediable && accepted > 0 {
		fmt.Fprintf(&b, "To apply the %d accepted finding(s), call review_remediate with fromRun=%v, output=patch|apply and allowWrite=true.\n",
			accepted, structured["runId"])
	}
	b.WriteString("The full machine-readable result is in structuredContent.")
	return b.String()
}

func renderReceipt(out run.RemediateOutcome, err error, patch *proto.Resource) string {
	r := out.Receipt
	var b strings.Builder
	// An unmatched selector is listed first too: the write was smaller than requested.
	if out.Selection.Selective() && len(out.Selection.Unmatched) > 0 {
		fmt.Fprintf(&b, "SELECTOR MATCHED NOTHING: %d of %d fingerprint(s) in `select` name no finding in run %s's accepted set (%s). They wrote nothing and nothing was fetched for them. Check them against that run's accepted findings — the selector is the `fingerprint` field, never a finding's `id`.\n",
			len(out.Selection.Unmatched), len(out.Selection.Requested), out.SourceRunID,
			strings.Join(out.Selection.Unmatched, ", "))
	}
	// The refusal is listed before the applied summary.
	if n := out.Refused(); n > 0 {
		files := make([]string, 0, n)
		for _, ref := range out.Refusals {
			files = append(files, ref.File)
		}
		fmt.Fprintf(&b, "REFUSED: %d finding(s) target a PROTECTED PATH and were NOT applied (%s) — the denylist is not overridable, so nothing was written to them. Every other accepted finding was applied normally. Do NOT re-run this remediation: the same finding would be refused identically. Fix those paths by hand if they need changing.\n",
			n, strings.Join(files, ", "))
	}
	switch {
	case err != nil && out.Cancelled:
		fmt.Fprintf(&b, "Remediation CANCELLED (run %s). Nothing was committed to the live workspace.\n", r.RunID)
	case err != nil:
		fmt.Fprintf(&b, "Remediation halted (run %s): %s\n", r.RunID, capText(err.Error()))
	case r.Committed:
		fmt.Fprintf(&b, "Remediation applied (run %s): %d finding(s) written to %d file(s) in the live workspace.\n",
			r.RunID, len(r.Applied), len(r.Files))
	default:
		fmt.Fprintf(&b, "Remediation produced a patch (run %s): %d finding(s) across %d file(s). NOTHING was written to the workspace.\n",
			r.RunID, len(r.Applied), len(r.Files))
	}
	if out.Selection.Selective() {
		fmt.Fprintf(&b, "Selective apply: %d fingerprint(s) requested, %d matched. Only the matched findings were considered for writing.\n",
			len(out.Selection.Requested), len(out.Selection.Matched))
	}
	fmt.Fprintf(&b, "Journal: %d intended hunk(s), recorded before any write. Base hashes re-verified: %d.\n",
		len(r.Intended), r.BaseHashesVerified)
	if len(r.NotApplied) > 0 {
		fmt.Fprintf(&b, "%d finding(s) were NOT applied; see receipt.notApplied for each reason.\n", len(r.NotApplied))
	}
	if r.PatchArtifact != "" {
		if patch != nil && patch.URI != "" {
			// Explain how to collect the patch, since some clients show the model only text.
			fmt.Fprintf(&b, "Complete patch: fetch it with resources/read on %s, then verify it against %s. It is linked, never inlined — a truncated patch that still parses reads as complete.\n",
				patch.URI, r.PatchSHA256)
		} else {
			fmt.Fprintf(&b, "Complete patch: %s in run record %s (%s). It is referenced, never inlined. This server could not publish it as a resource, so it is reachable only on the machine that owns the run record.\n",
				r.PatchArtifact, r.RunID, r.PatchSHA256)
		}
	}
	b.WriteString("The receipt is also persisted to the run record, so this answer survives a lost response.")
	return b.String()
}
