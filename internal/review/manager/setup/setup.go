// Package setup configures reviewmesh: it writes the selected-scope config and shared adapter paths,
// runs the interactive setup wizard, guides doctor repairs, and provides read projections of the
// resolved config. It never writes secrets.
package setup

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	esetup "github.com/Tim-Butterfield/aimesh/internal/review/engine/setup"
	"github.com/Tim-Butterfield/aimesh/internal/review/utility/doctor"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// Manager owns setup and repair. It writes only the config store, never secrets, and exposes read
// projections of the resolved config (see views.go).
type Manager struct {
	Adapters map[string]model.Adapter
	Out      io.Writer
	// Cfg, Layers and ArtifactDir are the read state for the projections in views.go.
	Cfg         config.Config
	Layers      config.Layers
	ArtifactDir string
	// WriteScope selects the config layer mutations write to: empty or "user" for the user config,
	// "project" for this folder's project config. A project scope without a project config is blocked.
	WriteScope string
}

// Result reports what setup or repair did.
type Result struct {
	ConfigWritten bool
	Path          string
	Backup        string // path of the pre-write backup, when one was taken (model-key cleanup)
	Messages      []string
	// Conflict is set, and ConfigWritten stays false, when EditProfileLane's choice would overwrite a
	// different existing catalog entry and Replace was not passed.
	Conflict *SavedModelConflict
}

func (m *Manager) log(msg string) {
	if m.Out != nil {
		fmt.Fprintln(m.Out, msg)
	}
}

// engine returns the pure SetupEngine (stateless) the manager delegates planning to.
func (m *Manager) engine() *esetup.Engine { return esetup.New() }

// targetConfigPath returns the config file under baseDir to write: an existing discovered config if
// present, else config.yaml, so a new config.yaml never shadows an existing config.json.
func targetConfigPath(baseDir string) string {
	if p := config.DiscoverProjectConfig(baseDir); p != "" {
		return p
	}
	return config.ProjectConfigPath(baseDir)
}

// Setup writes the shipped seed config under baseDir unless a config already exists. With a non-nil
// ask it confirms first. It never writes secrets.
func (m *Manager) Setup(baseDir string, ask review.Prompter) (Result, error) {
	path := targetConfigPath(baseDir)

	m.log(fmt.Sprintf("Detected runnable adapters: %v", m.availableAdapters()))
	m.log("Real cloud adapters (devin-cli, claude-code, codex-cli, agy-cli, ollama, gemini-cli) need their own CLI installed and authenticated; they are not configured by this step.")
	m.log("Note: reviewmesh never stores secrets in config — authentication stays with each adapter's CLI.")

	return m.writeConfigIfAbsent(path, config.Default(), ask, "default profile: default (unconfigured — configure its lanes or copy a ready-made profile)", esetup.SetupKindDefault)
}

// availableAdapters returns the sorted names of registered adapters that report available. It makes
// no model call.
func (m *Manager) availableAdapters() []string {
	avail := make([]string, 0, len(m.Adapters))
	for name, a := range m.Adapters {
		if ok, _ := a.Available(); ok {
			avail = append(avail, name)
		}
	}
	sort.Strings(avail)
	return avail
}

