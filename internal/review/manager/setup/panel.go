package setup

// The BLIND REVIEWER PANEL's config-write seam.
//
// A panel is an ORDERED list, so add / remove / reorder are all the same operation: write the
// whole list. There is deliberately NO per-seat write path — a per-seat route could not express
// a reorder, and two live write paths to the same bytes is exactly the second source of truth
// exploremesh removed when it consolidated its roster edits onto one whole-profile save. One
// request, one patch, one atomic file write: a rejected panel persists nothing.

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// PanelSeatInput is one seat as the editor submits it: a CONFIGURED adapter plus a CONFIGURED
// model-catalog key. Compose-not-configure applies to the workbench too — the panel editor
// selects from what the Adapters tab and the model catalog already define; it never mints an
// adapter or a model.
type PanelSeatInput struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
}

// SavePanelResult reports the outcome of SaveReviewerPanel.
type SavePanelResult struct {
	Written  bool
	Profile  string
	Seats    int
	Path     string
	Messages []string
}

// SaveReviewerPanel writes a profile's ENTIRE ordered blind-primary panel in ONE patch.
//
// It also completes the MIGRATION in the same write: `profiles.<name>.lanes.reviewer` (the
// one-seat sugar) is deleted, because a layer that named both spellings would be refused on the
// next load. Saving a panel of one is therefore a normal, complete operation — the panel spelling
// simply becomes the profile's spelling.
//
// Validation is fail-closed and mirrors the resolver's rules so the editor cannot persist a panel
// a run would reject: 1..MaxReviewerSeats seats, each naming a configured adapter and a catalog
// key that maps to it, and no two seats sharing an identity.
func (m *Manager) SaveReviewerPanel(profile string, seats []PanelSeatInput) (SavePanelResult, error) {
	res := SavePanelResult{Profile: profile, Seats: len(seats)}
	if strings.TrimSpace(profile) == "" {
		return res, fault.New(fault.Usage, "a profile name is required")
	}
	if _, ok := m.Cfg.Profiles[profile]; !ok {
		return res, fault.New(fault.Usage, fmt.Sprintf("profile %q does not exist", profile))
	}
	// Layer guard (identical to the lane edit): a profile a higher-precedence layer defines
	// would SHADOW this write, so refuse with the honest reason rather than writing a phantom.
	if layer, p := m.higherLayerDefining(profile); layer != "" {
		return res, fault.New(fault.Usage, fmt.Sprintf("profile %q is also defined in the %s config layer (%s), which this workbench does not edit — a %s panel change would be shadowed by it; edit that file directly", ProfileDisplayName(profile), layer, p, m.scopeLabel()))
	}
	if len(seats) == 0 {
		return res, fault.New(fault.Usage, "a reviewer panel needs at least one seat — the blind primary stage is what produces findings")
	}
	if len(seats) > review.MaxReviewerSeats {
		return res, fault.New(fault.Usage, fmt.Sprintf("a reviewer panel holds at most %d seats (each seat is a real model CLI); remove a seat", review.MaxReviewerSeats))
	}
	identities := map[string]int{}
	lanes := make([]any, 0, len(seats))
	for i, s := range seats {
		adapter, modelKey := strings.TrimSpace(s.Adapter), strings.TrimSpace(s.Model)
		if adapter == "" || modelKey == "" {
			return res, fault.New(fault.Usage, fmt.Sprintf("reviewer seat %d needs an adapter and a model", i+1))
		}
		if a, ok := m.Cfg.Adapters[adapter]; !ok || !a.IsEnabled() {
			return res, fault.New(fault.Usage, fmt.Sprintf("reviewer seat %d names adapter %q, which is not configured/enabled", i+1, AdapterDisplayName(adapter)))
		}
		entry, ok := m.Cfg.ModelCatalog[modelKey]
		if !ok {
			return res, fault.New(fault.Usage, fmt.Sprintf("reviewer seat %d names model %q, which is not in the model catalog", i+1, modelKey))
		}
		if _, ok := entry.Adapters[adapter]; !ok {
			return res, fault.New(fault.Usage, fmt.Sprintf("reviewer seat %d: model %q has no argument for adapter %q", i+1, modelKey, AdapterDisplayName(adapter)))
		}
		id := adapter + "\x00" + modelKey
		if prev, dup := identities[id]; dup {
			return res, fault.New(fault.Usage, fmt.Sprintf("reviewer seats %d and %d are identical (%s / %s) — an identical seat adds no independent vantage and would double-count as agreement", prev+1, i+1, AdapterDisplayName(adapter), m.ModelDisplayLabel(modelKey)))
		}
		identities[id] = i
		lanes = append(lanes, map[string]any{
			"execution": "adapter",
			"adapter":   adapter,
			"model":     modelKey,
		})
	}

	path, err := m.writeTarget()
	if err != nil {
		return res, err
	}
	// ONE patch: the whole ordered panel, plus the removal of the legacy one-seat spelling. The
	// order in this slice IS the panel's order — that is how a reorder persists.
	ops := []config.SetOp{
		{Path: []string{"profiles", profile, "reviewers"}, Value: lanes},
		{Path: []string{"profiles", profile, "lanes", string(review.RoleReviewer)}, Delete: true},
	}
	if err := config.ApplyPatchToFile(path, config.ConfigPatch{Ops: ops}); err != nil {
		return res, err
	}
	res.Written, res.Path = true, path
	res.Messages = append(res.Messages, fmt.Sprintf("Saved a %d-seat reviewer panel for %s in the %s.", len(seats), ProfileDisplayName(profile), m.scopeLabel()))
	m.log(res.Messages[0] + " (" + path + ")")
	return res, nil
}
