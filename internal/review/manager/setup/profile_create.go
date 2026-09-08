package setup

// New Profile creation (the web-UI "New profile…" dialog's governed write path). Validation and
// initial-model composition are Go-owned; the SPA only collects name/description/per-role adapter
// and renders the result. Writes go through SetupEngine → ConfigAccess, like CopyProfile.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// profileKeyPattern is the accepted profile-name (map key) shape: lowercase alphanumerics and
// hyphens, starting alphanumeric. Matches the shipped seed's conventions.
var profileKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// createRoles is the fixed pipeline order; author_remediator + reviewer are required.
var createRoles = []string{"author_remediator", "reviewer", "cross_check", "verifier"}

// adapterDefaultKey returns the SINGLE adapterDefault-marked catalog key reachable through an
// adapter (deterministic; sorted for stable diagnostics). It rejects the zero- and
// multiple-default cases with clear errors so lane initialization is never map-order dependent.
func (m *Manager) adapterDefaultKey(adapter string) (string, error) {
	var keys []string
	for k, e := range m.Cfg.ModelCatalog {
		if _, ok := e.Adapters[adapter]; ok && e.IsAdapterDefault() {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	switch len(keys) {
	case 0:
		return "", fault.New(fault.Usage, fmt.Sprintf("adapter %q has no default model in the catalog — configure a saved model for it first", adapter))
	case 1:
		return keys[0], nil
	default:
		return "", fault.New(fault.Usage, fmt.Sprintf("adapter %q has multiple default models (%s) — resolve the ambiguity in your config", adapter, strings.Join(keys, ", ")))
	}
}

// CreateProfile creates a new profile from a name/description and a per-role adapter selection —
// each lane initialized with the adapter's DEFAULT catalog key and the role-appropriate
// execution. author_remediator and reviewer are required; cross_check/verifier are optional
// (empty = omit). Every selected adapter must be implemented + configured (fake is always
// configured). A single non-fake adapter yields an informational note, not a block. The write is
// planned by SetupEngine and written through ConfigAccess (user scope).
func (m *Manager) CreateProfile(name, description string, laneAdapters map[string]string) (Result, error) {
	var res Result
	if name == "" {
		return res, fault.New(fault.Usage, "a profile name is required")
	}
	if !profileKeyPattern.MatchString(name) {
		return res, fault.New(fault.Usage, fmt.Sprintf("profile name %q is not a valid key (use lowercase letters, digits, and hyphens; start with a letter or digit)", name))
	}
	if _, exists := m.Cfg.Profiles[name]; exists {
		return res, fault.New(fault.Usage, fmt.Sprintf("profile %q already exists — choose a different name or copy/edit the existing one", name))
	}
	if layer, p := m.higherLayerDefining(name); layer != "" {
		return res, fault.New(fault.Usage, fmt.Sprintf("profile %q is defined in the %s config layer (%s), which this workbench does not edit — choose a different name", name, layer, p))
	}
	if laneAdapters["author_remediator"] == "" || laneAdapters["reviewer"] == "" {
		return res, fault.New(fault.Usage, "author_remediator and reviewer are required — choose a configured adapter for each")
	}

	lanes := map[string]any{}
	nonFake := 0
	for _, role := range createRoles {
		adapter := laneAdapters[role]
		if adapter == "" {
			continue // optional roles (cross_check/verifier) may be omitted
		}
		if _, impl := m.Adapters[adapter]; !impl {
			return res, fault.New(fault.Usage, fmt.Sprintf("adapter %q (for %s) is not implemented in this build", adapter, role))
		}
		if adapter != "fake" {
			if a, ok := m.Cfg.Adapters[adapter]; !ok || a.Path == "" {
				return res, fault.New(fault.Usage, fmt.Sprintf("adapter %q (for %s) is not configured — set its binary path on the Adapters page first", adapter, role))
			}
			nonFake++
		}
		key, err := m.adapterDefaultKey(adapter)
		if err != nil {
			return res, err
		}
		lanes[role] = map[string]any{"execution": laneExecutionFor(role), "adapter": adapter, "model": key}
	}

	content := map[string]any{"lanes": lanes}
	if description != "" {
		content["description"] = description
	}
	patch, err := m.engine().PlanProfileCopy(name, content)
	if err != nil {
		return res, err
	}
	path, err := m.writeTarget()
	if err != nil {
		return res, err
	}
	if err := config.ApplyPatchToFile(path, patch); err != nil {
		return res, err
	}
	res.ConfigWritten, res.Path = true, path
	res.Messages = append(res.Messages, fmt.Sprintf("Created profile %q. Configure lane models next.", name))
	if nonFake == 1 {
		res.Messages = append(res.Messages, "Note: only one non-fake adapter was selected — provider diversity is reduced (you can add cross_check/verifier lanes later).")
	}
	m.log(res.Messages[0] + " (" + path + ")")
	return res, nil
}
