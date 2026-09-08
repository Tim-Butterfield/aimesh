package run

// FROM-RUN REMEDIATION — applying an ALREADY-ADJUDICATED decision set.
//
// `RunContext` runs the whole governed cycle and, in patch/apply mode, writes at the end of it.
// This file is the OTHER half of the two-phase remediation design (see docs/mcp.md, the
// `review_remediate` double opt-in): a prior REPORT run paid for the panel and the host adjudication, its accepted
// set exists as an inspectable artifact, and this path applies exactly that set — no reviewers, no
// re-adjudication, no second opinion that could differ from the one the human read.
//
// Six properties make that safe, and all six live here rather than in a surface:
//
//  1. THE DECISION SET IS RECONCILED BY IDENTITY. Findings and decisions are paired by finding ID,
//     not by array position, and a set that does not reconcile (different lengths, a duplicate or
//     empty id, a decision bound to a different finding) is a halt. Positional pairing silently
//     authorizes the wrong finding for any record that was reordered, truncated or migrated.
//  2. THE WORKSPACE IS BOUND BY IDENTITY, not by pathname. The reviewed root's device+inode and
//     canonical path are captured by the run that produced the decision set and must still name the
//     same object here. A path is not a repository: re-pointing it at a different tree whose
//     targeted files happen to hash the same would otherwise pass every other check.
//  3. STALENESS IS ENFORCED — TWICE. The base hashes captured when the decision set was produced are
//     re-verified against the live workspace before anything is written, AND each destination's
//     content is re-verified against the same pin inside the commit window, immediately before it is
//     replaced. The first check is cheap and catches the ordinary case; the second is what survives
//     the minutes a model call can take (see workspace.CommitExpecting for the residue that no
//     lock-free design can close).
//  4. THE WRITE WINDOW IS JOURNALED, DURABLY, FIRST. Every intended hunk — including a digest of its
//     replacement text and a digest of the whole intent — is computed and PERSISTED (fsynced) before
//     the first edit is applied. A journal that cannot be written is a HALT: the promise is
//     "journal before write", so a write with no journal must be unreachable rather than merely
//     unlikely. Cancellation is honored only at the window's boundaries; once the commit is entered
//     it is uncancellable BY CONSTRUCTION (see writeWindow), so "cancelled" and "wrote something"
//     are mutually exclusive answers.
//  5. AN APPLIED FINDING IS ALL-OR-NOTHING. A finding's hunks are staged: if any hunk fails, the
//     finding's earlier hunks are rolled back out of the copy, so a finding the receipt reports as
//     NOT applied has nothing of it in the committed bytes.
//  6. THE RECEIPT ALWAYS EXISTS, AND IT REPORTS THE COMMITTED REALITY. It is written to the run
//     directory on every path — success, halt, cancellation, nothing-to-do — and its applied set,
//     its file list and its `committed` flag are derived ONLY from a commit that actually succeeded.
//     A receipt that could not be persisted is a terminal failure of the run: an unrecorded write is
//     worse than a refused one.
//
// The write-layer rules are unchanged and are not re-implemented: the same `scope.Resolver` guard,
// the same protected-path denylist, the same isolated copy, the same commit. This path can only
// write LESS than a full cycle would (it never re-derives an accepted finding), never more.
//
// WHAT REMAINS UNPROTECTED, stated plainly. There is no exclusive lock on the workspace, and this
// path does not pretend to take one. Two things follow. (a) A writer that lands inside the few
// syscalls between the commit's content re-verification and its create is neither prevented nor
// detected. (b) A crash between the commit and the receipt leaves the tree written and the receipt
// absent; the run directory is made reconcilable for exactly that case — `remediation/journal.json`
// says what was intended and `remediation/commit-attempt.json` (fsynced before the first live write)
// says the commit was entered, so a run directory with an attempt and no receipt is the signature of
// an interrupted write window rather than an ambiguity.

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

// Stable MACHINE reason codes for the from-run remediation path (lower_snake, never sentences).
const (
	// ReasonStaleDecisionSet — a file the decision set targets changed between the report run and
	// this apply. The decisions describe a workspace that no longer exists.
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
	// ReasonStageRollbackFailed — a finding's partially-applied hunks could not be rolled back out
	// of the copy, so the copy no longer matches any recorded intent.
	ReasonStageRollbackFailed = "remediation_stage_rollback_failed"
	// ReasonApplyCommitFailed — the commit to the live workspace failed (and was rolled back).
	ReasonApplyCommitFailed = "apply_commit_failed"
)

