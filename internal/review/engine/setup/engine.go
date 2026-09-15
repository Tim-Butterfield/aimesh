// Package setup is the setup engine: pure planning and decisions for setup and repair. It performs
// no I/O; it takes facts the setup manager gathered (config presence, the adapter list, a doctor
// issue) and returns plans, validation results and guidance. The setup wizard, guided
// `doctor --fix` repair and user-to-project promotion all use it through the setup manager, which is
// the only config writer.
package setup

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// Engine is the stateless setup and repair planner.
type Engine struct{}

// New returns an Engine.
func New() *Engine { return &Engine{} }

// SetupKind identifies which seed an initial setup would write. It does not affect whether setup
// writes.
type SetupKind string

const (
	SetupKindDefault          SetupKind = "default"
	SetupKindFullyLocalOllama SetupKind = "fully_local_ollama"
)

// ConfigPresence reports whether the target config exists.
type ConfigPresence struct {
	Exists bool
}

// SetupPlan is the decision for an initial setup.
type SetupPlan struct {
	ShouldWrite bool
	Reason      string
}

// PlanInitialSetup decides whether an initial setup writes: only when no config exists at the target.
// An existing config is never overwritten.
func (e *Engine) PlanInitialSetup(kind SetupKind, presence ConfigPresence) SetupPlan {
	if presence.Exists {
		return SetupPlan{ShouldWrite: false, Reason: "preserved existing config"}
	}
	return SetupPlan{ShouldWrite: true, Reason: "write " + string(kind) + " seed"}
}

// ValidateAdapterForPathCapture returns a usage fault unless name is non-empty and one of the
// configurable adapters, for `setup --adapter <name> --path <p>`.
func (e *Engine) ValidateAdapterForPathCapture(name string, configurable []string) error {
	if name == "" {
		return fault.New(fault.Usage, "an adapter name is required (--adapter)")
	}
	if slices.Contains(configurable, name) {
		return nil
	}
	return fault.New(fault.Usage,
		fmt.Sprintf("unknown adapter %q; configurable adapters: %s", name, strings.Join(configurable, ", ")))
}

// RepairIssue is a doctor check the engine turns into guidance.
type RepairIssue struct {
	Name    string // e.g. "adapter: claude-code"
	Detail  string
	BinHint string // the adapter's binary name (e.g. "claude" for "claude-code"); "" otherwise
}

// RepairGuidance is the message, and optional exact command, printed for a doctor issue.
// WritesConfig is always false; interactive repair writes through PatchFor instead.
type RepairGuidance struct {
	Message      string
	Command      string // exact repair command, "" when there is no single command
	WritesConfig bool
}

// RepairGuidance maps a doctor issue to guidance: the exact `setup --adapter … --path …` command for
// an adapter issue, otherwise a pointer to the docs.
func (e *Engine) RepairGuidance(issue RepairIssue) RepairGuidance {
	if name, ok := strings.CutPrefix(issue.Name, "adapter: "); ok {
		hint := issue.BinHint
		if hint == "" {
			hint = name
		}
		return RepairGuidance{
			Message: fmt.Sprintf("Issue: %s — %s.", issue.Name, issue.Detail),
			Command: fmt.Sprintf("reviewmesh setup --adapter %s --path /full/path/to/%s", name, hint),
		}
	}
	return RepairGuidance{
		Message: fmt.Sprintf("Issue: %s — %s. Guidance: see docs/adapters.md / docs/configuration.md; run `reviewmesh setup` to (re)write config.", issue.Name, issue.Detail),
	}
}
