package manager

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"
)

// This file is the governed write surface for USER-DEFINED generic ACP adapters (the "Add ACP" flow).
// There is no fixed ACP catalog: a user points the generic ACP driver at any ACP-capable CLI binary, the
// launch args are auto-detected + validated (a real handshake), and the instance is saved to the
// user-scope shared .aimesh/adapters.yaml `acpAdapters`. registry.ResolveACPInstances then synthesizes
// it into a runnable adapter on the next re-resolve.

// acpProbeTimeout bounds a candidate-binary probe: ValidateCandidate launches the real CLI (each try is
// itself watchdog-bounded), so this caps the total detect/validate wall time.
const acpProbeTimeout = 90 * time.Second

// DetectDTO reports probing a candidate ACP binary: the launch Args that worked, the Model the session
// reported (may be empty), a SuggestedTitle, and OK/Detail.
type DetectDTO struct {
	OK             bool     `json:"ok"`
	Args           []string `json:"args"`
	Model          string   `json:"model,omitempty"`
	SuggestedTitle string   `json:"suggestedTitle,omitempty"`
	Detail         string   `json:"detail,omitempty"`
}

// DetectACP validates a candidate binary as an ACP adapter: it finds the launch args (via `--help` + a
// real ACP handshake) and reads the model the session reports, proposing an editable "ACP: <CLI>" title.
// Read-only (no config write). It launches the real CLI but is bounded by acpProbeTimeout + the acpagent
// startup watchdog.
func (m *Manager) DetectACP(path string, argsHint []string) (DetectDTO, error) {
	if strings.TrimSpace(path) == "" {
		return DetectDTO{}, fault.New(fault.Usage, "a binary path is required")
	}
	if err := validateBinaryPath(path); err != nil {
		return DetectDTO{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), acpProbeTimeout)
	defer cancel()
	c := acpagent.ValidateCandidate(ctx, path, argsHint)
	return DetectDTO{OK: c.OK, Args: c.Args, Model: c.Model, SuggestedTitle: c.Title, Detail: c.Detail}, nil
}

// SaveACP adds or updates a user-defined ACP adapter instance in the user-scope shared adapters.yaml.
// name is the stable key — when empty it is derived from the binary basename (`acp-<basename>`) and
// uniquified. It VALIDATES the candidate (a real ACP handshake) to confirm the launch args and capture
// the reported model (the expected model for identity verification). A handshake failure does NOT block
// the save — the instance is still written (with the user's args, no captured model) and the failure is
// surfaced as a warning — so a CLI mid-login can be saved and re-validated later. Re-resolves + bumps.
func (m *Manager) SaveACP(name, title, path string, args []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if strings.TrimSpace(path) == "" {
		return nil, fault.New(fault.Usage, "a binary path is required")
	}
	if err := validateBinaryPath(path); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(name)
	if key == "" {
		key = m.uniqueACPKeyLocked(path)
	}
	if strings.TrimSpace(title) == "" {
		title = "ACP: " + humanizeBase(path)
	}
	if len(args) == 0 {
		args = []string{"--acp"}
	}

	// Validate: confirm the launch args + capture the reported model. On success save the args that
	// actually worked; on failure keep the user's args and warn (no model captured).
	ctx, cancel := context.WithTimeout(context.Background(), acpProbeTimeout)
	defer cancel()
	var messages []string
	var model string
	c := acpagent.ValidateCandidate(ctx, path, args)
	if c.OK {
		args, model = c.Args, c.Model
	} else {
		messages = append(messages, fmt.Sprintf("warning: saved, but the ACP handshake did not complete (%s) — the model is not captured; complete the CLI's own login/folder-trust, then re-save to enable identity verification", c.Detail))
	}
	if err := adapterlocations.Update(m.sharedPath, func(loc *adapterlocations.Locations) {
		loc.ACPAdapters[key] = adapterlocations.ACPInstance{Title: title, Path: path, Args: args, Model: model}
	}); err != nil {
		return nil, err
	}
	if err := m.reresolveLocked(); err != nil {
		return nil, err
	}
	m.generation++
	if model != "" {
		messages = append(messages, fmt.Sprintf("Saved ACP adapter %q (%s) — validated, model %s.", title, key, model))
	} else {
		messages = append(messages, fmt.Sprintf("Saved ACP adapter %q (%s).", title, key))
	}
	return messages, nil
}

// RemoveACP deletes a user-defined ACP adapter instance from the user-scope shared adapters.yaml. It is
// BLOCKED (a *BlockedError with the using slots) while the roster references the adapter, and reports a
// clear error when the named instance is not a saved ACP adapter.
func (m *Manager) RemoveACP(name string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fault.New(fault.Usage, "an adapter name is required")
	}
	if uses := m.adapterUsageLocked()[name]; len(uses) > 0 {
		return nil, &BlockedError{
			Message: fmt.Sprintf("%s is used by %d roster slot(s); reconfigure the roster first", m.adapterDisplay(name), len(uses)),
			UsedBy:  uses,
		}
	}
	removed := false
	if err := adapterlocations.Update(m.sharedPath, func(loc *adapterlocations.Locations) {
		if _, ok := loc.ACPAdapters[name]; ok {
			removed = true
			delete(loc.ACPAdapters, name)
		}
	}); err != nil {
		return nil, err
	}
	if !removed {
		return nil, fault.New(fault.Config, fmt.Sprintf("%q is not a saved ACP adapter", name))
	}
	if err := m.reresolveLocked(); err != nil {
		return nil, err
	}
	m.generation++
	return []string{fmt.Sprintf("Removed the ACP adapter %q.", name)}, nil
}

// uniqueACPKeyLocked derives a stable slug key `acp-<basename>` for a new instance, uniquified against
// the shared file's existing ACP instances (…-2, …-3). Caller holds the mutex.
func (m *Manager) uniqueACPKeyLocked(binPath string) string {
	base := acpSlug(filepath.Base(binPath))
	if base == "" {
		base = "cli"
	}
	key := "acp-" + base
	existing := map[string]bool{}
	if loc, err := adapterlocations.Load(m.sharedPath); err == nil {
		for n := range loc.ACPAdapters {
			existing[n] = true
		}
	}
	if !existing[key] {
		return key
	}
	for i := 2; ; i++ {
		k := fmt.Sprintf("%s-%d", key, i)
		if !existing[k] {
			return k
		}
	}
}

// acpSlug lowercases a binary basename into a config-key-safe slug (alnum runs joined by '-').
func acpSlug(base string) string {
	base = strings.TrimSuffix(strings.ToLower(base), ".exe")
	var b strings.Builder
	prevDash := false
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// humanizeBase title-cases a binary basename for a default ACP title.
func humanizeBase(binPath string) string {
	base := strings.TrimSuffix(filepath.Base(binPath), ".exe")
	if base == "" {
		return "CLI"
	}
	return strings.ToUpper(base[:1]) + base[1:]
}