// BaseHashAbsent is the recorded digest of a file that did not exist when the decision set was
// captured. It is a VALUE rather than an omission so that "this file appeared after the review"
// is a mismatch like any other, instead of an untracked key.
//
// It is meshcore's pin sentinel by DEFINITION, not by coincidence: the same map is handed to
// workspace.CommitExpecting, so the two layers cannot drift into disagreeing about what "absent"
// spells.
const BaseHashAbsent = workspace.ContentAbsent

// IntendedHunk is one entry of the write journal: an edit that is ABOUT to be attempted. It is
// recorded before any write, so the journal is a statement of intent that cannot be retro-fitted
// to whatever happened to succeed.
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
	// ReplacementSHA256 is the digest of the replacement text AS PROPOSED (before the line-ending
	// adaptation the copy write applies). A byte COUNT does not identify a replacement — every
	// string of that length shares it — so without this the journal cannot answer "what was this
	// call about to write", only "how much of it". The text itself is deliberately not recorded:
	// the journal is an audit record, not a second copy of the patch.
	ReplacementSHA256 string `json:"replacementSha256"`
}

// Journal is the complete pre-write record for one remediation.
type Journal struct {
	SchemaVersion int            `json:"schemaVersion"`
	RunID         string         `json:"runId"`
	SourceRunID   string         `json:"sourceRunId,omitempty"`
	Mode          string         `json:"mode"`
	Hunks         []IntendedHunk `json:"hunks"`
	// IntentSHA256 is a canonical digest over the WHOLE ordered hunk list. One value identifies
	// the complete intent, so a receipt and a journal can be tied together, and a journal that was
	// edited after the fact does not match the receipt that quotes it.
	IntentSHA256 string `json:"intentSha256"`
}

// hunkDigest is the digest format used for a replacement (meshcore's pin format, so one spelling
// of "sha256:<hex>" is used everywhere in this repo's write path).
func hunkDigest(s string) string { return workspace.ContentPin([]byte(s)) }

// intentDigest is the canonical whole-intent digest: each hunk's JSON encoding, length-prefixed so
// no concatenation of two hunks can be mistaken for a different pair, hashed in order. Order is
// part of the intent — the same hunks applied in a different order are a different write.
func intentDigest(hs []IntendedHunk) string {
	h := sha256.New()
	for _, k := range hs {
		// IntendedHunk is basic types only, so Marshal cannot fail; the error is checked anyway so
		// a future field that CAN fail does not silently produce a digest over nothing.
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
	// Reason is the stable machine code when the finding was NOT applied.
	Reason string `json:"reason,omitempty"`
}

// Receipt is the durable answer to "what did this call actually write". It is persisted to the run
// directory on EVERY path — including a cancelled call, which gets no response to carry it.
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
	// IntentSHA256 is the journal's whole-intent digest, repeated here so the receipt and the
	// journal are tied by value rather than by both merely existing in the same directory.
	IntentSHA256 string `json:"intentSha256,omitempty"`
	// PatchArtifact is the run-directory-relative patch path ("" when nothing changed), and
	// PatchSHA256 its digest — a link plus a hash, never inline content.
	PatchArtifact string `json:"patchArtifact,omitempty"`
	PatchSHA256   string `json:"patchSha256,omitempty"`
	// CommitAttempted reports whether the LIVE commit was entered. It is the difference between
	// "nothing was tried" and "something was tried and rolled back or interrupted", which
	// `Committed` alone cannot express — and it is what makes an interrupted run reconcilable
	// against the fsynced `remediation/commit-attempt.json` marker.
	CommitAttempted bool `json:"commitAttempted"`
	// Committed reports whether the LIVE commit completed successfully. It is derived ONLY from a
	// commit that returned success: false for patch mode by construction, false for every
	// cancelled call, and false for a commit that failed and rolled back. WHAT it wrote is Files —
	// a successful commit with an empty Files wrote nothing because nothing differed, which is a
	// different fact from not having committed at all.
	Committed bool `json:"committed"`
	// Selection is the narrowing selection this window was given, if any (D8-A). It rides the
	// DURABLE receipt for the same reason `outcome` and `refused` do: a caller whose response was
	// cancelled must still be able to answer "which findings did I ask for, and did any selector
	// name nothing" from the run directory alone.
	Selection *review.ApplySelection `json:"selection,omitempty"`
}

