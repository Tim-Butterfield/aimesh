package setup

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	esetup "github.com/Tim-Butterfield/aimesh/internal/review/engine/setup"
	"github.com/Tim-Butterfield/aimesh/internal/review/utility/doctor"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// This file exposes doctor repairs to the web-UI surface as a non-linear projection + apply:
// doctor FINDS issues (utility), the manager maps each failing check to a repair OPTION (reusing
// SetupEngine.RepairGuidance), and ApplyRepair performs ONLY a user-selected, confirmed adapter
// binary-path write through the single config-write path. Nothing auto-applies; re-auth and
// provider/model/trust changes stay guidance-only (never automated).

// RepairOption is a projection of one failing doctor check into a presentable repair. `Applyable`
// is true only for an adapter binary-path issue the UI can fix in place (kind=set_binary_path,
// which needs a path value); everything else is guidance-only.
type RepairOption struct {
	Check     string `json:"check"`
	Detail    string `json:"detail"`
	Message   string `json:"message"`
	Command   string `json:"command,omitempty"`
	Kind      string `json:"kind"` // "set_binary_path" | "guidance"
	Target    string `json:"target,omitempty"`
	Applyable bool   `json:"applyable"`
}

// RepairOptions projects the failing checks of a doctor report into repair options. Passing
// checks are skipped. It performs NO I/O and never writes.
func (m *Manager) RepairOptions(rep doctor.Report) []RepairOption {
	out := make([]RepairOption, 0)
	for _, c := range rep.Checks {
		if c.OK {
			continue
		}
		name, isAdapter := strings.CutPrefix(c.Name, "adapter: ")
		hint := ""
		if isAdapter {
			hint = m.detectHint(name)
		}
		g := m.engine().RepairGuidance(esetup.RepairIssue{Name: c.Name, Detail: c.Detail, BinHint: hint})
		opt := RepairOption{Check: c.Name, Detail: c.Detail, Message: g.Message, Command: g.Command, Kind: "guidance"}
		if isAdapter && m.isConfigurable(name) {
			opt.Kind = string(esetup.ActionSetBinaryPath)
			opt.Target = name
			opt.Applyable = true
		}
		out = append(out, opt)
	}
	return out
}

// ApplyRepair applies a single user-selected, confirmed repair through the shared repair path.
// The ONLY applyable kind is `set_binary_path` (record a validated adapter binary path, user
// scope); every other kind (e.g. re-authentication) is guidance-only and rejected — reviewmesh
// never automates provider/model/trust/auth changes.
func (m *Manager) ApplyRepair(kind, target, value string) (Result, error) {
	var res Result
	// set_default_profile: restore a legacy/stale defaultProfile pointer to the fixed
	// workbench default (`default`). It is a repair, not a "set as default" feature — the
	// written value is always the literal "default", never a caller-chosen name.
	if esetup.RepairActionKind(kind) == esetup.ActionSetDefaultProfile {
		if _, ok := m.Cfg.Profiles["default"]; !ok {
			return res, fault.New(fault.Config, "no profile named `default` exists to restore the default pointer to")
		}
		if m.Cfg.DefaultProfile == "default" {
			return res, fault.New(fault.Usage, "defaultProfile already points at `default` — nothing to repair")
		}
		patch, err := m.engine().PatchFor(esetup.RepairAction{Kind: esetup.ActionSetDefaultProfile}, "")
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
		res.Messages = append(res.Messages, "repaired defaultProfile — it points at `default` again")
		m.log(fmt.Sprintf("Applied repair: set defaultProfile = \"default\" in %s (was %q; user-selected + confirmed; no profile modified or deleted).", path, m.Cfg.DefaultProfile))
		return res, nil
	}
	if esetup.RepairActionKind(kind) != esetup.ActionSetBinaryPath {
		return res, fault.New(fault.Usage,
			fmt.Sprintf("repair %q is guidance-only — no automatic write (re-authentication and provider/model/trust changes stay with the adapter's own CLI)", kind))
	}
	// set_binary_path is exactly "record an adapter path" — route it through the shared-file write seam
	// (adapter binary paths live only in .aimesh/adapters.yaml).
	if err := m.engine().ValidateAdapterForPathCapture(target, m.configurableNames()); err != nil {
		return res, err
	}
	if err := validateBinaryPath(value); err != nil {
		return res, err
	}
	res, err := m.writeAdapterPathShared(target, value)
	if err != nil {
		return res, err
	}
	m.log(fmt.Sprintf("Applied repair: set adapters.%s.path = %s in %s (user-selected + confirmed; no auth/model change).", target, value, res.Path))
	return res, nil
}
