// Package setup is the SetupManager: the interactive setup/repair use case (CUC-3).
// It is the only config-store writer: write/update the selected-scope config, detect
// adapters, behave safely when non-interactive, and never write secrets. The wizard
// (`setup --interactive`) supports default-profile choice, validated adapter binary-path
// capture, and per-role lane **model selection from the catalog** (when a lane's adapter has
// >=2 catalog entries). The richer halt-class-keyed repairs (model choice / identity
// mismatch) and a pre-write live model-identity probe remain target/future.
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

// Manager owns setup/repair. It writes only the config store, never secrets. It also exposes
// pure read projections (config/profiles/adapters/privacy DTOs — see views.go) so a Client
// surface (CLI, web UI) can render config state without importing ResourceAccess directly.
type Manager struct {
	Adapters map[string]model.Adapter
	Out      io.Writer
	// Read state for the projection methods (views.go). Optional for the write-only paths.
	Cfg         config.Config
	Layers      config.Layers
	ArtifactDir string
	// WriteScope selects the config LAYER governed mutations write to: "" / "user" = user/global
	// (the default; every existing caller is unchanged), "project" = the project config for this
	// folder. Go owns scope resolution + layer semantics; a Client sets it per request. A project
	// scope with no project config is BLOCKED (never a silent user write).
	WriteScope string
}

// Result reports what setup/repair did.
type Result struct {
	ConfigWritten bool
	Path          string
	Backup        string // path of the pre-write backup, when one was taken (model-key cleanup)
	Messages      []string
	// Conflict is set (and ConfigWritten stays false) when EditProfileLane needs a governed
	// saved-model conflict resolution: the discovered/manual choice would overwrite an existing
	// generated catalog key with different content and the caller did not pass Replace. The web
	// UI surfaces it (Replace/Cancel + used-by) rather than failing with an opaque error.
	Conflict *SavedModelConflict
}

func (m *Manager) log(msg string) {
	if m.Out != nil {
		fmt.Fprintln(m.Out, msg)
	}
}

// engine returns the pure SetupEngine (stateless) the manager delegates planning to.
func (m *Manager) engine() *esetup.Engine { return esetup.New() }

// targetConfigPath resolves which config file under baseDir to write/patch: the existing
// discovered config (e.g. a legacy `config.json`) if present, else the default
// `config.yaml`. This avoids creating a shadowing `config.yaml` next to an existing
// `config.json` (which LoadLayered would then prefer, hiding the user's config).
func targetConfigPath(baseDir string) string {
	if p := config.DiscoverProjectConfig(baseDir); p != "" {
		return p
	}
	return config.ProjectConfigPath(baseDir)
}

// Setup initializes/updates the config under baseDir (the caller-chosen scope —
// user/global by default, project when requested). When ask is nil
// (non-interactive) it writes the default seed config idempotently and reports;
// when interactive it confirms first. It never writes secrets.
func (m *Manager) Setup(baseDir string, ask review.Prompter) (Result, error) {
	path := targetConfigPath(baseDir)

	m.log(fmt.Sprintf("Detected runnable adapters: %v", m.availableAdapters()))
	m.log("Real cloud adapters (devin-cli, claude-code, codex-cli, agy-cli, ollama, gemini-cli) need their own CLI installed and authenticated; they are not configured by this step.")
	m.log("Note: reviewmesh never stores secrets in config — authentication stays with each adapter's CLI.")

	return m.writeConfigIfAbsent(path, config.Default(), ask, "default profile: default (unconfigured — configure its lanes or copy a ready-made profile)", esetup.SetupKindDefault)
}

// availableAdapters returns the sorted names of registered adapters that report runnable
// (a safe presence check — no model call, no auth).
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