// RemediateRequest is one from-run remediation.
type RemediateRequest struct {
	// Workspace is the live workspace the accepted findings were adjudicated against.
	Workspace string
	// WorkspaceIdentity is that root's CANONICAL IDENTITY as the run which produced the decision
	// set captured it. A pathname is not a repository: without this, the same string can be made
	// to name a different tree between the review and the apply. It is REQUIRED — an unbound
	// request halts rather than trusting the string.
	WorkspaceIdentity WorkspaceIdentity
	// Mode must be ModePatch or ModeApply. ModeReport has nothing to remediate.
	Mode    review.Mode
	Surface string
	Profile string
	// ReviewerPanel is carried only so the plan resolves to the SAME configuration the report run
	// used (it selects the author_remediator lane); no reviewer seat is executed on this path.
	ReviewerPanel []review.SeatSpec
	// TrustedRoots are the out-of-band trusted roots of an agent surface (see Request.TrustedRoots).
	// They are ENFORCED here, not merely carried: the workspace must resolve inside them before a
	// copy is made. A surface whose caller is not a human establishes its roots at launch, and a
	// decision set that travelled from another surface (or a registry entry that outlived a root
	// change) must not be able to write outside the set in force NOW.
	TrustedRoots []string
	// SourceRunID identifies the report run whose decision set this applies.
	SourceRunID string
	// Findings / Decisions are the ALREADY-ADJUDICATED set, aligned by index exactly as
	// RunOutcome carries them. Nothing here is re-judged.
	Findings  []review.Finding
	Decisions []review.Decision
	// Select, when non-nil, NARROWS the accepted set to the findings whose HOST-COMPUTED
	// fingerprint it names (D8-A). nil applies the whole accepted set; an EMPTY non-nil slice is a
	// refusal, never a fallback to "everything". See writeRequest.Select — this is carried straight
	// through to the one governed write path, unexamined here, so the rule has one implementation.
	Select []string
	// Shown is the set of workspace-relative files the reviewers were actually shown. It gates the
	// write exactly as it does in a full cycle: a finding about a file no reviewer saw is never
	// applied.
	Shown map[string]bool
	// BaseHashes is path → digest as captured when the decision set was produced (see BaseHashes).
	BaseHashes map[string]string
	// OnJournal, when set, receives the write journal at the moment it is durable and BEFORE the
	// first edit is applied. It must not block or panic.
	OnJournal func(Journal)
	// VerifyCommands / VerifyTimeout are the operator's own build/test commands, run on the
	// containment copy before and after the accepted set is applied to it, and RECORDED. Empty means
	// nothing is executed. See verify.go — in particular, why the second pass runs after the commit.
	VerifyCommands []string
	VerifyTimeout  time.Duration
	// AllowProtectedPaths waives the protected-config half of containment (root admission + the
	// write denylist), never the secret family. See Request.AllowProtectedPaths. It matters on
	// THIS path too: a from-run apply replays a decision set whose report run had the waiver, and
	// an apply without it would refuse every destination the report was allowed to propose.
	AllowProtectedPaths bool
	OnEvent             func(audit.EventLine)
	// Trace is the caller's W3C trace context, when the surface that accepted the request carried
	// one. It is the trace of the APPLY turn, deliberately not inherited from the report run: the
	// two are separate requests and a host that traced them separately must be able to see that.
	// nil records nothing. See review.Trace.
	Trace *review.Trace
}

// RemediateOutcome is the result of a from-run remediation. It is populated on every path,
// including a halt, so a caller always has the receipt.
type RemediateOutcome struct {
	RunID       string
	RunDir      string
	SourceRunID string
	Mode        review.Mode
	Journal     Journal
	Receipt     Receipt
	Cancelled   bool
	Withheld    []review.WithheldFile
	// Selection is what a narrowing `select` did — requested, matched, unmatched (D8-A). nil when
	// no selection was supplied. Populated on every path, refusals included, so a caller learns
	// which of its selectors named nothing even when the call refused for naming nothing at all.
	Selection *review.ApplySelection
	// Refusals lists the findings refused because their target is a PROTECTED PATH. Nothing was
	// written to any of them (the denylist is non-overridable), every other accepted finding was
	// applied, and the non-emptiness of this slice is what makes the call answer `isError: true`
	// with `outcome: "partial_refusal"` rather than reading as a clean success (D8-C, §13.4).
	Refusals []review.ApplyRefusal
	// Verification is what the operator's own build/test commands did on the containment copy,
	// before and after this remediation's edits. nil when none were supplied. It is a RECORD:
	// nothing here branches on it, and a red result blocked nothing.
	Verification *review.VerificationReport
}

