package run

// governedWrite is the only code that turns adjudicated findings into bytes in a live workspace. The
// journal, content pins, cancel/commit gate, per-finding staging, receipt and edit-target
// authorization live here, so they hold on every surface: Manager.Remediate and handleMode's
// patch/apply branch both call it, and neither commits on its own.
//
// Callers differ only in inputs:
//
//   - Pins: a fromRun remediation carries base hashes from the report run and re-verifies them
//     against the live tree before copying; a full cycle records pins from the isolated copy its edits
//     derive from. Either way a destination whose content changed is never written.
//   - Trusted roots: a stored decision set is checked against the roots in force now; a full cycle is
//     not, because an agent surface may review a directory it materialized (see Request.TrustedRoots).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/remediation"
	"github.com/Tim-Butterfield/aimesh/internal/review/schema"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// defaultWriteArtifacts is the run-relative directory a write window records itself in, as named in
// docs/mcp.md and docs/security.md. The first write window of any run uses it.
const defaultWriteArtifacts = "remediation"

// Reason codes for a finding that produced no write, recorded on the receipt.
const (
	// reasonTargetNotShownOrExcluded: the file is empty, excluded, or was never shown to a reviewer.
	reasonTargetNotShownOrExcluded = "target_not_shown_or_excluded"
	// reasonTargetAbsentFromCopy: the file is not in this window's isolated copy, so there is no
	// base to edit.
	reasonTargetAbsentFromCopy = "target_absent_from_copy"
	// reasonNoEditProposed: neither the model nor the deterministic engine produced an edit.
	reasonNoEditProposed = "no_edit_proposed"
	// reasonEditFailed: at least one hunk did not apply, so the whole finding was rolled back.
	reasonEditFailed = "edit_failed"
)

// ReasonApplyRefusedProtectedPath is the run-level reason code for a remediation that completed with
// at least one protected-path refusal (exit 7). Each refused receipt row carries the per-finding code
// review.ApplyRefusalProtectedPath.
const ReasonApplyRefusedProtectedPath = "apply_refused_protected_path"

// Reason codes for a selective apply that names no writable finding. Both refuse before the write
// window, so nothing is copied or written.
const (
	// ReasonSelectionEmpty: `select` was supplied and is empty; an empty filter never means
	// "apply everything".
	ReasonSelectionEmpty = "apply_selection_empty"
	// ReasonSelectionMatchedNothing: no fingerprint in `select` names a finding in the accepted set.
	// A partial match proceeds and reports the unmatched entries.
	ReasonSelectionMatchedNothing = "apply_selection_matched_nothing"
)

// findingWrite is one accepted finding's write outcome, enough for the caller to finalize its
// decision state.
type findingWrite struct {
	FindingID string
	File      string
	// Applied is true when the finding's hunks are in the committed bytes (apply) or the produced
	// diff (patch).
	Applied bool
	// Skipped is true when the finding produced no write: its target was not shown, excluded or
	// empty, or no edit was proposed.
	Skipped bool
	// Refused is true when the finding's target is a protected path. Unlike a skip, a refusal
	// declines a proposed write and marks the run as not cleanly successful.
	Refused bool
	// Reason is the stable machine code for a finding that was not applied.
	Reason string
}

// writeRequest is one governed write. Its fields supply facts to the write rules; none selects a
// different rule.
type writeRequest struct {
	// Workspace is the live root being written, and Identity its canonical identity as captured when
	// the write was decided. An unbound identity halts.
	Workspace string
	Identity  WorkspaceIdentity
	// Mode is ModePatch or ModeApply.
	Mode review.Mode
	// Plan is the resolved plan, used only to select the author_remediator lane.
	Plan review.RunPlan
	// SourceRunID names the run whose decision set this applies; empty for this run's own.
	SourceRunID string
	// Artifacts is the run-relative directory for this window's journal, commit-attempt marker and
	// receipt; empty means defaultWriteArtifacts.
	Artifacts string
	// Findings and Decisions are the adjudicated set. They are reconciled by finding id, never by
	// position.
	Findings  []review.Finding
	Decisions []review.Decision
	// Accept reports whether a reconciled decision authorizes a write.
	Accept func(review.Decision) bool
	// Select, when non-nil, narrows the accepted set to findings whose host-computed fingerprint it
	// names. Accept decides what may be written and Select what the caller asked for; the write is
	// their intersection, and a selection can only reduce it.
	Select []string
	// SelectPrimary marks the window whose selection outcome is reported and whose empty match is a
	// refusal. Later cycles of a converging apply are not measured, because a selected finding may
	// already have been applied.
	SelectPrimary bool
	// Shown is the set of workspace-relative files a reviewer was shown.
	Shown map[string]bool
	// Pins maps each path to the content digest this write was decided against. PinsFromCopy records
	// them from the isolated copy instead. A nil map means nothing may be written.
	Pins         map[string]string
	PinsFromCopy bool
	// TrustedRoots, when non-empty, must contain the workspace.
	TrustedRoots []string
	// OnJournal, when set, receives the journal once it is durable and before the first edit. It must
	// not block or panic.
	OnJournal func(Journal)
	// AllowProtectedPaths waives the protected-config half of containment for both root admission and
	// the write denylist. Secrets are unaffected. See Request.AllowProtectedPaths.
	AllowProtectedPaths bool
	// VerifyCommands and VerifyTimeout carry the operator's build/test commands. See verify.go.
	VerifyCommands []string
	VerifyTimeout  time.Duration
}

