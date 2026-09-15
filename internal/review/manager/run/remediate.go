package run

// Remediate applies an already-adjudicated decision set from an earlier report run, with no reviewers
// and no re-adjudication, so exactly the set that was inspected is written (see docs/mcp.md). All
// writing goes through governedWrite, which reconciles decisions by finding id, binds the workspace by
// identity, checks base hashes before the write window and again inside the commit, journals durably
// before the first edit, stages each finding all-or-nothing, and always persists a receipt.
//
// There is no exclusive workspace lock. A writer landing between the commit's final content check and
// its replace is not detected, and a crash between commit and receipt leaves remediation/journal.json
// and remediation/commit-attempt.json without a receipt, which identifies an interrupted write window.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/rootfile"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// Reason codes for the fromRun remediation path.
const (
	// ReasonStaleDecisionSet — a file the decision set targets changed between the report run and
	// this apply, so the decisions describe a different workspace.
	ReasonStaleDecisionSet = "stale_decision_set"
	// ReasonRemediateModeInvalid — the requested output mode is not patch/apply.
	ReasonRemediateModeInvalid = "remediate_mode_invalid"
	// ReasonRemediateModeRefused — the config ceiling for this surface refused the requested mode.
	ReasonRemediateModeRefused = "remediate_mode_refused"
	// ReasonRemediateCancelled — the call was cancelled at a write-window boundary; nothing was
	// committed.
	ReasonRemediateCancelled = "remediate_cancelled"
	// ReasonDecisionSetUnreconciled — the findings and the decisions do not reconcile by ID, so
	// which decision authorizes which finding is not knowable.
	ReasonDecisionSetUnreconciled = "decision_set_unreconciled"
	// ReasonWorkspaceUnbound — the request carries no canonical identity for the reviewed root, so
	// "is this the tree that was reviewed" cannot be answered.
	ReasonWorkspaceUnbound = "remediation_workspace_unbound"
	// ReasonWorkspaceIdentityChanged — the path names a different object than the reviewed root.
	ReasonWorkspaceIdentityChanged = "remediation_workspace_identity_changed"
	// ReasonEditOutsideFinding — a proposed edit targets a file its finding does not name.
	ReasonEditOutsideFinding = "remediation_edit_outside_finding"
	// ReasonEditTargetUnreviewed — a proposed edit targets a file that was never shown to a
	// reviewer, or that carries no base hash from the run that judged it.
	ReasonEditTargetUnreviewed = "remediation_edit_target_unreviewed"
	// ReasonJournalUnwritable — the write journal could not be made durable, so the write window
	// must not open.
	ReasonJournalUnwritable = "remediation_journal_unwritable"
	// ReasonReceiptUnwritable — the receipt could not be persisted. It is terminal: the receipt is
	// the only thing anyone can read afterwards.
	ReasonReceiptUnwritable = "remediation_receipt_unwritable"
	// ReasonPatchUnwritable — the patch artifact or its summary could not be written, so the
	// receipt cannot honestly reference it.
	ReasonPatchUnwritable = "remediation_patch_unwritable"
	// ReasonDiffFailed — the copy could not be diffed against the live tree, so what would be
	// committed is not knowable.
	ReasonDiffFailed = "remediation_diff_failed"
	// ReasonStageRollbackFailed — a finding's partially applied hunks could not be rolled back out
	// of the copy, so the copy matches no recorded intent.
	ReasonStageRollbackFailed = "remediation_stage_rollback_failed"
	// ReasonApplyCommitFailed — the commit to the live workspace failed (and was rolled back).
	ReasonApplyCommitFailed = "apply_commit_failed"
)

// BaseHashAbsent is the recorded digest of a file that did not exist when the decision set was
// captured, so a file that appears later is a mismatch. It is meshcore's pin sentinel, shared with
// workspace.CommitExpecting.
const BaseHashAbsent = workspace.ContentAbsent

// IntendedHunk is one write-journal entry: an edit about to be attempted, recorded before any write.
type IntendedHunk struct {
	FindingID string `json:"findingId"`
	File      string `json:"file"`
	// Anchor is the edit's anchor text ("" = prepend), Occurrence which match it targets.
	Anchor     string `json:"anchor,omitempty"`
	Occurrence int    `json:"occurrence,omitempty"`
	// Bytes is the size of the replacement text; Origin is "model" (an author_remediator-authored
	// anchored fix) or "marker" (the deterministic engine fallback).
	Bytes  int    `json:"bytes"`
	Origin string `json:"origin"`
	// ReplacementSHA256 is the digest of the replacement text as proposed, before line-ending
	// adaptation. The journal records the digest, not the text.
	ReplacementSHA256 string `json:"replacementSha256"`
}

