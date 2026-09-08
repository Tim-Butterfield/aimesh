package review

import (
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

// --- Generic model-operation primitives now live in meshcore/core; the reviewmesh
// root package re-exports them as type aliases + re-declared consts so existing app
// code (review.X) is unchanged while the substrate packages depend on core directly. ---

type ModelArg = core.ModelArg
type IdentityMethod = core.IdentityMethod
type IdentityEvidence = core.IdentityEvidence
type HaltClass = core.HaltClass
type ArtifactDelivery = core.ArtifactDelivery
type Edit = core.Edit
type AdapterCaps = core.AdapterCaps
type SurfaceCaps = core.SurfaceCaps

const (
	IdentityEnvelope   = core.IdentityEnvelope
	IdentityTrace      = core.IdentityTrace
	IdentitySelfReport = core.IdentitySelfReport
	IdentityAmbient    = core.IdentityAmbient

	EvidenceEnvelope      = core.EvidenceEnvelope
	EvidenceTrace         = core.EvidenceTrace
	EvidenceCLIStatus     = core.EvidenceCLIStatus
	EvidenceInvocationTag = core.EvidenceInvocationTag
	EvidenceSelfReport    = core.EvidenceSelfReport
	EvidenceNone          = core.EvidenceNone

	VerifVerified     = core.VerifVerified
	VerifSelfReported = core.VerifSelfReported
	VerifUnknown      = core.VerifUnknown
	VerifMismatch     = core.VerifMismatch
	VerifUnverified   = core.VerifUnverified

	ArtifactReadsFromDir = core.ArtifactReadsFromDir
	ArtifactInline       = core.ArtifactInline
)

// --- Enumerated kinds (canonical wire values are the snake_case string literals) ---

// Kind classifies a finding.
type Kind string

const (
	KindPass         Kind = "pass"
	KindFail         Kind = "fail"
	KindAmbiguity    Kind = "ambiguity"
	KindInconsistent Kind = "inconsistency"
	KindGap          Kind = "gap"
	KindRisk         Kind = "risk"
)

// Severity ranks a finding.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Verdict is a reviewer/adjudication outcome for a whole pass.
type Verdict string

const (
	VerdictApprove            Verdict = "approve"
	VerdictApproveSuggestions Verdict = "approve_with_suggestions"
	VerdictRequestChanges     Verdict = "request_changes"
	VerdictBlock              Verdict = "block"
)

// Mode is what the host does with accepted findings.
type Mode string

const (
	ModeReport Mode = "report"
	ModePatch  Mode = "patch"
	ModeApply  Mode = "apply"
)

// Role is a function in the review sequence the Manager fills.
type Role string

const (
	RoleAuthorRemediator Role = "author_remediator"
	RoleReviewer         Role = "reviewer"
	RoleCrossCheck       Role = "cross_check"
	RoleVerifier         Role = "verifier"
)

// Phase identifies which call in the sequence produced a result.
type Phase string

const (
	PhasePreflight    Phase = "preflight"
	PhaseAuthorReview Phase = "semantic_author_review" // optional report-only host self-review
	PhaseIterate      Phase = "semantic_iterate"
	PhaseCrossCheck   Phase = "semantic_cross_check"
	PhaseVerify       Phase = "semantic_verify"
	PhaseAdjudicate   Phase = "semantic_adjudicate"
	// PhaseRemediate labels a model-driven remediation call: the host author_remediator turns ONE
	// accepted finding into a concrete, anchored Edit against the target file (apply/patch modes only).
	PhaseRemediate Phase = "semantic_remediate"
	// PhaseValidateAdjudicate labels a host-adjudication call made ONLY to prove the configured
	// author_remediator adapter/model can run its role during ACP validation (a synthetic readiness
	// probe when the reviewer produced zero natural findings). It is the persisted CallStatus.Phase
	// label only; the model prompt/output still use PhaseAdjudicate's schema (semantic_adjudicate).
	PhaseValidateAdjudicate Phase = "validate_adjudicate"
)

// DecisionState is the disposition of a finding (fed forward as addressed-context).
type DecisionState string

const (
	StateApplied          DecisionState = "applied"
	StateInvalid          DecisionState = "invalid"
	StateSkipped          DecisionState = "skipped"
	StateAlreadyAddr      DecisionState = "already_addressed"
	StateUpstreamConflict DecisionState = "upstream_conflict_deferred"
	StateWithheldClassE   DecisionState = "withheld_class_e"
	StateReportedValid    DecisionState = "reported_valid"
	StateReportedInvalid  DecisionState = "reported_invalid"
)

// --- Review domain types ---

// Finding is one issue (or pass) reported by a reviewer or evidence hook.
type Finding struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Detail     string   `json:"detail,omitempty"`
	Suggestion string   `json:"suggestion,omitempty"`
	Kind       Kind     `json:"kind"`
	Severity   Severity `json:"severity"`
	File       string   `json:"file,omitempty"`
	Location   string   `json:"location,omitempty"`  // line range, section, or symbol; "" if unknown
	Source     string   `json:"source,omitempty"`    // "reviewer" | "evidence"
	Primitive  string   `json:"primitive,omitempty"` // the primitive that produced it (e.g. "P1")
}

// --- Reviewer panel (variable-count blind primary stage) ---
//
// The blind primary stage is a PANEL of N ordered seats (N >= 1). The count is variable BY
// CONSTRUCTION: nothing in the schema, the engine, the surfaces, or the UI encodes a particular
// number. A panel of one is the historical single-reviewer behavior, unchanged. `cross_check`
// and `verifier` are NOT seats — they are distinguished single phase lanes with different
// information topologies (informed-vs-blind, applyable-vs-report-only), so they never join the
// panel however many seats it has.

// MaxReviewerSeats is the ceiling on panel size. It exists because an unbounded panel is a
// spend/DoS vector (each seat is a real model CLI), and it matches exploremesh's ACP fan-out
// cap so the two apps refuse the same magnitude. It is a CEILING, never a shape: any count in
// 1..MaxReviewerSeats is equally ordinary.
const MaxReviewerSeats = 16

// SeatSpec is a REQUESTED blind-primary seat as a surface composes it (CLI `--reviewer`,
// ACP `_meta.reviewmesh.panel`). COMPOSE-NOT-CONFIGURE: the adapter and model must already be
// defined by the configuration — a request selects and orders configured identities, it can
// never introduce an adapter, a binary path, or a launch argument.
type SeatSpec struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// Identity returns the seat's uniqueness key: the FULL (adapter, model, effort) triple. Two
// seats with an identical triple add no independent vantage, so a panel naming one twice is a
// configuration error rather than a doubled "agreement".
func (s SeatSpec) Identity() string { return s.Adapter + "\x00" + s.Model + "\x00" + s.Effort }

