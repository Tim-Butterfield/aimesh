package setup

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// This file adds the PURE config-patch planning primitives the future interactive wizard
// and guided repair will use: "given already-validated choices/values, what minimal
// config.ConfigPatch should be written?" — without doing any I/O. The SetupManager owns
// validating paths, applying the patch to a decoded config map, and writing it.
//
// Dependency direction: engine/setup → config (pure value type) is one-way; config never
// imports engine/setup. The manager imports both.

// PlanAdapterPathPatch returns the minimal patch that sets `adapters.<adapter>.path`.
// It owns only the non-empty checks; adapter-name *eligibility* (known, not `fake`) is
// the caller's responsibility via ValidateAdapterForPathCapture, and binary-path validity
// is the manager's (filesystem) responsibility.
func (e *Engine) PlanAdapterPathPatch(adapter, path string) (config.ConfigPatch, error) {
	if adapter == "" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "an adapter name is required")
	}
	if path == "" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "a binary path is required")
	}
	return config.ConfigPatch{Ops: []config.SetOp{
		{Path: []string{"adapters", adapter, "path"}, Value: path},
	}}, nil
}

// LaneChoice is an already-validated per-role lane selection for the future setup wizard.
// (Effort/thinking is NOT a lane field in this schema — it lives on the model catalog —
// so it is intentionally absent here.)
type LaneChoice struct {
	Role    string
	Adapter string
	Model   string
}

// PlanLaneChoices returns the minimal patch that sets each role's lane adapter + model
// under the given profile: `profiles.<profile>.lanes.<role>.{adapter,model}`. Pure; it
// validates that the profile and each choice's fields are non-empty and emits only those
// ops (no unrelated changes).
func (e *Engine) PlanLaneChoices(profile string, choices []LaneChoice) (config.ConfigPatch, error) {
	if profile == "" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "a profile name is required")
	}
	var ops []config.SetOp
	for _, c := range choices {
		if c.Role == "" || c.Adapter == "" || c.Model == "" {
			return config.ConfigPatch{}, fault.New(fault.Usage,
				fmt.Sprintf("lane choice requires role, adapter, and model (got role=%q adapter=%q model=%q)", c.Role, c.Adapter, c.Model))
		}
		ops = append(ops,
			config.SetOp{Path: []string{"profiles", profile, "lanes", c.Role, "adapter"}, Value: c.Adapter},
			config.SetOp{Path: []string{"profiles", profile, "lanes", c.Role, "model"}, Value: c.Model},
		)
	}
	return config.ConfigPatch{Ops: ops}, nil
}

// PlanLaneEdit returns the minimal patch for editing ONE lane of a profile — the web-UI lane
// editor's planning step. It sets `profiles.<profile>.lanes.<role>.{adapter,model}` and, only
// when `execution` is non-empty (the manager passes it only for a lane that does not exist
// yet), `…lanes.<role>.execution` — an existing lane's execution is deliberately left alone.
// Pure: non-empty checks only; the manager owns validating that the profile/role/adapter/model
// are real, catalog-consistent choices, and owns the write.
func (e *Engine) PlanLaneEdit(profile, role, adapter, model, execution string) (config.ConfigPatch, error) {
	patch, err := e.PlanLaneChoices(profile, []LaneChoice{{Role: role, Adapter: adapter, Model: model}})
	if err != nil {
		return config.ConfigPatch{}, err
	}
	if execution != "" {
		patch.Ops = append(patch.Ops,
			config.SetOp{Path: []string{"profiles", profile, "lanes", role, "execution"}, Value: execution})
	}
	return patch, nil
}

// PlanLaneClear returns the minimal patch that DELETES one lane of a profile —
// `profiles.<profile>.lanes.<role>` — the web-UI lane editor's "Clear configuration" step. It removes
// the user-layer explicit lane so a lower layer's lane (if any) is revealed, else the lane becomes
// unset/skipped. Pure: non-empty checks only; the manager owns layer/ownership/no-op guards + the write.
// The path is confined to `profiles`, so it can never touch a shared `modelCatalog` entry.
func (e *Engine) PlanLaneClear(profile, role string) (config.ConfigPatch, error) {
	if profile == "" || role == "" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "a profile name and role are required")
	}
	return config.ConfigPatch{Ops: []config.SetOp{
		{Path: []string{"profiles", profile, "lanes", role}, Delete: true},
	}}, nil
}