// SetupWizard runs the interactive setup flow on the shared interactive stack — the
// prompted counterpart of Setup. It inspects the existing config, detects adapters, lets
// the user pick the PROFILE TO CONFIGURE (`defaultProfile` is never repointed — the one
// default profile is the profile named `default`) and record adapter binary paths (each
// validated before it is recorded), shows a summary, and writes ONLY after explicit confirmation: a fresh
// config is written as the seed plus the chosen values; an existing config is surgically
// patched (unrelated fields preserved, a malformed file refused). It writes through
// ConfigAccess only and never writes secrets. With a nil Prompter it prints deterministic
// guidance and returns a usage error — a non-interactive surface gets guidance, not prompts
// (RB-17).
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

	// Working config: an existing one is strict-loaded so a malformed file is refused up
	// front (never silently overwritten); otherwise start from the shipped seed.
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

	// 1. Choose the profile to CONFIGURE (its lane models, step 3) — only among profiles
	// actually present in the config. This deliberately does NOT choose or write
	// `defaultProfile`: the one default profile is the profile named `default`, and changing
	// the effective default means editing `default`'s lanes or copying another profile onto
	// it — never repointing the `defaultProfile` key (the same fixed semantics as the web UI;
	// a stale non-`default` pointer from an older build is a legacy posture the web UI offers
	// a confirmed repair for).
	profiles := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		// The shipped-but-HIDDEN fake profile (config.FakeProfile) is test-only — never offer it as a
		// profile the user configures (it mirrors the `fake` ADAPTER being hidden from the UI).
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

	// 2. Record adapter binary paths (loop) — validate each before recording it.
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

	// 3. Per-role model selection from the catalog — configure a lane's model without
	// hand-editing config. Offered ONLY for the chosen profile's non-fake lanes that have
	// >=2 catalog entries for their adapter; choosing a catalog entry selects the model AND
	// its effort/thinking together (effort lives on the catalog entry — the schema-consistent
	// shape; no per-lane effort field is invented). With the shipped one-entry-per-adapter
	// catalog there is nothing to choose, so no prompt is shown — add catalog entries (or a
	// project config) to surface choices.
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

	// 4. Summary + 5. confirm before any write.
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

	// 6. Write through ConfigAccess only (atomic; no secrets). `defaultProfile` is never written here
	// (see step 1). This is a SPLIT write: lane models → config.yaml (profiles), adapter binary paths →
	// the scope's shared adapters.yaml. Resolve the shared target FIRST so a project-scope-with-no-root
	// case blocks before either file is touched (no partial write).
	var shared string
	if len(pathNames) > 0 {
		s, err := m.sharedWriteTarget()
		if err != nil {
			return res, err
		}
		shared = s
	}

	// config.yaml: lane models (+ create a base config on the absent path).
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

	// Shared adapters.yaml: each chosen adapter path. Per-file partial-failure honesty — the config.yaml
	// write above already landed, so a failure here is reported as such (not silently swallowed).
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

