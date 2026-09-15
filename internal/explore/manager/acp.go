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

// This file holds the writes for user-defined ACP adapters. A user points the generic ACP driver at an
// ACP-capable CLI; its launch arguments are detected and validated with a real handshake, and the
// instance is saved to `acpAdapters` in the user-scope shared adapters.yaml, where
// registry.ResolveACPInstances picks it up.

// acpProbeTimeout bounds detection and validation of a candidate binary, which launch the real CLI.
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

// DetectACP validates a candidate binary as an ACP adapter: it finds working launch arguments with
// `--help` and a real handshake, and reads the model the session reports. It writes no configuration.
// The run is bounded by acpProbeTimeout and the acpagent startup watchdog.
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

// SaveACP adds or updates a user-defined ACP adapter in the user-scope shared adapters.yaml. An empty
// name derives `acp-<basename>`, made unique. The candidate is validated with a real handshake to confirm
// the launch arguments and capture the reported model. A failed handshake still saves the instance, with
// the given arguments and no model, and returns a warning, so a CLI awaiting login can be saved and
// validated later.
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
	if model != "" {
		messages = append(messages, fmt.Sprintf("Saved ACP adapter %q (%s) — validated, model %s.", title, key, model))
	} else {
		messages = append(messages, fmt.Sprintf("Saved ACP adapter %q (%s).", title, key))
	}
	return messages, nil
}

// RemoveACP deletes a user-defined ACP adapter from the user-scope shared adapters.yaml. It returns a
// *BlockedError while a roster slot uses the adapter, and an error when no such ACP adapter is saved.
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
	return []string{fmt.Sprintf("Removed the ACP adapter %q.", name)}, nil
}

// uniqueACPKeyLocked returns `acp-<basename>` for binPath, suffixed (-2, -3, ...) to avoid an existing
// ACP instance name. The caller holds m.mu.
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

// acpSlug lowercases a binary basename into a key-safe slug: runs of letters and digits joined by '-'.
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

// humanizeBase capitalizes a binary basename for a default ACP title.
func humanizeBase(binPath string) string {
	base := strings.TrimSuffix(filepath.Base(binPath), ".exe")
	if base == "" {
		return "CLI"
	}
	return strings.ToUpper(base[:1]) + base[1:]
}
