package setup

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// The planners in this file turn already-validated choices into minimal config patches without
// I/O. The setup manager validates inputs and writes the patches.

// PlanAdapterPathPatch returns the patch that sets `adapters.<adapter>.path`. It checks only that
// both values are non-empty; the caller validates the adapter name and binary path.
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

// LaneChoice is a validated per-role lane selection. Effort is a catalog field, not a lane field.
type LaneChoice struct {
	Role    string
	Adapter string
	Model   string
}

// PlanLaneChoices returns the patch that sets `profiles.<profile>.lanes.<role>.{adapter,model}` for
// each choice. It checks only that the profile and every field are non-empty.
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

// PlanPromotion returns the patch that promotes a user config's explicitly set `defaultProfile` into
// a project config. userRaw holds only explicitly present keys. Adapter paths live in the shared
// adapters.yaml and are promoted separately by the manager.
func (e *Engine) PlanPromotion(userRaw map[string]any) config.ConfigPatch {
	var ops []config.SetOp
	if dp, ok := userRaw["defaultProfile"].(string); ok && dp != "" {
		ops = append(ops, config.SetOp{Path: []string{"defaultProfile"}, Value: dp})
	}
	return config.ConfigPatch{Ops: ops}
}

// CatalogRename is one model-key rename: modelCatalog.<OldKey> becomes <NewKey>, and each LaneRef's
// model is repointed to NewKey. Entry is the content to write at NewKey, or nil when NewKey already
// holds identical content.
type CatalogRename struct {
	OldKey   string
	NewKey   string
	Entry    map[string]any
	LaneRefs [][2]string // {profile, role}
}

// PlanModelKeyCleanup returns one patch applying every rename: it writes each new catalog key,
// repoints the lanes that reference it and deletes the old key.
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

// RepairActionKind enumerates the repair actions the engine can plan a patch for.
type RepairActionKind string

const (
	ActionSetBinaryPath  RepairActionKind = "set_binary_path"
	ActionReauthenticate RepairActionKind = "reauthenticate" // no config write; auth stays with the CLI
	// ActionSetDefaultProfile resets a stale `defaultProfile` to "default". It never writes any
	// other value.
	ActionSetDefaultProfile RepairActionKind = "set_default_profile"
)

// RepairAction is a chosen repair: its kind and target. The value, such as a validated path, is
// supplied separately.
type RepairAction struct {
	Kind   RepairActionKind
	Target string // e.g. the adapter name for set_binary_path
}

// PatchFor turns a repair action and its value into a config patch. `reauthenticate` returns an
// empty patch, and an unsupported kind is an error.
func (e *Engine) PatchFor(action RepairAction, value string) (config.ConfigPatch, error) {
	switch action.Kind {
	case ActionSetBinaryPath:
		return e.PlanAdapterPathPatch(action.Target, value)
	case ActionSetDefaultProfile:
		// The value is ignored: this repair only restores "default".
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