// Journal is the complete pre-write record for one remediation.
type Journal struct {
	SchemaVersion int            `json:"schemaVersion"`
	RunID         string         `json:"runId"`
	SourceRunID   string         `json:"sourceRunId,omitempty"`
	Mode          string         `json:"mode"`
	Hunks         []IntendedHunk `json:"hunks"`
	// IntentSHA256 is a digest over the whole ordered hunk list; the receipt repeats it to tie the
	// two together.
	IntentSHA256 string `json:"intentSha256"`
}

// hunkDigest returns a replacement's digest in meshcore's sha256:<hex> pin format.
func hunkDigest(s string) string { return workspace.ContentPin([]byte(s)) }

// intentDigest hashes each hunk's length-prefixed JSON encoding in order; order is part of the intent.
func intentDigest(hs []IntendedHunk) string {
	h := sha256.New()
	for _, k := range hs {
		// Marshal cannot fail for these fields; the fallback keeps a future field from producing a
		// digest over nothing.
		b, err := json.Marshal(k)
		if err != nil {
			b = []byte(fmt.Sprintf("unencodable:%+v", k))
		}
		fmt.Fprintf(h, "%d:", len(b))
		h.Write(b)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// AppliedFinding is one finding's write outcome.
type AppliedFinding struct {
	FindingID string `json:"findingId"`
	File      string `json:"file,omitempty"`
	// State is the finding's terminal decision state (applied | reported_valid | skipped).
	State string `json:"state"`
	// Reason is the reason code when the finding was not applied.
	Reason string `json:"reason,omitempty"`
}

// Receipt is the durable record of what a call wrote, persisted on every path including
// cancellation.
type Receipt struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         string `json:"runId"`
	SourceRunID   string `json:"sourceRunId,omitempty"`
	Mode          string `json:"mode"`
	// Status is complete | halted | cancelled.
	Status     string `json:"status"`
	ReasonCode string `json:"reasonCode,omitempty"`
	// BaseHashesVerified is how many files' base hashes were re-verified before the window opened.
	BaseHashesVerified int              `json:"baseHashesVerified"`
	Intended           []IntendedHunk   `json:"intended"`
	Applied            []AppliedFinding `json:"applied"`
	NotApplied         []AppliedFinding `json:"notApplied"`
	Files              []string         `json:"files"`
	// IntentSHA256 repeats the journal's whole-intent digest, tying receipt and journal by value.
	IntentSHA256 string `json:"intentSha256,omitempty"`
	// PatchArtifact is the run-relative patch path (empty when nothing changed), and PatchSHA256 its
	// digest.
	PatchArtifact string `json:"patchArtifact,omitempty"`
	PatchSHA256   string `json:"patchSha256,omitempty"`
	// CommitAttempted reports whether the live commit was entered, distinguishing a rolled-back or
	// interrupted attempt from no attempt. It pairs with remediation/commit-attempt.json.
	CommitAttempted bool `json:"commitAttempted"`
	// Committed reports whether the live commit succeeded: false in patch mode, on cancellation and
	// on rollback. Files lists what it wrote, which is empty when nothing differed.
	Committed bool `json:"committed"`
	// Selection is the narrowing selection this window was given, recorded so it can be read from
	// the run directory even if the response was lost.
	Selection *review.ApplySelection `json:"selection,omitempty"`
}

// RemediateRequest is one from-run remediation.
type RemediateRequest struct {
	// Workspace is the live workspace the accepted findings were adjudicated against.
	Workspace string
	// WorkspaceIdentity is the root's canonical identity as captured by the report run. It is
	// required; an unbound request halts.
	WorkspaceIdentity WorkspaceIdentity
	// Mode must be ModePatch or ModeApply.
	Mode    review.Mode
	Surface string
	Profile string
	// ReviewerPanel is carried only so the plan resolves the report run's author_remediator lane; no
	// reviewer seat runs.
	ReviewerPanel []review.SeatSpec
	// ComposedRoles are the source run's composed role seats, carried for the same reason.
	ComposedRoles map[review.Role]review.SeatSpec
	// TrustedRoots are an agent surface's roots (see Request.TrustedRoots); the workspace must resolve
	// inside the roots in force at apply time.
	TrustedRoots []string
	// SourceRunID identifies the report run whose decision set this applies.
	SourceRunID string
	// Findings and Decisions are the adjudicated set, index-aligned as in RunOutcome; nothing is
	// re-judged.
	Findings  []review.Finding
	Decisions []review.Decision
	// Select narrows the accepted set by host-computed fingerprint; see writeRequest.Select.
	Select []string
	// Shown is the set of files reviewers were shown; a finding about any other file is never applied.
	Shown map[string]bool
	// BaseHashes maps each path to its digest as captured with the decision set.
	BaseHashes map[string]string
	// OnJournal, when set, receives the journal once it is durable and before the first edit. It must
	// not block or panic.
	OnJournal func(Journal)
	// VerifyCommands and VerifyTimeout are the operator's build/test commands, run on the containment
	// copy before and after the edits and recorded. See verify.go.
	VerifyCommands []string
	VerifyTimeout  time.Duration
	// AllowProtectedPaths waives the protected-config half of containment, never secrets; see
	// Request.AllowProtectedPaths. A fromRun apply needs it whenever its report run had it.
	AllowProtectedPaths bool
	OnEvent             func(audit.EventLine)
	// Trace is the apply turn's own W3C trace context, not the report run's; nil records nothing.
	Trace *review.Trace
}

// RemediateOutcome is the result of a fromRun remediation, populated on every path including a halt.
type RemediateOutcome struct {
	RunID       string
	RunDir      string
	SourceRunID string
	Mode        review.Mode
	Journal     Journal
	Receipt     Receipt
	Cancelled   bool
	Withheld    []review.WithheldFile
	// Selection is what a narrowing select did; nil when none was supplied. It is set on refusals too.
	Selection *review.ApplySelection
	// Refusals lists findings refused because they target a protected path; nothing was written to
	// them. A non-empty list makes the call a partial refusal.
	Refusals []review.ApplyRefusal
	// Verification is what the operator's commands did before and after the edits; nil when none were
	// supplied. Nothing branches on it.
	Verification *review.VerificationReport
}

// Applied is how many findings reached the live tree (apply) or the diff (patch), counted from the
// receipt.
func (o RemediateOutcome) Applied() int { return len(o.Receipt.Applied) }

// Refused is how many findings were refused for a protected path.
func (o RemediateOutcome) Refused() int { return len(o.Refusals) }

// Outcome is the write-outcome discriminator, derived by review.ApplyOutcome as on every surface.
func (o RemediateOutcome) Outcome() string {
	return review.ApplyOutcome(o.Applied(), o.Refused())
}

// BaseHashes digests the given workspace-relative files as they currently exist, reading through rootfile.
// An unreadable file is recorded as BaseHashAbsent, so becoming readable later counts as a change.
func BaseHashes(workspaceRoot string, rels []string) map[string]string {
	out := make(map[string]string, len(rels))
	for _, rel := range rels {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		if _, done := out[rel]; done {
			continue
		}
		b, err := rootfile.ReadUnder(workspaceRoot, rel)
		if err != nil {
			out[rel] = BaseHashAbsent
			continue
		}
		sum := sha256.Sum256(b)
		out[rel] = "sha256:" + hex.EncodeToString(sum[:])
	}
	return out
}

// StaleBaseHashes returns, in stable order, the files whose current digest differs from the one
// captured with the decision set. An empty result is the positive statement that every targeted
// file is byte-identical to what was reviewed.
func StaleBaseHashes(workspaceRoot string, want map[string]string) []string {
	rels := make([]string, 0, len(want))
	for rel := range want {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	now := BaseHashes(workspaceRoot, rels)
	var stale []string
	for _, rel := range rels {
		if now[rel] != want[rel] {
			stale = append(stale, rel)
		}
	}
	return stale
}

// AcceptedForApply reports whether a decision is in the accepted set a fromRun remediation may write:
// valid, not refused by the write-path rule, and reported_valid, the state report mode gives every
// actionable finding.
func AcceptedForApply(d review.Decision) bool {
	if !d.Valid || d.ApplyRefusalReason != "" {
		return false
	}
	if d.Applyable != nil && !*d.Applyable {
		return false
	}
	return d.State == review.StateReportedValid
}

// --- workspace identity ---

// WorkspaceIdentity is a reviewed root's identity, captured by the report run and re-verified before
// applying. Binding by filesystem identity as well as canonical path stops a different tree with
// identical target files from being written under the same path.
type WorkspaceIdentity struct {
	// Info is os.Stat's report for the root at capture time.
	Info fs.FileInfo
	// Key is the durable identity (device and inode on Unix, volume serial and file index on Windows),
	// resolved eagerly by rootIdentityKey. It is compared instead of os.SameFile because on Windows
	// SameFile lazily reopens by path and would compare a substitute against itself. It is empty when
	// the platform cannot express one.
	Key string
	// Canonical is the symlink-resolved absolute path at capture time; matching it as well as Key
	// distinguishes the same directory from a new one that reused the inode.
	Canonical string
}

// Bound reports whether an identity was captured.
func (w WorkspaceIdentity) Bound() bool { return w.Info != nil && w.Canonical != "" }

// CaptureWorkspaceIdentity records a workspace root's identity.
func CaptureWorkspaceIdentity(root string) (WorkspaceIdentity, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return WorkspaceIdentity{}, fmt.Errorf("resolve workspace root %q: %w", root, err)
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return WorkspaceIdentity{}, fmt.Errorf("canonicalize workspace root %q: %w", root, err)
	}
	fi, err := os.Stat(canon)
	if err != nil {
		return WorkspaceIdentity{}, fmt.Errorf("stat workspace root %q: %w", canon, err)
	}
	return WorkspaceIdentity{Info: fi, Key: rootIdentityKey(canon, fi), Canonical: canon}, nil
}

// verifyAgainst returns an error unless root still names the captured object. It compares durable keys
// when both captures have one and falls back to os.SameFile (see WorkspaceIdentity.Key).
func (w WorkspaceIdentity) verifyAgainst(root string) error {
	now, err := CaptureWorkspaceIdentity(root)
	if err != nil {
		return fmt.Errorf("the reviewed workspace root cannot be resolved now: %w", err)
	}
	sameObject := os.SameFile(w.Info, now.Info)
	if w.Key != "" && now.Key != "" {
		sameObject = w.Key == now.Key
	}
	if !sameObject {
		return fmt.Errorf("%q no longer names the directory that was reviewed (it names a different filesystem object now)", root)
	}
	if !sameCanonicalPath(w.Canonical, now.Canonical) {
		return fmt.Errorf("%q resolves to %q, but the review resolved it to %q", root, now.Canonical, w.Canonical)
	}
	return nil
}

// foldPaths is true on Windows, whose filesystems are case-insensitive. It is a variable so tests can
// exercise the Windows branch on any platform.
var foldPaths = runtime.GOOS == "windows"

// sameCanonicalPath compares two canonical paths, folding case only on Windows.
func sameCanonicalPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if foldPaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// --- decision-set reconciliation ---

// acceptedTarget is one accepted finding, the decision that accepted it, and its original index, which
// keeps per-finding call ids stable.
type acceptedTarget struct {
	finding  review.Finding
	decision review.Decision
	index    int
}

// reconcileDecisions pairs decisions to findings by finding id and returns the pairs accept
// authorizes. Positional pairing would silently authorize the wrong finding if a stored set were
// reordered or truncated, so a set that does not reconcile is refused whole. A full cycle runs it
// too, after uniqueFindingIDs, because lane-supplied ids can repeat.
func reconcileDecisions(findings []review.Finding, decisions []review.Decision, accept func(review.Decision) bool) ([]acceptedTarget, error) {
	if len(findings) != len(decisions) {
		return nil, fmt.Errorf("%d finding(s) but %d decision(s): the two arrays must correspond one-to-one",
			len(findings), len(decisions))
	}
	seen := make(map[string]bool, len(findings))
	var accepted []acceptedTarget
	for i, f := range findings {
		id := strings.TrimSpace(f.ID)
		if id == "" {
			return nil, fmt.Errorf("finding at position %d has no id, so no decision can be bound to it", i)
		}
		if seen[id] {
			return nil, fmt.Errorf("finding id %q appears more than once, so a decision naming it is ambiguous", id)
		}
		seen[id] = true
		d := decisions[i]
		if strings.TrimSpace(d.FindingID) != id {
			return nil, fmt.Errorf("the decision at position %d is bound to finding %q, but position %d carries finding %q",
				i, d.FindingID, i, id)
		}
		if accept(d) {
			accepted = append(accepted, acceptedTarget{finding: f, decision: d, index: i})
		}
	}
	return accepted, nil
}

// uniqueFindingIDs makes every finding id present and unique in place, and rebinds each decision to its
// finding's id. Lane-supplied ids can collide after merging, and ids authorize writes. Ids that are
// already unique are unchanged, because the remediation marker quotes them.
func uniqueFindingIDs(findings []review.Finding, decisions []review.Decision) {
	seen := make(map[string]bool, len(findings))
	for i := range findings {
		id := strings.TrimSpace(findings[i].ID)
		if id == "" || seen[id] {
			base := id
			if base == "" {
				base = "finding"
			}
			for n := 2; ; n++ {
				cand := fmt.Sprintf("%s#%d", base, n)
				if !seen[cand] {
					id = cand
					break
				}
			}
		}
		seen[id] = true
		findings[i].ID = id
		if i < len(decisions) {
			decisions[i].FindingID = id
		}
	}
}

// --- durability ---

// writeDurableJSON writes v as JSON to a run-relative path durably; see writeDurable. Unlike
// audit.Run's best-effort writers, the journal and receipt must be able to fail and must be on disk
// before the run proceeds.
func writeDurableJSON(runDir, rel string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(runDir, rel, append(b, '\n'))
}

// writeDurable writes data to a run-relative path and fsyncs the file. The parent directory is also
// synced where the platform allows it; Windows cannot sync a directory handle.
func writeDurable(runDir, rel string, data []byte) error {
	p := filepath.Join(runDir, filepath.FromSlash(rel))
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync() // best effort: not supported on every platform
		d.Close()
	}
	return nil
}