// PlanPromotion selects the **project-relevant** config.yaml keys to copy from a user/global config
// (decoded as a raw map of explicitly-present keys) into a project config, as a minimal `ConfigPatch`.
// It promotes **only** `defaultProfile` (when explicitly set + non-empty). Adapter binary paths are
// NOT part of this patch — they live in the shared `.aimesh/adapters.yaml` and are promoted separately
// (dual-target) by the manager. It copies no other section (never a bulk copy) and never any
// secret-bearing field (the config has none). Pure: no I/O; the caller applies the returned patch.
func (e *Engine) PlanPromotion(userRaw map[string]any) config.ConfigPatch {
	var ops []config.SetOp
	if dp, ok := userRaw["defaultProfile"].(string); ok && dp != "" {
		ops = append(ops, config.SetOp{Path: []string{"defaultProfile"}, Value: dp})
	}
	return config.ConfigPatch{Ops: ops}
}

// PlanProfileCopy returns the minimal patch that writes a copy of a source profile under
// the target name: a single full-subtree SetOp `profiles.<target>` = the source profile's
// content (already projected to a plain map by the manager). Copying onto an existing target
// replaces it wholesale (the leaf assignment overwrites the whole subtree). Pure: it validates
// only that the target name is non-empty and the content is present; the manager owns source
// existence, the replace-confirmation, and the write.
func (e *Engine) PlanProfileCopy(target string, content map[string]any) (config.ConfigPatch, error) {
	if target == "" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "a target profile name is required")
	}
	if content == nil {
		return config.ConfigPatch{}, fault.New(fault.Usage, "the source profile content is required")
	}
	return config.ConfigPatch{Ops: []config.SetOp{
		{Path: []string{"profiles", target}, Value: content},
	}}, nil
}

// PlanCatalogEntry returns the minimal patch that writes `modelCatalog.<key>` as a full
// subtree — how the lane editor materializes a discovered/manual model+effort choice as a
// first-class catalog entry (effort stays a CATALOG field, per the schema: lanes reference
// entries; no per-lane effort field is invented). Pure: non-empty checks only; the manager
// owns validating the choice against the adapter-owned rules and the write.
// CatalogRename is one model-key rename the manager resolved (pure input to the planner): rename
// modelCatalog.<OldKey> → <NewKey> and repoint each LaneRef's model to NewKey. Entry is the content
// to write at NewKey (nil when NewKey already exists with identical content — then only the lane
// repoint + old-key deletion happen).
type CatalogRename struct {
	OldKey   string
	NewKey   string
	Entry    map[string]any
	LaneRefs [][2]string // {profile, role}
}

// PlanModelKeyCleanup assembles ONE patch that renames generated model-catalog keys off their
// adapter-key prefix and repoints the (user-layer) profile lanes that reference them. Pure: the
// manager owns detection, the layer/collision guards, and the ConfigAccess write.
func (e *Engine) PlanModelKeyCleanup(renames []CatalogRename) config.ConfigPatch {
	var ops []config.SetOp
	for _, r := range renames {
		if r.NewKey != "" && r.Entry != nil {
			ops = append(ops, config.SetOp{Path: []string{"modelCatalog", r.NewKey}, Value: r.Entry})
		}
		for _, ref := range r.LaneRefs {
			ops = append(ops, config.SetOp{Path: []string{"profiles", ref[0], "lanes", ref[1], "model"}, Value: r.NewKey})
		}
		if r.OldKey != "" {
			ops = append(ops, config.SetOp{Path: []string{"modelCatalog", r.OldKey}, Delete: true})
		}
	}
	return config.ConfigPatch{Ops: ops}
}

func (e *Engine) PlanCatalogEntry(key string, entry map[string]any) (config.ConfigPatch, error) {
	if key == "" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "a catalog key is required")
	}
	if entry == nil {
		return config.ConfigPatch{}, fault.New(fault.Usage, "the catalog entry content is required")
	}
	return config.ConfigPatch{Ops: []config.SetOp{
		{Path: []string{"modelCatalog", key}, Value: entry},
	}}, nil
}