// SetupLocalOllama writes a selected-scope config for the opt-in fully-local-ollama
// profile (no secrets, no cloud). The model tag comes from modelTag (typically
// REVIEWMESH_OLLAMA_MODEL); an empty tag still writes a usable config and prints a
// recommendation to set the tag. The `default` profile (fake adapter) remains the default.
func (m *Manager) SetupLocalOllama(baseDir, modelTag string, ask review.Prompter) (Result, error) {
	path := targetConfigPath(baseDir)
	// fully-local-ollama is an EXAMPLE profile (not shipped in the seed); compose it here so
	// `setup --profile fully-local-ollama` still writes a usable config for it.
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

// SetAdapterPath records an adapter's binary path in the selected-scope SHARED adapters.yaml
// (`.aimesh/adapters.yaml`) — NOT in config.yaml — preserving every other adapter entry. It validates
// the adapter name and binary path BEFORE writing (so a bad input never touches disk); it writes no
// secrets, runs no authentication, and makes no model call. The write scope comes from the Manager's
// WriteScope (the CLI sets it from --scope); baseDir is retained for signature compatibility but the
// shared target is scope-anchored (user → AIMESH_HOME; project → repo root) rather than baseDir-derived.
func (m *Manager) SetAdapterPath(baseDir, name, binPath string) (Result, error) {
	_ = baseDir // shared target is scope-anchored, not baseDir-derived (see doc)
	if err := m.engine().ValidateAdapterForPathCapture(name, m.configurableNames()); err != nil {
		return Result{}, err
	}
	if err := validateBinaryPath(binPath); err != nil {
		return Result{}, err
	}
	return m.writeAdapterPathShared(name, binPath)
}

// PromoteUserToProject copies a **minimal, selected** set of project-relevant overrides from
// the user/global config into the project config under projectBaseDir. It is **explicit only**
// — never a side effect of plain `setup`, and never a bulk copy. By default it promotes only
// `defaultProfile`; machine-specific `adapters.<name>.path` values are promoted ONLY when
// includePaths is true. It writes through the single `ConfigPatch` path (`ConfigAccess`),
// preserving every unrelated project field and refusing a malformed project file; it never
// writes secrets. Gating: an interactive `ask` confirms after a summary; a non-interactive
// call must pass assumeYes, else it prints the plan and returns a usage error without writing.
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
	patch := m.engine().PlanPromotion(userRaw) // defaultProfile → project config.yaml
	// Adapter binary paths live in the shared adapters.yaml; promote them dual-target (user shared →
	// project shared) only when requested. Resolve names up front for the summary + no-op check.
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
	// Resolve the project shared target BEFORE any write so a no-repo case blocks without a partial write.
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

// ProjectPromotionPreview is the NON-writing preview of a user→project promotion, for the web UI's
// confirmation summary. It reports the target path, whether a project config already exists (creation
// is refused when it does), and exactly which keys PromoteUserToProject would write.
type ProjectPromotionPreview struct {
	TargetPath     string   `json:"targetPath"`
	Exists         bool     `json:"exists"`
	DefaultProfile string   `json:"defaultProfile,omitempty"`
	AdapterPaths   []string `json:"adapterPaths,omitempty"`
	Ops            int      `json:"ops"`
}

// PlanProjectPromotion computes what PromoteUserToProject WOULD write — WITHOUT writing — so the web UI
// can show a confirmation summary before the user commits. It reuses the SAME planner (PlanPromotion)
// and target-path resolution as the write, and only reads the user config + probes whether a project
// config already exists. No config write path is duplicated: the actual write stays PromoteUserToProject.
func (m *Manager) PlanProjectPromotion(projectBaseDir string, includePaths bool) (ProjectPromotionPreview, error) {
	var pv ProjectPromotionPreview
	userPath := config.DiscoverUserConfig()
	if userPath == "" {
		return pv, fault.New(fault.Config, "no user/global config found to promote from (run `reviewmesh setup` first)")
	}
	userRaw, err := config.LoadRawMap(userPath)
	if err != nil {
		return pv, err
	}
	pv.TargetPath = targetConfigPath(projectBaseDir)
	pv.Exists = config.DiscoverProjectConfig(projectBaseDir) != "" // covers .yaml/.yml/.json (legacy paths)
	patch := m.engine().PlanPromotion(userRaw)
	pv.Ops = len(patch.Ops)
	for _, op := range patch.Ops {
		if len(op.Path) == 1 && op.Path[0] == "defaultProfile" {
			pv.DefaultProfile, _ = op.Value.(string)
		}
	}
	// Adapter binary paths would be copied from the user shared adapters.yaml → project shared file.
	if includePaths {
		if userShared, err := config.SharedUserLocationsPath(); err == nil {
			pv.AdapterPaths = config.SharedAdapterNames(userShared)
			pv.Ops += len(pv.AdapterPaths)
		}
	}
	return pv, nil
}

// CreateProjectConfig writes a NEW, valid, minimal project config (an override-only layer) under base,
// through the single ConfigAccess write path — the governed primitive behind the web UI's "Create
// project config" action. It is create-only (refuses when a project config already exists) and ALWAYS
// writes a valid, non-empty file: `schemaVersion: 1` plus whatever PlanPromotion selects from the user
// config (defaultProfile always; machine-specific adapter paths only when includePaths). So the user
// never has to hand-create a blank file, and an empty/invalid file is never produced. No second write
// path: it reuses the same PlanPromotion planner + config.ApplyPatchToFile as PromoteUserToProject.
func (m *Manager) CreateProjectConfig(base string, includePaths bool) (Result, error) {
	var res Result
	if config.DiscoverProjectConfig(base) != "" {
		return res, fault.New(fault.Usage, "a project config already exists under this folder — this action only creates one when none exists")
	}
	var patch config.ConfigPatch
	if userPath := config.DiscoverUserConfig(); userPath != "" {
		if userRaw, err := config.LoadRawMap(userPath); err == nil {
			patch = m.engine().PlanPromotion(userRaw)
		}
	}
	// Guarantee a valid minimal override even when there is nothing to promote (schemaVersion first).
	patch.Ops = append([]config.SetOp{{Path: []string{"schemaVersion"}, Value: 1}}, patch.Ops...)
	projPath := targetConfigPath(base)
	if err := config.ApplyPatchToFile(projPath, patch); err != nil {
		return res, err
	}
	res.ConfigWritten, res.Path = true, projPath
	res.Messages = append(res.Messages, "created a project config at "+projPath+" (override-only; no secrets)")
	// Copy the user's saved adapter paths into the project shared adapters.yaml when requested.
	if includePaths {
		if projShared, ok := config.SharedProjectLocationsPath(base); ok {
			if userShared, err := config.SharedUserLocationsPath(); err == nil {
				if copied, cerr := config.CopySharedAdapterPaths(userShared, projShared); cerr != nil {
					return res, fmt.Errorf("created %s, but copying adapter paths into %s failed: %w", projPath, projShared, cerr)
				} else if len(copied) > 0 {
					res.Messages = append(res.Messages, fmt.Sprintf("copied %d adapter path(s) into %s", len(copied), projShared))
				}
			}
		}
	}
	m.log(res.Messages[0])
	return res, nil
}

// detectHint returns the binary name to suggest in a repair command (e.g. "claude"
// for "claude-code"), falling back to the adapter name.
func (m *Manager) detectHint(name string) string {
	if a, ok := m.Adapters[name]; ok {
		if d, ok := a.(interface{ DetectName() string }); ok {
			return d.DetectName()
		}
	}
	return name
}

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

// Repair surfaces doctor issues and, on an interactive surface, offers a guided repair.
// For every failed check it prints the plain-English issue + (for an adapter binary issue)
// the exact `setup` command. When `ask` is non-nil (interactive) it additionally offers to
// fix an adapter **binary path** in place — prompt → validate the path → write via the
// shared `ConfigPatch` path (`SetupEngine.PatchFor` → `ConfigAccess`), the same single
// config writer setup uses; it never writes secrets and never automates native CLI
// login/trust (auth stays with the adapter's CLI). Non-adapter issues (and any
// re-authentication need) stay **guidance-only**. With a nil `Prompter` it prints guidance
// and writes nothing (RB-17). Model-choice / identity-mismatch repairs are target/future.
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
		// The manager resolves the binary hint (live adapter) for an adapter issue; the
		// SetupEngine maps the issue → plain-English guidance + (for adapter issues) the
		// exact repair command.
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
		// Interactive guided repair: for an adapter binary issue, offer to set the path now.
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
