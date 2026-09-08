package schema

// Phase labels are the opaque model.Call.Phase strings exploremesh sends to an adapter, so a fake
// (or a real recipe) can tell which bookend/leg of the run it is answering. They are exploremesh's
// vocabulary — meshcore treats Phase as an opaque string.
const (
	PhaseFormulate    = "formulate"    // collator: raw_task + minimum schema → explorer_task_payload
	PhaseExplore      = "explore"      // explorer: the shared payload → a schema-valid response
	PhaseSynthesize   = "synthesize"   // collator: verified responses → collator-output
	PhaseCanonicalize = "canonicalize" // canonicalizer (decoupled, §4): raw nominations → a proposed canonical partition
	// PhasePreflight is the CHEAP capability/identity probe issued to the collator + every canonicalizer
	// BEFORE the explorer fan-out (design §1): a fallback swap must be caught before explorer tokens are
	// spent, not after. Its body is irrelevant — only the resolved model identity is consumed.
	PhasePreflight = "preflight"
	// PhaseConfirm is the BINDING confirmation round (design §4): each explorer is shown the provisional
	// raw→canonical ledger (with attribution, in a persisted randomized order) and returns TYPED challenges.
	PhaseConfirm = "confirm"
	// PhaseBallot is the explicit BALLOT round (design §3 Shortlist / §4): each explorer ranks/approves
	// the CONFIRMED canonical IDs under decision inputs the host froze + hashed beforehand. It is a distinct
	// phase from PhaseExplore precisely because it is a governance act, not an exploration: an adapter (and a
	// recording) can tell a preference solicitation apart from a research call.
	PhaseBallot = "ballot"
)
