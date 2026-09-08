package run

// THE GOVERNED WRITE PATH — the ONE place in this repository that turns an adjudicated finding
// into bytes in a live workspace.
//
// It exists so there is exactly ONE. The hardening — a journal, content pins, a cancel/commit gate,
// a receipt and edit-target authorization — lives here and therefore holds on EVERY surface.
// A second write path is how those guarantees diverge: a branch that commits with a plain, unpinned
// `ws.Commit` re-opens the data-loss defect they close (an editor's in-place save during a long
// remediation, silently overwritten from the stale base) on whichever surface skipped them.
//
// So the guarantees are never copied: they are this function. `Manager.Remediate` (remediate.go)
// and `handleMode`'s patch/apply branch — the branch the CLI and the ACP surface reach — are both
// callers of `governedWrite`, and neither of them contains a `Commit` call of its own. What differs
// between them is INPUT, never rule:
//
//   - WHERE THE PINS COME FROM. A from-run remediation is decided against base hashes captured by
//     an EARLIER run (the report run a human read), so those pins ride the request and are
//     re-verified against the live tree before anything is copied. A full cycle decides against the
//     isolated copy it just took, so it RECORDS its pins from that copy — the exact bytes its edits
//     are derived from. Either way the pin means the same thing at the commit: "this destination is
//     still the content this write was decided against, or nothing is written".
//   - WHETHER OUT-OF-BAND TRUSTED ROOTS GATE THE WORKSPACE. They do for a decision set that
//     travelled (it can outlive the roots its run was launched with); they deliberately do not for a
//     full cycle, because an agent surface may review a directory IT materialized, which no human
//     root covers (see Request.TrustedRoots).
//
// Everything else — the journal, the cancel/commit gate, per-finding staging, the commit-attempt
// marker, the pinned commit, the receipt — is this function, once, for every surface.

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

// defaultWriteArtifacts is the run-relative directory a write window records itself in. It is the
// path docs/mcp.md and docs/security.md name, so the FIRST write window of any run — every MCP
// remediation, and the first cycle of a CLI/ACP apply — keeps it.
const defaultWriteArtifacts = "remediation"

// Stable MACHINE reason codes for a finding that produced no write. They are recorded on the
// receipt and mapped to a decision state by the caller, so both surfaces spell the same outcome
// the same way.
const (
	// reasonTargetNotShownOrExcluded — the finding's file is empty, excluded/internal, or was
	// never shown to a reviewer.
	reasonTargetNotShownOrExcluded = "target_not_shown_or_excluded"
	// reasonTargetAbsentFromCopy — the finding's file was not in this run's isolated copy (it was
	// withheld for a containment reason, or it stopped existing between the copies), so there is
	// no base to write against and nothing to edit.
	reasonTargetAbsentFromCopy = "target_absent_from_copy"
	// reasonNoEditProposed — neither the model nor the deterministic engine produced an edit.
	reasonNoEditProposed = "no_edit_proposed"
	// reasonEditFailed — at least one of the finding's hunks did not apply, so the finding was
	// rolled back out of the copy whole.
	reasonEditFailed = "edit_failed"
)

// ReasonApplyRefusedProtectedPath is the RUN-LEVEL machine reason code for a remediation that
// completed with at least one protected-path refusal. It is the code the CLI reports beside exit
// 7 and the code the projection carries; the PER-FINDING code on the receipt row is
// `review.ApplyRefusalProtectedPath` ("protected_path").
//
// The two are deliberately different strings. `protected_path` answers "why was THIS finding not
// applied"; `apply_refused_protected_path` answers "why did this RUN not exit 0" — and a reader
// who saw only the per-finding code beside an exit code would have to infer that the run-level
// consequence exists at all.
const ReasonApplyRefusedProtectedPath = "apply_refused_protected_path"

// The two ways a SELECTIVE APPLY refuses. Both are refusals BEFORE the write window — nothing is
// copied, nothing is planned and nothing is written — because both mean the caller asked for a
// narrowing that names no writable finding, and the fail-closed answer to "write exactly these
// zero things" is to write nothing and say so, not to fall back to writing everything.
const (
	// ReasonSelectionEmpty — `select` was supplied and is empty. An empty narrowing filter must
	// never mean "apply everything": that is the one outcome a caller can neither detect nor
	// survive, and it is the same fail-closed discipline `intersectRoots` applies to roots.
	ReasonSelectionEmpty = "apply_selection_empty"
	// ReasonSelectionMatchedNothing — every fingerprint in `select` named a finding that is not in
	// this run's accepted set. The design names the empty-list case; this is the same caller error
	// arriving one step later (a mistyped or stale fingerprint list), and it gets the same answer
	// for the same reason. A PARTIAL match is NOT this: it proceeds, and the unmatched entries are
	// reported on the result and recorded on the receipt.
	ReasonSelectionMatchedNothing = "apply_selection_matched_nothing"
)