// SetupWizard runs interactive setup: it picks a profile to configure, records validated adapter
// binary paths, optionally chooses lane models from the catalog, shows a summary, and writes only after
// confirmation. An existing config is patched with unrelated fields preserved; a new one starts from
// the shipped seed. defaultProfile is never changed. With a nil Prompter it prints guidance and returns
// a usage error.
func (m *Manager) SetupWizard(baseDir string, ask review.Prompter) (Result, error) {
	var res Result
	if ask == nil {
		m.log("Interactive setup needs a terminal. Run `reviewmesh setup` for non-interactive defaults, or `aimesh review setup --adapter <name> --path <path>` to record an adapter binary path.")
		return res, fault.New(fault.Usage, "interactive setup requires a terminal (no prompter)")
	}
	path := targetConfigPath(baseDir)
	_, statErr := os.Stat(path)
	exists := statErr == nil

	m.log(fmt.Sprintf("Detected runnable adapters: %v", m.availableAdapters()))
	m.log("Authentication stays with each adapter's own CLI; reviewmesh never stores secrets.")

	// Strict-load an existing config so a malformed file is refused rather than overwritten.
	var cfg config.Config
	if exists {
		loaded, err := config.Load(path)
		if err != nil {
			return res, err
		}
		cfg = loaded
		m.log("Updating existing config at " + path + " (unrelated settings preserved).")
	} else {
		cfg = config.Default()
	}

	// 1. Choose the profile to configure. This never writes defaultProfile: the default profile is the
	// profile named `default`.
	profiles := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		// Never offer the hidden test profile.
		if config.IsHiddenProfile(name) {
			continue
		}
		profiles = append(profiles, name)
	}
	sort.Strings(profiles)
	chosenProfile := "default"
	if _, ok := cfg.Profiles[chosenProfile]; !ok && len(profiles) > 0 {
		chosenProfile = profiles[0]
	}
	if len(profiles) > 0 {
		if sel := ask.Choose("Which profile do you want to configure?", profiles); sel >= 0 && sel < len(profiles) {
			chosenProfile = profiles[sel]
		}
	}

	// 2. Record adapter binary paths, validating each first.
	paths := map[string]string{}
	cfgNames := m.configurableNames()
	for len(cfgNames) > 0 && ask.Confirm("Record a binary path for an adapter?") {
		sel := ask.Choose("Which adapter?", cfgNames)
		if sel < 0 || sel >= len(cfgNames) {
			m.log("  No adapter selected; skipping.")
			continue
		}
		name := cfgNames[sel]
		p := ask.AskPath("Full path to the " + name + " binary:")
		if err := validateBinaryPath(p); err != nil {
			m.log("  Rejected: " + err.Error() + " — not recorded.")
			continue
		}
		paths[name] = p
		m.log("  Will set adapters." + name + ".path")
	}
	pathNames := make([]string, 0, len(paths))
	for name := range paths {
		pathNames = append(pathNames, name)
	}
	sort.Strings(pathNames)

	// 3. Offer model selection for the chosen profile's non-fake lanes whose adapter has at least two
	// catalog entries. A catalog entry sets the model and its effort together.
	var laneChoices []esetup.LaneChoice
	if prof, ok := cfg.Profiles[chosenProfile]; ok {
		roles := make([]string, 0, len(prof.Lanes))
		for r := range prof.Lanes {
			roles = append(roles, r)
		}
		sort.Strings(roles)
		for _, role := range roles {
			adapter := prof.Lanes[role].Adapter
			keys := catalogKeysForAdapter(cfg, adapter)
			if adapter == "" || adapter == "fake" || len(keys) < 2 {
				continue
			}
			if !ask.Confirm(fmt.Sprintf("Choose the model for the %q lane (%s)?", role, adapter)) {
				continue
			}
			labels := make([]string, len(keys))
			for i, k := range keys {
				labels[i] = catalogLabel(cfg, k)
			}
			sel := ask.Choose(fmt.Sprintf("Model for %s (%s)?", role, adapter), labels)
			if sel < 0 || sel >= len(keys) {
				m.log("  No model selected; keeping the profile default.")
				continue
			}
			laneChoices = append(laneChoices, esetup.LaneChoice{Role: role, Adapter: adapter, Model: keys[sel]})
			m.log("  Will set profiles." + chosenProfile + ".lanes." + role + ".model = " + keys[sel])
		}
	}

	// 4. Summarize, then 5. confirm before any write.
	summary := "(none)"
	if len(pathNames) > 0 {
		summary = strings.Join(pathNames, ", ")
	}
	modelSummary := "(profile defaults)"
	if len(laneChoices) > 0 {
		parts := make([]string, len(laneChoices))
		for i, lc := range laneChoices {
			parts[i] = lc.Role + "→" + lc.Model
		}
		modelSummary = strings.Join(parts, ", ")
	}
	m.log("Summary: profile configured=" + chosenProfile + " (defaultProfile is not changed); adapter paths to record: " + summary + "; lane models: " + modelSummary)
	if !ask.Confirm("Write this configuration to " + path + "?") {
		m.log("Cancelled; nothing was written.")
		res.Messages = append(res.Messages, "cancelled")
		return res, nil
	}

	// 6. Write lane models to config.yaml and adapter paths to the scope's shared adapters.yaml.
	// Resolve the shared target first, so project scope outside a repository blocks before any write.
	var shared string
	if len(pathNames) > 0 {
		s, err := m.sharedWriteTarget()
		if err != nil {
			return res, err
		}
		shared = s
	}

	// config.yaml: lane models, creating the file when absent.
	if exists {
		if len(laneChoices) > 0 {
			lanePatch, err := m.engine().PlanLaneChoices(chosenProfile, laneChoices)
			if err != nil {
				return res, err
			}
			if err := config.ApplyPatchToFile(path, lanePatch); err != nil {
				return res, err
			}
			res.Messages = append(res.Messages, "wrote "+path)
		}
	} else {
		for _, lc := range laneChoices {
			lane := cfg.Profiles[chosenProfile].Lanes[lc.Role]
			lane.Adapter, lane.Model = lc.Adapter, lc.Model
			cfg.Profiles[chosenProfile].Lanes[lc.Role] = lane
		}
		if err := cfg.WriteYAMLFile(path); err != nil {
			return res, err
		}
		res.Messages = append(res.Messages, "wrote "+path)
	}

	// Shared adapters.yaml: config.yaml is already written, so a failure here is reported as partial.
	for _, name := range pathNames {
		if err := config.SetSharedAdapterPath(shared, name, paths[name]); err != nil {
			return res, fmt.Errorf("config written to %s, but recording adapters.%s.path in %s failed: %w", path, name, shared, err)
		}
	}
	if len(pathNames) > 0 {
		res.Messages = append(res.Messages, fmt.Sprintf("recorded %d adapter path(s) in %s", len(pathNames), shared))
	}

	res.ConfigWritten, res.Path = true, path
	m.log("Wrote " + path + " (configured profile " + chosenProfile + "; defaultProfile unchanged; no secrets).")
	return res, nil
}

