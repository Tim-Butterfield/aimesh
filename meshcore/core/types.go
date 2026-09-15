// Package core holds meshcore's shared primitive types: model-identity evidence tiers and
// verification statuses, halt classes, adapter and surface capabilities, the model argument, artifact
// delivery modes, and the appliable Edit. It carries no application concepts.
package core

// ModelArg is the opaque, adapter-specific argument string for a catalog model.
type ModelArg string

// IdentityMethod is how an adapter's actual model identity is extracted.
type IdentityMethod string

// Identity extraction methods.
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

// Identity evidence tiers, strongest first.
const (
	EvidenceEnvelope      IdentityEvidence = "envelope"       // strongest: structured CLI/provider usage envelope
	EvidenceTrace         IdentityEvidence = "trace"          // strong: deterministic CLI trace/event/stderr line
	EvidenceCLIStatus     IdentityEvidence = "cli_status"     // medium: a CLI status/probe confirms the selected model
	EvidenceInvocationTag IdentityEvidence = "invocation_tag" // medium: local runtime tag (e.g. the ollama run tag)
	EvidenceSelfReport    IdentityEvidence = "self_report"    // weak: the model was asked to name itself
	EvidenceNone          IdentityEvidence = "none"           // no usable identity evidence
)

// Verification statuses recorded per call. None of them decides whether a call's output is used.
// self_reported is weaker than verified; unknown means no clear identity; mismatch is a wrong model
// proven by strong evidence, carried as a caveat; unverified is set by non-identity halts
// (containment, cancellation, invocation failure), which do stop a call.
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

// Artifact delivery modes.
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