// Seat identity tiers (the per-seat strength of the model-identity evidence). They are the
// verification statuses of meshcore/verify, restated here as the vocabulary the provenance ledger
// uses. Each supporting seat carries its tier so a reader can see what the support consists of;
// no tier changes what happens to the finding (see ../docs/model-identity.md).
const (
	SeatIdentityVerified     = VerifVerified
	SeatIdentitySelfReported = VerifSelfReported
	SeatIdentityUnknown      = VerifUnknown
)

// ApplyRefusalProtectedPath is the STABLE MACHINE code for a finding whose target resolves to a
// PROTECTED PATH (meshcore's non-overridable write denylist: `.env`, `.git/`, agent client config,
// `.aimesh/`, …). The denylist itself is untouched and non-overridable — nothing is ever written
// there. What this value records is that ONE such finding no longer discards the whole run: it is
// marked not-applied with this reason, the remaining findings are applied, and the refusal is
// surfaced as a first-class fact on every surface (D8-C; design §13.2/§13.4).
//
// It is deliberately a value in the SAME enum as `authority_only` / `no_workspace_evidence` rather
// than a new concept — the same field, the same receipt row, the same renderer. What differs is
// the coarse signal it drives: those two describe findings that were never eligible to be
// applied and still exit 0; this one describes a finding the model DID target at a path we will
// not let it touch, so the run must not read as unqualified success (§13.4.3).
const ApplyRefusalProtectedPath = "protected_path"

// ApplyRefusal is one finding refused because its target is a protected path. It is keyed on the
// HOST-COMPUTED fingerprint, never the model-authored `Finding.ID`: the fingerprint is derived by
// us from the finding's file and location, so a model can change what a fingerprint names
// only by proposing a different finding, openly. (`Finding.ID` is additionally renumbered after
// the write window, so it is not even stable across one run's own projection.)
type ApplyRefusal struct {
	// Fingerprint is the host-computed identity a caller can act on in a follow-up selection.
	Fingerprint string `json:"fingerprint"`
	// File is the workspace-relative target that was refused.
	File string `json:"file"`
	// Reason is the stable machine code — today always ApplyRefusalProtectedPath.
	Reason string `json:"reason"`
	// FindingID is the finding's id AS THE WRITE PATH SAW IT. It is carried for the receipt and
	// the decision-state reconciliation, and is deliberately NOT projected onto any wire payload.
	FindingID string `json:"-"`
}

// ApplySelection is the record of a SELECTIVE APPLY: which host-computed fingerprints a caller
// asked to narrow the write set to, which of them named a finding in the run's accepted set, and
// which named nothing (design §13.3, decision D8-A).
//
// THE KEY IS THE HOST-COMPUTED FINGERPRINT AND NEVER THE MODEL-AUTHORED `Finding.ID`. That is the
// entire security content of selective apply. A model-authored identifier is model-controlled, so
// keying the write set on one would let a model relabel findings until "apply only this one"
// selected something else. A fingerprint is derived by us from the finding's own file and
// normalized location, so a model can change what a fingerprint NAMES only by proposing a different
// finding, openly. (`Finding.ID` is additionally renumbered after the write window, so it is not
// even stable within one run.)
//
// A selection can only ever REDUCE what is written. An unmatched fingerprint is DROPPED — never
// looked up anywhere, never fetched from another run — and reported here, because a caller that
// mistyped one must find out rather than quietly get a wider or narrower write than it asked for.
type ApplySelection struct {
	// Requested is the caller's list, trimmed and deduped, in the order it was given.
	Requested []string `json:"requested"`
	// Matched are the requested fingerprints that named a finding in this run's accepted set.
	Matched []string `json:"matched"`
	// Unmatched are the requested fingerprints that named nothing. They wrote nothing and reached
	// nothing; they are reported so a mistyped selector is visible rather than silent.
	Unmatched []string `json:"unmatched"`
}

// Selective returns true when a selection was actually supplied (as opposed to the zero value,
// which means "no narrowing: apply the whole accepted set").
func (s *ApplySelection) Selective() bool { return s != nil && len(s.Requested) > 0 }

// Apply outcomes — the discriminator carried on every WRITE-PRODUCING result (design §13.4.1).
// Its absence means "not a write run"; a consumer that ignores it is less informed but never
// wrong, because the coarse not-clean signal each surface carries (MCP `isError`, the CLI exit
// code, ACP `stopReason`) is keyed on `counts.refused > 0` and not on this field.
const (
	// OutcomeApplied — every accepted finding that could be written was written, and nothing was
	// refused for a protected path.
	OutcomeApplied = "applied"
	// OutcomePartialRefusal — the write completed, at least one finding was applied, and at least
	// one was refused for a protected path.
	OutcomePartialRefusal = "partial_refusal"
	// OutcomeNothingApplied — the write completed and no finding was applied. It covers both the
	// empty case (nothing to do) and the case where every accepted finding was refused; the two
	// are told apart by `counts.refused`, which is also what drives the coarse signal.
	OutcomeNothingApplied = "nothing_applied"
)

// ApplyOutcome is the ONE derivation of the outcome discriminator, so no surface can compute a
// different answer from the same two counts. `applied` counts findings whose hunks reached the
// live tree (apply) or the produced diff (patch); `refused` counts protected-path refusals.
func ApplyOutcome(applied, refused int) string {
	switch {
	case applied == 0:
		return OutcomeNothingApplied
	case refused > 0:
		return OutcomePartialRefusal
	default:
		return OutcomeApplied
	}
}

// SeatRef is one seat's contribution to a finding — the per-finding half of the provenance
// ledger. Every field is HOST-recorded: the seat identifiers come from the resolved panel and
// the identity tier from the host's own verification of that seat's call. No model asserts any
// of it, and no model is ever asked to count.
type SeatRef struct {
	SeatID       string `json:"seatId"`
	Adapter      string `json:"adapter"`
	Model        string `json:"model"`
	IdentityTier string `json:"identityTier"`
}

