package review

import (
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

// Generic model-operation primitives live in meshcore/core; this package re-exports them so app
// code can refer to review.X while the substrate packages depend on core directly.

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

// Verdict is a reviewer or adjudication outcome for a whole pass.
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
	// PhaseRemediate labels a remediation call: the author_remediator turns one accepted finding
	// into an anchored Edit against its target file (patch and apply modes only).
	PhaseRemediate Phase = "semantic_remediate"
	// PhaseValidateAdjudicate labels a host-adjudication call made only to prove the
	// author_remediator can run during ACP validation. It is a CallStatus.Phase label; the prompt and
	// output use PhaseAdjudicate's schema.
	PhaseValidateAdjudicate Phase = "validate_adjudicate"
)

// DecisionState is the disposition of a finding, fed forward as addressed context.
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

// The blind primary stage is a panel of one or more ordered seats. cross_check and verifier are
// single phase lanes with different information topologies, so they never join the panel.

// MaxReviewerSeats is the ceiling on panel size. Each seat is a real model CLI call, so the panel
// is bounded; the value matches exploremesh's ACP fan-out cap.
const MaxReviewerSeats = 16

// SeatSpec is a requested blind-primary seat as a surface composes it (CLI `--reviewer`, an MCP or
// ACP panel).
type SeatSpec struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// Identity returns the seat's uniqueness key, the full (adapter, model, effort) triple. A panel
// naming the same triple twice is a configuration error rather than a doubled agreement.
func (s SeatSpec) Identity() string { return s.Adapter + "\x00" + s.Model + "\x00" + s.Effort }

// Seat identity tiers: the verification statuses of meshcore/verify, as the provenance ledger names
// them. A tier describes a seat's support and never changes what happens to a finding (see
// docs/model-identity.md).
const (
	SeatIdentityVerified     = VerifVerified
	SeatIdentitySelfReported = VerifSelfReported
	SeatIdentityUnknown      = VerifUnknown
)

// ApplyRefusalProtectedPath is the refusal reason for a finding whose target is a protected path
// (meshcore's non-overridable write denylist). The finding is not applied, the remaining findings
// are, and the run is reported as not cleanly successful.
const ApplyRefusalProtectedPath = "protected_path"

// ApplyRefusal is one finding refused because its target is a protected path. It is keyed on the
// host-computed fingerprint rather than the model-authored Finding.ID, which a model controls and
// which is renumbered after the write window.
type ApplyRefusal struct {
	// Fingerprint is the host-computed identity a caller can act on in a follow-up selection.
	Fingerprint string `json:"fingerprint"`
	// File is the workspace-relative target that was refused.
	File string `json:"file"`
	// Reason is the machine code, currently always ApplyRefusalProtectedPath.
	Reason string `json:"reason"`
	// FindingID is the finding's id as the write path saw it. It feeds the receipt and decision
	// reconciliation and is never projected onto a wire payload.
	FindingID string `json:"-"`
}

// ApplySelection records a selective apply: the fingerprints a caller asked to narrow the write set
// to, which matched a finding in the run's accepted set, and which matched nothing.
//
// The key is the host-computed fingerprint, never the model-authored Finding.ID: a model could
// relabel its own ids until a selection named something else. A selection only reduces what is
// written; an unmatched fingerprint is dropped and reported.
type ApplySelection struct {
	// Requested is the caller's list, trimmed and deduplicated, in the given order.
	Requested []string `json:"requested"`
	// Matched are the requested fingerprints that named a finding in this run's accepted set.
	Matched []string `json:"matched"`
	// Unmatched are the requested fingerprints that named nothing and wrote nothing.
	Unmatched []string `json:"unmatched"`
}

// Selective reports whether a selection was supplied. The zero value means the whole accepted set
// is applied.
func (s *ApplySelection) Selective() bool { return s != nil && len(s.Requested) > 0 }

