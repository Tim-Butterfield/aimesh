package acp

// FROM-RUN EXECUTION — the ACP write turn that applies the set the host inspected.
//
// WHAT THIS CLOSES. The two-turn rule (§9.4, D5) made a write turn CARRY the run handle its report
// turn returned, so a write whose own response is lost stays discoverable. But the handle was
// required, recorded, and not resolved: turn 2 ran its own governed cycle and applied its OWN
// adjudication. A `select` taken from turn 1 therefore matched only if turn 2's panel happened to
// raise the same finding again, and landed in `unmatched` otherwise. The same named capability —
// `fromRun` — meant "apply the decision set you inspected" on MCP and "re-adjudicate and hope" here,
// which the surface-parity invariant does not permit.
//
// So a write turn carrying `fromRun` now goes through `Manager.Remediate`: the SAME governed write
// path, driven by the SAME already-adjudicated decision set the source run recorded. No reviewers
// run, no second adjudication happens, and `select` narrows the stored accepted set.
//
// WHERE THE SET COMES FROM, and why it is not a registry. MCP holds its decision sets in memory and
// says so — they die with the process. ACP reads the set from the source run's own DIRECTORY
// (`run.ReadDecisionSet`), which survives a restart and is the artifact a host was already handed
// a handle to. That also means this surface needs no job shape: no `runId` vocabulary, no
// `run_status`, no `run_result`. The handle is still the `runDir` a report turn returned.
//
// WHERE THE HANDLE IS JUDGED. It arrives from a peer, so it is verified against this agent's own
// artifact directory before anything is read — lexically first, so a path outside that directory is
// refused without the filesystem being consulted, and a peer gains no existence oracle. Every way of
// failing answers with one `run_handle_unknown`.
//
// WHAT STILL GATES THE WRITE — unchanged, and deliberately not re-implemented here: the reviewed
// root's identity binding (device+inode plus canonical path, carried durably on the set), the
// source run's base-hash pins verified before the window opens, `governedWrite`'s per-destination
// content pins verified inside the commit, the trusted roots in force NOW, the protected-path
// denylist, the journal, the cancel/commit gate and the receipt. A stored set applied against a
// tree that has moved on HALTS with `stale_decision_set`; it does not write.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// Stable MACHINE reason codes this path owns (lower_snake, never sentences).
const (
	// reasonFromRunArgsRefused — a run-forming argument (`profile`, `panel`, `authority`) rode a
	// `fromRun` turn. Those are the SOURCE run's and are not re-decided here, so the parameter is
	// refused by name rather than ignored.
	reasonFromRunArgsRefused = "from_run_args_refused"
	// reasonFromRunWorkspaceMismatch — this turn NAMED a workspace that is not the tree the source
	// run judged. The set is written to the tree it was adjudicated against; a caller that named a
	// different one is told so rather than having its request silently redirected.
	reasonFromRunWorkspaceMismatch = "from_run_workspace_mismatch"
)

