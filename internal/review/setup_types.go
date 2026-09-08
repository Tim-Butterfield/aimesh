package review

// Command is the user-invoked command a Surface carries.
type Command string

const (
	CommandReview Command = "review"
	CommandDoctor Command = "doctor"
	CommandSetup  Command = "setup"
	CommandRepair Command = "repair" // doctor --fix
)

// ConfigScope is where a config write lands.
type ConfigScope string

const (
	ScopeAsk     ConfigScope = "" // empty: the wizard prompts
	ScopeProject ConfigScope = "project"
	ScopeUser    ConfigScope = "user"
)

// SetupRequest is a setup-wizard invocation (CUC-3). Scope optional ("" = ask).
type SetupRequest struct {
	Roles []Role
	Scope ConfigScope
}

// RepairRequest is a repair-wizard invocation (doctor --fix). Scope optional ("" = ask).
type RepairRequest struct {
	Issues []DoctorIssue
	Scope  ConfigScope
}

// SetupOutcome is the result of a setup/repair run.
type SetupOutcome struct {
	Status          string
	ConfigWritten   bool
	RemainingIssues []DoctorIssue
	Halt            *HaltClass
}

// ExitCode implements Outcome.
func (o SetupOutcome) ExitCode() int {
	if o.Halt != nil || len(o.RemainingIssues) > 0 {
		return 1
	}
	return 0
}

// HaltClass implements Outcome.
func (o SetupOutcome) HaltClass() *HaltClass { return o.Halt }

// AdapterStatus is what detection reports for one adapter.
type AdapterStatus struct {
	Name    string
	Present bool
	Path    string
	AuthOK  bool
}

// LaneSuggestion / LaneChoice are a (role, adapter, model, thinking) tuple.
type LaneSuggestion struct {
	Role          Role
	Adapter       string
	Model         string
	ThinkingLevel string
}

// LaneChoice is a confirmed lane selection.
type LaneChoice = LaneSuggestion

// PathProbe is gathered by the Manager via ModelAccess.ValidateBinary.
type PathProbe struct {
	Exists     bool
	Executable bool
	Identity   IdentityResult
}

// PathCheck is the Engine's decision about an entered path.
type PathCheck struct {
	OK     bool
	Reason string
}

// DoctorIssue is one problem found by doctor (carries the halt class).
type DoctorIssue struct {
	Role    Role
	Adapter string
	Halt    HaltClass
	Detail  string
}

// RepairAction is a template fix the wizard offers (the patch is built later via PatchFor).
type RepairAction struct {
	Kind        string // reauthenticate | set_binary_path | choose_model | swap_or_reprobe | edit_config
	Target      string // the adapter (or other target) the action applies to
	Instruction string // e.g. the login command to show for reauthenticate
	Options     []string
}

// ConfigPatch is a minimal, declarative set of config changes applied to one scope.
type ConfigPatch struct {
	Ops []ConfigOp
}

// ConfigOp is one declarative change (set a key to a value).
type ConfigOp struct {
	Path  string // dotted config path, e.g. "adapters.codex-cli.path"
	Value string
}

// IdentityResult is the outcome of a model/binary identity probe.
type IdentityResult struct {
	Text    string // raw reported identity
	Model   string // normalized
	Method  IdentityMethod
	Success bool
}

// Availability is what an adapter reports about itself.
type Availability struct {
	BinaryPresent bool
	AuthOK        bool // where checkable
	Detail        string
}

// VerifyResult is returned by ModelVerifier.Verify.
type VerifyResult struct {
	OK        bool
	Halt      *HaltClass
	Transient bool
}

// PatchSet is a unified diff.
type PatchSet struct {
	Unified string
}

// ApplyResult is the outcome of applying one Edit.
type ApplyResult struct {
	OK     bool
	Detail string
}

// WorkspaceHandle identifies a workspace (the live one or a disposable copy).
type WorkspaceHandle struct {
	Root   string
	IsLive bool
}

// FindingsView and Event are Reporter payloads.
type FindingsView struct {
	Plan     RunPlan
	Findings []Finding
}

// Event is a progress event.
type Event struct {
	Message string
}

// PerCallRecord, RunSummary, Filter are audit payloads.
type PerCallRecord struct {
	Status    CallStatus
	Decisions []Decision
}

// RunSummary is the per-run summary.
type RunSummary struct {
	RunID    string
	Plan     RunPlan
	Findings []Finding
	Halt     *HaltClass
}

// Filter selects prior per-call records for addressed-context.
type Filter struct {
	RunID string
	Role  Role
}

// ModelCatalog is the read-only catalog view passed to the pure SetupEngine.
type ModelCatalog struct {
	Entries map[string]CatalogEntry // keyed by catalog key
}

// CatalogEntry is one canonical model profile.
type CatalogEntry struct {
	Provider       string
	CanonicalModel string
	Effort         string
	DisplayName    string
	Authoritative  bool
	Adapters       map[string]ModelArg // per-adapter modelArg
}

// Config is the merged configuration view.
type Config struct {
	Catalog ModelCatalog
	// further config sections (profiles, adapters, defaults, surfaces, policy,
	// review, validation, containment) are added during implementation.
}

// ModelCatalogView returns the catalog from a Config (helper for the wizard).
func (c Config) ModelCatalogView() ModelCatalog { return c.Catalog }
