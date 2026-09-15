package schema

// Phase labels are the model.Call.Phase values sent to an adapter, so a fake or a recording can tell
// which step of the run a call belongs to. meshcore treats them as opaque strings.
const (
	PhaseFormulate    = "formulate"    // collator: raw task and minimum schema to explorer payload
	PhaseExplore      = "explore"      // explorer: shared payload to a schema-valid response
	PhaseSynthesize   = "synthesize"   // collator: verified responses to collator output
	PhaseCanonicalize = "canonicalize" // canonicalizer: raw nominations to a proposed canonical partition
	// PhasePreflight is the cheap identity probe sent to the collator and each canonicalizer before the
	// explorer fan-out, so a model substitution is caught before explorer calls are spent. Only the
	// reported model identity is used.
	PhasePreflight = "preflight"
	// PhaseConfirm is the confirmation round: each explorer sees the provisional canonical ledger, with
	// attribution in a recorded random order, and returns typed challenges.
	PhaseConfirm = "confirm"
	// PhaseBallot is the shortlist ballot round: each explorer ranks and approves the confirmed canonical
	// IDs under decision inputs frozen beforehand. It is distinct from PhaseExplore so a preference
	// solicitation can be told apart from a research call.
	PhaseBallot = "ballot"
)