// Applied is how many findings this remediation put into the live tree (apply) or the produced
// diff (patch). It reads the receipt rather than keeping a second counter: the receipt is the
// durable answer, and a count derived anywhere else could disagree with it.
func (o RemediateOutcome) Applied() int { return len(o.Receipt.Applied) }

// Refused is how many findings were refused for a protected path.
func (o RemediateOutcome) Refused() int { return len(o.Refusals) }

// Outcome is the write-outcome discriminator for this remediation — the same derivation every
// surface uses (review.ApplyOutcome), so MCP, ACP and the CLI cannot spell the same run
// differently.
func (o RemediateOutcome) Outcome() string {
	return review.ApplyOutcome(o.Applied(), o.Refused())
}

// BaseHashes digests the given workspace-relative files as they exist NOW, reading each through an
// identity-bound handle on the workspace root (never by re-opening a path string). A file that
// cannot be read is recorded as BaseHashAbsent rather than skipped: an unreadable file at capture
// time and a readable one at apply time is precisely the change the pin exists to catch.
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

// AcceptedForApply reports whether an already-adjudicated decision is part of the ACCEPTED set a
// from-run remediation may write.
//
// It is deliberately narrow. A report run finalizes every actionable finding as
// `reported_valid` — that state IS the accepted set, and it is the only one this path acts on.
// Everything else (invalid, skipped, already-addressed, withheld, upstream-conflicted) was decided
// against, and a finding the WRITE-PATH RULE refused (authority-only support, no workspace
// evidence, weak-identity-only support) stays refused here: the refusal was computed once, by the
// host, over the run that produced it.
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

// WorkspaceIdentity is a reviewed workspace root's identity, captured by the run that produced a
// decision set and re-verified by the run that applies it.
//
// The stored workspace on a decision set is a STRING, and a string is not a repository. Point it at
// a different checkout whose targeted files happen to carry the same bytes and every other check
// passes: the base hashes match (same content), `scope.New` then authorizes the substitute as its
// own root, and the accepted set is written into a tree no one reviewed. Binding the root by
// device+inode AND by canonical path closes that: the same object, reached by the same resolved
// name, or nothing is written.
type WorkspaceIdentity struct {
	// Info is os.Stat's report for the root at capture time.
	Info fs.FileInfo
	// Key is the durable identity — device+inode on unix, volume serial + file index on Windows —
	// resolved EAGERLY at capture time by rootIdentityKey. It is the authoritative comparison, and
	// os.SameFile is not, because os.SameFile is not a point-in-time capture on every platform: on
	// Windows os.Stat defers loading the file index to a lazy loadFileId that reopens BY PATH the
	// first time SameFile is called, so comparing two captured FileInfos after a swap compares the
	// substitute against itself and reports a match. Empty only when the platform could not express
	// one, which is stated at every use rather than papered over.
	Key string
	// Canonical is the symlink-resolved absolute path at capture time. It is carried alongside the
	// inode because an inode is reused after a delete: matching both is what distinguishes "the
	// same directory" from "a new directory that inherited its inode number".
	Canonical string
}

// Bound reports whether an identity was actually captured.
func (w WorkspaceIdentity) Bound() bool { return w.Info != nil && w.Canonical != "" }

// CaptureWorkspaceIdentity records a workspace root's identity. It is called by the surface that
// completes a REPORT run, and its result travels with the decision set.
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

// verifyAgainst requires the live path to still name the captured object.
//
// The durable key is preferred over os.SameFile wherever both captures have one: os.SameFile is a
// deferred, path-reopening comparison on Windows (see WorkspaceIdentity.Key), so it cannot detect
// the substitution this check exists to refuse. SameFile remains the fallback for a platform that
// cannot express a key, where it is still better than comparing pathnames alone.
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

// foldPaths is true only on Windows, where the filesystem itself is case-insensitive. It is a VAR
// so the Windows branch of the reviewed-root identity check can be exercised on any platform —
// this repository does not gate on Windows, and a containment branch compiled everywhere but
// executed nowhere is not a tested branch.
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

// acceptedTarget is one accepted finding together with the decision that accepted it, and its
// position in the arrays it came from (so a per-finding call id is stable regardless of how many
// earlier findings were refused).
type acceptedTarget struct {
	finding  review.Finding
	decision review.Decision
	index    int
}