// SeatStatus is one seat in the EXECUTED-ROSTER echo: what was requested, what ran, and how it
// ended. Every requested seat appears exactly once, including a seat that halted the run — NO
// SILENT DEGRADATION: N seats requested is N seats accounted for, or a clear failure.
type SeatStatus struct {
	SeatID  string `json:"seatId"`
	Index   int    `json:"index"` // 1-based position in the requested (ordered) panel
	Adapter string `json:"adapter"`
	Model   string `json:"model"` // catalog key
	Effort  string `json:"effort,omitempty"`
	// Status is the seat's terminal state:
	//   completed     — ran and produced a schema-valid result
	//   budget_capped — completed, but the run-level round budget stopped further rounds
	//   halted        — this seat's failure halted the RUN (identity, adapter, containment, schema)
	//   not_started   — the run halted on an earlier-indexed seat before this one produced a result
	Status string `json:"status"`
	// Rounds is how many stabilization rounds this seat actually ran, Findings how many raw
	// findings it reported across them (before host dedup).
	Rounds   int `json:"rounds"`
	Findings int `json:"findings"`
	// IdentityTier is the strongest identity classification observed for this seat's calls.
	IdentityTier string `json:"identityTier,omitempty"`
	// ReasonCode is the STABLE MACHINE code when the seat did not complete normally.
	ReasonCode string `json:"reasonCode,omitempty"`
	// Signal and Detail are THIS SEAT'S OWN actionable cause, and they exist because a panel
	// halt is decided by one seat while the others may have failed for entirely different,
	// independently fixable reasons.
	//
	// Measured 2026-08-11: a two-seat panel failed with seat 1 on a model its account cannot use
	// and seat 2 on a model name its CLI has never had. Both were dispatched, both were paid for,
	// and the operator was shown ONE cause. Recovering the other meant opening the run directory.
	// The run-level Failure is still the first-by-index halt (that contract is unchanged); these
	// two fields are what let every OTHER seat's cause reach a caller alongside it.
	//
	// Signal is the meshcore/clihint classification for this seat (folder_trust | login_required |
	// model_invalid | update_prompt | timeout, or empty). Detail is the one-line human cause. Both
	// are empty for a seat that completed.
	Signal string `json:"signal,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Decision is the host's adjudication of a finding.
type Decision struct {
	FindingID        string        `json:"findingId"`
	Valid            bool          `json:"valid"`
	State            DecisionState `json:"decisionState"`
	SeverityAdjusted Severity      `json:"severityAdjusted,omitempty"`
	Reasoning        string        `json:"reasoning,omitempty"`
	// SupportingSeats / AgreementCount / DissentingSeats are the HOST-COMPUTED provenance
	// ledger for a panel finding. They live on the DECISION (the host's own record), never on
	// the model-parsed Finding, so there is no field for a model to assert an agreement in:
	// counts are host arithmetic over which seats reported the deduped fingerprint, exactly as
	// exploremesh computes corroboration host-side. AgreementCount is len(SupportingSeats) —
	// carried explicitly so a consumer never recounts the array and reaches a different number.
	// DissentingSeats are the seats that RAN successfully and did not report this finding
	// (knowable only for seats that completed; a halted/not-started seat is in neither list).
	SupportingSeats []SeatRef `json:"supportingSeats,omitempty"`
	AgreementCount  int       `json:"agreementCount,omitempty"`
	DissentingSeats []string  `json:"dissentingSeats,omitempty"`
	// DistinctModels is how many different MODELS the supporting seats ran. It is NOT an adjusted
	// agreement figure and must never be read as one: `agreementCount` is unchanged and means what
	// it always meant. This is a count of a different thing, computed the same way — "3 seats, 2
	// distinct models" states the situation without anyone having to interpret a weighting.
	//
	// Absent for a finding with no supporting seats (an evidence hook, the cross-check, the
	// verifier), where there is no agreement to qualify at all.
	DistinctModels int `json:"distinctModels,omitempty"`
	// AgreementIndependence is what those seats were WORTH: `distinct_models`, or `shared_model`
	// when at least two of them ran the same model. A shared model does not make the finding wrong —
	// it means the agreeing seats share priors, so their errors correlate and the extra agreement is
	// worth less than the count suggests.
	//
	// Only the MODEL STRING is asserted. Vendor, base family and weight lineage are deliberately not:
	// they are not observable from a model string, and a figure resting on a table the host cannot
	// verify would be worse than the raw count.
	AgreementIndependence string `json:"agreementIndependence,omitempty"`
	// Consensus is what the seats that RAN did with this finding: `unanimous` (every completed seat
	// reported it), `majority` (more reported it than did not), or `contested` (a minority reported
	// it, or the seats split evenly).
	//
	// A SILENT SEAT IS NOT A SEAT THAT DISAGREED. Seats are blind and each reports what IT found, so
	// a seat missing from SupportingSeats may have disagreed, may never have reached that file, or
	// may have stopped when its own set stabilized — indistinguishable here, and not guessed at. So
	// `contested` is NOT evidence against the finding and does not make it less likely to be real:
	// it marks the finding worth reading rather than skimming. Nothing is dropped, downgraded,
	// reordered or gated on it.
	//
	// Absent when there is no panel behind the finding, and absent when fewer than two seats
	// completed — one seat cannot be unanimous with itself.
	Consensus string `json:"consensus,omitempty"`
	// Applyable is the WRITE-PATH RULE's verdict, and is set (to a non-nil false) ONLY when
	// the rule REFUSES the finding: an applied hunk must trace to evidence in the workspace
	// copy, so a finding supported only by authority text is reportable but never applyable.
	// Absent (nil) means the write-path rule did not refuse it — the ordinary mode/disposition
	// gating still governs whether anything is written.
	Applyable *bool `json:"applyable,omitempty"`
	// ApplyRefusalReason is the STABLE MACHINE code for that refusal
	// ("authority_only" | "no_workspace_evidence" | "protected_path");
	// empty when Applyable is nil. The first three are decided BEFORE the write path is offered
	// anything; `protected_path` is decided BY the write path, at the moment it authorizes the
	// target, and is the only one whose run must not read as unqualified success.
	ApplyRefusalReason string `json:"applyRefusalReason,omitempty"` // Grounding is the HOST's mechanical check of the finding's citation: does the cited file exist,
	// are the cited lines there, does the named symbol appear. It reads files and runs nothing.
	//
	// IT IS A LABEL AND NEVER A DISPOSITION, on the same rule, and for
	// exactly the same reason. A finding whose citation did not resolve is still raised, still
	// adjudicated on its merits, still reported at its own severity — because a reviewer that named
	// the wrong file may have found a real defect a line away, and a host that cannot read the code
	// is in no position to decide otherwise. Nothing in the engine reads this field back.
	Grounding *CitationGrounding `json:"grounding,omitempty"`
}

// CitationGrounding is what this run could verify about a finding's citation WITHOUT executing
// anything: the file, the lines, the symbol.
//
// It is a FLOOR, not a corroboration. `grounded` says the finding points at something real; it says
// nothing about whether the code there does what the finding claims. That distinction is the reason
// the run-level report carries a fixed note stating it — a label that quietly grew into "verified"
// would manufacture confidence, which is the failure this whole codebase is arranged against.
type CitationGrounding struct {
	// File and Location are the citation AS THE FINDING GAVE IT, echoed so a reader can see what was
	// checked rather than infer it.
	File     string `json:"file,omitempty"`
	Location string `json:"location,omitempty"`
	// Status is a stable machine code: grounded | no_citation | file_missing | line_out_of_range |
	// symbol_absent | location_unchecked | unreadable.
	Status string `json:"status"`
	// Detail says, in a sentence, what was checked and what happened — never a verdict on the finding.
	Detail string `json:"detail,omitempty"`
	// LineCount is the cited file's length when it was read, so a line-range failure is checkable
	// rather than asserted.
	LineCount int `json:"lineCount,omitempty"`
}

// VerificationReport is what the PROJECT'S OWN build/test commands did, run on the containment copy
// before and after the accepted findings were applied to it.
//
// IT IS A RECORD, NOT A GATE. No result here invalidated a finding, dropped one, downgraded one, or
// blocked the commit — the second pass finishes before the commit window even opens, and nothing
// downstream branches on it. A test suite does not know whether a reviewer's finding is real, and it
// does not know whether the code it exercised is the code that changed.
//
// READ `Delta` FIRST. Absolute green is not the signal and never was: real repositories routinely
// have a red suite, and a tool that demanded green-before would refuse to run on exactly the
// repositories most in need of review. What is worth knowing is whether the commands answered
// DIFFERENTLY afterwards.
type VerificationReport struct {
	// Root is the containment copy the commands ran in — never the live workspace.
	Root string `json:"root"`
	// Commands is the operator's list, echoed as given, so a reader sees what ran without inferring
	// it from the results.
	Commands []string `json:"commands"`
	// Before is the baseline pass, taken on the copy before any edit reached it. After is the second
	// pass, taken after every edit and before the commit. After is EMPTY on a report run, which
	// writes nothing and therefore has no "after" — that case is `Delta: baseline_only`.
	Before []VerificationResult `json:"before,omitempty"`
	After  []VerificationResult `json:"after,omitempty"`
	// Delta is the HOST's comparison: unchanged_pass | unchanged_fail | fixed | broken |
	// baseline_only | not_comparable. `unchanged_fail` is an ORDINARY outcome — the suite was red
	// and the change did not make it worse.
	Delta string `json:"delta"`
	// Note is the fixed sentence stating both limits, carried in the artifact rather than left to
	// each surface to phrase.
	Note string `json:"note"`
}

// VerificationResult is one command's outcome in one pass.
type VerificationResult struct {
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exitCode"`
	// TimedOut marks a command that hit its per-command budget. It is SEPARATE from a non-zero exit
	// because it is a statement about the budget rather than about the code: a timeout makes the two
	// passes not comparable, and folding it into "failed" would blame a change for a wall clock.
	TimedOut bool `json:"timedOut,omitempty"`
	// Unstartable carries the reason the command could not be launched at all (no shell, a bad
	// directory). A fact about this host, not about the project.
	Unstartable string `json:"unstartable,omitempty"`
	DurationMs  int64  `json:"durationMs"`
	// Output is the TAIL of the combined streams, bounded. The tail is kept because a build tool
	// prints its banner first and its error last.
	Output string `json:"output,omitempty"`
}