// findingWrite is one accepted finding's write outcome, as the write path observed it. It is what a
// caller needs to finalize the finding's decision state without re-deriving anything.
type findingWrite struct {
	FindingID string
	File      string
	// Applied is true when the finding's hunks are in the bytes that were committed (apply) or in
	// the diff that was produced (patch).
	Applied bool
	// Skipped is true when the finding never produced a write at all: its target was not shown,
	// excluded or empty, or no edit was proposed for it. It is a different disposition from
	// "attempted and failed", and callers render it differently.
	Skipped bool
	// Refused is true when the finding's target resolved to a PROTECTED PATH. It is a third
	// disposition, not a flavour of Skipped: a skip is an absence of anything to write, whereas a
	// refusal is the host declining a write the model did propose — and only the refusal makes
	// the run report itself as not cleanly successful.
	Refused bool
	// Reason is the stable machine code for a finding that was not applied.
	Reason string
}

// writeRequest is one governed write. Its fields are inputs — nothing here selects a different
// RULE, only a different source for a fact the rule needs.
type writeRequest struct {
	// Workspace is the live root being written. Identity is that root's canonical identity as
	// captured by the thing that decided this write (the report run, for a from-run remediation;
	// the start of the cycle, for a full cycle). An unbound identity is a halt: a path is not a
	// repository.
	Workspace string
	Identity  WorkspaceIdentity
	// Mode must be ModePatch or ModeApply; the caller has already refused ModeReport.
	Mode review.Mode
	// Plan is the RESOLVED plan, used only to select the author_remediator lane.
	Plan review.RunPlan
	// SourceRunID names the run whose decision set this applies ("" when it is this run's own).
	SourceRunID string
	// Artifacts is the run-relative directory for this window's journal, commit-attempt marker and
	// receipt. Empty means defaultWriteArtifacts. A run with more than one write window (a
	// converging apply) gives its later windows their own directory so no journal is overwritten.
	Artifacts string
	// Findings / Decisions are the adjudicated set, index-aligned. They are RECONCILED BY FINDING
	// ID here, never trusted by position.
	Findings  []review.Finding
	Decisions []review.Decision
	// Accept reports whether a reconciled decision authorizes a write. A from-run remediation
	// accepts the report run's `reported_valid` set; a full cycle accepts its actionable,
	// non-refused decisions.
	Accept func(review.Decision) bool
	// Select, when non-nil, NARROWS the accepted set to the findings whose HOST-COMPUTED
	// fingerprint it names (design §13.3, decision D8-A). nil means no narrowing.
	//
	// It composes with Accept rather than replacing it: `Accept` decides what a run is ALLOWED to
	// write, `Select` decides which of that the caller ASKED to write, and the intersection is what
	// happens. Like the `roots` narrowing argument it can only reduce — a fingerprint naming
	// nothing in this set is dropped, never resolved against another run, and never fetched from
	// anywhere. That is why threading it here, into the one governed write path, reaches the CLI,
	// ACP and MCP at once and cannot mean three things on three surfaces.
	Select []string
	// SelectPrimary marks the window whose selection outcome is REPORTED and whose "matched
	// nothing" is a refusal.
	//
	// A converging apply opens a write window PER CYCLE and the selection narrows every one of
	// them. Only the first is measured against the caller's list, and the reason is not a nicety:
	// by cycle 2 a selected finding may legitimately be absent because cycle 1 applied it, so
	// reporting it as an unmatched selector would be a false alarm and refusing the cycle over it
	// would abort a run that did exactly what was asked.
	SelectPrimary bool
	// Shown is the set of workspace-relative files a reviewer was actually shown.
	Shown map[string]bool
	// Pins is path → the content digest this write was decided against. When PinsFromCopy is set it
	// is IGNORED and recorded from the isolated copy instead (see the file comment). A nil map is
	// normalized to an empty one: a pinned commit is exhaustive, and "no pins" must mean "nothing
	// may be written", never "write everything unchecked".
	Pins         map[string]string
	PinsFromCopy bool
	// TrustedRoots, when non-empty, must contain the workspace. See the file comment for why this
	// is an input rather than a universal rule.
	TrustedRoots []string
	// OnJournal, when set, receives the journal at the moment it is durable and BEFORE the first
	// edit. It must not block or panic.
	OnJournal func(Journal)
	// AllowProtectedPaths waives the protected-config half of containment for this window — the
	// root admission AND the write denylist, which must agree or the window opens on a tree it
	// cannot write. Secrets are unaffected. See run.Request.AllowProtectedPaths.
	AllowProtectedPaths bool
	// VerifyCommands / VerifyTimeout carry the operator's own build/test commands into this window.
	// Empty means nothing is executed. See verify.go.
	VerifyCommands []string
	VerifyTimeout  time.Duration
}

