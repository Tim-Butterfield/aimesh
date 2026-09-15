package acp

// This file implements the ACP write turn that applies a decision set the host inspected.
//
// A write turn carries the runDir its report turn returned and applies that run's recorded
// decision set through Manager.Remediate, the governed write path shared with MCP and the CLI. No
// reviewers run, and select narrows the stored accepted set.
//
// The handle comes from a peer, so it is verified against the turn workspace's run-record
// locations before anything is read, lexically first; every failure answers run_handle_unknown.
// The write itself is gated by the governed write path: the workspace identity binding, the
// source run's base-hash pins, per-destination content pins, the turn's scope, the protected-path
// denylist, the journal and the cancel gate. A tree that has changed halts with
// stale_decision_set.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Reason codes for a refused fromRun turn.
const (
	// reasonFromRunArgsRefused refuses a run-forming argument on a fromRun turn; those inputs belong
	// to the source run.
	reasonFromRunArgsRefused = "from_run_args_refused"
	// reasonFromRunWorkspaceMismatch refuses a turn whose workspace (named, or the session cwd) is not
	// the tree the source run reviewed.
	reasonFromRunWorkspaceMismatch = "from_run_workspace_mismatch"
)

// fromRunWrite applies a prior run's decision set. runAndRespond calls it after the mode ceiling,
// the run-handle rule and the authority declaration check have passed.
func (s *Server) fromRunWrite(ctx context.Context, req rpcRequest, notif bool, workspace string,
	ownedWorkspace bool, effective, requested review.Mode, degradeReason, sessionID string,
	sel reviewmeshMeta, wt writeTurn, turn *scope.Resolver, f Framer) *rpcResponse {

	refuse := func(msg string, reason string, extra map[string]any) *rpcResponse {
		if notif {
			return nil
		}
		data := map[string]any{"reasonCode": reason}
		maps.Copy(data, extra)
		return errResp(req.ID, codeInvalidParams, msg, data)
	}

	// Run-forming arguments are refused by name rather than ignored: the panel and authority were
	// settled by the source run. A malformed authority declaration was already refused by
	// authority.Validate.
	var named []string
	if sel.Panel != nil {
		named = append(named, "`panel`")
	}
	if len(sel.Authority) > 0 {
		named = append(named, "`authority`")
	}
	// A dry run or readiness probe on a write turn would suggest nothing is written.
	if sel.DryRun {
		named = append(named, "`dryRun`")
	}
	if sel.VerifyReadiness {
		named = append(named, "`verifyReadiness`")
	}
	if len(named) > 0 {
		return refuse("invalid params: "+strings.Join(named, " and ")+
			" cannot ride a `fromRun` turn — that turn APPLIES the accepted set an earlier `report` turn already produced, so the panel and the authority manifest are the source run's and are not re-decided here. "+
			"Nothing would be re-reviewed or re-judged, so the parameter is refused rather than ignored. Drop it, or run a fresh `report` turn with the panel and authority you want and apply THAT run.",
			reasonFromRunArgsRefused, nil)
	}
	// An inline workspace is deleted when its turn ends, so there is no tree to apply a stored set to.
	if ownedWorkspace {
		return refuse("invalid params: `fromRun` cannot be combined with `inlineWorkspace` — the inline content is materialized into a directory this agent owns and deletes when the turn ends, so a decision set from an earlier run has nothing real to be written to.",
			run.ReasonInlineWorkspaceNotRemediable, nil)
	}

	// The handle must be absolute, since the agent shares no working directory with its host, and is
	// resolved only among the declared workspace's run-record locations.
	if !filepath.IsAbs(strings.TrimSpace(wt.FromRun)) {
		return refuse("invalid params: `fromRun` must be the absolute `runDir` an earlier `report` turn returned",
			run.ReasonRunHandleUnknown, nil)
	}
	set, sourceDir, herr := s.Manager.ReadDecisionSetFor(workspace, wt.FromRun)
	if herr != nil {
		return refuse("invalid params: "+herr.Error(), fault.ReasonOf(herr), nil)
	}
	if rerr := set.Remediable(); rerr != nil {
		return refuse("invalid params: "+rerr.Error(), fault.ReasonOf(rerr),
			map[string]any{"sourceRunDir": sourceDir})
	}
	// The declared workspace must be the tree the source run reviewed; a different one is refused,
	// not redirected.
	if !set.NamesWorkspace(workspace) {
		return refuse(fmt.Sprintf(
			"invalid params: this turn's workspace is %q, but run %q adjudicated %q. A `fromRun` write is applied to the tree its decisions were made against; declare that workspace, or apply a run that reviewed the tree you mean.",
			workspace, set.RunID, set.Workspace),
			reasonFromRunWorkspaceMismatch, map[string]any{"sourceRunDir": sourceDir})
	}
	// Re-establish the identity binding before any spend: the canonical path and the device and inode
	// key must still match what the source run recorded.
	identity, ierr := set.BindWorkspace()
	if ierr != nil {
		return refuse("invalid params: "+ierr.Error(), fault.ReasonOf(ierr),
			map[string]any{"sourceRunDir": sourceDir})
	}

	rq := run.RemediateRequest{
		// Write against the canonical workspace, which BindWorkspace just verified.
		Workspace:         set.WorkspaceCanonical,
		WorkspaceIdentity: identity,
		Mode:              effective,
		Surface:           "acp",
		// The source run's panel and role seats, so the plan resolves the same author_remediator seat. No
		// reviewer runs.
		Profile:       set.Profile,
		ReviewerPanel: set.Panel,
		ComposedRoles: set.Roles,
		// Re-gate the write against this turn's declared scope, which governedWrite enforces.
		TrustedRoots: turn.Roots(),
		SourceRunID:  set.RunID,
		Findings:     set.Findings,
		Decisions:    set.Decisions,
		// The selection passes unexamined to the governed write path.
		Select:     wt.Select,
		Shown:      set.ShownSet(),
		BaseHashes: set.BaseHashes,
		// The operator's launch waivers apply here as they do to any governed write.
		AllowProtectedPaths: s.AllowProtectedPaths,
	}
	if sessionID != "" {
		// Forward progress while the run context is live, as the report path does.
		rq.OnEvent = func(ev audit.EventLine) {
			if ctx.Err() != nil {
				return
			}
			s.notify(f, "session/update", sessionUpdateParams{
				SessionID: sessionID,
				Update:    acpProgressUpdate(ev.EventType, ev.Level, ev.Message, ev.Timestamp),
			})
		}
	}
	// The turn budget wraps the request context, so a timeout cancels the remediation the same way a
	// host cancel does.
	runCtx, runCancel := context.WithTimeout(ctx, s.turnBudget())
	defer runCancel()
	out, rerr := s.Manager.Remediate(runCtx, rq)
	if notif {
		return nil // notification: ran for side effects, no response
	}
	return s.fromRunResponse(req, out, set, sourceDir, effective, requested, degradeReason, sessionID != "", rerr)
}