// Apply outcomes, carried on every write-producing result. The coarse not-clean signal each surface
// carries (MCP isError, the CLI exit code, ACP stopReason) is keyed on counts.refused, not on this
// value.
const (
	// OutcomeApplied means every writable accepted finding was written and none was refused.
	OutcomeApplied = "applied"
	// OutcomePartialRefusal means at least one finding was applied and at least one was refused.
	OutcomePartialRefusal = "partial_refusal"
	// OutcomeNothingApplied means no finding was applied: either there was nothing to do or every
	// accepted finding was refused, which counts.refused distinguishes.
	OutcomeNothingApplied = "nothing_applied"
)

// ApplyOutcome derives the outcome discriminator from the applied and refused counts. applied
// counts findings whose hunks reached the live tree (apply) or the diff (patch); refused counts
// protected-path refusals.
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

// SeatRef is one seat's contribution to a finding in the provenance ledger. Every field is recorded
// by the host from the resolved panel and its own identity verification; no model asserts any of it.
type SeatRef struct {
	SeatID       string `json:"seatId"`
	Adapter      string `json:"adapter"`
	Model        string `json:"model"`
	IdentityTier string `json:"identityTier"`
}

// SeatStatus is one seat in the executed roster: what was requested, what ran, and how it ended.
// Every requested seat appears exactly once, including one that halted the run.
type SeatStatus struct {
	SeatID  string `json:"seatId"`
	Index   int    `json:"index"` // 1-based position in the requested panel
	Adapter string `json:"adapter"`
	Model   string `json:"model"` // catalog key
	Effort  string `json:"effort,omitempty"`
	// Status is the seat's terminal state:
	//   completed     — ran and produced a schema-valid result
	//   budget_capped — completed, but the run-level round budget stopped further rounds
	//   halted        — this seat's failure halted the run (identity, adapter, containment, schema)
	//   not_started   — the run halted on an earlier seat before this one produced a result
	Status string `json:"status"`
	// Rounds is how many stabilization rounds this seat ran; Findings is how many raw findings it
	// reported across them, before host deduplication.
	Rounds   int `json:"rounds"`
	Findings int `json:"findings"`
	// IdentityTier is the strongest identity classification observed for this seat's calls.
	IdentityTier string `json:"identityTier,omitempty"`
	// ReasonCode is the machine code when the seat did not complete normally.
	ReasonCode string `json:"reasonCode,omitempty"`
	// Signal and Detail are this seat's own cause. The run-level Failure names only the first
	// halting seat, and other seats may have failed for different, independently fixable reasons.
	// Signal is the meshcore/clihint classification (folder_trust | login_required | model_invalid |
	// update_prompt | timeout, or empty); Detail is a one-line cause. Both are empty on completion.
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
	// SupportingSeats, AgreementCount and DissentingSeats are the host-computed provenance ledger
	// for a panel finding. They live on the decision rather than the model-parsed Finding so no model
	// can assert agreement. AgreementCount is len(SupportingSeats). DissentingSeats are seats that
	// completed and did not report the finding; a halted or unstarted seat is in neither list.
	SupportingSeats []SeatRef `json:"supportingSeats,omitempty"`
	AgreementCount  int       `json:"agreementCount,omitempty"`
	DissentingSeats []string  `json:"dissentingSeats,omitempty"`
	// DistinctModels is how many different models the supporting seats ran. It is not an adjusted
	// agreement figure. It is absent when no seat supports the finding (an evidence hook, the
	// cross-check, the verifier).
	DistinctModels int `json:"distinctModels,omitempty"`
	// AgreementIndependence is `distinct_models`, or `shared_model` when at least two supporting
	// seats ran the same model and their errors may correlate. Only the model string is compared;
	// vendor and model lineage are not observable from it.
	AgreementIndependence string `json:"agreementIndependence,omitempty"`
	// Consensus is what the completed seats did with this finding: `unanimous`, `majority`, or
	// `contested` (a minority reported it, or the seats split evenly).
	//
	// Seats are blind, so a seat that did not report a finding may simply not have looked there.
	// `contested` marks a finding worth reading closely; nothing is dropped, downgraded, reordered or
	// gated on it. Absent without a panel or when fewer than two seats completed.
	Consensus string `json:"consensus,omitempty"`
	// Applyable is set to false only when the write-path rule refuses the finding: an applied hunk
	// must trace to evidence in the workspace copy, so an authority-only finding is never applied.
	// nil means the rule did not refuse it; mode and disposition still govern writing.
	Applyable *bool `json:"applyable,omitempty"`
	// ApplyRefusalReason is the machine code for that refusal ("authority_only" |
	// "no_workspace_evidence" | "protected_path"); empty when Applyable is nil. `protected_path` is
	// decided by the write path itself and is the only reason that makes a run not cleanly successful.
	ApplyRefusalReason string `json:"applyRefusalReason,omitempty"`
	// Grounding is the host's non-executing check of the finding's citation: whether the cited file,
	// lines and symbol exist. It is a label only; nothing in the engine reads it back, because a
	// finding with a wrong citation may still describe a real defect.
	Grounding *CitationGrounding `json:"grounding,omitempty"`
}

