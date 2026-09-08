// Package setup is the SetupEngine: the PURE planning/decision layer for the setup/repair
// use case (CUC-3). It has NO side effects — it never reads/writes files, stats paths,
// runs binaries, loads/writes config, reads env, or prints. It takes already-gathered
// facts (config presence, the known-adapter list, a doctor issue) and returns plans,
// validation results, and guidance values. The SetupManager owns all I/O and is the only
// config writer; it delegates these decisions here so the future interactive wizard and
// guided `doctor --fix` repair share one engine instead of duplicating logic.
//
// Scope (current): the pure decisions/planners behind both the non-interactive paths and
// the built interactive flows — the setup wizard (`setup --interactive`, incl. per-role lane
// model selection from the catalog via PlanLaneChoices), the bounded guided repair
// (`doctor --fix --interactive`, adapter binary-path), and `user→project` promotion
// (`setup --scope project --from user`, via PlanPromotion) consume these through the
// SetupManager. The richer repairs (model choice / identity mismatch) and the **pre-write
// LIVE model-identity probe** remain target/future (post-release; see docs/design.md →
// Interactive stack contract).
package setup

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// Engine is the pure setup/repair planner. It is stateless.
type Engine struct{}

// New returns a SetupEngine.
func New() *Engine { return &Engine{} }

// SetupKind identifies which seed an initial setup would write (cosmetic to the decision;
// the ShouldWrite outcome depends only on whether a config already exists).
type SetupKind string

const (
	SetupKindDefault          SetupKind = "default"
	SetupKindFullyLocalOllama SetupKind = "fully_local_ollama"
)

// ConfigPresence is the I/O-gathered fact the engine needs about the target config.
type ConfigPresence struct {
	Exists bool
}

// SetupPlan is the engine's decision for an initial (plain / profile) setup.
type SetupPlan struct {
	ShouldWrite bool
	Reason      string
}

// PlanInitialSetup decides whether plain/profile setup should write: it writes only when
// no config exists at the target, and otherwise preserves it (never clobbers). This is
// the pure decision behind the manager's idempotent writeConfigIfAbsent.
func (e *Engine) PlanInitialSetup(kind SetupKind, presence ConfigPresence) SetupPlan {
	if presence.Exists {
		return SetupPlan{ShouldWrite: false, Reason: "preserved existing config"}
	}
	return SetupPlan{ShouldWrite: true, Reason: "write " + string(kind) + " seed"}
}

// ValidateAdapterForPathCapture validates that an adapter name is eligible for
// `setup --adapter <name> --path <p>`: it must be non-empty and one of the configurable
// adapters (the registered adapters excluding the built-in `fake`). Pure: the manager
// supplies the sorted configurable-name list. Returns a usage fault otherwise.
func (e *Engine) ValidateAdapterForPathCapture(name string, configurable []string) error {
	if name == "" {
		return fault.New(fault.Usage, "an adapter name is required (--adapter)")
	}
	for _, n := range configurable {
		if n == name {
			return nil
		}
	}
	return fault.New(fault.Usage,
		fmt.Sprintf("unknown adapter %q; configurable adapters: %s", name, strings.Join(configurable, ", ")))
}

// RepairIssue is a doctor check the engine turns into guidance. BinHint is the binary
// name the manager resolved for an adapter issue (e.g. "claude" for "claude-code");
// the engine never touches a live adapter to discover it.
type RepairIssue struct {
	Name    string // e.g. "adapter: claude-code"
	Detail  string
	BinHint string // resolved by the manager for adapter issues; "" otherwise
}

// RepairGuidance is the plain-English message (+ optional exact command) the manager
// prints for a doctor issue. WritesConfig is always false: this guidance is print-only.
// (The bounded interactive repair — `doctor --fix --interactive` — applies an adapter
// binary-path fix separately, via `PatchFor` → `ConfigAccess`, not through this value.)
type RepairGuidance struct {
	Message      string
	Command      string // exact repair command, "" when there is no single command
	WritesConfig bool
}

// RepairGuidance maps a doctor issue to plain-English guidance. For a missing/invalid
// adapter binary it returns the exact `setup --adapter … --path …` command; otherwise a
// general pointer. It never plans a config write (guidance-only, per current behavior).
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