// PanelComposition is the run-level record of WHAT THE BLIND PANEL WAS: each seat as configured, and
// how many distinct models it convened between them.
//
// It exists because an agreement count is arithmetic over SEATS, and seats are not automatically
// independent vantages. A panel that runs one model behind two vendor CLIs produces agreements whose
// errors correlate, and the count alone cannot say so. This says so, without touching the count.
//
// Only the model STRING is asserted, never vendor or base family — see Decision.AgreementIndependence
// for why a taxonomy the host cannot verify would be worse than the raw number.
type PanelComposition struct {
	// Seats is the blind panel in requested order, as configured.
	Seats []SeatComposition `json:"seats"`
	// DistinctModels is how many different models those seats run between them.
	DistinctModels int `json:"distinctModels"`
	// Independence is `distinct_models` or `shared_model` for the panel as a whole.
	Independence string `json:"independence"`
	// Note is the fixed sentence stating the limit in both directions, carried in the artifact rather
	// than left to each surface — shared_model invites discarding a real agreement, distinct_models
	// invites treating one as proof, and neither follows.
	Note string `json:"note"`
}

// SeatComposition is one blind-panel seat as the composition record describes it: what an operator
// configured, in the order they configured it.
type SeatComposition struct {
	SeatID  string `json:"seatId"`
	Index   int    `json:"index"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
	// ModelSource says where this seat's model string came from: `catalog` (a configured
	// modelCatalog key, resolved to that entry's per-adapter argument) or `passthrough` (the caller
	// composed a seat naming something the catalog does not define, and the string went to the
	// adapter verbatim).
	//
	// It is RECORDED, never acted on — the adapter is validated fail-closed either way, and nothing
	// about the seat's findings, agreement or applyability changes. A reader wants it because a
	// pass-through model is one the operator's configuration says nothing about: `list` will not
	// show it, and whether it exists at all was decided by the provider, not here.
	ModelSource string `json:"modelSource,omitempty"`
	// PassThroughHint names the catalog key this model most plausibly meant, when one is close.
	// Present only on a pass-through seat, and only when something is close enough to say.
	PassThroughHint string `json:"passThroughHint,omitempty"`
}

// DissentSummary is the run-level tally of the per-finding consensus labels, so a surface can say
// how much of a result the panel actually agreed on without walking every decision — and so a reader
// always sees the denominator. "Three contested findings" means one thing out of four and another
// out of forty.
//
// It is a tally over Decision.Consensus, never a second source of truth.
type DissentSummary struct {
	// Panelled is how many findings had a describable panel behind them: Unanimous + Majority +
	// Contested is that same number. Findings with no panel (an evidence hook, the cross-check, the
	// verifier) and runs where fewer than two seats completed are in none of these columns.
	Panelled  int `json:"panelled"`
	Unanimous int `json:"unanimous"`
	Majority  int `json:"majority"`
	Contested int `json:"contested"`
	// Note is the fixed sentence stating what silence does and does not mean, carried in the artifact
	// rather than left to each surface to phrase — a surface that abbreviated it would turn a reading
	// prompt into a verdict, which is the one misreading this record must not permit.
	Note string `json:"note"`
}

// GroundingSummary is the run-level tally of the citation check, so a surface can state what the pass
// found without walking every decision — and so a reader always sees the denominator. "Two findings
// cite files that do not exist" means one thing out of five findings and another out of fifty.
type GroundingSummary struct {
	// Root is the tree the citations were checked against.
	Root string `json:"root"`
	// Checked is how many findings carried a citation this pass examined; Grounded + Unresolved is
	// that same number, split by outcome.
	Checked    int `json:"checked"`
	Grounded   int `json:"grounded"`
	Unresolved int `json:"unresolved"`
	// NoCitation counts findings that name no file at all. It is deliberately NOT part of Checked:
	// a finding about the change as a whole has nothing to resolve, and folding it into either
	// column would make the ratio a statement about something else.
	NoCitation int `json:"noCitation"`
	// ByStatus is the full breakdown, keyed by the stable status codes on CitationGrounding.
	ByStatus map[string]int `json:"byStatus,omitempty"`
	// Note is the fixed sentence stating the limit of the claim, carried in the artifact rather than
	// left to each surface to phrase — so no surface can report the check as stronger than it is.
	Note string `json:"note"`
}

// CallSpec describes one model call.
type CallSpec struct {
	Role           Role
	Phase          Phase
	OuterCycle     int
	InnerIteration int
	Model          string // catalog key
	ModelArg       ModelArg
	ThinkingLevel  string // "fast" | "standard" | "deep"
	PromptPath     string
	Artifact       ArtifactDelivery
	RunDir         string
	CallID, RunID  string
}

// CallStatus is the normalized result of one model call.
type CallStatus struct {
	SchemaVersion      int              `json:"schemaVersion"`
	CallID             string           `json:"callId"`
	RunID              string           `json:"runId"`
	Role               Role             `json:"role"`
	Phase              Phase            `json:"phase"`
	Adapter            string           `json:"adapter"`
	RequestedModel     string           `json:"requestedModel"`
	ActualModel        string           `json:"actualModel,omitempty"`
	NativeThinking     string           `json:"nativeThinking,omitempty"`
	ExitCode           int              `json:"exitCode"`
	VerificationStatus string           `json:"verificationStatus"`         // verified | self_reported | unverified
	IdentityEvidence   IdentityEvidence `json:"identityEvidence,omitempty"` // the evidence tier behind verificationStatus
	ResultArtifact     string           `json:"resultArtifact,omitempty"`
	StderrArtifact     string           `json:"stderrArtifact,omitempty"`
	HaltClass          *HaltClass       `json:"haltClass"`
	ReasonCode         string           `json:"reasonCode,omitempty"` // stable MACHINE code (never a sentence)
	// Signal is the classified actionability hint for a FAILED call (meshcore/clihint:
	// folder_trust | login_required | model_invalid | update_prompt | timeout). Absent
	// when the call succeeded or nothing classified.
	Signal           string    `json:"signal,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`         // reviewer attempts made (>1 ⇒ a schema-failure retry occurred)
	OutputNormalized bool      `json:"outputNormalized,omitempty"` // a fenced/surrounded JSON object was extracted before parsing
	CompletedAt      time.Time `json:"completedAt"`
	// parsed reviewer output (semantic_iterate / _cross_check / _verify):
	Summary  string    `json:"summary,omitempty"`
	Verdict  Verdict   `json:"verdict,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	// parsed host output (semantic_adjudicate):
	HostAdjudications []HostAdjudication `json:"hostAdjudications,omitempty"`
}

// HostAdjudication is the per-finding host judgment (typed host-adjudication payload).
type HostAdjudication struct {
	FindingID        string        `json:"findingId"`
	Validity         string        `json:"validity"` // "valid" | "invalid"
	DecisionState    DecisionState `json:"decisionState"`
	SeverityAdjusted Severity      `json:"severityAdjusted,omitempty"`
	Reasoning        string        `json:"reasoning,omitempty"`
}

// --- Resolution / planning types ---

// LaneResolution is the resolved executor for one role.
type LaneResolution struct {
	Role      Role
	Execution string // "host" | "adapter"
	Adapter   string
	Model     string // catalog key
	ModelArg  ModelArg
	Effort    string // resolved reasoning effort/thinking ("" = unspecified); opaque to the core
	// SeatID identifies a blind-primary PANEL seat ("reviewer" for seat 1, "reviewer-2",
	// "reviewer-3", … for the rest). It is set on the resolved PANEL roster only. It is EMPTY
	// on the single-slot lanes (author_remediator, cross_check, verifier), which are roles
	// rather than seats, and empty on the `Lanes[RoleReviewer]` compatibility alias — so the
	// role map that existing consumers (and the audit artifacts written from it) see is
	// byte-identical to a pre-panel build.
	SeatID string `json:",omitempty"`
	// ModelPassThrough marks a seat whose Model was NOT a model-catalog key and was handed to the
	// adapter verbatim. It is true only for a seat the CALLER COMPOSED at the surface
	// (`--reviewer`, an MCP `panel`), never for a lane a profile declared: a config file declares
	// its own vocabulary, so an unknown key there is a typo to be caught, while a composed seat is
	// a caller asserting intent about their own machine.
	//
	// It is RECORDED, never acted on. Nothing is refused, downgraded or gated because of it — the
	// ADAPTER is what carries trust and identity-evidence capability, and it is still validated
	// fail-closed against the configured set. See docs/review.md.
	ModelPassThrough bool `json:",omitempty"`
	// PassThroughHint names the catalog key this model most plausibly meant, when one is close
	// enough to be worth saying ("claude-opus-5" against a catalog holding "claude-opus-5-high").
	// Empty when nothing is close.
	//
	// It exists because pass-through turns a typo from a refusal into a silent call to a provider:
	// the run still proceeds — that is the point of pass-through — but the reader is told what we
	// noticed rather than left to discover it in a vendor error.
	PassThroughHint string `json:",omitempty"`
}

// RunPlan is the fully resolved plan for a review.
//
// NOTE on the reviewer panel: `Lanes[RoleReviewer]` is the panel's FIRST SEAT, kept in the role
// map so every role-shaped consumer (preflight, doctor, privacy, the ACP plan view) keeps
// working unchanged. It is an alias, not the panel: the authoritative, ordered roster is
// resolved separately (config.Config.ResolvePanel) and echoed on RunOutcome.Panel. Never infer
// the panel's size from this map.
type RunPlan struct {
	Mode    Mode
	Surface string
	Lanes   map[Role]LaneResolution
}

// CopyScope is what a disposable containment copy contains.
type CopyScope struct {
	Targets []string // files under review
	Context []string // extra files reviewers need
}

// Override is a per-lane invocation override.
type Override struct {
	Adapter string
	Model   string
}

// ReviewRequest is a review invocation (CUC-1).
type ReviewRequest struct {
	Scope      CopyScope
	Mode       Mode
	Profile    string
	Overrides  map[Role]Override
	ConfigPath string
}

// ArtifactState accumulates across a run; the addressed-context source.
type ArtifactState struct {
	Scope      CopyScope
	OuterCycle int
	Decisions  []Decision
}

// Outcome is satisfied by both RunOutcome and SetupOutcome.
type Outcome interface {
	ExitCode() int
	HaltClass() *HaltClass
}

// RunOutcome is the result of a review run.
type RunOutcome struct {
	// Status is the run's terminal state. A RETURNED outcome carries exactly one of
	// "stable", "planned" or "halted": Run sets "stable" on the success path, a DRY RUN sets
	// "planned" at the pre-spend stop (and is the only thing that does — a status a caller can
	// key on to tell "resolved and priced" from "resolved and reviewed", which they must never
	// confuse, since a planned run has zero findings for a reason that is not "there were
	// none"), and every error path goes through Manager.halt, which sets "halted". "running" is
	// the value the outcome is initialized with and is never observable to a caller (it is
	// overwritten before any return and before run-state.json is written).
	//
	// There is no "single-pass" Status and nothing assigns one — report and patch modes
	// ARE single-pass (the outer cycle breaks after one), but that fact is carried by
	// Mode, not by Status, so a branch keyed on Status for it could never be reached.
	Status   string // "running" (in-flight only) → "stable" | "halted"
	Mode     Mode
	Plan     RunPlan
	Findings []Finding
	// Decisions is the host's disposition for each finding, ALIGNED BY INDEX with
	// Findings (Decisions[i] decides Findings[i]). It is the same data run-state and the
	// review summary are written from, exposed on the outcome so a surface can build a
	// machine projection without re-reading run artifacts or re-deriving dispositions.
	// It may be shorter than Findings only if a caller constructs the outcome by hand;
	// consumers must bounds-check.
	Decisions []Decision
	Halt      *HaltClass
	// HaltReason is the STABLE MACHINE reason code for the halt (lower_snake, e.g.
	// "adapter_exited_nonzero", "scope_write_denied"); HaltSignal is the classified
	// actionability hint when the failure produced one. Both are empty on success. They
	// mirror what halt-record.json persists, so a surface reports the same codes the audit
	// trail does without re-reading the run directory.
	HaltReason string
	HaltSignal string
	RunDir     string
	// RunID is the opaque run identifier (also the run-directory name). Carried
	// explicitly so a consumer never has to parse it back out of RunDir.
	RunID string
	// Failure carries the specific lane that caused a halt (nil on success). It lets a surface
	// (e.g. the web-UI ACP validator) present the actionable underlying adapter failure — role,
	// adapter, model, exit code, and a captured stderr excerpt — instead of only the top-level
	// "adapter X exited N" message. The excerpt is raw here; surfaces sanitize/cap it.
	Failure *LaneFailure
	// IdentityCaveats records lanes that RAN and returned schema-valid output but whose model identity
	// is not fully verified (unknown or weak self-reported). It is populated only after a lane's
	// accepted attempt parses successfully, so a caveat always corresponds to a real, valid run. A
	// surface (the ACP validator) presents these as "passed with caveats"; the raw values here are
	// sanitized at the surface boundary.
	IdentityCaveats []IdentityCaveat
	// Panel is the EXECUTED-ROSTER ECHO of the blind primary panel: one entry per REQUESTED
	// seat, in requested order, with what actually happened to it. It is populated for every
	// run (a panel of one included), so a consumer never has to infer the panel's size — and a
	// seat that halted or never started is present and labelled rather than absent. It is the
	// same data written to `panel/roster.json` in the run record.
	Panel []SeatStatus
	// Authority is the INCLUSION MANIFEST for the authority/context documents this run was
	// judged against — one entry per declared document, with the full/embedded hashes and
	// byte counts. Empty when no authority was declared. It is the SAME data written to
	// `authority/inclusion-manifest.json` in the run record, carried here so a surface can
	// project it without re-reading the run directory.
	Authority []AuthorityInclusion
	// Withheld lists files a CONTAINMENT rule kept out of the reviewed set — see WithheldFile.
	// Empty for the overwhelmingly common run where nothing was withheld.
	Withheld []WithheldFile
	// Refusals lists the findings the write path refused because their target is a PROTECTED
	// PATH. It is empty for the overwhelmingly common run, and its non-emptiness is what makes a
	// patch/apply run "not cleanly successful" on every surface: the CLI exits 7, ACP answers
	// `stopReason: "refusal"`, MCP sets `isError: true`. Nothing was written to any of these
	// paths — the denylist is non-overridable — and every OTHER accepted finding was applied
	// normally, which is the whole point of recording the refusal instead of halting (D8-C).
	Refusals []ApplyRefusal
	// Selection is what a narrowing `select` did on this run's FIRST write window — requested,
	// matched and unmatched host-computed fingerprints (D8-A). nil when no selection was supplied.
	// It rides the outcome so the CLI and the ACP surface report the same three lists the receipt
	// records, rather than each re-deriving a subset of them.
	Selection *ApplySelection
	// Applied is how many findings the write path put into the live tree (apply) or the produced
	// diff (patch), summed over every outer cycle. It rides the outcome so a surface can report
	// `applied: 7, refused: 1` without recounting decision states — the two numbers must come
	// from the same place or a caller can be told a refusal happened and shown a total that does
	// not admit it.
	Applied int
	// ShownFiles are the workspace-relative files that were actually SHOWN to an applyable lane
	// (the blind panel and the cross-check), sorted. It is the apply-safety gate made explicit: the
	// host only ever edits a file a reviewer saw, so a finding naming a file nobody was shown is
	// never written.
	//
	// It rides the outcome because a LATER, separate remediation (the MCP surface's two-phase
	// `review_remediate --fromRun`, which applies an already-adjudicated set without re-reviewing)
	// must enforce exactly the same gate as the run that produced the decisions. Re-deriving it
	// there — from what happens to be on disk at apply time — would be a different, weaker rule.
	ShownFiles []string
	// Trace is the caller's W3C trace context, when the surface that accepted the request carried
	// one. nil for every CLI run and every legacy-era request. It rides the outcome so that the
	// SAME value reaches run-state.json on both the success and the halt path, without threading a
	// request through the halt writer — a halted run is the one most worth correlating.
	Trace *Trace // Grounding is the run-level tally of the non-executing citation check: how many findings pointed
	// at something real, how many did not, and the fixed note stating the limit of that claim. nil
	// when there was no reviewed tree to check against (the ACP validation probe), because an empty
	// report would assert a check that never ran.
	//
	// It is a TALLY over the per-finding labels on Decision.Grounding, not a second source of truth:
	// a surface that wants the detail walks the decisions, and a surface that wants the headline
	// reads this, and the two cannot disagree because one is computed from the other.
	Grounding *GroundingSummary
	// Composition is what the blind panel WAS — its seats and how many distinct models they ran
	// between them. nil for a run with no blind panel. It qualifies every agreement count in the
	// result without changing any of them.
	Composition *PanelComposition
	// Dissent is how much of this result the panel actually agreed on: the tally of the per-finding
	// consensus labels. nil when no finding had a describable panel behind it, because a record of
	// zeroes would suggest a panel that agreed on nothing rather than no panel at all.
	//
	// It qualifies nothing away. A contested finding is reported, adjudicated, applyable and counted
	// exactly as a unanimous one is — see the manager's dissent.go for why silence cannot be read as
	// disagreement.
	Dissent *DissentSummary
	// PartialPanel is present when a CAPACITY failure — a provider out of quota, a wall clock
	// reached — removed a seat and the run continued at a smaller denominator instead of discarding
	// the seats that had answered. Absent otherwise, so its presence is the signal.
	//
	// It is a *run.PartialPanel, held as `any` here because the type lives in the manager package
	// alongside the classification that produces it; every surface projects it through the one
	// derivation rather than reaching into it.
	PartialPanel any
	// Scope is present when this run was NARROWED — explicit paths, a time window, or a
	// version-control baseline. Absent on a full-tree run, so its presence is the signal that the
	// review's silence covers only part of the tree.
	//
	// A *run.ScopeSummary, held as `any` for the same reason PartialPanel is.
	Scope any
	// Verification is what the project's own build/test commands did on the containment copy, when
	// the operator supplied any. nil otherwise, which is every run that named no command — so the
	// default run's record is unchanged by the feature existing.
	Verification *VerificationReport
	// Shape is what the run WOULD have done, and is populated ONLY on a dry run
	// (Request.DryRun) — nil on every real run, whose shape is no longer a prediction. See
	// RunShape.
	Shape *RunShape
}

// RunShape is WHAT A REVIEW WOULD DO, disclosed BEFORE anything is spent: the answer to "what
// is this about to cost me", asked by someone who has not yet agreed to pay it.
//
// It is produced by a DRY RUN and by nothing else. A dry run resolves the plan, the panel, the
// authority documents and the static preflight exactly as a real run does — writing the same
// resolved-plan.json, config-effective.json and doctor.json — and then STOPS, in the window
// after the last knowable configuration error and before the first model call. So every seat
// listed here is a seat that RESOLVED, and every adapter named here is one that answered
// Available. A shape is not a promise that the run will succeed; it is a statement that
// nothing left between here and the first call is a configuration question.
//
// THE CALL COUNTS ARE A RANGE, AND THAT IS NOT HEDGING. MinModelCalls is what this run cannot
// avoid spending; MaxModelCalls is what every configured cap multiplies out to. A single
// number would have to be one of three wrong things: the floor (understating a run that
// iterates), the ceiling (overstating the ordinary run that converges on its first cycle), or
// a guess about model behaviour aimesh does not have. Both bounds count LOGICAL calls — a
// bounded retry after a schema-invalid response is an additional adapter invocation that
// neither bound includes, which the rendered disclosure says out loud.
type RunShape struct {
	Mode    Mode   `json:"mode"`
	Surface string `json:"surface"`
	// Seats is the blind primary panel in REQUESTED ORDER. Every seat runs; MaxParallel bounds
	// only how many run at once, so shortening this list is the only thing that removes a call.
	Seats []ShapeSeat `json:"seats"`
	// Lanes is every OTHER resolved role (author_remediator, cross_check, verifier), sorted by
	// role name. A role ABSENT from this list is a call this run will not make.
	Lanes []ShapeLane `json:"lanes"`
	// MaxParallel is the requested parallelism bound; 0 runs every seat at once. It changes how
	// long the run takes and never how much it costs.
	MaxParallel int `json:"maxParallel"`
	// OuterCycles is how many times the whole review cycle can repeat. It is 1 for report and
	// patch whatever the configuration says, because neither commits anything and there is
	// therefore nothing to converge.
	OuterCycles int `json:"outerCycles"`
	// PanelRounds is the run-level budget for ONE cycle's panel: the total blind rounds all
	// seats SHARE, never a per-seat figure.
	PanelRounds int `json:"panelRounds"`
	// MinModelCalls and MaxModelCalls bound this run's model invocations across every lane and
	// every cycle. See the type comment for why this is a range rather than a number.
	MinModelCalls int `json:"minModelCalls"`
	MaxModelCalls int `json:"maxModelCalls"`
	// Egress is WHERE THIS RUN'S CONTENT GOES: one entry per distinct destination, naming the seats
	// and lanes that reach it. It is the fact a dry run is most worth reading for — the other
	// numbers price the run, this one says who ends up holding the reviewed code.
	//
	// It is computed from the RESOLVED panel, so it describes this run rather than a table someone
	// has to cross-reference by hand: an ad-hoc panel with no profile gets an accurate answer, and a
	// destination that is unknown says so rather than being omitted.
	Egress []ShapeEgress `json:"egress,omitempty"`
	// ReadinessProbes is how many one-token readiness invocations this run would make before
	// dispatching anything — one per DISTINCT adapter/model/effort, and 0 unless the caller asked
	// for them. They are counted in BOTH bounds because, unlike everything else in the ceiling,
	// they are not contingent on anything: a run that enables them cannot make fewer.
	//
	// They are cheap on purpose and they buy the expensive thing: without them a panel pays for its
	// first seat's whole prompt before discovering that a later seat's CLI was blocked on login.
	ReadinessProbes int `json:"readinessProbes"`
	// Writes says whether the effective mode can modify the workspace. The dry run that
	// produced this shape wrote nothing regardless of what it says.
	Writes bool `json:"writes"`
	// DeterministicHost records that adjudication needs no model at all — no author_remediator
	// lane is configured, or it resolves to the `fake` adapter. When true, every adjudication
	// this run performs is in-process and free, which is why MinModelCalls can be well under
	// MaxModelCalls for the same panel.
	DeterministicHost bool `json:"deterministicHost"`
	// Payload is WHAT THOSE CALLS WOULD CARRY. The call counts price the run; this says what the
	// run would be looking at, which is the other half of the same question and the half a
	// reader cannot infer. See ShapePayload.
	Payload ShapePayload `json:"payload"`
}

// ShapePayload is the workspace content a run would put in front of its reviewers, disclosed
// before anything is copied or sent.
//
// WHY THE SHAPE NEEDS IT. A call count is the same for `--dry-run .` over a monorepo and
// `--dry-run internal/review/manager/run` over one package — measured 2026-08-11, the two printed
// byte-identical disclosures — because the panel, the lanes and the caps are identical. What
// differs is entirely here: which files the reviewers see and how many bytes reach them.
//
// IT IS THE WHOLE WORKSPACE, minus only what containment excludes. There is no size or count
// budget in the collector any more (see meshcore/workspace.Snippet for why the old one was
// indefensible), so Files and Bytes are a measurement rather than a bound, and every file counted
// here is carried in full.
//
// It is computed by walking the LIVE tree under the same rules the run's own collector uses (see
// meshcore/workspace.PreviewPayload), so it is what the run would carry rather than an estimate
// of it — but it is computed EARLIER, and a workspace edited between the dry run and the real one
// is a different workspace.
type ShapePayload struct {
	// Files and Bytes are what the prompt would carry: every file that survived containment, and
	// their total size. Bytes is the figure worth reading before a large panel — it is sent once
	// per seat, per round.
	Files int `json:"files"`
	Bytes int `json:"bytes"`
	// Paths is every file that would be shown, sorted. A count answers "how much"; only the list
	// answers "is the file I care about in there", which is the question that decides whether the
	// run is worth paying for.
	Paths []string `json:"paths"`
	// Withheld is what containment kept out — a hardlinked file whose in-root name may be a
	// second name for something outside it — rendered with its reason. Empty on an ordinary
	// workspace. It is disclosed here for the reason it is recorded on a real run: an omission
	// nobody is told about is indistinguishable from a file that never existed.
	Withheld []string `json:"withheld,omitempty"`
}

// ShapeSeat is one blind primary panel seat as a dry run would invoke it.
type ShapeSeat struct {
	SeatID  string `json:"seatId"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// ShapeEgress is one destination this run's content would reach, and who reaches it.
//
// Grouped BY DESTINATION rather than listed per seat, because the question it answers is "who ends
// up with my code", and four seats behind one provider is one disclosure, not four.
type ShapeEgress struct {
	// Destination is the party that receives the content, in the words a user would recognise.
	// Empty exactly when Known is false.
	Destination string `json:"destination,omitempty"`
	// Local is true when nothing leaves the machine. A consumer deciding whether a run is offline
	// branches on THIS, never on the destination string.
	Local bool `json:"local,omitempty"`
	// Known is false for an adapter this tool does not own — a user-defined ACP instance points at
	// a binary the operator chose, so its destination is theirs to know. Reported as unknown rather
	// than omitted: a missing row would read as "nothing goes there".
	Known bool `json:"known"`
	// Note qualifies a destination that routes onward (a gateway) or that the operator selected.
	Note string `json:"note,omitempty"`
	// Via names the seats and roles that reach this destination, in run order.
	Via []string `json:"via"`
}

// ShapeLane is one non-panel role as a dry run would invoke it. Execution "host" runs
// in-process and costs no model call; "adapter" invokes a model CLI.
type ShapeLane struct {
	Role      Role   `json:"role"`
	Execution string `json:"execution"`
	Adapter   string `json:"adapter"`
	Model     string `json:"model"`
	Effort    string `json:"effort,omitempty"`
}

// Trace is the caller's W3C trace context, CARRIED VERBATIM from the request that started a run so
// the run directory can be correlated with the caller's own trace.
//
// It is carried, never interpreted, and never validated. The `2026-07-28` base-protocol page reserves
// the three unprefixed keys — "As an exception to the prefix requirement above, the keys
// `traceparent`, `tracestate`, and `baggage` are reserved for OpenTelemetry trace context
// propagation. When present, their values MUST follow W3C Trace Context and W3C Baggage formats
// respectively." — and says of every reserved `_meta` key that "implementations MUST NOT make
// assumptions about values at these keys". That format MUST binds the SENDER; refusing a malformed
// `traceparent` here would be this server asserting a meaning for a field it does not own, so a
// malformed value is recorded exactly as it arrived and the caller's own tooling decides.
//
// WHAT IT MEASURES, stated inside the record itself via Measures: correlation, and nothing else. No
// run in this repo records token counts or cost — that recorded gap is why any spend figure quoted
// about a run is an ESTIMATE and must say so. A trace id makes a run externally correlatable with
// whatever the caller DOES meter; it does not make this run metered, and a reader of
// `run-state.json` must not infer that it does.
type Trace struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
	Baggage     string `json:"baggage,omitempty"`
	// Measures names what this block does and does not account for. It is a constant today
	// (MeasuresCorrelationOnly); the day a run records tokens or cost the value changes, so a
	// consumer can tell the two eras of the record apart instead of inferring it from which
	// fields happen to be present.
	Measures string `json:"measures"`
}

