// Package model defines the contract every model adapter implements and the shared call and result
// shapes. Concrete adapters live in subpackages.
package model

import (
	"context"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

// Call is one model invocation request handed to an adapter. Role and Phase are opaque labels carried
// for audit; the application owns their meaning.
type Call struct {
	Role     string        // opaque label (app's role)
	Phase    string        // opaque label (app's phase)
	Model    string        // catalog key
	ModelArg core.ModelArg // the opaque adapter argument (the expected identity)
	Effort   string        // resolved reasoning effort/thinking ("" = unspecified); recipe decides use
	CopyRoot string        // the isolated workspace copy the model may read
	// WorkDir is the working directory the CLI process runs in. Empty means the process's own cwd, so a
	// caller that must keep an agentic CLI out of the launching project must set a fresh isolated
	// directory. The ACP adapter uses a fresh temp dir when CopyRoot is empty. Read-only recipe flags
	// are a second containment layer.
	WorkDir string
	Prompt  string // the rendered prompt
}

// Result is the raw outcome of an adapter invocation, before normalization.
type Result struct {
	Stdout      []byte
	Stderr      []byte
	ExitCode    int
	ActualModel string                // the model the adapter reports actually answered
	Evidence    core.IdentityEvidence // the tier of signal behind ActualModel ("" = none)
	// Payload is the semantic content when the CLI wraps it in an envelope, such as claude-code's
	// `result` field. Callers parse Payload when present, else Stdout; Stdout is always kept for audit.
	Payload []byte
}

// Adapter is the ModelAccess contract. Invoke takes a context so a run-level
// cancellation (CLI signal or ACP `cancel`) terminates an in-flight call.
type Adapter interface {
	Name() string
	Available() (ok bool, detail string)
	Invoke(ctx context.Context, c Call) (Result, error)
}

// DiscoveredModel is one model surfaced by an adapter's model-discovery mechanism
// (e.g. an `ollama list` tag, a codex bundled-catalog slug, an agy display name).
type DiscoveredModel struct {
	Arg           string   // the adapter-specific model argument to configure/invoke with
	DefaultEffort string   // the mechanism's reported default effort ("" = none reported)
	Efforts       []string // supported efforts when the mechanism reports them (nil = none reported)
}

// Lister is the optional model-discovery capability. Discovery runs only a listing command, never a
// model invocation, and only on demand, never from default tests or doctor.
type Lister interface {
	// ListModels runs the adapter's discovery mechanism and parses its output.
	ListModels(ctx context.Context) ([]DiscoveredModel, error)
	// DiscoveryMechanism names the mechanism for honest display: the command shape
	// (e.g. "ollama list") and its kind ("local" | "bundled" | "best_effort").
	DiscoveryMechanism() (command, kind string)
}

// ArgPreviewer is the optional capability to render the invocation argv for a (model, effort) choice,
// built from the same recipe real calls use. The argv starts with the binary name, and the prompt
// position is the placeholder "<prompt>".
type ArgPreviewer interface {
	PreviewArgs(modelArg, effort string) []string
}