// PlanCatalogEntryDeletion returns the minimal patch that deletes `modelCatalog.<key>` from the
// target config layer. Pure: refuses only an empty key; the manager owns the layer/used-by
// guards (deletion is offered only for a user-owned, unshadowed, unused saved model) and the write.
func (e *Engine) PlanCatalogEntryDeletion(key string) (config.ConfigPatch, error) {
	if key == "" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "a catalog key is required")
	}
	return config.ConfigPatch{Ops: []config.SetOp{
		{Path: []string{"modelCatalog", key}, Delete: true},
	}}, nil
}

// PlanProfileDeletion returns the minimal patch that deletes `profiles.<name>` from the
// target config layer. Pure: it refuses only the structurally-invalid cases (empty name; the
// fixed workbench default profile `default`, which may never be deleted); the manager owns
// the confirmation handshake, the merged/raw-layer existence checks, the dangling
// defaultProfile-pointer guard, and the write.
func (e *Engine) PlanProfileDeletion(name string) (config.ConfigPatch, error) {
	if name == "" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "a profile name is required")
	}
	if name == "default" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "the `default` profile cannot be deleted")
	}
	return config.ConfigPatch{Ops: []config.SetOp{
		{Path: []string{"profiles", name}, Delete: true},
	}}, nil
}

// PlanAdapterRemoval returns the minimal patch that deletes `adapters.<name>` from the
// target config layer. Pure: it validates only that the name is non-empty; the manager owns
// the usage guard (removal is blocked while any profile/lane references the adapter) and the
// write. Deleting an adapter absent from the target layer is a harmless no-op at apply time.
func (e *Engine) PlanAdapterRemoval(name string) (config.ConfigPatch, error) {
	if name == "" {
		return config.ConfigPatch{}, fault.New(fault.Usage, "an adapter name is required")
	}
	return config.ConfigPatch{Ops: []config.SetOp{
		{Path: []string{"adapters", name}, Delete: true},
	}}, nil
}

// RepairActionKind enumerates the repair actions the engine can plan a patch for.
type RepairActionKind string

const (
	ActionSetBinaryPath  RepairActionKind = "set_binary_path"
	ActionReauthenticate RepairActionKind = "reauthenticate" // no config write — auth stays with the CLI
	// ActionSetDefaultProfile repairs a legacy/stale `defaultProfile` pointer (e.g. `fake`
	// from an older build) back to the fixed workbench default, the profile named `default`.
	// It only ever writes the literal value "default" — it is a REPAIR, not a "set as
	// default" feature (repointing defaultProfile at arbitrary profiles stays unexposed).
	ActionSetDefaultProfile RepairActionKind = "set_default_profile"
)

// RepairAction is a chosen repair (its template kind + target adapter). The concrete
// value (e.g. a validated path) is supplied separately, after the prompt.
type RepairAction struct {
	Kind   RepairActionKind
	Target string // e.g. the adapter name for set_binary_path
}

// PatchFor turns a chosen RepairAction + supplied value into a minimal ConfigPatch. It is
// the post-prompt step of the Repair algorithm. `reauthenticate` writes nothing (returns
// an empty patch — auth/trust stays with the native CLI). An unsupported kind is a clear
// error rather than a guessed edit.
func (e *Engine) PatchFor(action RepairAction, value string) (config.ConfigPatch, error) {
	switch action.Kind {
	case ActionSetBinaryPath:
		return e.PlanAdapterPathPatch(action.Target, value)
	case ActionSetDefaultProfile:
		// The value is deliberately ignored: this repair only restores the fixed default.
		return config.ConfigPatch{Ops: []config.SetOp{
			{Path: []string{"defaultProfile"}, Value: "default"},
		}}, nil
	case ActionReauthenticate:
		return config.ConfigPatch{}, nil // no-op: nothing to write
	default:
		return config.ConfigPatch{}, fault.New(fault.Usage,
			fmt.Sprintf("unsupported repair action kind %q", action.Kind))
	}
}