// reconcileDecisions pairs decisions to findings BY FINDING ID and returns the subset `accept`
// authorizes.
//
// The wire form carries the two as parallel arrays, and pairing them by POSITION is an assumption
// about an artifact that has travelled: through a registry, possibly through a schema migration,
// possibly through a client that re-serialized it. A set that was reordered or truncated then
// authorizes a DIFFERENT finding than the one a human read — silently, because every individual
// value is still well-formed. So the alignment is verified rather than assumed, and a set that does
// not reconcile is refused whole: there is no safe subset of a set whose bindings are unknown.
//
// The same check runs for a full cycle, where the arrays never left memory. That is not ceremony:
// the ids are lane-supplied, and an id that repeats makes "which finding does this decision
// authorize" unanswerable in exactly the same way — see uniqueFindingIDs, which is what a full
// cycle runs BEFORE those ids are allowed to authorize a write.
//
// `accept` decides which reconciled pairs are writable; nothing here re-judges a decision.
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

// uniqueFindingIDs makes every finding's id present and unique IN PLACE, and re-binds each
// decision to the id its finding now carries.
//
// A full cycle's ids come from the lanes: two lanes can both call their first finding "F-001", and
// the merge keeps both (it dedupes by fingerprint, not by id). Those ids then authorize writes,
// where "which decision authorizes which finding" must have exactly one answer — so a collision is
// resolved here, before the write path binds anything, rather than being tolerated by a weaker
// binding rule.
//
// It is deliberately MINIMAL: an id that is already present and unique is left exactly as it was,
// because a finding's id is user-visible (the remediation marker quotes it). Only a duplicate or an
// empty id is rewritten, and the run's final renumbering still assigns the stable f1..fN ids
// afterwards.
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

// writeDurableJSON writes v to a run-relative path and makes it DURABLE before returning: the file
// is fsynced, and its parent directory is fsynced too so the directory entry itself survives.
//
// audit.Run's ordinary writers are best-effort by design (an events line that does not reach disk
// must not fail a run). The journal and the receipt are not in that class: the journal is the
// precondition of opening the write window, and the receipt is the only thing anyone can read
// afterwards. Both must be able to FAIL, and both must be on disk — not merely in the page cache —
// before the run proceeds past them.
//
// The parent-directory fsync is best effort: on Windows a directory handle cannot be synced, and
// treating that platform difference as a run failure would refuse every Windows remediation. The
// FILE's sync is not best effort anywhere.
func writeDurableJSON(runDir, rel string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(runDir, rel, append(b, '\n'))
}

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

// writeWindow makes "this call was cancelled" and "this call entered the live commit" mutually
// exclusive answers, decided ONCE under a lock.
//
// Reading ctx.Err() and then calling Commit is two acts: a cancellation landing between them is not
// observed by the check, and a caller told "cancelled" can still have had its commit run. The gate
// collapses them. enter() is the only place the decision is made; after it returns true the context
// is never consulted again for this call's disposition, so a late cancellation cannot retroactively
// turn a completed commit into a cancelled one. That is not a limitation being papered over — an
// entered write window is deliberately UNCANCELLABLE (a commit interrupted halfway is the outcome
// the rollback exists to prevent), and this makes the receipt say so honestly.
//
// What the lock is and is not, stated plainly: Remediate runs on ONE goroutine, so today the mutex
// arbitrates nothing at runtime. Its job is to make the invariant a single guarded act with one
// entry point, so that a future observer of cancellation (a watchdog, a surface that wants to
// answer "did it start?") has somewhere correct to synchronize instead of re-reading ctx beside the
// commit — which is exactly the shape that produced the defect.
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

// testHookInsideWriteWindow runs after the write window has been entered and before the live commit
// begins. It is this file's ONLY test seam, and it exists for one reason: the property under test —
// "a cancellation that lands after the gate does not make this call report itself cancelled" — is a
// race, and staging a real one would make the test flaky and would prove nothing when it passed. It
// is nil in every non-test build and is reachable from no exported API.
var testHookInsideWriteWindow func()