// --- the write window ---

// writeWindow makes cancellation and entering the live commit mutually exclusive, decided once under
// a lock. After enter returns true the context is never consulted again, so an entered commit, which
// is deliberately uncancellable, cannot be reported as cancelled. The write path runs on one goroutine
// today; the lock gives any future cancellation observer a single place to synchronize.
type writeWindow struct {
	mu      sync.Mutex
	entered bool
}

// enter reports whether the commit may begin. It returns false exactly once cancellation won.
func (w *writeWindow) enter(ctx context.Context) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.entered {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	w.entered = true
	return true
}

// testHookInsideWriteWindow runs after the write window is entered and before the live commit, so a
// test can inject a late cancellation deterministically. It is nil outside tests.
var testHookInsideWriteWindow func()

// Remediate applies an already-adjudicated decision set to the workspace. See the file comment.
func (m *Manager) Remediate(ctx context.Context, req RemediateRequest) (RemediateOutcome, error) {
	var out RemediateOutcome
	out.SourceRunID, out.Mode = req.SourceRunID, req.Mode

	if req.Mode != review.ModePatch && req.Mode != review.ModeApply {
		return out, fault.New(fault.Usage, fmt.Sprintf(
			"remediation mode %q is not writable — use %q (emit a diff) or %q (write the workspace); %q has nothing to remediate",
			req.Mode, review.ModePatch, review.ModeApply, review.ModeReport)).
			WithReason(ReasonRemediateModeInvalid)
	}

	available := make(map[string]bool, len(m.Adapters))
	for name, a := range m.Adapters {
		ok, _ := a.Available()
		available[name] = ok
	}
	plan, err := m.Cfg.Resolve(config.ResolveRequest{
		Profile: req.Profile, Mode: req.Mode, Surface: req.Surface,
		Available: available, ReviewerPanel: req.ReviewerPanel, ComposedRoles: req.ComposedRoles,
	})
	if err != nil {
		return out, err
	}
	// The mode ceiling is re-resolved here rather than trusted from the surface.
	if plan.Mode != req.Mode {
		return out, fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: the %q surface's write-authority ceiling resolves %q down to %q — grant the capability in config (`surfaces.capabilitiesBySurface.%s: [%s]`) rather than expecting the ceiling to be bypassed",
			req.Surface, req.Mode, plan.Mode, req.Surface, config.CapabilityAllowRemediate)).
			WithReason(ReasonRemediateModeRefused)
	}

	startedAt := m.now()
	run, rerr := audit.NewRun(m.artifactDirFor(req.Workspace), "", startedAt)
	if rerr != nil {
		return out, fault.Wrap(fault.Internal, "create run dir", rerr).WithReason("remediation_run_dir_failed")
	}
	run.OnEvent = req.OnEvent
	out.RunID, out.RunDir = run.ID, run.Dir
	_ = run.WriteJSON("resolved-plan.json", plan)
	_ = run.Event(startedAt, "info", "remediation_started", "from-run remediation started", map[string]any{
		"mode": string(req.Mode), "sourceRunId": req.SourceRunID, "findings": len(req.Findings),
	})

	// governedWrite does all writing (see writepath.go). This path supplies pins from the report run,
	// accepts its reported_valid set, and enforces the trusted roots in force at apply time.
	res, werr := m.governedWrite(ctx, run, writeRequest{
		Workspace: req.Workspace, Identity: req.WorkspaceIdentity,
		Mode: req.Mode, Plan: plan, SourceRunID: req.SourceRunID,
		Findings: req.Findings, Decisions: req.Decisions, Accept: AcceptedForApply,
		// A fromRun remediation has a single write window, so it is always primary.
		Select: req.Select, SelectPrimary: true,
		Shown: req.Shown, Pins: req.BaseHashes, TrustedRoots: req.TrustedRoots,
		OnJournal:      req.OnJournal,
		VerifyCommands: req.VerifyCommands, VerifyTimeout: req.VerifyTimeout,
		AllowProtectedPaths: req.AllowProtectedPaths,
	})
	out.Journal, out.Receipt, out.Cancelled = res.Journal, res.Receipt, res.Cancelled
	out.Verification = res.Verification
	out.Withheld = appendWithheld(out.Withheld, res.Withheld...)
	out.Refusals = res.Refusals
	out.Selection = res.Selection
	if werr != nil {
		return out, werr
	}
	if res.Accepted == 0 {
		return out, nil // nothing was accepted; governedWrite already recorded the receipt
	}
	_ = run.Event(m.now(), "info", "run_completed", "from-run remediation complete", map[string]any{
		"applied": out.Applied(), "refused": out.Refused(), "outcome": out.Outcome(),
		"files": len(out.Receipt.Files), "committed": out.Receipt.Committed,
	})
	// Record outcome and refusals in run-state too, so they can be read without the response.
	runState := map[string]any{
		"schemaVersion": 1, "runId": run.ID, "kind": "remediation", "sourceRunId": req.SourceRunID,
		"mode": string(req.Mode), "status": out.Receipt.Status, "committed": out.Receipt.Committed,
		"outcome": out.Outcome(), "applied": out.Applied(), "refused": out.Refused(),
		"startedAt": startedAt.UTC().Format(time.RFC3339), "completedAt": m.now().UTC().Format(time.RFC3339),
	}
	if out.Selection.Selective() {
		runState["selection"] = out.Selection
	}
	// The apply turn's own trace context, carried verbatim. Absent when the caller sent none.
	if req.Trace != nil {
		runState["trace"] = req.Trace
	}
	_ = run.WriteJSON("run-state.json", runState)
	return out, nil
}