// CitationGrounding is what a run verified about a finding's citation without executing anything.
// `grounded` means the citation points at something real, not that the finding is correct.
type CitationGrounding struct {
	// File and Location echo the citation as the finding gave it.
	File     string `json:"file,omitempty"`
	Location string `json:"location,omitempty"`
	// Status is a machine code: grounded | no_citation | file_missing | line_out_of_range |
	// symbol_absent | location_unchecked | unreadable.
	Status string `json:"status"`
	// Detail says what was checked and what happened; it is never a verdict on the finding.
	Detail string `json:"detail,omitempty"`
	// LineCount is the cited file's length when read, so a line-range failure is checkable.
	LineCount int `json:"lineCount,omitempty"`
}

// VerificationReport is what the project's own build and test commands did on the containment copy,
// before and after the accepted findings were applied to it.
//
// It is a record, not a gate: no result invalidates, drops or downgrades a finding or blocks the
// commit. Read Delta first; many repositories have a failing suite, and what matters is whether the
// commands answered differently afterwards.
type VerificationReport struct {
	// Root is the containment copy the commands ran in, never the live workspace.
	Root string `json:"root"`
	// Commands is the operator's list as given.
	Commands []string `json:"commands"`
	// Before is the baseline pass on the unedited copy; After is the pass after every edit and before
	// the commit. After is empty on a report run (Delta baseline_only).
	Before []VerificationResult `json:"before,omitempty"`
	After  []VerificationResult `json:"after,omitempty"`
	// Delta is the host's comparison: unchanged_pass | unchanged_fail | fixed | broken |
	// baseline_only | not_comparable.
	Delta string `json:"delta"`
	// Note is the fixed sentence stating the report's limits.
	Note string `json:"note"`
}

// VerificationResult is one command's outcome in one pass.
type VerificationResult struct {
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exitCode"`
	// TimedOut marks a command that hit its budget. It is kept apart from a non-zero exit because a
	// timeout makes the passes not comparable rather than blaming the change.
	TimedOut bool `json:"timedOut,omitempty"`
	// Unstartable is the reason the command could not be launched (no shell, a bad directory).
	Unstartable string `json:"unstartable,omitempty"`
	DurationMs  int64  `json:"durationMs"`
	// Output is the bounded tail of the combined streams, where build tools print their errors.
	Output string `json:"output,omitempty"`
}

// PanelComposition records what the blind panel was: each seat as configured and how many distinct
// models the seats ran. Agreement counts are over seats, and seats running one model are not
// independent; this qualifies the counts without changing them.
type PanelComposition struct {
	// Seats is the blind panel in requested order.
	Seats []SeatComposition `json:"seats"`
	// DistinctModels is how many different models the seats run.
	DistinctModels int `json:"distinctModels"`
	// Independence is `distinct_models` or `shared_model` for the panel as a whole.
	Independence string `json:"independence"`
	// Note is the fixed sentence stating the limit of the independence claim.
	Note string `json:"note"`
}

