package review

// This file declares the service contracts for each layer of the architecture.

// Surface is a host-surface client. It calls one Manager per command and renders the returned
// Outcome; the Manager never calls up into the client.
type Surface interface {
	Command() Command
	ReviewRequest() ReviewRequest // valid when Command() == CommandReview
	SetupRequest() SetupRequest   // valid when Command() == CommandSetup
	RepairRequest() RepairRequest // valid when Command() == CommandRepair
	Capabilities() SurfaceCaps
	Reporter() Reporter
	Prompter() Prompter // nil on a non-interactive surface
	Render(o Outcome)
}

// ReviewManager owns the review-and-remediate use case.
type ReviewManager interface {
	Run(req ReviewRequest, caps SurfaceCaps, rep Reporter) RunOutcome
}

// SetupManager owns the setup and repair use case. It is the only component that writes the
// config store.
type SetupManager interface {
	Setup(req SetupRequest, caps SurfaceCaps, rep Reporter, ask Prompter) SetupOutcome
	Repair(req RepairRequest, caps SurfaceCaps, rep Reporter, ask Prompter) SetupOutcome
}

// AdjudicationEngine judges findings. hostJudgment is the verified CallStatus from the
// author_remediator adjudication call.
type AdjudicationEngine interface {
	Judge(findings []Finding, hostJudgment CallStatus, state ArtifactState) []Decision
}

// RemediationEngine returns the edits for an accepted decision without writing them.
type RemediationEngine interface {
	Propose(decision Decision, state ArtifactState) ([]Edit, error)
}

// SetupEngine makes setup and repair decisions; the Manager performs all I/O.
type SetupEngine interface {
	SuggestLanes(detected []AdapterStatus, want []Role, cat ModelCatalog) []LaneSuggestion
	ValidatePath(adapter string, probe PathProbe) PathCheck
	PlanConfig(choices []LaneChoice, scope ConfigScope, cat ModelCatalog) ConfigPatch
	RepairActions(issue DoctorIssue, cat ModelCatalog) []RepairAction
	PatchFor(action RepairAction, value string, scope ConfigScope, cat ModelCatalog) ConfigPatch
}

// ModelAccess is the contract every adapter implements.
type ModelAccess interface {
	Available() Availability
	ValidateBinary(path string) PathProbe                             // checks a candidate binary path
	ProbeIdentity(model ModelArg, pathOverride string) IdentityResult // pathOverride "" means the configured binary
	Invoke(spec CallSpec, ws WorkspaceHandle) CallStatus
	Caps() AdapterCaps
}

// WorkspaceAccess handles containment and the application of remediation.
type WorkspaceAccess interface {
	Live() WorkspaceHandle
	ProvideIsolatedCopy(scope CopyScope) WorkspaceHandle
	ReadArtifact(h WorkspaceHandle, path string) ([]byte, error)
	ApplyEdit(h WorkspaceHandle, edit Edit) ApplyResult
	Diff(h WorkspaceHandle) PatchSet
	Commit(h WorkspaceHandle) error
	Discard(h WorkspaceHandle) (PatchSet, *HaltClass) // returns the M5 diff and halt class on unexpected mutation
}

// AuditAccess persists and re-reads the audit trail.
type AuditAccess interface {
	WritePerCall(rec PerCallRecord)
	WriteSummary(s RunSummary)
	ReadPriorPerCall(filter Filter) []PerCallRecord
}

// ConfigAccess reads and writes the config store. Only SetupManager writes.
type ConfigAccess interface {
	Read() (Config, error)
	Layer(scope ConfigScope) (Config, error)
	Write(scope ConfigScope, patch ConfigPatch) error
	Path(scope ConfigScope) string
}

// Resolver turns configuration, detection and capabilities into a run plan.
type Resolver interface {
	Resolve(req ReviewRequest, detected []AdapterStatus, caps SurfaceCaps) (RunPlan, error)
}

// ModelVerifier compares the requested vs actual model identity of a call.
type ModelVerifier interface {
	Verify(status CallStatus) VerifyResult
}

// RetryPolicy decides whether to retry after a failed verification.
type RetryPolicy interface {
	Retry(v VerifyResult, attempt int) bool
}

// Reporter is the output sink the Client supplies; a Manager calls it downward.
type Reporter interface {
	AnnounceResolved(plan RunPlan)
	EmitFindings(view FindingsView)
	EmitPatch(patch PatchSet)
	Progress(ev Event)
}

// Prompter is the input source the client supplies for the wizard; nil when non-interactive.
type Prompter interface {
	Ask(prompt string) string
	Choose(prompt string, options []string) int
	Confirm(prompt string) bool
	AskPath(prompt string) string
}
