package setup

import (
	"fmt"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// This file holds the discrete config mutations: copy or delete a profile, edit or clear a lane,
// delete a saved model, and configure or remove an adapter path. Each validates, delegates planning to
// the pure SetupEngine, and writes through config.ApplyPatchToFile. There is no "set default profile"
// operation: defaultProfile stays `default`, and customization happens by editing or copying profiles.

// CopyResult reports the outcome of CopyProfile.
type CopyResult struct {
	Written         bool
	ReplaceRequired bool   // target exists and the exact confirmation was not echoed → caller must confirm
	Blocked         bool   // copy-to-existing would not be a faithful replace (layered/lossy) → not written
	Message         string // the confirmation prompt (ReplaceRequired), a block reason, or a success note
	Source          string
	Target          string
	Path            string
}

// RemoveResult reports the outcome of RemoveAdapter.
type RemoveResult struct {
	Removed bool
	Blocked bool         // still referenced by a profile/lane → not removed
	UsedBy  []AdapterUse // the blocking uses, present when Blocked
	Message string
	Name    string
	Path    string
}

// userConfigTarget returns the user-scope config file: the discovered user config if present, else
// the canonical ~/.aimesh/review/config.yaml.
func (m *Manager) userConfigTarget() (string, error) {
	if p := config.DiscoverUserConfig(); p != "" {
		return p, nil
	}
	return config.UserConfigPath()
}

const (
	writeScopeUser    = "user"
	writeScopeProject = "project"
)

// scope normalizes WriteScope ("" → user).
func (m *Manager) scope() string {
	if m.WriteScope == writeScopeProject {
		return writeScopeProject
	}
	return writeScopeUser
}

// scopeLabel is the human name of the write scope, for block/success messages.
func (m *Manager) scopeLabel() string {
	if m.scope() == writeScopeProject {
		return "project config"
	}
	return "user/global config"
}

// writeTarget resolves the config file for the selected write scope. A project target is revalidated
// at write time: Layers is a load-time snapshot and ApplyPatchToFile creates an absent file, so a
// project config removed since loading must block rather than be recreated or fall back to user scope.
func (m *Manager) writeTarget() (string, error) {
	// Reject an unknown scope rather than treating it as a user write.
	if m.WriteScope != "" && m.WriteScope != writeScopeUser && m.WriteScope != writeScopeProject {
		return "", fault.New(fault.Usage, fmt.Sprintf("unknown config write scope %q (expected %q or %q)", m.WriteScope, writeScopeUser, writeScopeProject))
	}
	if m.scope() == writeScopeProject {
		p := m.Layers.ProjectPath
		if p == "" {
			return "", fault.New(fault.Usage, "no project config for this folder to edit — create a project config first")
		}
		if _, err := config.LoadRawMap(p); err != nil {
			return "", fault.New(fault.Usage, "the project config is missing or unreadable now — run the command again (it may have been deleted or edited since it was loaded)")
		}
		return p, nil
	}
	return m.userConfigTarget()
}

// sharedWriteTarget resolves the shared adapters file (.aimesh/adapters.yaml) for the selected write
// scope; adapter paths live there, not in config.yaml. Project scope is anchored at the repository root
// and blocks outside a repository rather than using a cwd-relative path. User scope is anchored at
// AIMESH_HOME.
func (m *Manager) sharedWriteTarget() (string, error) {
	if m.WriteScope != "" && m.WriteScope != writeScopeUser && m.WriteScope != writeScopeProject {
		return "", fault.New(fault.Usage, fmt.Sprintf("unknown config write scope %q (expected %q or %q)", m.WriteScope, writeScopeUser, writeScopeProject))
	}
	if m.scope() == writeScopeProject {
		if !m.Layers.SharedProjectHasRoot || m.Layers.SharedProjectPath == "" {
			return "", fault.New(fault.Usage, "this folder is not inside a repository, so there is no project scope for adapter paths — run `reviewmesh init` (or `repo init`) in the repo root first, or save to the user scope")
		}
		return m.Layers.SharedProjectPath, nil
	}
	if m.Layers.SharedUserPath != "" {
		return m.Layers.SharedUserPath, nil
	}
	return config.SharedUserLocationsPath()
}

// layerRef names a loaded config layer + its file path.
type layerRef struct{ layer, path string }

// higherWriteLayers returns the loaded layers above the write scope, highest first; they would shadow a
// write (precedence: shipped < user < project < explicit). User scope yields project and explicit;
// project scope yields explicit only.
func (m *Manager) higherWriteLayers() []layerRef {
	var out []layerRef
	if m.Layers.ExplicitLoaded && m.Layers.ExplicitPath != "" {
		out = append(out, layerRef{"explicit", m.Layers.ExplicitPath})
	}
	if m.scope() == writeScopeUser && m.Layers.ProjectLoaded && m.Layers.ProjectPath != "" {
		out = append(out, layerRef{"project", m.Layers.ProjectPath})
	}
	return out
}

// DeleteResult reports the outcome of DeleteProfile.
type DeleteResult struct {
	Deleted         bool
	ConfirmRequired bool   // the exact confirmation was not echoed → caller must confirm
	Blocked         bool   // structurally unsafe (dangling default pointer / not in the user layer)
	Message         string // the confirmation prompt, the block reason, or a success note
	Name            string
	Path            string
}

// ClearLaneResult is the outcome of clearing a profile lane's explicit user-layer configuration.
type ClearLaneResult struct {
	Profile         string
	Role            string
	Cleared         bool   // the user-layer lane override was deleted
	Blocked         bool   // a higher (project/explicit) layer defines this lane → clear would be shadowed
	ConfirmRequired bool   // the caller must echo the confirm token (Message) to proceed
	NoOp            bool   // nothing to clear: the lane has no user-layer override (inherited/unset)
	RevealedLower   bool   // after clearing, a lower (built-in) layer's lane is revealed
	Path            string // the user config file written
	Message         string
}

// DeleteSavedModelResult reports the outcome of DeleteSavedModel.
type DeleteSavedModelResult struct {
	Key           string
	ConfigWritten bool
	Path          string
	Blocked       bool
	UsedBy        []string
	Message       string
}

// writeAdapterPathShared records a validated adapter binary path in the write scope's shared
// adapters file (.aimesh/adapters.yaml). SetAdapterPath and ConfigureAdapterPath both use it.
func (m *Manager) writeAdapterPathShared(name, binPath string) (Result, error) {
	var res Result
	shared, err := m.sharedWriteTarget()
	if err != nil {
		return res, err
	}
	if err := config.SetSharedAdapterPath(shared, name, binPath); err != nil {
		return res, err
	}
	res.ConfigWritten, res.Path = true, shared
	res.Messages = append(res.Messages, fmt.Sprintf("set adapters.%s.path", name))
	m.log(fmt.Sprintf("Recorded adapters.%s.path = %s in %s (no secrets; native auth/trust stays with the CLI).", name, binPath, shared))
	return res, nil
}
