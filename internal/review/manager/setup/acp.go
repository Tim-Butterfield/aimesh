package setup

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"
)

// This file is the governed write surface for USER-DEFINED generic ACP adapters — the "Add ACP" flow.
// There is no fixed ACP catalog: a user points the generic ACP driver at any ACP-capable CLI binary,
// the launch args are auto-detected + validated, and the instance is saved to the scope's shared
// .aimesh/adapters.yaml (config.SetACPInstance). LoadLayered then synthesizes it into a typed adapter.

// ACPDetectResult reports probing a candidate ACP binary: the launch Args that worked, the Model the
// session reported (may be empty), a SuggestedTitle, and OK/Detail.
type ACPDetectResult struct {
	OK             bool     `json:"ok"`
	Args           []string `json:"args"`
	Model          string   `json:"model,omitempty"`
	SuggestedTitle string   `json:"suggestedTitle,omitempty"`
	Detail         string   `json:"detail,omitempty"`
}

// DetectACPAdapter validates a candidate binary as an ACP adapter: it finds the launch args (via
// `--help` + a real ACP handshake) and reads the model the session reports, proposing an editable
// "ACP: <CLI>" title. It launches the real CLI but is bounded by the acpagent startup watchdog.
func (m *Manager) DetectACPAdapter(ctx context.Context, path string, argsHint []string) (ACPDetectResult, error) {
	if strings.TrimSpace(path) == "" {
		return ACPDetectResult{}, fault.New(fault.Usage, "a binary path is required")
	}
	if err := validateBinaryPath(path); err != nil {
		return ACPDetectResult{}, err
	}
	c := acpagent.ValidateCandidate(ctx, path, argsHint)
	return ACPDetectResult{OK: c.OK, Args: c.Args, Model: c.Model, SuggestedTitle: c.Title, Detail: c.Detail}, nil
}

// SaveACPAdapter adds or updates a user-defined ACP adapter instance in the selected scope's shared
// adapters.yaml. name is the stable key (empty → derived from the binary and uniquified for an add);
// title/path/args are the editable instance. It VALIDATES the candidate (a real ACP handshake) to
// confirm the launch args and capture the agent's reported model — so the instance is review-ready
// (identity verification has an expected model). A validation failure does NOT block the save (the
// instance is still written, minus a captured model), but it is surfaced as a warning so the user can
// complete the CLI's own login/folder-trust and re-save.
func (m *Manager) SaveACPAdapter(ctx context.Context, name, title, path string, args []string) (Result, error) {
	var res Result
	if strings.TrimSpace(path) == "" {
		return res, fault.New(fault.Usage, "a binary path is required")
	}
	if err := validateBinaryPath(path); err != nil {
		return res, err
	}
	shared, err := m.sharedWriteTarget()
	if err != nil {
		return res, err
	}
	key := name
	if key == "" {
		key = m.uniqueACPKey(shared, path)
	}
	if strings.TrimSpace(title) == "" {
		title = "ACP: " + humanizeBase(path)
	}
	if len(args) == 0 {
		args = []string{"--acp"}
	}
	// Validate the candidate: confirm the launch args + capture the reported model. On success we save
	// the args that actually worked; on failure we keep the user's args and warn (no model captured).
	var model string
	c := acpagent.ValidateCandidate(ctx, path, args)
	if c.OK {
		args, model = c.Args, c.Model
	} else {
		res.Messages = append(res.Messages, fmt.Sprintf("warning: saved, but the ACP handshake did not complete (%s) — the model is not captured; complete the CLI's own login/folder-trust, then re-save to enable identity verification", c.Detail))
	}
	if err := config.SetACPInstance(shared, key, config.ACPInstance{Title: title, Path: path, Args: args, Model: model}); err != nil {
		return res, err
	}
	res.ConfigWritten, res.Path = true, shared
	if model != "" {
		res.Messages = append(res.Messages, fmt.Sprintf("saved ACP adapter %q (%s) — validated, model %s", title, key, model))
	} else {
		res.Messages = append(res.Messages, fmt.Sprintf("saved ACP adapter %q (%s)", title, key))
	}
	m.log(fmt.Sprintf("Saved ACP adapter %q → %s in %s (path %s; args %v; model %q).", title, key, shared, path, args, model))
	return res, nil
}

// RemoveACPAdapter deletes a user-defined ACP adapter instance from the scope's shared adapters.yaml.
// It is blocked while a profile lane references the adapter.
func (m *Manager) RemoveACPAdapter(name string) (RemoveResult, error) {
	res := RemoveResult{Name: name}
	if name == "" {
		return res, fault.New(fault.Usage, "an adapter name is required")
	}
	if uses := m.adapterUsage()[name]; len(uses) > 0 {
		res.Blocked, res.UsedBy = true, uses
		res.Message = fmt.Sprintf("%s is in use by %d profile lane(s); reconfigure those profiles first", AdapterDisplayName(name), len(uses))
		return res, nil
	}
	shared, err := m.sharedWriteTarget()
	if err != nil {
		return res, err
	}
	removed, err := config.DeleteACPInstance(shared, name)
	if err != nil {
		return res, err
	}
	if !removed {
		res.Blocked = true
		res.Message = fmt.Sprintf("%q is not a saved ACP adapter in the %s.", name, m.scopeLabel())
		return res, nil
	}
	res.Removed, res.Path = true, shared
	res.Message = fmt.Sprintf("Removed the ACP adapter %q from the %s.", name, m.scopeLabel())
	m.log(res.Message)
	return res, nil
}

// uniqueACPKey derives a stable slug key `acp-<basename>` for a new instance, uniquified against the
// scope's existing ACP instances (…-2, …-3).
func (m *Manager) uniqueACPKey(sharedPath, binPath string) string {
	base := acpSlug(filepath.Base(binPath))
	if base == "" {
		base = "cli"
	}
	key := "acp-" + base
	existing := map[string]bool{}
	for _, n := range config.ACPInstanceNames(sharedPath) {
		existing[n] = true
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

// humanizeBase title-cases a binary basename for a default title.
func humanizeBase(binPath string) string {
	base := strings.TrimSuffix(filepath.Base(binPath), ".exe")
	if base == "" {
		return "CLI"
	}
	return strings.ToUpper(base[:1]) + base[1:]
}