// MeasuresCorrelationOnly is Trace.Measures' only value today: this run carries a correlation
// identity and no token or cost accounting.
const MeasuresCorrelationOnly = "correlation_only"

// NewTrace returns the run-record block for a caller's trace context, or nil when the caller sent
// none — nil is what keeps a trace-free run's `run-state.json` byte-identical to before the field
// existed, which is what lets this ship without moving the artifact's schema version.
func NewTrace(traceParent, traceState, baggage string) *Trace {
	if traceParent == "" && traceState == "" && baggage == "" {
		return nil
	}
	return &Trace{
		TraceParent: traceParent, TraceState: traceState, Baggage: baggage,
		Measures: MeasuresCorrelationOnly,
	}
}

// WithheldFile is one file that a containment rule deliberately kept OUT of the reviewed set:
// not copied into the isolated workspace, or not collected into the prompt.
//
// It exists because the alternative is a SILENT OMISSION. meshcore withholds a hardlinked
// regular file rather than halting the run (its innocuous in-tree name may be a second name
// for `~/.ssh/id_rsa`, but an in-tree `cp -al`/dedup hardlink is ordinary, and halting on one
// would let any writer in a trusted tree force a halt at will). Withholding without saying so
// would make "this file was not reviewed" indistinguishable from "there was no such file" —
// and a reviewer's silence about a file it was never shown reads as approval. So the fact is
// carried on the outcome, written to the run record, and projected to the caller.
type WithheldFile struct {
	// Path is workspace-relative, slash-normalized.
	Path string
	// Reason is meshcore's STABLE machine code for the rule that withheld it (e.g.
	// "workspace_hardlink_denied"), so a consumer branches on a code, not on prose.
	Reason string
	// Rule is the human sentence naming what refused, and Detail its specifics (e.g. the
	// link count). Either may be empty.
	Rule   string
	Detail string
	// Stage is WHERE it was withheld: "copy" (never placed in the isolated copy) or
	// "snippets" (in the copy, but never rendered into a prompt). The distinction matters to
	// a reader: a file withheld at "copy" was also never visible to an agentic reviewer CLI
	// working in that directory.
	Stage string
}