// writeResult is what the window did. It is populated on every path, including a halt.
type writeResult struct {
	Journal   Journal
	Receipt   Receipt
	Cancelled bool
	Withheld  []review.WithheldFile
	Findings  []findingWrite
	// Refusals are the protected-path refusals, keyed by host-computed fingerprint rather than the
	// model-authored finding id.
	Refusals []review.ApplyRefusal
	// Selection is what a narrowing selection did, set only on the primary window.
	Selection *review.ApplySelection
	// Accepted counts decisions that authorized a write after selection. Zero means the window
	// closed before any copy was made.
	Accepted int
	// Committed reports whether the live commit completed; always false in patch mode.
	Committed bool
	// Applied counts findings whose hunks reached the live tree.
	Applied int
	// Verification is what the operator's commands did before and after the edits; nil when none
	// were supplied. Nothing branches on it.
	Verification *review.VerificationReport
	// UndoPatch is the absolute path of the reverse-appliable patch, set only for an apply into a tree
	// without version control.
	UndoPatch string
}

// selectAccepted narrows a reconciled accepted set to the findings whose host-computed fingerprint a
// caller named, and reports what matched. It keys on schema.Fingerprint, never the model-authored
// Finding.ID, so a model cannot relabel findings to steer a selection. It is a pure filter: a
// fingerprint that names nothing in the set lands in Unmatched.
func selectAccepted(accepted []acceptedTarget, want []string) (review.ApplySelection, []acceptedTarget) {
	sel := review.ApplySelection{Requested: []string{}, Matched: []string{}, Unmatched: []string{}}
	// Trim and deduplicate the caller's list, preserving order.
	seen := map[string]bool{}
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		sel.Requested = append(sel.Requested, w)
	}
	if len(sel.Requested) == 0 {
		return sel, nil
	}
	// Index by fingerprint; if two accepted findings share one, selecting it selects both.
	byFP := map[string][]acceptedTarget{}
	for _, t := range accepted {
		fp := schema.Fingerprint(t.finding)
		byFP[fp] = append(byFP[fp], t)
	}
	picked := map[int]bool{}
	for _, w := range sel.Requested {
		hits, ok := byFP[w]
		if !ok {
			sel.Unmatched = append(sel.Unmatched, w)
			continue
		}
		sel.Matched = append(sel.Matched, w)
		for _, t := range hits {
			picked[t.index] = true
		}
	}
	// Keep the accepted set's order; call ids and the journal follow it.
	var narrowed []acceptedTarget
	for _, t := range accepted {
		if picked[t.index] {
			narrowed = append(narrowed, t)
		}
	}
	return sel, narrowed
}

// artifact resolves a run-relative path inside this window's artifact directory.
func (w writeRequest) artifact(name string) string {
	dir := w.Artifacts
	if dir == "" {
		dir = defaultWriteArtifacts
	}
	return path.Join(dir, name)
}