// authorizeEditTarget checks what root confinement cannot: whether a model-proposed edit targets what
// was reviewed. The edit must target its finding's own file, a file reviewers were shown, and a file
// with a real base-hash pin; cross-file edits are not supported. Every failure halts rather than
// skips, since a proposal that reached outside its authorization cannot be partly trusted.
func authorizeEditTarget(f review.Finding, e review.Edit, shown map[string]bool, pins map[string]string) error {
	if e.File != f.File {
		return fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: finding %s is about %q, but a proposed edit targets %q. An accepted finding authorizes edits to the file it was adjudicated about and to no other — %q was never shown to a reviewer under this finding, never judged, and carries no base hash from the run that judged it.",
			f.ID, f.File, e.File, e.File)).WithHalt(ScopeHaltClass).WithReason(ReasonEditOutsideFinding)
	}
	if !shown[e.File] {
		return fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: a proposed edit targets %q, which no reviewer was shown. A finding about a file nobody saw is never applied, and neither is an edit to one.",
			e.File)).WithHalt(ScopeHaltClass).WithReason(ReasonEditTargetUnreviewed)
	}
	if pin, ok := pins[e.File]; !ok || pin == BaseHashAbsent {
		return fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: a proposed edit targets %q, for which the review recorded no base content (it was not among the files the accepted set pinned, or it did not exist when the review ran). Nothing can verify that this file is still what was judged — neither before the write window nor inside the commit.",
			e.File)).WithHalt(ScopeHaltClass).WithReason(ReasonEditTargetUnreviewed)
	}
	return nil
}

// isDestinationDrift reports whether a commit refusal means the live destination is not what this
// write was decided against: its content changed, it was swapped, or it has no recorded base.
func isDestinationDrift(r workspace.Reason) bool {
	switch r {
	case workspace.ReasonDestinationContentChanged,
		workspace.ReasonDestinationChanged,
		workspace.ReasonDestinationUnpinned:
		return true
	}
	return false
}

// changedFiles projects a diff's changes to their paths.
func changedFiles(changes []workspace.FileChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.File)
	}
	return out
}