// Remediate applies an already-adjudicated decision set to the workspace. See the file comment for
// the properties it exists to guarantee.
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
		Available: available, ReviewerPanel: req.ReviewerPanel,
	})
	if err != nil {
		return out, err
	}
	// The config ceiling is AUTHORITATIVE, and it is re-read here rather than trusted from the
	// surface: a surface that computed the ceiling itself would be the only thing standing between
	// a request and a live write.
	if plan.Mode != req.Mode {
		return out, fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: the %q surface's write-authority ceiling resolves %q down to %q — grant the capability in config (`surfaces.capabilitiesBySurface.%s: [%s]`) rather than expecting the ceiling to be bypassed",
			req.Surface, req.Mode, plan.Mode, req.Surface, config.CapabilityAllowRemediate)).
			WithReason(ReasonRemediateModeRefused)
	}

	startedAt := m.now()
	run, rerr := audit.NewRun(m.ArtifactDir, "", startedAt)
	if rerr != nil {
		return out, fault.Wrap(fault.Internal, "create run dir", rerr).WithReason("remediation_run_dir_failed")
	}
	run.OnEvent = req.OnEvent
	out.RunID, out.RunDir = run.ID, run.Dir
	_ = run.WriteJSON("resolved-plan.json", plan)
	_ = run.Event(startedAt, "info", "remediation_started", "from-run remediation started", map[string]any{
		"mode": string(req.Mode), "sourceRunId": req.SourceRunID, "findings": len(req.Findings),
	})

	// FROM HERE ON THIS FUNCTION DOES NOT WRITE. The journal, the pins, the cancel/commit gate,
	// the per-finding staging, the commit and the receipt are governedWrite's, and they are the
	// SAME ones every other surface gets — see writepath.go for why that is the whole point.
	//
	// What this path contributes that a full cycle cannot: the pins were captured by an EARLIER
	// run (so they are re-verified against the live tree before anything is copied), the accepted
	// set is the report run's `reported_valid` set, and the out-of-band trusted roots are enforced
	// because a decision set is durable and can outlive them.
	res, werr := m.governedWrite(ctx, run, writeRequest{
		Workspace: req.Workspace, Identity: req.WorkspaceIdentity,
		Mode: req.Mode, Plan: plan, SourceRunID: req.SourceRunID,
		Findings: req.Findings, Decisions: req.Decisions, Accept: AcceptedForApply,
		// A from-run remediation opens exactly ONE write window, so it is always the primary one:
		// its selection is the caller's list and is measured against it.
		Select: req.Select, SelectPrimary: true,
		Shown: req.Shown, Pins: req.BaseHashes, TrustedRoots: req.TrustedRoots,
		OnJournal:      req.OnJournal,
		VerifyCommands: req.VerifyCommands, VerifyTimeout: req.VerifyTimeout,
		// The dirty check is asked HERE, at the apply, and is deliberately not inherited from the
		// report run that produced this decision set: that run's view of the tree is minutes or hours
		// old, and the pins it recorded cover only the files it targeted.
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
	// `outcome` and `refused` are recorded HERE as well as on the wire: a partially-refused run
	// that the caller never collected a response for must still be answerable from the run
	// directory alone, which is the same reason the receipt is fsynced.
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

// authorizeEditTarget answers the three questions ROOT CONFINEMENT DOES NOT ANSWER about one
// proposed edit. Confinement (scope.Resolver + the protected-path denylist) says where a write may
// land; it says nothing about what was reviewed. An edit is model-authored output, so an accepted
// finding about `a.go` can come back proposing a hunk in any other in-root, non-denylisted file —
// a file no reviewer was shown under this finding, no host adjudicated, and no run base-hashed.
// Applying it would be a write nobody judged, wearing the authorization of one they did.
//
// So an edit must be:
//
//  1. INSIDE ITS FINDING. The finding names the file it was adjudicated about; its edits target
//     that file and no other. There is deliberately NO bypass. Cross-file remediation would need
//     an explicit per-finding authorization carried in the adjudicated decision set — so that a
//     human reads it before it is written — plus a record of that authorization in the receipt.
//     Neither exists, nothing in this repo proposes cross-file edits, and the honest
//     implementation of "not authorized" is "not possible".
//  2. SHOWN. The same gate a full cycle applies to a finding, applied to the thing being written.
//  3. PINNED. Present in the base-hash set with real content, so "is this file still what was
//     judged" is answerable at all — before the window and again inside the commit.
//
// Every failure is a HALT, never a skip. A proposal that reached outside its authorization is not
// a proposal whose other hunks can be assumed sound, and a skip would let the run finish reporting
// success.
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

// isDestinationDrift reports whether a commit refusal means "the live destination is not what this
// write was decided against" — content changed, object swapped, or no base recorded for it at all.
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