// SetupLocalOllama writes a selected-scope config with the fully-local-ollama profile, using modelTag
// (typically REVIEWMESH_OLLAMA_MODEL). An empty tag still writes a usable config and recommends setting
// one. The `default` profile remains the default.
func (m *Manager) SetupLocalOllama(baseDir, modelTag string, ask review.Prompter) (Result, error) {
	path := targetConfigPath(baseDir)
	// fully-local-ollama is an example profile, not part of the seed, so add it here.
	cfg := config.WithOllamaModel(config.Default(), modelTag)
	if p, ok := config.ExampleProfile("fully-local-ollama"); ok {
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]config.Profile{}
		}
		cfg.Profiles["fully-local-ollama"] = p
	}
	if modelTag == "" {
		m.log("No Ollama model tag provided. Set REVIEWMESH_OLLAMA_MODEL or edit modelCatalog.ollama-local.adapters.ollama.modelArg in the written config (pick a tag from `ollama list`).")
	} else {
		m.log("Configuring fully-local-ollama with model tag: " + modelTag)
	}
	m.log("Note: the `default` profile ships unconfigured and remains the default; no cloud adapter is selected. Run with --profile fully-local-ollama to use Ollama.")
	return m.writeConfigIfAbsent(path, cfg, ask, "fully-local-ollama available; default profile: "+cfg.DefaultProfile, esetup.SetupKindFullyLocalOllama)
}

// SetAdapterPath records an adapter's binary path in the write scope's shared adapters.yaml, preserving
// other entries. It validates the name and path before writing and writes no secrets. The target comes
// from WriteScope; baseDir is unused.
func (m *Manager) SetAdapterPath(baseDir, name, binPath string) (Result, error) {
	_ = baseDir // the target is scope-anchored
	if err := m.engine().ValidateAdapterForPathCapture(name, m.configurableNames()); err != nil {
		return Result{}, err
	}
	if err := validateBinaryPath(binPath); err != nil {
		return Result{}, err
	}
	return m.writeAdapterPathShared(name, binPath)
}