// writeResult is what the window did. It is populated on every path, including a halt, so a caller
// always has the receipt.
type writeResult struct {
	Journal   Journal
	Receipt   Receipt
	Cancelled bool
	Withheld  []review.WithheldFile
	Findings  []findingWrite
	// Refusals is the protected-path refusals this window recorded, keyed on the host-computed
	// fingerprint. It is the same set as the `Refused` entries in Findings, projected into the
	// form every surface reports — carried once here so no surface re-derives it (and no surface
	// keys it on the model-authored finding id).
	Refusals []review.ApplyRefusal
	// Selection is what a narrowing selection did, on the window that reports it (SelectPrimary).
	// nil when no selection was supplied, or on a later cycle of a converging apply.
	Selection *review.ApplySelection
	// Accepted is how many reconciled decisions authorized a write AFTER any narrowing selection.
	// Zero means the window closed before a copy was ever made — there was nothing to do, which is
	// a different fact from "everything was skipped".
	Accepted int
	// Committed reports whether the LIVE commit completed. False for patch mode by construction.
	Committed bool
	// Applied counts the findings whose hunks reached the live tree (0 unless Committed).
	Applied int
	// Verification is what the operator's own commands did on the copy, before and after the edits.
	// nil when none were supplied. It is a RECORD: nothing in this window branches on it.
	Verification *review.VerificationReport
	// UndoPatch is the absolute path of the reverse-appliable patch, set ONLY for an apply into a
	// tree with no version control — the case where it is the only way back. Empty everywhere else,
	// because in a repository the answer is the VCS and naming a patch instead would be noise.
	UndoPatch string
}

