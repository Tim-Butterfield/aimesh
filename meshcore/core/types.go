// Package core holds meshcore's domain-agnostic primitive types, shared across the
// substrate packages (adapter, identity, containment, fault, …): model-identity
// evidence tiers + verification-status vocabulary, halt classification, adapter
// capabilities, the appliable Edit, and the model-argument / artifact-delivery vocab.
// It carries NO app-domain concepts — no roles, lanes, findings, or review phases.
package core

// ModelArg is the opaque, adapter-specific argument string for a catalog model.
type ModelArg string

// IdentityMethod is how an adapter's actual model identity is extracted.
type IdentityMethod string

const (
	IdentityEnvelope   IdentityMethod = "envelope"
	IdentityTrace      IdentityMethod = "trace"
	IdentitySelfReport IdentityMethod = "self_report"
	IdentityAmbient    IdentityMethod = "ambient_unverifiable"
)

// IdentityEvidence ranks, strongest→weakest, the kind of signal that produced a
// call's actual-model identity. It is recorded per call so a weak (self-report)
// signal is never silently presented as a strong (envelope/trace) one.
type IdentityEvidence string

const (
	EvidenceEnvelope      IdentityEvidence = "envelope"       // strongest: structured CLI/provider usage envelope
	EvidenceTrace         IdentityEvidence = "trace"          // strong: deterministic CLI trace/event/stderr line
	EvidenceCLIStatus     IdentityEvidence = "cli_status"     // medium: a CLI status/probe confirms the selected model
	EvidenceInvocationTag IdentityEvidence = "invocation_tag" // medium: local runtime tag (e.g. the ollama run tag)
	EvidenceSelfReport    IdentityEvidence = "self_report"    // weak: the model was asked to name itself
	EvidenceNone          IdentityEvidence = "none"           // no usable identity evidence
)

// Verification-status values (the per-call verification status). Every one of them is a LABEL
// recorded on the call: none of them decides whether the call's output is used. self_reported is a
// distinct, weaker disposition than verified — it is NOT silently verified. unknown is no usable/clear
// identity; mismatch is a proven strong-evidence wrong model, carried as a prominent caveat rather
// than a halt; unverified is set directly by non-identity halts (containment, cancellation,
// invocation failure), which DO stop a call — for reasons that have nothing to do with identity.
const (
	VerifVerified     = "verified"
	VerifSelfReported = "self_reported"
	VerifUnknown      = "unknown"
	VerifMismatch     = "mismatch"
	VerifUnverified   = "unverified"
)

// HaltClass is the classification of a call result (A–G adapter, M1–M5 mechanical).
type HaltClass string

// ArtifactDelivery is how a call's artifact reaches the model.
type ArtifactDelivery string

const (
	ArtifactReadsFromDir ArtifactDelivery = "reads_from_dir"
	ArtifactInline       ArtifactDelivery = "inline_content"
)

// Edit is a structured, appliable change (one or more search/replace blocks per file).
type Edit struct {
	File        string `json:"file"`
	Anchor      string `json:"anchor"` // exact pre-image to locate (whole file for a full replace)
	Replacement string `json:"replacement"`
	Occurrence  int    `json:"occurrence,omitempty"` // 1-based; 0 = all/unique
}

// AdapterCaps is what an adapter declares about itself.
type AdapterCaps struct {
	ReadOnly      string // "enforced" | "contained" | "none"
	CanWrite      bool
	Thinking      string // "separable" | "name_bound" | "unsupported"
	ModelIdentity IdentityMethod
	Artifact      ArtifactDelivery
}

// SurfaceCaps is what a host surface advertises.
type SurfaceCaps struct {
	WorkspaceRoot bool
	FileRead      bool
	FileWrite     bool
	DiffContext   bool
	ArtifactDir   bool
	Interactive   bool   // can prompt the user (gates the wizard); false for ci
	AmbientModel  string // "" if no ambient model session
}