// IdentityCaveat is one lane whose model identity could not be fully verified even though it ran and
// returned schema-valid output (Status is VerifUnknown or VerifSelfReported — never a mismatch, which
// halts). Raw config identifiers here are sanitized/display-mapped at a surface boundary.
type IdentityCaveat struct {
	Role           string
	Adapter        string           // stable adapter key
	RequestedModel string           // the requested model arg / catalog key
	Evidence       IdentityEvidence // produced evidence tier ("" / none when unknown)
	Status         string           // VerifUnknown | VerifSelfReported
	// ReportedModel is the model the adapter actually reported, when it reported one. For a matching
	// self-report it echoes the confirmed model; for a WEAK non-match (a self-report that named a
	// DIFFERENT model — downgraded to unknown, never a mismatch/halt) it preserves that suspicious
	// signal instead of hiding it. Empty when nothing was reported. Sanitized at a surface boundary.
	ReportedModel string
}

// LaneFailure describes the lane whose adapter caused a review halt.
type LaneFailure struct {
	Role          string // e.g. "reviewer", "author_remediator"
	Adapter       string // stable adapter key (e.g. "codex-cli")
	Model         string // catalog key the lane referenced
	ModelArg      string // effective model argument passed to the CLI
	ExitCode      int    // adapter process exit code (0 when the halt is not an exit)
	HaltClass     string // e.g. "A" (adapter exited non-zero), "E" (identity)
	ReasonCode    string // e.g. "adapter_exited_nonzero"
	StderrExcerpt string // RAW adapter stderr (surfaces sanitize + cap before display)
	StdoutExcerpt string // RAW adapter stdout (some CLIs print actionable errors here)
	// Signal is the classified, actionable hint for this failure (meshcore/clihint:
	// folder_trust | login_required | model_invalid | update_prompt | timeout), or "" when
	// nothing classified. It is derived ONCE, here, so the hint a human is shown and the
	// signal a machine consumer reads can never disagree.
	Signal string
}

// ExitCode implements Outcome.
func (o RunOutcome) ExitCode() int {
	if o.Halt != nil {
		return 1
	}
	return 0
}

// HaltClass implements Outcome.
func (o RunOutcome) HaltClass() *HaltClass { return o.Halt }