// selectAccepted narrows a reconciled accepted set to the findings whose HOST-COMPUTED fingerprint
// a caller named, and reports exactly what the narrowing did.
//
// THE KEY IS `schema.Fingerprint(finding)` AND NOTHING ELSE. Not `Finding.ID`, which is
// model-authored (a model that could relabel findings could otherwise steer which one a caller's
// "apply only this" selects) and which this run renumbers after the write window anyway. Not the
// file, which is not unique. The fingerprint is derived by us from the finding's own file and
// normalized location, so the only way a model changes what a fingerprint names is by proposing a
// different finding, in the open, where a human reads it.
//
// It is a PURE FILTER over the set it is given. There is no lookup, no fallback and no fetch: a
// fingerprint that names nothing here names nothing, full stop, and lands in Unmatched. That is what
// makes "a selection can only reduce" a property of the code rather than a promise.
func selectAccepted(accepted []acceptedTarget, want []string) (review.ApplySelection, []acceptedTarget) {
	sel := review.ApplySelection{Requested: []string{}, Matched: []string{}, Unmatched: []string{}}
	// Trim and dedupe the caller's list, preserving its order. A repeated selector is a typo, not a
	// request to apply something twice, and a blank one names nothing at all.
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
	// Index the accepted set by fingerprint. Two accepted findings can in principle share one (the
	// fingerprint is the DEDUP identity), and if they do, selecting it selects both — which is the
	// honest reading of "apply the finding this names".
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
	// The narrowed set keeps the ORIGINAL ORDER of the accepted set rather than the caller's. The
	// write path's per-finding call ids and its journal are ordered by that index, and letting a
	// caller reorder the write sequence would be a capability nobody asked for.
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

// governedWrite is the one write path. See the file comment.
//
// It owns the isolated copy, the journal, the staging, the commit and the receipt; the caller owns
// the run directory, the mode ceiling and what the decision states become afterwards. On every
// return — success, halt or cancellation — the receipt has been persisted or the returned error
// says why it could not be.
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
	// finish persists the receipt. It is the ONLY thing anyone can read afterwards, so a failure to
	// write it is a terminal failure of the run rather than a logged inconvenience — and it is
	// written DURABLY, because a receipt that is only in the page cache does not answer the
	// question a crash asks.
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
	// halt records the outcome and returns the error the caller must return. A receipt that could
	// not be persisted REPLACES the fault it was recording: a caller told only about the original
	// cause would believe a receipt exists for it, and an unrecorded outcome is the worse failure.
	halt := func(status, reason string, cause error) error {
		if perr := finish(status, reason); perr != nil {
			return fault.Wrap(fault.Internal,
				fmt.Sprintf("the remediation %s (%s) could NOT be recorded — treat this run as unreconciled and inspect %s", status, reason, run.Dir),
				errors.Join(perr, cause)).WithReason(ReasonReceiptUnwritable)
		}
		return cause
	}

	// THE DECISION SET, reconciled by finding ID before anything else is done with it. An
	// unreconcilable set is refused whole: there is no safe subset of a set whose bindings are
	// unknown.
	accepted, rcerr := reconcileDecisions(req.Findings, req.Decisions, req.Accept)
	if rcerr != nil {
		return out, halt("halted", ReasonDecisionSetUnreconciled, fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: the decision set does not reconcile with its findings — %v. Decisions are bound to findings by ID, never by position; re-run review_report and remediate from that run rather than from a set that has been reordered, truncated or hand-assembled.",
			rcerr)).WithReason(ReasonDecisionSetUnreconciled))
	}

	// THE NARROWING SELECTION, applied to the reconciled set and to nothing else (D8-A, §13.3).
	//
	// It is placed HERE deliberately: after reconciliation, so a selector can only name a finding
	// whose decision binding has already been verified; and before every other check, so a run
	// narrowed to nothing is refused before a workspace is stat-ed, a copy is taken, or a model is
	// called. Selection can only REDUCE — there is no branch below that adds a finding back.
	if req.Select != nil {
		sel, narrowed := selectAccepted(accepted, req.Select)
		if req.SelectPrimary {
			out.Selection = &sel
			// The selection is recorded on the DURABLE receipt before anything can refuse, so a
			// refused selection is answerable from the run directory too.
			receipt.Selection = &sel
		}
		if len(sel.Requested) == 0 {
			return out, halt("halted", ReasonSelectionEmpty, fault.New(fault.Usage,
				"remediation refused: `select` was supplied and is empty. An empty narrowing filter does NOT mean \"apply everything\" — it names zero findings, so this call would write nothing while reading as a normal apply. Omit `select` to apply the whole accepted set, or name the host-computed fingerprints you want.").
				WithReason(ReasonSelectionEmpty))
		}
		// The matched-nothing refusal belongs to the window that is measured against the caller's
		// list. On a later cycle of a converging apply an empty match means "cycle 1 already
		// applied them", which is success, not a caller error.
		if req.SelectPrimary && len(sel.Matched) == 0 {
			return out, halt("halted", ReasonSelectionMatchedNothing, fault.New(fault.Usage, fmt.Sprintf(
				"remediation refused: none of the %d fingerprint(s) in `select` names a finding in this run's accepted set (%s). Nothing was written. A selector is the HOST-COMPUTED `fingerprint` from the accepted findings of the run being applied — never a finding's `id`, which is model-authored and renumbered.",
				len(sel.Requested), strings.Join(sel.Unmatched, ", "))).
				WithReason(ReasonSelectionMatchedNothing))
		}
		accepted = narrowed
	}

	// THE WORKSPACE, bound by identity rather than by pathname. Checked before the staleness pins,
	// because matching hashes in the WRONG tree is precisely the failure this catches.
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

	// STALENESS, pass one — only for pins that came from an EARLIER run. Checked before the copy is
	// made, before any model call, before any write: the cheapest possible place to discover that
	// the decision set describes a workspace that has moved on. It is NOT the last check; each
	// destination's content is verified again inside the commit, because everything between here and
	// there takes time. A window that records its own pins from the copy has nothing to check here —
	// its pins cannot be stale yet, which is exactly why the second check is the load-bearing one.
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
	// The out-of-band trusted roots are ENFORCED, not merely carried, when the caller supplies
	// them: the workspace must resolve inside the set in force NOW. A decision set is a durable
	// artifact and can outlive the roots its run was launched with, so the check belongs here rather
	// than only at the surface that first accepted the path.
	//
	// The operator's waiver is applied to THIS resolver too: it decides whether the workspace is an
	// acceptable write destination, so leaving it strict here would refuse the very root the waiver
	// exists to admit, while still reporting the failure as a trusted-roots problem.
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
	// Root confinement for this write: every live destination must land inside the workspace the
	// caller consented to, and must not hit a protected path. It is built HERE, from the workspace
	// this write is about, so no surface and no caller gets to supply a wider one.
	confine, serr := scope.NewWith(scope.Options{AllowProtectedWrites: req.AllowProtectedPaths}, req.Workspace)
	if serr != nil {
		return out, halt("halted", string(scope.ReasonUnresolvable),
			fault.Wrap(fault.Config, "resolve workspace root", serr).
				WithHalt(ScopeHaltClass).WithReason(string(scope.ReasonUnresolvable)))
	}
	ws.Guard = confine

	// A DIRTY TREE IS RECORDED, NEVER REFUSED. An apply into a tree holding uncommitted work used to
	// halt here. It does not any more, because the premise was wrong about who uses this: working on
	// a dirty tree is the normal state of the work, not an anomaly worth interrupting, and a refusal
	// that fires on the normal case is a flag every invocation has to carry.
	//
	// What made the refusal defensible was the claim that undoing our edits would take the user's
	// with them. That is only true of the BLUNT undo (`git checkout -- .`). A precise one has always
	// existed and is produced on this very path: `changes.patch` is a complete reverse-appliable
	// delta of everything this run wrote, so `git apply -R` undoes exactly ours and nothing else.
	// See the undo_is_this_patch event below, which already names it for the no-VCS case.
	//
	// The fact is still worth having in the record — an operator reading a run later should be able
	// to see that the tree was not clean when it was written to — so it stays as an EVENT.
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

	// THE VERIFICATION BASELINE — the project's own commands, before any edit exists.
	//
	// It runs on a THROWAWAY copy of its own, NOT on `rcopy`, and that is not fastidiousness. `rcopy`
	// is the tree the commit is derived from, and the commit writes back every file in it that differs
	// from live — so a `go build` or an `npm install` here would put object files and node_modules
	// into the user's repository. See baselineOnAThrowawayCopy. Both copies are taken from the same
	// live tree moments apart, so they are byte-identical and the comparison holds.
	verifyBefore := m.baselineOnAThrowawayCopy(ctx, run, ws, req.Workspace, req.VerifyCommands, req.VerifyTimeout)

	// absentFromCopy holds targets this window could not read a base for out of its own copy —
	// withheld for a containment reason (a hardlink), or gone between the reviewer's copy and this
	// one. See the pin block below for why they are skipped rather than halted.
	absentFromCopy := map[string]bool{}
	// canTarget is the gate a finding must pass before it can be written at all: a real,
	// non-excluded path that a reviewer was actually shown and that this window holds a base for.
	canTarget := func(f review.Finding) bool {
		return f.File != "" && !workspace.IsExcluded(f.File) && req.Shown[f.File] && !absentFromCopy[f.File]
	}

	// THE PINS, for a window that records its own. They are read from the ISOLATED COPY, which is
	// byte-identical to the live tree at the moment it was taken and is the very base every edit
	// below is derived from. Reading them from the live tree instead would leave a gap: a save that
	// landed between the copy and the pin would be adopted as the base and then silently
	// overwritten. Reading them from the copy closes that by construction.
	if req.PinsFromCopy {
		rels := make([]string, 0, len(accepted))
		for _, t := range accepted {
			if canTarget(t.finding) {
				rels = append(rels, t.finding.File)
			}
		}
		pins = BaseHashes(rcopy.Root, rels)
		// A target the copy does not hold is not a writable target, and it is a SKIP rather than an
		// authorization halt. The distinction is real: a request that pins a file the tree does not
		// have is a claim contradicted by disk (a halt — see authorizeEditTarget), while a file
		// this window simply could not copy is a caveat the run has already surfaced. Dropping the
		// key rather than keeping an `absent` pin also keeps the commit's pin set exhaustive over
		// what is actually being written.
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

	// PASS 1 — compute every intended hunk. Nothing is written in this pass, so the journal below
	// is a complete statement of intent rather than a running commentary on what already happened.
	type plannedEdit struct {
		finding review.Finding
		edits   []review.Edit
		origin  string
	}
	var planned []plannedEdit
	for _, t := range accepted {
		f := t.finding
		// Root confinement + the protected-path denylist, BEFORE the skip rules. Two refusals
		// come back from here and they are answered DIFFERENTLY, which is D8-C:
		//
		//   - PROTECTED PATH (the non-overridable write denylist, inside the consented root) is a
		//     RECORDED REFUSAL. Nothing is written there — that part is unchanged and
		//     non-negotiable — but the remaining findings proceed. The original rule made this a
		//     halt, and the objection it answered was to a SILENT skip letting a run report
		//     success; a refusal that is named in the receipt, counted in the summary, rendered
		//     first in the text and carried in the run's coarse not-clean signal is not silent.
		//     What the halt actually cost was every OTHER finding in a paid run — and, worse, a
		//     caller could only learn WHICH finding was protected by paying for the run that told
		//     them.
		//   - ANYTHING ELSE — a target resolving outside the consented root, an unresolvable path,
		//     a resolver with no roots — still HALTS. Those are escapes, not policy; the denylist
		//     held in the first case and did not even get to speak in the others.
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
				// The FINGERPRINT, not the id: it is host-computed from the finding's own
				// content, so a model cannot relabel a finding to steer which one a caller's
				// follow-up selection names — and unlike the id it survives this run's own
				// post-write renumbering.
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
			// EVERY EDIT TARGET IS JUDGED — and scope is only one of the questions. See
			// authorizeEditTarget: confinement answers WHERE a write may land, never WHAT WAS
			// REVIEWED.
			if terr := authorizeEditTarget(f, e, req.Shown, pins); terr != nil {
				return out, halt("halted", fault.ReasonOf(terr), terr)
			}
			// STILL A HALT HERE, and deliberately so. `authorizeEditTarget` above has already
			// established that `e.File == f.File`, and `f.File` passed the finding-level
			// authorization at the top of this loop — so the DENYLIST branch is unreachable at
			// this point, and D8-C's recorded-refusal answer has nothing to apply to. What can
			// still fire is a target that resolves outside the root between the two checks, which
			// is an escape and halts on both.
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

	// THE JOURNAL IS A PRECONDITION, not a side effect. It is made durable BEFORE the first edit,
	// and a journal that cannot be written HALTS: the contract this path advertises is
	// "journal before write", and a full or read-only disk must therefore make the write
	// unreachable rather than make the journal optional.
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

	// WRITE-WINDOW BOUNDARY. Cancellation is honored HERE and at the commit, and nowhere in
	// between: a cancellation observed mid-hunk would leave the copy in a state nobody journaled.
	if ctx.Err() != nil {
		_, _, _ = ws.Discard(rcopy)
		out.Cancelled = true
		_ = run.Event(m.now(), "warn", "remediation_completed", "cancelled before the write window opened; nothing was written", nil)
		return out, halt("cancelled", ReasonRemediateCancelled,
			fault.Wrap(fault.Policy, "remediation cancelled before any write", ctx.Err()).
				WithHalt("F").WithReason(ReasonRemediateCancelled))
	}

	// PASS 2 — apply to the COPY. The live tree is still untouched: only Commit reaches it.
	//
	// A FINDING IS ALL-OR-NOTHING. Its hunks are staged first, so a finding whose second hunk
	// fails does not leave its first hunk in the copy — the copy is what Commit ships, so a
	// half-applied finding recorded as "not applied" would be a receipt stating the opposite of
	// what shipped. Per-FINDING staging is chosen over aborting the whole remediation because the
	// finding is the unit the receipt reports and the unit a human accepted: one finding whose
	// anchor drifted should not discard the others, and with staging that choice costs no
	// honesty. A rollback that itself fails is a different matter and halts the run: at that
	// point the copy matches no recorded intent.
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

	// A DIFF FAILURE IS A HALT. What would be committed is derived from it; a dropped error here
	// would produce a receipt whose file list is silently empty for a run that wrote.
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
		// THE UNDO, for a tree that has no other one.
		//
		// `changes.patch` is a complete reverse-appliable delta of everything this run wrote, and it
		// is produced on the APPLY path as well as the patch path — so an undo has always existed
		// here. In a repository nobody needs it: `git checkout` is the obvious move. In a tree under
		// NO version control it is the only way back, and it was neither stated nor reliably durable.
		//
		// It is named here, on the record, rather than left for someone to deduce from an artifact
		// listing. The durability half is handled before the run starts (see EnsureDurableRunDir):
		// this same tree is the one whose run directory would otherwise default to the OS temp
		// directory, which is precisely the wrong place to keep the only copy of an undo.
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
		// THE SECOND BOUNDARY, decided once under a lock: cancelled, or committing, never both.
		var window writeWindow
		if !window.enter(ctx) {
			_, _, _ = ws.Discard(rcopy)
			out.Cancelled = true
			_ = run.Event(m.now(), "warn", "remediation_completed", "cancelled before commit; the live workspace is unchanged", nil)
			return out, halt("cancelled", ReasonRemediateCancelled,
				fault.Wrap(fault.Policy, "remediation cancelled before apply", ctx.Err()).
					WithHalt("F").WithReason(ReasonRemediateCancelled))
		}
		// The window is open. From here the context no longer decides this call's disposition —
		// see writeWindow — and the marker below makes the fact durable BEFORE the first live
		// write, so a crash leaves a run directory that says "a commit was entered" rather than
		// one that is silent about it.
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
		// STALENESS, pass two — inside the commit, per destination, immediately before each
		// replacement. The pins are the very base this write was decided against, so a file edited
		// during the model calls above is refused here even though its inode never changed (an
		// in-place save keeps it) and even though the cheap check above passed minutes ago.
		//
		// THIS IS THE ONLY LIVE-WORKSPACE COMMIT IN THE PRODUCT. Every surface reaches it.
		committed, commitErr := ws.CommitExpecting(rcopy, pins)
		if commitErr != nil {
			_, _, _ = ws.Discard(rcopy)
			// The commit rolled back, so NOTHING reached the live tree: every planned finding is
			// recorded as not applied, and the receipt says so rather than describing the copy.
			for _, r := range results {
				receipt.NotApplied = append(receipt.NotApplied, AppliedFinding{
					FindingID: r.finding.ID, File: r.finding.File,
					State: string(review.StateReportedValid), Reason: ReasonApplyCommitFailed,
				})
				out.Findings = append(out.Findings, findingWrite{
					FindingID: r.finding.ID, File: r.finding.File, Reason: ReasonApplyCommitFailed,
				})
			}
			// A destination that changed under us — by content, by identity, or by turning up with
			// no recorded base at all — is the SAME event the pre-window staleness check exists to
			// catch, discovered later. It is reported with the same reason code so a caller
			// branches on one thing, and it is the one commit failure that is a POLICY refusal
			// rather than an internal error: nothing malfunctioned, the world moved.
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
		// THE RECEIPT NOW DESCRIBES THE COMMITTED REALITY, and only it: the applied set, the file
		// list and `committed` are all derived from the commit that succeeded, never from the copy
		// that preceded it.
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
		// PATCH MODE. Nothing reached the live tree by construction, so the "applied" set is the
		// set that reached the DIFF, and the file list is the diff's.
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

	// THE SECOND VERIFICATION PASS — on the same copy, now carrying every applied edit.
	//
	// IT RUNS AFTER THE COMMIT, and that ordering is load-bearing twice over.
	//
	// First, it is what stops this feature writing into someone's repository. `CommitExpecting`
	// re-enumerates the WHOLE copy and writes back every file that differs from live — so a `go
	// build` or an `npm test` run before the commit would have its object files, binaries and
	// coverage directories committed into the user's tree. A tool that reviews code must not leave
	// build output behind, and no denylist would have caught it reliably, because build artifacts
	// look exactly like source.
	//
	// Second, it makes "a failing suite never blocks a commit" STRUCTURAL rather than a promise
	// somebody has to keep. There is no branch to get wrong: by the time this runs, the commit is
	// already done. Nothing was lost by moving it — the commit does not modify the copy, so this
	// measures precisely the tree the edits produced.
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

// eventVerification records one pass's per-command outcome. The OUTPUT is deliberately not in the
// event data: it is already in verification.json, and a multi-kilobyte build log inside an event line
// makes the event stream unreadable for every other purpose it serves.
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
