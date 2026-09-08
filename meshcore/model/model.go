// Package model is the ModelAccess layer: the contract every adapter implements
// and the shared call/result shapes. Concrete adapters live in subpackages
// (Batch 1 ships only the deterministic fake under model/fake).
package model

import (
	"context"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
)

// Call is one model invocation request handed to an adapter. Role and Phase are OPAQUE
// string labels (meshcore carries them for audit/attribution; it does not interpret them —
// the app owns the typed Role/Phase enums and converts at the boundary).
type Call struct {
	Role     string        // opaque label (app's role)
	Phase    string        // opaque label (app's phase)
	Model    string        // catalog key
	ModelArg core.ModelArg // the opaque adapter argument (the expected identity)
	Effort   string        // resolved reasoning effort/thinking ("" = unspecified); recipe decides use
	CopyRoot string        // the isolated workspace copy the reviewer may read
	// WorkDir is the working directory the CLI process runs in (CONTAINMENT). Empty → the process's
	// own cwd, so a caller that must not let an agentic CLI read/wander the launching project (e.g.
	// exploremesh, which reviews nothing on disk) MUST set this to a fresh isolated dir. The shell
	// adapter sets cmd.Dir to it; the ACP adapter already runs in a fresh temp dir when CopyRoot is
	// empty. Read-only recipe flags remain the second containment layer.
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
	// Payload is the SEMANTIC content when the CLI wraps it in an envelope (e.g.
	// claude-code `--output-format json` → the reviewer JSON inside `result`). The
	// Manager parses Payload when present, else Stdout; Stdout is always kept for audit.
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

// Lister is the OPTIONAL model-discovery capability an adapter may implement. Discovery
// runs a metadata/listing command only (never a model invocation, never token spend) and
// is invoked strictly on demand — never from default tests, doctor, or page load. Adapters
// without a discovery mechanism simply do not implement it.
type Lister interface {
	// ListModels runs the adapter's discovery mechanism and parses its output.
	ListModels(ctx context.Context) ([]DiscoveredModel, error)
	// DiscoveryMechanism names the mechanism for honest display: the command shape
	// (e.g. "ollama list") and its kind ("local" | "bundled" | "best_effort").
	DiscoveryMechanism() (command, kind string)
}

// ArgPreviewer is the OPTIONAL capability to render the EFFECTIVE invocation argv for a
// pending (model, effort) choice — composed from the same recipe that real calls use, so
// the preview can never drift from actual behavior. The returned argv starts with the
// binary name; the prompt position is shown as the literal placeholder "<prompt>".
type ArgPreviewer interface {
	PreviewArgs(modelArg, effort string) []string
}