// fromRunResponse renders one fromRun write turn.
//
// Cancellation is checked first: a cancelled remediation returns a fault and a receipt, and the
// turn answers with a normal PromptResponse carrying the run handle so the host can read the
// receipt showing nothing was committed.
func (s *Server) fromRunResponse(req rpcRequest, out run.RemediateOutcome, set *run.StoredDecisionSet,
	sourceDir string, effective, requested review.Mode, degradeReason string, isSession bool, rerr error) *rpcResponse {

	if out.Cancelled || errors.Is(rerr, context.Canceled) {
		rm := s.withWrites(map[string]any{
			"status": "cancelled", "mode": string(effective),
			"runDir": out.RunDir, "sourceRunDir": sourceDir, "fromRun": true,
		})
		if isSession {
			return okResp(req.ID, promptResponse("cancelled", rm))
		}
		return okResp(req.ID, rm)
	}
	if rerr != nil {
		// A refused or halted write is an error response carrying the halt taxonomy as machine values, the
		// same shape as a halted review turn. runDir is the remediation run, whose receipt records whether
		// anything was committed. The halt class comes from the fault.
		halt := ""
		if ft, ok := errors.AsType[*fault.Fault](rerr); ok {
			halt = ft.Halt
		}
		data := map[string]any{
			"exitCode": int(fault.CodeOf(rerr)), "haltClass": halt,
			"runDir": out.RunDir, "sourceRunDir": sourceDir,
			"reasonCode": fault.ReasonOf(rerr), "fromRun": true,
		}
		if out.Selection.Selective() {
			data["selection"] = selectionPayload(out.Selection)
		}
		return errResp(req.ID, codeReviewHalt, "remediation halted: "+rerr.Error(), data)
	}

	rm := s.withWrites(map[string]any{
		"status": "stable",
		"mode":   string(effective),
		// runDir is the remediation run (journal, commit marker, receipt); sourceRunDir is the canonical
		// directory of the run whose decisions were applied. Compare resolved paths when matching it to the
		// handle sent, since symlinks can make spellings differ.
		"runDir":       out.RunDir,
		"sourceRunDir": sourceDir,
		"fromRun":      true,
		// The findings count is the source run's; this turn raised none.
		"findings":      len(set.Findings),
		"patchArtifact": out.RunDir + "/patches/changes.patch",
		"outcome":       out.Outcome(),
		"applied":       out.Applied(),
		"refused":       out.Refused(),
	})
	if out.Selection.Selective() {
		rm["selection"] = selectionPayload(out.Selection)
	}
	if len(out.Refusals) > 0 {
		refusals := make([]map[string]any, 0, len(out.Refusals))
		for _, r := range out.Refusals {
			refusals = append(refusals, map[string]any{
				"fingerprint": r.Fingerprint, "file": r.File, "reason": r.Reason,
			})
		}
		rm["refusals"] = refusals
	}
	if len(out.Withheld) > 0 {
		withheld := make([]map[string]any, 0, len(out.Withheld))
		for _, w := range out.Withheld {
			withheld = append(withheld, map[string]any{
				"path": w.Path, "reason": w.Reason, "rule": w.Rule, "detail": w.Detail, "stage": w.Stage,
			})
		}
		rm["withheld"] = withheld
	}
	if degradeReason != "" {
		rm["modeDegraded"] = true
		rm["requestedMode"] = string(requested)
		rm["modeReason"] = degradeReason
	}
	if isSession {
		// A partial refusal answers refusal, as on a full-cycle write turn; applied reports what was written.
		if len(out.Refusals) > 0 {
			return okResp(req.ID, promptResponse("refusal", rm))
		}
		return okResp(req.ID, promptResponse("end_turn", rm))
	}
	return okResp(req.ID, rm)
}

// selectionPayload projects a selection into the requested, matched and unmatched lists. All three
// are always present, since an absent unmatched list and an empty one mean different things.
func selectionPayload(s *review.ApplySelection) map[string]any {
	return map[string]any{
		"requested": append([]string{}, s.Requested...),
		"matched":   append([]string{}, s.Matched...),
		"unmatched": append([]string{}, s.Unmatched...),
	}
}