// governedWrite performs one write window (see the file comment). It owns the isolated copy, journal,
// staging, commit and receipt; the caller owns the run directory and the resulting decision states.
// On every return the receipt has been persisted, or the error says why it could not be.
func (m *Manager) governedWrite(ctx context.Context, run *audit.Run, req writeRequest) (writeResult, error) {
	var out writeResult
	out.Findings = []findingWrite{}
	out.Refusals = []review.ApplyRefusal{}

	pins := req.Pins
	if pins == nil {
		pins = map[string]string{}
	}

	receipt := Receipt{
		SchemaVersion: 1, RunID: run.ID, SourceRunID: req.SourceRunID,
		Mode: string(req.Mode), Status: "complete",
		BaseHashesVerified: len(pins),
		Intended:           []IntendedHunk{}, Applied: []AppliedFinding{},
		NotApplied: []AppliedFinding{}, Files: []string{},
	}
	// finish persists the receipt durably. It is the record of what happened, so failing to write it
	// fails the run.
	finish := func(status, reason string) error {
		receipt.Status, receipt.ReasonCode = status, reason
		receipt.IntentSHA256 = intentDigest(receipt.Intended)
		out.Receipt = receipt
		if perr := writeDurableJSON(run.Dir, req.artifact("receipt.json"), receipt); perr != nil {
			return fault.Wrap(fault.Internal, "persist remediation receipt", perr).
				WithReason(ReasonReceiptUnwritable)
		}
		return nil
	}
	// halt records the outcome and returns the error to return. If the receipt cannot be persisted,
	// that failure replaces the original fault, so the caller never assumes a receipt exists.
	halt := func(status, reason string, cause error) error {
		if perr := finish(status, reason); perr != nil {
			return fault.Wrap(fault.Internal,
				fmt.Sprintf("the remediation %s (%s) could NOT be recorded — treat this run as unreconciled and inspect %s", status, reason, run.Dir),
				errors.Join(perr, cause)).WithReason(ReasonReceiptUnwritable)
		}
		return cause
	}

	// Reconcile decisions to findings by id first; an unreconcilable set is refused whole.
	accepted, rcerr := reconcileDecisions(req.Findings, req.Decisions, req.Accept)
	if rcerr != nil {
		return out, halt("halted", ReasonDecisionSetUnreconciled, fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: the decision set does not reconcile with its findings — %v. Decisions are bound to findings by ID, never by position; re-run review_report and remediate from that run rather than from a set that has been reordered, truncated or hand-assembled.",
			rcerr)).WithReason(ReasonDecisionSetUnreconciled))
	}

	// Apply the selection after reconciliation, so it names only verified bindings, and before any
	// other check, so a selection of nothing is refused before any copy or model call.
	if req.Select != nil {
		sel, narrowed := selectAccepted(accepted, req.Select)
		if req.SelectPrimary {
			out.Selection = &sel
			// Record the selection on the receipt before anything can refuse.
			receipt.Selection = &sel
		}
		if len(sel.Requested) == 0 {
			return out, halt("halted", ReasonSelectionEmpty, fault.New(fault.Usage,
				"remediation refused: `select` was supplied and is empty. An empty narrowing filter does NOT mean \"apply everything\" — it names zero findings, so this call would write nothing while reading as a normal apply. Omit `select` to apply the whole accepted set, or name the host-computed fingerprints you want.").
				WithReason(ReasonSelectionEmpty))
		}
		// Only the primary window refuses an empty match; on a later cycle it means the findings
		// were already applied.
		if req.SelectPrimary && len(sel.Matched) == 0 {
			return out, halt("halted", ReasonSelectionMatchedNothing, fault.New(fault.Usage, fmt.Sprintf(
				"remediation refused: none of the %d fingerprint(s) in `select` names a finding in this run's accepted set (%s). Nothing was written. A selector is the HOST-COMPUTED `fingerprint` from the accepted findings of the run being applied — never a finding's `id`, which is model-authored and renumbered.",
				len(sel.Requested), strings.Join(sel.Unmatched, ", "))).
				WithReason(ReasonSelectionMatchedNothing))
		}
		accepted = narrowed
	}

	// Bind the workspace by identity, not pathname, before checking pins: matching hashes in the wrong
	// tree is the failure this catches.
	if !req.Identity.Bound() {
		return out, halt("halted", ReasonWorkspaceUnbound, fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: this request carries no canonical identity for the reviewed workspace %q, so there is no way to verify that the path still names the tree that was reviewed. The run that produced the decision set must capture it (review.CaptureWorkspaceIdentity) and carry it on the request.",
			req.Workspace)).WithReason(ReasonWorkspaceUnbound))
	}
	if verr := req.Identity.verifyAgainst(req.Workspace); verr != nil {
		_ = run.Event(m.now(), "error", "run_halted", "workspace identity changed", map[string]any{"workspace": req.Workspace})
		return out, halt("halted", ReasonWorkspaceIdentityChanged, fault.Wrap(fault.Policy, fmt.Sprintf(
			"remediation refused: %v. The accepted set was adjudicated against a specific directory, not against a path string — re-run review_report against the tree you mean to change.",
			verr), verr).WithReason(ReasonWorkspaceIdentityChanged))
	}

	// First staleness check, for pins from an earlier run: refuse before any copy or model call. The
	// commit re-verifies every destination, which is the check that matters for pins taken from the copy.
	if !req.PinsFromCopy {
		if stale := StaleBaseHashes(req.Workspace, pins); len(stale) > 0 {
			_ = run.Event(m.now(), "error", "run_halted", "decision set is stale", map[string]any{"files": stale})
			return out, halt("halted", ReasonStaleDecisionSet, fault.New(fault.Policy, fmt.Sprintf(
				"remediation refused: %d file(s) changed since the review that produced these decisions (%s). The accepted set describes a workspace that no longer exists — re-run review_report and remediate from the new run.",
				len(stale), strings.Join(stale, ", "))).WithReason(ReasonStaleDecisionSet))
		}
	}

	out.Accepted = len(accepted)
	if len(accepted) == 0 {
		if perr := finish("complete", ""); perr != nil {
			return out, perr
		}
		_ = run.Event(m.now(), "info", "remediation_completed", "no accepted findings to apply", nil)
		return out, nil
	}

	ws := workspace.New(m.TempBase)
	ws.AllowProtectedRoots = req.AllowProtectedPaths
	// A stored decision set can outlive the roots its run was launched with, so enforce the roots in
	// force now. The protected-path waiver applies here too, or it would refuse the root it admits.
	if len(req.TrustedRoots) > 0 {
		trust, terr := scope.NewWith(scope.Options{AllowProtectedWrites: req.AllowProtectedPaths}, req.TrustedRoots...)
		if terr != nil {
			return out, halt("halted", string(scope.ReasonUnresolvable),
				fault.Wrap(fault.Config, "resolve trusted roots", terr).
					WithHalt(ScopeHaltClass).WithReason(string(scope.ReasonUnresolvable)))
		}
		if _, derr := trust.ResolveWrite(req.Workspace); derr != nil {
			if sf := scopeFault(derr); sf != nil {
				return out, halt("halted", sf.Reason(), sf)
			}
			return out, halt("halted", string(scope.ReasonUnresolvable),
				fault.Wrap(fault.Containment, "workspace outside the trusted roots", derr).
					WithHalt(ScopeHaltClass).WithReason(string(scope.ReasonUnresolvable)))
		}
	}
	// Root confinement is built here from the workspace, so no caller can supply a wider one.
	confine, serr := scope.NewWith(scope.Options{AllowProtectedWrites: req.AllowProtectedPaths}, req.Workspace)
	if serr != nil {
		return out, halt("halted", string(scope.ReasonUnresolvable),
			fault.Wrap(fault.Config, "resolve workspace root", serr).
				WithHalt(ScopeHaltClass).WithReason(string(scope.ReasonUnresolvable)))
	}
	ws.Guard = confine

	// A dirty tree is recorded as an event, never refused: uncommitted work is normal, and
	// changes.patch reverse-applies exactly this run's edits.
	if req.Mode == review.ModeApply {
		if d := ProbeWorkspaceDirtiness(ctx, req.Workspace); d.Known && d.Dirty {
			_ = run.Event(m.now(), "warn", "workspace_dirty", d.Summary(), map[string]any{
				"vcs": d.VCS, "paths": d.Paths, "more": d.More,
			})
		}
	}

	rcopy, cerr := ws.Copy(req.Workspace, false, "remediation")
	if cerr != nil {
		return out, halt("halted", "remediation_copy_failed",
			fault.Wrap(fault.Internal, "provide remediation copy", cerr).WithReason("remediation_copy_failed"))
	}
	defer ws.Cleanup(rcopy)
	if rwh := withheldFrom(stageCopy, rcopy.Caveats); len(rwh) > 0 {
		m.eventWithheld(run, rwh)
		out.Withheld = appendWithheld(out.Withheld, rwh...)
	}

	// The verification baseline runs on a throwaway copy, not rcopy: the commit writes back every
	// file in rcopy that differs from live, so build output there would land in the user's tree.
	verifyBefore := m.baselineOnAThrowawayCopy(ctx, run, ws, req.Workspace, req.VerifyCommands, req.VerifyTimeout)

	// absentFromCopy holds targets this window's copy does not contain (withheld or deleted); they are
	// skipped, not halted.
	absentFromCopy := map[string]bool{}
	// canTarget reports whether a finding's file is non-excluded, was shown, and has a base in the copy.
	canTarget := func(f review.Finding) bool {
		return f.File != "" && !workspace.IsExcluded(f.File) && req.Shown[f.File] && !absentFromCopy[f.File]
	}

	// Record pins from the isolated copy, the base every edit derives from. Pinning the live tree
	// would adopt a save made after the copy as the base and then overwrite it.
	if req.PinsFromCopy {
		rels := make([]string, 0, len(accepted))
		for _, t := range accepted {
			if canTarget(t.finding) {
				rels = append(rels, t.finding.File)
			}
		}
		pins = BaseHashes(rcopy.Root, rels)
		// A target the copy lacks is skipped: the run has already recorded why it was not copied.
		// Dropping its pin keeps the commit's pin set exact.
		for rel, pin := range pins {
			if pin == BaseHashAbsent {
				absentFromCopy[rel] = true
				delete(pins, rel)
			}
		}
		receipt.BaseHashesVerified = len(pins)
	}

	hostLane, hasHost := req.Plan.Lanes[review.RoleAuthorRemediator]
	hostAdapter, hostReg := m.Adapters[hostLane.Adapter]
	modelRemediation := hasHost && hostLane.Adapter != "fake" && hostReg

	// Pass 1 computes every intended hunk without writing, so the journal is a complete statement of
	// intent.
	type plannedEdit struct {
		finding review.Finding
		edits   []review.Edit
		origin  string
	}
	var planned []plannedEdit
	for _, t := range accepted {
		f := t.finding
		// Check confinement before the skip rules. A protected-path denial is a recorded refusal and
		// the other findings proceed; any other denial (outside the root, unresolvable) is an escape
		// and halts.
		if aerr := authorizeTarget(ws, rcopy, f.File); aerr != nil {
			d, isDenial := scope.AsDenial(aerr)
			if !isDenial || d.Reason != scope.ReasonWriteDenied {
				return out, halt("halted", fault.ReasonOf(aerr), aerr)
			}
			receipt.NotApplied = append(receipt.NotApplied, AppliedFinding{
				FindingID: f.ID, File: f.File, State: string(review.StateReportedValid),
				Reason: review.ApplyRefusalProtectedPath,
			})
			out.Findings = append(out.Findings, findingWrite{
				FindingID: f.ID, File: f.File, Refused: true,
				Reason: review.ApplyRefusalProtectedPath,
			})
			out.Refusals = append(out.Refusals, review.ApplyRefusal{
				// Key by the host-computed fingerprint, which survives renumbering and cannot be
				// relabelled by a model.
				Fingerprint: schema.Fingerprint(f),
				File:        f.File,
				Reason:      review.ApplyRefusalProtectedPath,
				FindingID:   f.ID,
			})
			_ = run.Event(m.now(), "warn", "apply_refused_protected_path",
				"finding targets a protected path; refused and NOT applied, the rest of the remediation proceeds",
				map[string]any{"finding": f.ID, "file": f.File, "rule": d.Rule})
			continue
		}
		if !canTarget(f) {
			reason, message := reasonTargetNotShownOrExcluded, "finding targets an excluded/unshown/empty path; not applied"
			if absentFromCopy[f.File] {
				reason, message = reasonTargetAbsentFromCopy, "finding targets a file this run holds no copy of; not applied"
			}
			receipt.NotApplied = append(receipt.NotApplied, AppliedFinding{
				FindingID: f.ID, File: f.File, State: string(review.StateSkipped), Reason: reason,
			})
			out.Findings = append(out.Findings, findingWrite{
				FindingID: f.ID, File: f.File, Skipped: true, Reason: reason,
			})
			_ = run.Event(m.now(), "warn", "remediation_completed", message,
				map[string]any{"finding": f.ID, "file": f.File})
			continue
		}
		var edits []review.Edit
		origin := "marker"
		if modelRemediation {
			if content, truncated, readErr := readBounded(rcopy.Root, f.File); readErr == nil {
				if e := m.hostRemediationCall(ctx, run, ws, rcopy.Root, hostLane, hostAdapter, f, content, truncated, fmt.Sprintf("remediate-%d", t.index)); len(e) > 0 {
					edits, origin = e, "model"
				}
			}
		}
		if len(edits) == 0 {
			edits, origin = remediation.Propose(f), "marker"
		}
		if len(edits) == 0 {
			receipt.NotApplied = append(receipt.NotApplied, AppliedFinding{
				FindingID: f.ID, File: f.File, State: string(review.StateSkipped),
				Reason: reasonNoEditProposed,
			})
			out.Findings = append(out.Findings, findingWrite{
				FindingID: f.ID, File: f.File, Skipped: true, Reason: reasonNoEditProposed,
			})
			continue
		}
		for _, e := range edits {
			// Every edit target must match what was reviewed, not merely lie inside the root.
			if terr := authorizeEditTarget(f, e, req.Shown, pins); terr != nil {
				return out, halt("halted", fault.ReasonOf(terr), terr)
			}
			// e.File equals f.File, which already passed the denylist, so any denial here is an
			// escape and halts.
			if aerr := authorizeTarget(ws, rcopy, e.File); aerr != nil {
				return out, halt("halted", fault.ReasonOf(aerr), aerr)
			}
			receipt.Intended = append(receipt.Intended, IntendedHunk{
				FindingID: f.ID, File: e.File, Anchor: e.Anchor, Occurrence: e.Occurrence,
				Bytes: len(e.Replacement), Origin: origin,
				ReplacementSHA256: hunkDigest(e.Replacement),
			})
		}
		planned = append(planned, plannedEdit{finding: f, edits: edits, origin: origin})
	}

	// The journal must be durable before the first edit; if it cannot be written, nothing is written.
	journal := Journal{
		SchemaVersion: 1, RunID: run.ID, SourceRunID: req.SourceRunID, Mode: string(req.Mode),
		Hunks: receipt.Intended, IntentSHA256: intentDigest(receipt.Intended),
	}
	out.Journal = journal
	receipt.IntentSHA256 = journal.IntentSHA256
	if jerr := writeDurableJSON(run.Dir, req.artifact("journal.json"), journal); jerr != nil {
		return out, halt("halted", ReasonJournalUnwritable, fault.Wrap(fault.Internal, fmt.Sprintf(
			"the write journal could not be made durable in %s, so the write window was not opened and NOTHING was written",
			run.Dir), jerr).WithReason(ReasonJournalUnwritable))
	}
	_ = run.Event(m.now(), "info", "remediation_journaled", "intended hunks journaled before any write",
		map[string]any{"hunks": len(journal.Hunks), "files": len(journal.Hunks)})
	if req.OnJournal != nil {
		req.OnJournal(journal)
	}

	// Cancellation is honored here and at the commit only; mid-hunk it would leave an unjournaled copy.
	if ctx.Err() != nil {
		_, _, _ = ws.Discard(rcopy)
		out.Cancelled = true
		_ = run.Event(m.now(), "warn", "remediation_completed", "cancelled before the write window opened; nothing was written", nil)
		return out, halt("cancelled", ReasonRemediateCancelled,
			fault.Wrap(fault.Policy, "remediation cancelled before any write", ctx.Err()).
				WithHalt("F").WithReason(ReasonRemediateCancelled))
	}

	// Pass 2 applies edits to the copy; only the commit reaches the live tree. Each finding is
	// all-or-nothing: its files are snapshotted and restored if any hunk fails, so the receipt matches
	// what ships. A failed rollback halts, because the copy then matches no recorded intent.
	type findingResult struct {
		finding review.Finding
		applied bool
		reason  string
	}
	var results []findingResult
	for _, p := range planned {
		targets := make([]string, 0, len(p.edits))
		for _, e := range p.edits {
			targets = append(targets, e.File)
		}
		snap, serr := ws.SnapshotFiles(rcopy, targets)
		if serr != nil {
			return out, halt("halted", ReasonStageRollbackFailed, fault.Wrap(fault.Internal, fmt.Sprintf(
				"finding %s could not be staged for an all-or-nothing apply", p.finding.ID), serr).
				WithReason(ReasonStageRollbackFailed))
		}
		failed := 0
		for _, e := range p.edits {
			if aerr := ws.ApplyEdit(rcopy, e); aerr != nil {
				failed++
				_ = run.Event(m.now(), "warn", "remediation_completed", "edit not applied",
					map[string]any{"finding": p.finding.ID, "error": aerr.Error()})
			}
		}
		if failed > 0 {
			if rerr := ws.RestoreFiles(rcopy, snap); rerr != nil {
				return out, halt("halted", ReasonStageRollbackFailed, fault.Wrap(fault.Internal, fmt.Sprintf(
					"finding %s applied %d of %d hunk(s) and could not be rolled back out of the isolated copy; nothing was committed",
					p.finding.ID, len(p.edits)-failed, len(p.edits)), rerr).
					WithReason(ReasonStageRollbackFailed))
			}
			results = append(results, findingResult{finding: p.finding, reason: reasonEditFailed})
			continue
		}
		results = append(results, findingResult{finding: p.finding, applied: true})
	}

	// The commit is derived from the diff, so a diff failure halts.
	diff, changes, derr := ws.Diff(rcopy)
	if derr != nil {
		return out, halt("halted", ReasonDiffFailed, fault.Wrap(fault.Internal,
			"the isolated copy could not be diffed against the live workspace, so what a commit would write is not knowable; nothing was committed", derr).
			WithReason(ReasonDiffFailed))
	}
	if len(changes) > 0 {
		if diff != "" {
			if perr := writeDurable(run.Dir, "patches/changes.patch", []byte(diff)); perr != nil {
				return out, halt("halted", ReasonPatchUnwritable, fault.Wrap(fault.Internal,
					"the patch artifact could not be written, so the receipt cannot reference it; nothing was committed", perr).
					WithReason(ReasonPatchUnwritable))
			}
			sum := sha256.Sum256([]byte(diff))
			receipt.PatchArtifact, receipt.PatchSHA256 = "patches/changes.patch", "sha256:"+hex.EncodeToString(sum[:])
		}
		if perr := run.WriteJSON("patches/patch-summary.json", patchSummary(req.Mode, changes)); perr != nil {
			return out, halt("halted", ReasonPatchUnwritable, fault.Wrap(fault.Internal,
				"the patch summary could not be written; nothing was committed", perr).
				WithReason(ReasonPatchUnwritable))
		}
		_ = run.Event(m.now(), "info", "patch_written", "patch artifact written", map[string]any{"files": len(changes)})
		// In a tree without version control, changes.patch is the only undo, so name it on the
		// record. EnsureDurableRunDir keeps it out of the OS temp directory.
		if req.Mode == review.ModeApply && receipt.PatchArtifact != "" {
			if d := ProbeWorkspaceDirtiness(ctx, req.Workspace); !d.Known {
				out.UndoPatch = filepath.Join(run.Dir, receipt.PatchArtifact)
				_ = run.Event(m.now(), "warn", "undo_is_this_patch", fmt.Sprintf(
					"this tree is under no version control, so reverse-applying %s is the ONLY way to undo what this run is about to write (`git apply -R <patch>`, or `patch -R -p1 < <patch>`)", out.UndoPatch),
					map[string]any{"undoPatch": out.UndoPatch})
			}
		}
	}

	if req.Mode == review.ModeApply {
		// The second boundary is decided once under a lock: cancelled or committing, never both.
		var window writeWindow
		if !window.enter(ctx) {
			_, _, _ = ws.Discard(rcopy)
			out.Cancelled = true
			_ = run.Event(m.now(), "warn", "remediation_completed", "cancelled before commit; the live workspace is unchanged", nil)
			return out, halt("cancelled", ReasonRemediateCancelled,
				fault.Wrap(fault.Policy, "remediation cancelled before apply", ctx.Err()).
					WithHalt("F").WithReason(ReasonRemediateCancelled))
		}
		// The window is open and the context no longer decides the outcome (see writeWindow). The
		// durable marker records that a commit was entered, so a crash is visible.
		receipt.CommitAttempted = true
		if merr := writeDurableJSON(run.Dir, req.artifact("commit-attempt.json"), map[string]any{
			"schemaVersion": 1, "runId": run.ID, "sourceRunId": req.SourceRunID,
			"mode": string(req.Mode), "intentSha256": journal.IntentSHA256,
			"files": changedFiles(changes), "at": m.now().UTC().Format(time.RFC3339Nano),
		}); merr != nil {
			_, _, _ = ws.Discard(rcopy)
			return out, halt("halted", ReasonJournalUnwritable, fault.Wrap(fault.Internal,
				"the commit-attempt marker could not be made durable, so an interrupted commit would be unreconcilable; nothing was committed", merr).
				WithReason(ReasonJournalUnwritable))
		}
		if testHookInsideWriteWindow != nil {
			testHookInsideWriteWindow()
		}
		// Second staleness check: the commit verifies each destination against its pin immediately
		// before replacing it, catching in-place saves made during the model calls. This is the only
		// live-workspace commit.
		committed, commitErr := ws.CommitExpecting(rcopy, pins)
		if commitErr != nil {
			_, _, _ = ws.Discard(rcopy)
			// The commit rolled back, so every planned finding is recorded as not applied.
			for _, r := range results {
				receipt.NotApplied = append(receipt.NotApplied, AppliedFinding{
					FindingID: r.finding.ID, File: r.finding.File,
					State: string(review.StateReportedValid), Reason: ReasonApplyCommitFailed,
				})
				out.Findings = append(out.Findings, findingWrite{
					FindingID: r.finding.ID, File: r.finding.File, Reason: ReasonApplyCommitFailed,
				})
			}
			// A destination that drifted is the same event as a stale decision set, so it uses the
			// same reason code and is a policy refusal rather than an internal error.
			if wr, ok := workspace.AsRefusal(commitErr); ok && isDestinationDrift(wr.Reason) {
				return out, halt("halted", ReasonStaleDecisionSet, fault.Wrap(fault.Policy, fmt.Sprintf(
					"remediation refused at the commit boundary: %q is no longer what the review judged (%s). NOTHING was written — the commit rolled back. Re-run the review and remediate from the new run.",
					wr.Path, wr.Reason), commitErr).WithReason(ReasonStaleDecisionSet))
			}
			if sf := scopeFault(commitErr); sf != nil {
				return out, halt("halted", sf.Reason(), sf)
			}
			return out, halt("halted", ReasonApplyCommitFailed,
				fault.Wrap(fault.Internal, "commit remediation to live workspace", commitErr).
					WithReason(ReasonApplyCommitFailed))
		}
		// The receipt is derived from the successful commit, not from the copy.
		receipt.Committed, out.Committed = true, true
		receipt.Files = append(receipt.Files, committed...)
		for _, r := range results {
			if r.applied {
				receipt.Applied = append(receipt.Applied, AppliedFinding{
					FindingID: r.finding.ID, File: r.finding.File, State: string(review.StateApplied),
				})
				out.Findings = append(out.Findings, findingWrite{
					FindingID: r.finding.ID, File: r.finding.File, Applied: true,
				})
				out.Applied++
				continue
			}
			receipt.NotApplied = append(receipt.NotApplied, AppliedFinding{
				FindingID: r.finding.ID, File: r.finding.File,
				State: string(review.StateReportedValid), Reason: r.reason,
			})
			out.Findings = append(out.Findings, findingWrite{
				FindingID: r.finding.ID, File: r.finding.File, Reason: r.reason,
			})
		}
		_ = run.Event(m.now(), "info", "apply_committed", "applied edits to live workspace",
			map[string]any{"files": committed})
	} else {
		// Patch mode: "applied" means reached the diff, and the file list is the diff's.
		receipt.Files = append(receipt.Files, changedFiles(changes)...)
		for _, r := range results {
			if r.applied {
				receipt.Applied = append(receipt.Applied, AppliedFinding{
					FindingID: r.finding.ID, File: r.finding.File, State: string(review.StateReportedValid),
				})
				out.Findings = append(out.Findings, findingWrite{
					FindingID: r.finding.ID, File: r.finding.File, Applied: true,
				})
				continue
			}
			receipt.NotApplied = append(receipt.NotApplied, AppliedFinding{
				FindingID: r.finding.ID, File: r.finding.File,
				State: string(review.StateReportedValid), Reason: r.reason,
			})
			out.Findings = append(out.Findings, findingWrite{
				FindingID: r.finding.ID, File: r.finding.File, Reason: r.reason,
			})
		}
	}
	sort.Strings(receipt.Files)

	// The after-edit verification runs on the same copy after the commit: the commit writes back every
	// differing file, so build output from an earlier run would land in the user's tree, and running
	// afterwards means a failing suite cannot block the commit.
	if len(req.VerifyCommands) > 0 {
		_ = run.Event(m.now(), "info", "verification_after_start",
			fmt.Sprintf("re-running %d project command(s) on the containment copy, now with the applied edits", len(req.VerifyCommands)),
			map[string]any{"commands": req.VerifyCommands})
		after := runVerifyPass(ctx, rcopy.Root, req.VerifyCommands, req.VerifyTimeout)
		m.eventVerification(run, "verification_after_done", "after", after)
		out.Verification = newVerificationReport(rcopy.Root, req.VerifyCommands, verifyBefore, after)
		_ = run.WriteJSON(req.artifact("verification.json"), out.Verification)
		_ = run.Event(m.now(), "info", "verification_delta",
			"verification delta: "+out.Verification.Delta+" — RECORDED, not enforced: nothing was dropped, downgraded or blocked by it",
			map[string]any{"delta": out.Verification.Delta})
	}

	_, _, _ = ws.Discard(rcopy)
	if perr := finish("complete", ""); perr != nil {
		return out, perr
	}
	return out, nil
}

// eventVerification records one pass's per-command outcome. Command output stays in
// verification.json to keep event lines small.
func (m *Manager) eventVerification(run *audit.Run, eventType, pass string, results []review.VerificationResult) {
	rows := make([]map[string]any, 0, len(results))
	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
		}
		rows = append(rows, map[string]any{
			"command": r.Command, "ok": r.OK, "exitCode": r.ExitCode,
			"timedOut": r.TimedOut, "durationMs": r.DurationMs,
		})
	}
	_ = run.Event(m.now(), "info", eventType,
		fmt.Sprintf("%s pass: %d of %d project command(s) passed", pass, len(results)-failed, len(results)),
		map[string]any{"pass": pass, "commands": rows})
}