// SeatComposition is one blind-panel seat as configured, in configured order.
type SeatComposition struct {
	SeatID  string `json:"seatId"`
	Index   int    `json:"index"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
	// ModelSource is `catalog` (a modelCatalog key resolved to its adapter argument) or
	// `passthrough` (a composed seat's model string handed to the adapter verbatim). It is recorded
	// only; nothing about the seat's findings changes.
	ModelSource string `json:"modelSource,omitempty"`
	// PassThroughHint names the catalog key a pass-through model most plausibly meant, when one is
	// close.
	PassThroughHint string `json:"passThroughHint,omitempty"`
}

// DissentSummary tallies the per-finding consensus labels so a surface can report how much of a
// result the panel agreed on, always with the denominator. It is derived from Decision.Consensus.
type DissentSummary struct {
	// Panelled is how many findings had a describable panel: Unanimous + Majority + Contested.
	Panelled  int `json:"panelled"`
	Unanimous int `json:"unanimous"`
	Majority  int `json:"majority"`
	Contested int `json:"contested"`
	// Note is the fixed sentence stating what a seat's silence does and does not mean.
	Note string `json:"note"`
}

// GroundingSummary tallies the citation check across a run's findings, always with the denominator.
type GroundingSummary struct {
	// Root is the tree the citations were checked against.
	Root string `json:"root"`
	// Checked is how many findings carried a citation; Grounded + Unresolved equals Checked.
	Checked    int `json:"checked"`
	Grounded   int `json:"grounded"`
	Unresolved int `json:"unresolved"`
	// NoCitation counts findings that name no file. It is not part of Checked.
	NoCitation int `json:"noCitation"`
	// ByStatus is the breakdown by CitationGrounding status code.
	ByStatus map[string]int `json:"byStatus,omitempty"`
	// Note is the fixed sentence stating the limit of the check.
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
	ReasonCode         string           `json:"reasonCode,omitempty"` // machine code, never a sentence
	// Signal is the meshcore/clihint classification for a failed call (folder_trust |
	// login_required | model_invalid | update_prompt | timeout); absent otherwise.
	Signal           string    `json:"signal,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`         // reviewer attempts made; >1 means a schema-failure retry occurred
	OutputNormalized bool      `json:"outputNormalized,omitempty"` // a fenced or surrounded JSON object was extracted before parsing
	CompletedAt      time.Time `json:"completedAt"`
	// Parsed reviewer output (semantic_iterate, semantic_cross_check, semantic_verify).
	Summary  string    `json:"summary,omitempty"`
	Verdict  Verdict   `json:"verdict,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	// Parsed host output (semantic_adjudicate).
	HostAdjudications []HostAdjudication `json:"hostAdjudications,omitempty"`
}

// HostAdjudication is the host's typed judgment of one finding.
type HostAdjudication struct {
	FindingID        string        `json:"findingId"`
	Validity         string        `json:"validity"` // "valid" | "invalid"
	DecisionState    DecisionState `json:"decisionState"`
	SeverityAdjusted Severity      `json:"severityAdjusted,omitempty"`
	Reasoning        string        `json:"reasoning,omitempty"`
}

// LaneResolution is the resolved executor for one role.
type LaneResolution struct {
	Role      Role
	Execution string // "host" | "adapter"
	Adapter   string
	Model     string // catalog key
	ModelArg  ModelArg
	Effort    string // resolved reasoning effort ("" = unspecified); opaque to the core
	// SeatID identifies a panel seat ("reviewer" for seat 1, then "reviewer-2", …). It is empty on
	// the single-role lanes and on the Lanes[RoleReviewer] alias, which keeps the role map and the
	// audit artifacts written from it stable.
	SeatID string `json:",omitempty"`
	// ModelPassThrough marks a seat whose Model was not a catalog key and was handed to the adapter
	// verbatim. It is set only for a seat the caller composed; an unknown key in a profile is a typo
	// and is refused. It is recorded only; the adapter is still validated (see docs/review.md).
	ModelPassThrough bool `json:",omitempty"`
	// PassThroughHint names the catalog key this model most plausibly meant, when one is close
	// ("claude-opus-5" against a catalog holding "claude-opus-5-high"), so a typo is visible rather
	// than surfacing only as a provider error.
	PassThroughHint string `json:",omitempty"`
}

// RunPlan is the fully resolved plan for a review.
//
// Lanes[RoleReviewer] is the panel's first seat, kept so role-shaped consumers (preflight, doctor,
// privacy, the ACP plan view) work unchanged. The ordered panel is resolved separately
// (config.Config.ResolvePanel) and reported on RunOutcome.Panel; never infer panel size from Lanes.
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

// ReviewRequest is a review invocation.
type ReviewRequest struct {
	Scope      CopyScope
	Mode       Mode
	Profile    string
	Overrides  map[Role]Override
	ConfigPath string
}

// ArtifactState accumulates across a run and is the source of addressed context.
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
	// Status is the run's terminal state: "stable" on success, "planned" for a dry run stopped
	// before any spend, or "halted" (set by Manager.halt). "running" is the initial value and is
	// never returned. Whether a run was single-pass is carried by Mode, not Status.
	Status   string
	Mode     Mode
	Plan     RunPlan
	Findings []Finding
	// Decisions is the host's disposition for each finding, aligned by index with Findings. A
	// hand-built outcome may be shorter, so consumers must bounds-check.
	Decisions []Decision
	Halt      *HaltClass
	// HaltReason is the machine reason code for the halt (e.g. "adapter_exited_nonzero"), and
	// HaltSignal the classified hint when there is one. Both are empty on success and match
	// halt-record.json.
	HaltReason string
	HaltSignal string
	RunDir     string
	// RunID is the opaque run identifier, which is also the run directory's name.
	RunID string
	// Failure is the lane that caused a halt (nil on success): role, adapter, model, exit code and a
	// raw stderr excerpt that surfaces sanitize and cap.
	Failure *LaneFailure
	// IdentityCaveats lists lanes that ran and returned schema-valid output but whose model identity
	// is not fully verified. Values are raw; surfaces sanitize them.
	IdentityCaveats []IdentityCaveat
	// Panel is the executed roster: one entry per requested seat, in order, with what happened to
	// it. It matches panel/roster.json in the run record.
	Panel []SeatStatus
	// Authority is the inclusion manifest for the authority documents the run was judged against,
	// matching authority/inclusion-manifest.json. Empty when none were declared.
	Authority []AuthorityInclusion
	// Withheld lists files a containment rule kept out of the reviewed set.
	Withheld []WithheldFile
	// Refusals lists findings the write path refused because their target is a protected path.
	// A non-empty list makes a patch or apply run not cleanly successful on every surface (CLI exit
	// 7, ACP stopReason "refusal", MCP isError); every other accepted finding was still applied.
	Refusals []ApplyRefusal
	// Selection is what a narrowing `select` did on the first write window; nil when none was given.
	Selection *ApplySelection
	// Applied is how many findings the write path put into the live tree (apply) or the diff
	// (patch), summed over outer cycles, so it comes from the same place as Refusals.
	Applied int
	// ShownFiles are the sorted workspace-relative files shown to an applyable lane (the panel and
	// the cross-check). The host only edits a file a reviewer saw, and a later remediation from this
	// run (MCP `review_remediate`) enforces the same set rather than re-deriving it from disk.
	ShownFiles []string
	// Trace is the caller's W3C trace context, when the accepting surface carried one. It rides the
	// outcome so halted runs record it too.
	Trace *Trace
	// Grounding tallies the citation check over Decision.Grounding. nil when there was no reviewed
	// tree to check (the ACP validation probe).
	Grounding *GroundingSummary
	// Composition is what the blind panel was; nil for a run with no panel.
	Composition *PanelComposition
	// Dissent tallies the per-finding consensus labels; nil when no finding had a describable panel.
	// A contested finding is handled exactly like a unanimous one.
	Dissent *DissentSummary
	// PartialPanel is set when a capacity failure (quota, a wall clock) removed a seat and the run
	// continued with the remaining seats. It holds a *run.PartialPanel; the type lives in the manager
	// package beside the classification that produces it.
	PartialPanel any
	// Scope is set when the run was narrowed by explicit paths, a time window or a VCS baseline. It
	// holds a *run.ScopeSummary.
	Scope any
	// Verification is what the operator's build and test commands did on the containment copy; nil
	// when no command was supplied.
	Verification *VerificationReport
	// Shape is what the run would have done. It is set only on a dry run (Request.DryRun).
	Shape *RunShape
}

// RunShape is what a review would do, disclosed before anything is spent.
//
// A dry run resolves the plan, panel, authority documents and static preflight exactly as a real run
// does, writes the same resolved-plan.json, config-effective.json and doctor.json, and stops before
// the first model call. Every listed seat resolved and every named adapter was available; a shape is
// not a promise that the run will succeed.
//
// Call counts are a range: MinModelCalls is what the run cannot avoid, MaxModelCalls is every
// configured cap multiplied out. Both count logical calls; a retry after a schema-invalid response
// is an additional invocation neither bound includes.
type RunShape struct {
	Mode    Mode   `json:"mode"`
	Surface string `json:"surface"`
	// Seats is the blind panel in requested order. Every seat runs; MaxParallel bounds only
	// concurrency.
	Seats []ShapeSeat `json:"seats"`
	// Lanes is every other resolved role, sorted by role name. An absent role is a call the run
	// will not make.
	Lanes []ShapeLane `json:"lanes"`
	// SkippedSteps names the optional steps (cross_check, verifier) that have no seat.
	SkippedSteps []string `json:"skippedSteps,omitempty"`
	// MaxParallel is the requested parallelism bound; 0 runs every seat at once.
	MaxParallel int `json:"maxParallel"`
	// OuterCycles is how many times the review cycle can repeat; always 1 for report and patch.
	OuterCycles int `json:"outerCycles"`
	// PanelRounds is the total blind rounds all seats share in one cycle.
	PanelRounds int `json:"panelRounds"`
	// MinModelCalls and MaxModelCalls bound the run's model invocations across every lane and cycle.
	MinModelCalls int `json:"minModelCalls"`
	MaxModelCalls int `json:"maxModelCalls"`
	// Egress is where the run's content goes: one entry per distinct destination, naming the seats
	// and lanes that reach it, computed from the resolved panel.
	Egress []ShapeEgress `json:"egress,omitempty"`
	// ReadinessProbes is how many one-token readiness calls the run makes before dispatch: one per
	// distinct adapter/model/effort, and 0 unless requested. They are counted in both bounds.
	ReadinessProbes int `json:"readinessProbes"`
	// Writes reports whether the effective mode can modify the workspace. The dry run itself wrote
	// nothing.
	Writes bool `json:"writes"`
	// DeterministicHost reports that adjudication needs no model call: no author_remediator lane is
	// configured, or it resolves to the `fake` adapter.
	DeterministicHost bool `json:"deterministicHost"`
	// Payload is the workspace content the calls would carry.
	Payload ShapePayload `json:"payload"`
}

// SkippedOptionalSteps lists, in execution order, the optional steps a resolved plan does not take:
// cross_check and verifier run only when a lane is assigned to them.
func SkippedOptionalSteps(plan RunPlan) []string {
	var out []string
	for _, role := range []Role{RoleCrossCheck, RoleVerifier} {
		if _, ok := plan.Lanes[role]; !ok {
			out = append(out, string(role))
		}
	}
	return out
}

// ShapePayload is the workspace content a run would put in front of its reviewers, measured by
// walking the live tree under the collector's own containment rules (meshcore/workspace.PreviewPayloadWith).
// Every counted file is carried in full; a workspace edited after the dry run is a different payload.
type ShapePayload struct {
	// Files and Bytes are what the prompt would carry. Bytes is sent once per seat, per round.
	Files int `json:"files"`
	Bytes int `json:"bytes"`
	// Paths is every file that would be shown, sorted.
	Paths []string `json:"paths"`
	// Withheld is what containment kept out, rendered with its reason.
	Withheld []string `json:"withheld,omitempty"`
}

// ShapeSeat is one blind panel seat as a dry run would invoke it.
type ShapeSeat struct {
	SeatID  string `json:"seatId"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// ShapeEgress is one destination the run's content would reach, grouped by destination.
type ShapeEgress struct {
	// Destination is the receiving party in user-recognizable words; empty exactly when Known is
	// false.
	Destination string `json:"destination,omitempty"`
	// Local reports that nothing leaves the machine. Consumers branch on this, not on Destination.
	Local bool `json:"local,omitempty"`
	// Known is false for an adapter this tool does not own, such as a user-defined ACP instance.
	Known bool `json:"known"`
	// Note qualifies a destination that routes onward (a gateway) or that the operator selected.
	Note string `json:"note,omitempty"`
	// Via names the seats and roles that reach this destination, in run order.
	Via []string `json:"via"`
}

// ShapeLane is one non-panel role as a dry run would invoke it. Execution "host" runs in-process
// with no model call; "adapter" invokes a model CLI.
type ShapeLane struct {
	Role      Role   `json:"role"`
	Execution string `json:"execution"`
	Adapter   string `json:"adapter"`
	Model     string `json:"model"`
	Effort    string `json:"effort,omitempty"`
}

// Trace is the caller's W3C trace context, carried verbatim from the request that started a run so
// the run directory can be correlated with the caller's trace.
//
// Values are not validated: the MCP base protocol reserves `traceparent`, `tracestate` and `baggage`
// and forbids implementations from making assumptions about their values, so a malformed value is
// recorded as received. A trace makes a run correlatable; it does not record tokens or cost.
type Trace struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
	Baggage     string `json:"baggage,omitempty"`
	// Measures names what this block accounts for, so a consumer need not infer it from which fields
	// are present.
	Measures string `json:"measures"`
}