// PromoteUserToProject copies selected overrides from the user config into the project config under
// projectBaseDir: defaultProfile, plus adapter paths when includePaths is set. It preserves unrelated
// project fields, refuses a malformed project file, and writes no secrets. An interactive ask confirms
// after a summary; otherwise assumeYes is required, and without it nothing is written.
func (m *Manager) PromoteUserToProject(projectBaseDir string, includePaths, assumeYes bool, ask review.Prompter) (Result, error) {
	var res Result
	userPath := config.DiscoverUserConfig()
	if userPath == "" {
		return res, fault.New(fault.Config, "no user/global config found to promote from (run `reviewmesh setup` first)")
	}
	userRaw, err := config.LoadRawMap(userPath)
	if err != nil {
		return res, err
	}
	patch := m.engine().PlanPromotion(userRaw) // defaultProfile into the project config.yaml
	// Adapter paths are copied between shared adapters files only when requested.
	var pathNames []string
	if includePaths {
		if userShared, serr := config.SharedUserLocationsPath(); serr == nil {
			pathNames = config.SharedAdapterNames(userShared)
		}
	}
	if len(patch.Ops) == 0 && len(pathNames) == 0 {
		hint := ""
		if !includePaths {
			hint = " (adapter binary paths are skipped unless you pass --include-adapter-paths)"
		}
		m.log("Nothing to promote from " + userPath + hint + ".")
		res.Messages = append(res.Messages, "nothing to promote")
		return res, nil
	}
	m.log("Promote from " + userPath + " into the project config:")
	for _, op := range patch.Ops {
		m.log(fmt.Sprintf("  %s = %v", strings.Join(op.Path, "."), op.Value))
	}
	for _, n := range pathNames {
		m.log("  adapters." + n + ".path → project .aimesh/adapters.yaml")
	}
	if ask != nil {
		if !ask.Confirm("Write these settings into the project config?") {
			m.log("Cancelled; nothing was written.")
			res.Messages = append(res.Messages, "cancelled")
			return res, nil
		}
	} else if !assumeYes {
		m.log("Nothing written. Re-run with --yes to promote (or --interactive to confirm).")
		return res, fault.New(fault.Usage, "promotion needs --yes (non-interactive) or --interactive")
	}
	// Resolve the project shared target before any write, so a missing repository blocks cleanly.
	var projShared string
	if len(pathNames) > 0 {
		s, ok := config.SharedProjectLocationsPath(projectBaseDir)
		if !ok {
			return res, fault.New(fault.Usage, "this folder is not inside a repository, so there is no project scope for adapter paths — run `reviewmesh init` in the repo root first, or promote without --include-adapter-paths")
		}
		projShared = s
	}
	projPath := targetConfigPath(projectBaseDir)
	if len(patch.Ops) > 0 {
		if err := config.ApplyPatchToFile(projPath, patch); err != nil {
			return res, err
		}
		res.Messages = append(res.Messages, "promoted into "+projPath)
	}
	if len(pathNames) > 0 {
		userShared, _ := config.SharedUserLocationsPath()
		copied, cerr := config.CopySharedAdapterPaths(userShared, projShared)
		if cerr != nil {
			return res, fmt.Errorf("promoted config into %s, but copying adapter paths into %s failed: %w", projPath, projShared, cerr)
		}
		res.Messages = append(res.Messages, fmt.Sprintf("promoted %d adapter path(s) into %s", len(copied), projShared))
	}
	res.ConfigWritten, res.Path = true, projPath
	m.log("Promoted into " + projPath + " (+ adapter paths into the project .aimesh/adapters.yaml when requested; selected keys only; no secrets).")
	return res, nil
}

// ProjectPromotionPreview describes what PromoteUserToProject would write: the target path, whether a
// project config already exists, and the keys to write.
type ProjectPromotionPreview struct {
	TargetPath     string   `json:"targetPath"`
	Exists         bool     `json:"exists"`
	DefaultProfile string   `json:"defaultProfile,omitempty"`
	AdapterPaths   []string `json:"adapterPaths,omitempty"`
	Ops            int      `json:"ops"`
}

// detectHint returns the binary name to suggest in a repair command (for example "claude" for
// "claude-code"), falling back to the adapter name.
func (m *Manager) detectHint(name string) string {
	if a, ok := m.Adapters[name]; ok {
		if d, ok := a.(interface{ DetectName() string }); ok {
			return d.DetectName()
		}
	}
	return name
}