// fromRunWrite applies a prior run's decision set. It is reached only from runAndRespond, after the
// mode ceiling, the run-handle rule and the authority DECLARATION check have all passed.
func (s *Server) fromRunWrite(ctx context.Context, req rpcRequest, notif bool, workspace string,
	ownedWorkspace bool, effective, requested review.Mode, degradeReason, sessionID string,
	sel reviewmeshMeta, wt writeTurn, f Framer) *rpcResponse {

	refuse := func(msg string, reason string, extra map[string]any) *rpcResponse {
		if notif {
			return nil
		}
		data := map[string]any{"reasonCode": reason}
		for k, v := range extra {
			data[k] = v
		}
		return errResp(req.ID, codeInvalidParams, msg, data)
	}

	// RUN-FORMING ARGUMENTS ARE REFUSED, NOT IGNORED. Each names a governance input to the
	// adjudication: which models judged, which panel composed the accepted set, which intent it was
	// judged against. On this branch all three already exist — they came from the source run — so a
	// turn that supplies different ones is asking for something this call cannot do, and accepting
	// it would tell the host the opposite. (A MALFORMED authority declaration is refused earlier,
	// by authority.Validate, with its own more specific code; both are refusals.)
	var named []string
	if strings.TrimSpace(sel.Profile) != "" {
		named = append(named, "`profile`")
	}
	if len(sel.Panel) > 0 {
		named = append(named, "`panel`")
	}
	if len(sel.Authority) > 0 {
		named = append(named, "`authority`")
	}
	if len(named) > 0 {
		return refuse("invalid params: "+strings.Join(named, " and ")+
			" cannot ride a `fromRun` turn — that turn APPLIES the accepted set an earlier `report` turn already produced, so the panel, the profile and the authority manifest are the source run's and are not re-decided here. "+
			"Nothing would be re-reviewed or re-judged, so the parameter is refused rather than ignored. Drop it, or run a fresh `report` turn with the panel and authority you want and apply THAT run.",
			reasonFromRunArgsRefused, nil)
	}
	// An inline workspace is content this process materialized for THIS turn and deletes when the
	// turn ends. There is no tree for a stored decision set to be applied to.
	if ownedWorkspace {
		return refuse("invalid params: `fromRun` cannot be combined with `inlineWorkspace` — the inline content is materialized into a directory this agent owns and deletes when the turn ends, so a decision set from an earlier run has nothing real to be written to.",
			run.ReasonInlineWorkspaceNotRemediable, nil)
	}

	// THE HANDLE, verified rather than trusted. See the file comment.
	set, sourceDir, herr := s.Manager.ReadDecisionSet(wt.FromRun)
	if herr != nil {
		return refuse("invalid params: "+herr.Error(), fault.ReasonOf(herr), nil)
	}
	if rerr := set.Remediable(); rerr != nil {
		return refuse("invalid params: "+rerr.Error(), fault.ReasonOf(rerr),
			map[string]any{"sourceRunDir": sourceDir})
	}
	// A NAMED workspace is an assertion about which tree is being changed, and it must be the tree
	// the source run judged. An IMPLICIT one (the session cwd, the process working directory)
	// asserts nothing, so the source run's workspace simply stands — see writeTurn.WorkspaceNamed.
	if wt.WorkspaceNamed && !set.NamesWorkspace(workspace) {
		return refuse(fmt.Sprintf(
			"invalid params: this turn names workspace %q, but run %q adjudicated %q. A `fromRun` write is applied to the tree its decisions were made against, and a turn that named a different one is refused rather than silently redirected. Omit `workspace` to apply the source run's, or apply a run that reviewed the tree you mean.",
			workspace, set.RunID, set.Workspace),
			reasonFromRunWorkspaceMismatch, map[string]any{"sourceRunDir": sourceDir})
	}
	// THE IDENTITY BINDING, re-established across the report→apply gap: the canonical path must
	// still resolve to what it resolved to, and the durable device+inode key must still match. This
	// is the check a pathname cannot make, and it is pre-spend — nothing has been copied and no
	// model has been called, so a swapped tree costs a round trip rather than a run.
	identity, ierr := set.BindWorkspace()
	if ierr != nil {
		return refuse("invalid params: "+ierr.Error(), fault.ReasonOf(ierr),
			map[string]any{"sourceRunDir": sourceDir})
	}

	rq := run.RemediateRequest{
		// The CANONICAL workspace, because BindWorkspace has just proven that is what the source
		// run judged. `set.Workspace` is the path the run recorded and is kept for the audit trail;
		// resolving the write against the canonical form removes a whole class of "same tree,
		// different spelling" ambiguity from the one call that writes.
		Workspace:         set.WorkspaceCanonical,
		WorkspaceIdentity: identity,
		Mode:              effective,
		Surface:           "acp",
		// The SOURCE run's profile and panel, so the plan resolves the same author_remediator lane
		// the report run used. No reviewer seat is executed from them.
		Profile:       set.Profile,
		ReviewerPanel: set.Panel,
		// The trusted roots IN FORCE NOW. A decision set is durable and can outlive the roots the
		// run that produced it was launched with, so the write is re-gated against this
		// connection's — enforced inside governedWrite, not merely carried.
		TrustedRoots: s.trusted().Roots(),
		SourceRunID:  set.RunID,
		Findings:     set.Findings,
		Decisions:    set.Decisions,
		// The narrowing filter travels UNEXAMINED into the one governed write path — this surface
		// resolves no fingerprint. That is what keeps `select` one rule rather than three.
		Select:     wt.Select,
		Shown:      set.ShownSet(),
		BaseHashes: set.BaseHashes,
		// The OPERATOR's launch waivers. A from-run apply reaches the same governed write path as a
		// single-call apply, so it has to be governed by the same operator grants — otherwise the
		// launch flag would mean one thing for `session/prompt` and another for a from-run turn.
		AllowProtectedPaths: s.AllowProtectedPaths,
	}
	if sessionID != "" {
		// The same in-process progress sink the report path uses, gated on the run context so
		// nothing is emitted after cancellation or after the terminal response.
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
	// The turn budget wraps the request context (which a host `cancel` already cancels), so a
	// remediation that outlives it is cancelled exactly as a host cancellation would cancel it —
	// same path, same no-write-after-cancel gate.
	runCtx, runCancel := context.WithTimeout(ctx, s.turnBudget())
	defer runCancel()
	out, rerr := s.Manager.Remediate(runCtx, rq)
	if notif {
		return nil // notification: ran for side effects, no response
	}
	return s.fromRunResponse(req, out, set, sourceDir, effective, requested, degradeReason, sessionID != "", rerr)
}

// fromRunResponse renders one from-run write turn.
//
// CANCELLATION IS CHECKED FIRST, and that ordering is the contract rather than a preference: a
// cancelled remediation returns a fault AND a receipt, and this surface answers a cancelled prompt
// with a normal `PromptResponse` carrying the run handle — so the host can read the durable receipt
// that says nothing was committed. Turning it into a `-32000` would tell a caller the turn failed
// while the run directory says what actually happened.
func (s *Server) fromRunResponse(req rpcRequest, out run.RemediateOutcome, set *run.StoredDecisionSet,
	sourceDir string, effective, requested review.Mode, degradeReason string, isSession bool, rerr error) *rpcResponse {

	if out.Cancelled || errors.Is(rerr, context.Canceled) {
		rm := map[string]any{
			"status": "cancelled", "mode": string(effective),
			"runDir": out.RunDir, "sourceRunDir": sourceDir, "fromRun": true,
		}
		if isSession {
			return okResp(req.ID, promptResponse("cancelled", rm))
		}
		return okResp(req.ID, rm)
	}
	if rerr != nil {
		// A refused or halted write is an ERROR response on both paths, carrying the halt taxonomy
		// as MACHINE values — the same shape a halted review turn uses, so a host branches on one
		// thing. `runDir` is the REMEDIATION run: its receipt is what says whether anything was
		// committed (for a stale set, nothing was).
		// The halt class comes off the fault itself. A remediation has no RunOutcome to carry one,
		// and a class invented at the surface would be a second derivation of a fact the manager
		// already decided.
		halt := ""
		var ft *fault.Fault
		if errors.As(rerr, &ft) {
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

	rm := map[string]any{
		"status": "stable",
		"mode":   string(effective),
		// The REMEDIATION run's directory: this turn's journal, commit-attempt marker and receipt
		// are under it. `sourceRunDir` is the run whose decisions were applied — two runs, two
		// handles, neither standing in for the other.
		//
		// `sourceRunDir` is the CANONICAL directory, not the handle as the host spelled it: it names
		// the run this write actually read. A host that wants to compare it with the handle it sent
		// should compare resolved paths, because a symlinked temp or home directory makes the two
		// spellings differ while naming one run.
		"runDir":       out.RunDir,
		"sourceRunDir": sourceDir,
		"fromRun":      true,
		// The findings count is the SOURCE run's set, not a fresh count: this turn raised none.
		"findings":      len(set.Findings),
		"patchArtifact": out.RunDir + "/patches/changes.patch",
		"outcome":       out.Outcome(),
		"applied":       out.Applied(),
		"refused":       out.Refused(),
	}
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
		// Partial refusal answers `refusal`, exactly as it does on a full-cycle write turn: the
		// coarse signal over-states and `applied` sitting beside it is what keeps the fail-safe
		// reading from becoming a wrong one.
		if len(out.Refusals) > 0 {
			return okResp(req.ID, promptResponse("refusal", rm))
		}
		return okResp(req.ID, promptResponse("end_turn", rm))
	}
	return okResp(req.ID, rm)
}

// selectionPayload projects a selection into the three lists every write turn reports. All three are
// always present — an absent `unmatched` and an empty one are different facts.
func selectionPayload(s *review.ApplySelection) map[string]any {
	return map[string]any{
		"requested": append([]string{}, s.Requested...),
		"matched":   append([]string{}, s.Matched...),
		"unmatched": append([]string{}, s.Unmatched...),
	}
}