// MeasuresCorrelationOnly is the Trace.Measures value for a block that carries a correlation
// identity and no token or cost accounting.
const MeasuresCorrelationOnly = "correlation_only"

// NewTrace returns the run-record block for a caller's trace context, or nil when the caller sent
// none, which leaves a trace-free run-state.json unchanged.
func NewTrace(traceParent, traceState, baggage string) *Trace {
	if traceParent == "" && traceState == "" && baggage == "" {
		return nil
	}
	return &Trace{
		TraceParent: traceParent, TraceState: traceState, Baggage: baggage,
		Measures: MeasuresCorrelationOnly,
	}
}

// WithheldFile is one file a containment rule kept out of the reviewed set: not copied into the
// isolated workspace, or not collected into the prompt. It is recorded so that an unreviewed file is
// distinguishable from one that does not exist.
type WithheldFile struct {
	// Path is workspace-relative and slash-normalized.
	Path string
	// Reason is meshcore's machine code for the rule (e.g. "workspace_hardlink_denied").
	Reason string
	// Rule is the human sentence naming what refused, and Detail its specifics (e.g. the link
	// count). Either may be empty.
	Rule   string
	Detail string
	// Stage is "copy" (never placed in the isolated copy, so also invisible to an agentic reviewer
	// working there) or "snippets" (in the copy but never rendered into a prompt).
	Stage string
}

// IdentityCaveat is one lane that ran and returned schema-valid output but whose model identity is
// not fully verified (VerifUnknown or VerifSelfReported; a mismatch halts instead). Values are raw
// and sanitized at the surface.
type IdentityCaveat struct {
	Role           string
	Adapter        string           // stable adapter key
	RequestedModel string           // the requested model arg or catalog key
	Evidence       IdentityEvidence // produced evidence tier ("" or none when unknown)
	Status         string           // VerifUnknown | VerifSelfReported
	// ReportedModel is the model the adapter reported, if any. For a self-report naming a different
	// model (downgraded to unknown, not a mismatch) it preserves that signal.
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
	StderrExcerpt string // raw adapter stderr; surfaces sanitize and cap it
	StdoutExcerpt string // raw adapter stdout; some CLIs print actionable errors here
	// Signal is the meshcore/clihint classification for this failure (folder_trust |
	// login_required | model_invalid | update_prompt | timeout), or "". It is derived once so the
	// human hint and the machine signal agree.
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