// configurableNames returns the sorted names of registered adapters other than `fake`.
func (m *Manager) configurableNames() []string {
	out := make([]string, 0, len(m.Adapters))
	for n := range m.Adapters {
		if n != "fake" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// catalogKeysForAdapter returns the sorted model-catalog keys whose entry supports the given
// adapter (has an `Adapters[adapter]` modelArg) — the selectable models for a lane on that
// adapter. Empty adapter → none.
func catalogKeysForAdapter(cfg config.Config, adapter string) []string {
	if adapter == "" {
		return nil
	}
	var keys []string
	for k, entry := range cfg.ModelCatalog {
		if _, ok := entry.Adapters[adapter]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// catalogLabel is the user-facing label for a catalog key: its DisplayName (falling back to
// the key), the effort tier when set, and the key for disambiguation.
func catalogLabel(cfg config.Config, key string) string {
	entry, ok := cfg.ModelCatalog[key]
	if !ok {
		return key
	}
	label := entry.DisplayName
	if label == "" {
		label = key
	}
	if entry.Effort != "" {
		label += " [" + entry.Effort + "]"
	}
	return label + " (" + key + ")"
}

// validateBinaryPath checks a configured adapter binary path: it must exist, not be a
// directory, and (on POSIX) be executable.
func validateBinaryPath(p string) error {
	if p == "" {
		return fault.New(fault.Usage, "a binary path is required (--path)")
	}
	fi, err := os.Stat(p)
	if err != nil {
		return fault.New(fault.Config, fmt.Sprintf("path %q not found", p))
	}
	if fi.IsDir() {
		return fault.New(fault.Config, fmt.Sprintf("path %q is a directory, not a binary", p))
	}
	if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
		return fault.New(fault.Config, fmt.Sprintf("path %q is not executable", p))
	}
	return nil
}

// writeConfigIfAbsent writes cfg to path unless a config already exists there
// (idempotent, never clobbers). Interactive callers confirm first.
func (m *Manager) writeConfigIfAbsent(path string, cfg config.Config, ask review.Prompter, label string, kind esetup.SetupKind) (Result, error) {
	var res Result
	// I/O fact (does a config already exist?) → pure decision (preserve vs write).
	_, statErr := os.Stat(path)
	if plan := m.engine().PlanInitialSetup(kind, esetup.ConfigPresence{Exists: statErr == nil}); !plan.ShouldWrite {
		m.log("Existing config at " + path + " left unchanged (preserving your settings).")
		res.Path = path
		res.Messages = append(res.Messages, plan.Reason)
		return res, nil
	}
	if ask != nil {
		if !ask.Confirm(fmt.Sprintf("Write config to %s?", path)) {
			m.log("Cancelled; nothing was written.")
			res.Messages = append(res.Messages, "cancelled")
			return res, nil
		}
	}
	if err := cfg.WriteYAMLFile(path); err != nil {
		return res, err
	}
	res.ConfigWritten, res.Path = true, path
	res.Messages = append(res.Messages, "wrote "+path)
	m.log("Wrote " + path + " (" + label + ", no secrets).")
	return res, nil
}

// isConfigurable reports whether name is a registered, path-configurable adapter
// (anything except the built-in `fake`).
func (m *Manager) isConfigurable(name string) bool {
	if name == "fake" {
		return false
	}
	_, ok := m.Adapters[name]
	return ok
}

// Repair prints guidance for each failed doctor check, including the setup command for an adapter
// issue. With a non-nil ask it also offers to set a validated binary path for an adapter issue. Other
// issues, including authentication, are guidance only. With a nil Prompter it writes nothing.
func (m *Manager) Repair(baseDir string, rep doctor.Report, ask review.Prompter) Result {
	var res Result
	if rep.OK {
		m.log("doctor --fix: all checks pass; nothing to repair.")
		res.Messages = append(res.Messages, "nothing to fix")
		return res
	}
	path := targetConfigPath(baseDir)
	for _, c := range rep.Checks {
		if c.OK {
			continue
		}
		// Resolve the binary hint here; the engine maps the issue to guidance and a repair command.
		name, isAdapter := strings.CutPrefix(c.Name, "adapter: ")
		hint := ""
		if isAdapter {
			hint = m.detectHint(name)
		}
		g := m.engine().RepairGuidance(esetup.RepairIssue{Name: c.Name, Detail: c.Detail, BinHint: hint})
		m.log(g.Message)
		if g.Command != "" {
			m.log("  Fix: " + g.Command)
		}
		// For an adapter issue, offer to set the path now.
		if ask != nil && isAdapter && m.isConfigurable(name) {
			if ask.Confirm("Set a binary path for " + name + " now?") {
				p := ask.AskPath("Full path to the " + name + " binary:")
				if verr := validateBinaryPath(p); verr != nil {
					m.log("  Rejected: " + verr.Error() + " — not applied.")
				} else if patch, perr := m.engine().PatchFor(esetup.RepairAction{Kind: esetup.ActionSetBinaryPath, Target: name}, p); perr != nil {
					m.log("  Could not plan repair: " + perr.Error())
				} else if werr := config.ApplyPatchToFile(path, patch); werr != nil {
					m.log("  Write failed: " + werr.Error())
				} else {
					res.ConfigWritten = true
					res.Path = path
					res.Messages = append(res.Messages, "repaired adapters."+name+".path")
					m.log("  Applied: set adapters." + name + ".path in " + path)
					continue
				}
			}
		}
		res.Messages = append(res.Messages, c.Name)
	}
	if ask == nil {
		m.log("(non-interactive: printed guidance only; no changes made.)")
	}
	return res
}
