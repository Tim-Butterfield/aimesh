package setup

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// This file adds the discrete, non-linear config mutations the web-UI surface composes
// (copy a profile, configure/remove an adapter) — each a thin Manager method that validates,
// delegates planning to the pure SetupEngine, and writes through the single config-write path
// (config.ApplyPatchToFile). The Client (web UI) calls these; it never touches ResourceAccess.
// They write to the user/global scope. There is deliberately NO "set default profile" method:
// the initial UI keeps defaultProfile = `default` and customization happens by editing/copying.

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
	UsedBy  []AdapterUse // the blocking uses (present when Blocked) so the UI lists them first
	Message string
	Name    string
	Path    string
}

// ReplaceProfileConfirm is the exact confirmation a caller must echo (via confirmReplace) to
// overwrite an existing profile. Copying onto ANY existing target requires it.
func ReplaceProfileConfirm(target, source string) string {
	return fmt.Sprintf("This will replace the existing profile %q with a copy of %q. Do you want to continue?", ProfileDisplayName(target), ProfileDisplayName(source))
}

// lowerTargetLanesMissing is the SCOPE-AWARE analogue used by CopyProfile: the lane roles that a layer
// LOWER than the write scope defines for `target` but `src` does not — those lanes survive a copy of src
// over target (so it is not a faithful replace). Lower = shipped, plus (for a project write) the user
// layer. Nearest-lower vs farther-lower does not matter here — any lower contribution that the source
// lacks survives the merge.
func (m *Manager) lowerTargetLanesMissing(target string, src config.Profile) []string {
	roles := map[string]bool{}
	if p, ok := config.Default().Profiles[target]; ok {
		for r := range p.Lanes {
			roles[r] = true
		}
	}
	for _, l := range m.lowerWriteFileLayers() {
		raw, err := config.LoadRawMap(l.path)
		if err != nil {
			continue
		}
		profs, _ := raw["profiles"].(map[string]any)
		pm, _ := profs[target].(map[string]any)
		lanes, _ := pm["lanes"].(map[string]any)
		for r := range lanes {
			roles[r] = true
		}
	}
	var missing []string
	for r := range roles {
		if _, ok := src.Lanes[r]; !ok {
			missing = append(missing, r)
		}
	}
	sort.Strings(missing)
	return missing
}

// SnapshotConfig writes this manager's CURRENT resolved config as a self-contained config file
// under homeDir (homeDir/.reviewmesh/config.yaml). The web-UI ACP validator points the child
// `reviewmesh acp` at this isolated snapshot so the child reads a consistent, immutable copy of the
// config as-of validation start — a later config write to the live home cannot change what the
// child validates (closing the preflight/launch race). The manager's Cfg is an immutable snapshot
// (loadConfig swaps the whole config, never mutating in place), so this and IsFakeOnlyProfile see
// the same state.
//
// A non-empty defaultProfile is written as the snapshot's `defaultProfile`, which is how a
// user-selected (preflighted, fake-only) profile becomes the one the child validates. Only the
// throwaway snapshot is repointed — the live config's `defaultProfile` is never touched (there
// is still no "set as default" operation).
func (m *Manager) SnapshotConfig(homeDir, defaultProfile string) error {
	cfg := m.Cfg // struct copy; maps are shared but never mutated (loadConfig swaps wholesale)
	if defaultProfile != "" {
		if _, ok := cfg.Profiles[defaultProfile]; !ok {
			return fault.New(fault.Usage, fmt.Sprintf("profile %q does not exist", defaultProfile))
		}
		cfg.DefaultProfile = defaultProfile
	}
	if err := cfg.WriteYAMLFile(config.ProjectConfigPath(homeDir)); err != nil {
		return err
	}
	// Adapter binary PATHS are sourced only from the shared adapters.yaml, so bake the RESOLVED paths
	// into the snapshot's shared file (under homeDir, which the child reads as AIMESH_HOME). This keeps
	// the child's adapter resolution IMMUTABLE + consistent with preflight — it never re-resolves the
	// live user/project shared files. config.yaml carries no paths anymore.
	sharedPath := config.SharedLocationsPath(homeDir)
	for name, ad := range cfg.Adapters {
		if ad.Path == "" {
			continue
		}
		if err := config.SetSharedAdapterPath(sharedPath, name, ad.Path); err != nil {
			return err
		}
	}
	return nil
}

// userConfigTarget is the user/global config file the web-UI mutations write to: the existing
// discovered user config if present, else the canonical ~/.aimesh/review/config.yaml.
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

// writeTarget resolves the config FILE for the selected write scope. For the project scope it
// revalidates the target at write time — `Manager.Layers` is a load-time snapshot and
// `config.ApplyPatchToFile` CREATES an absent target, so a project config deleted/invalidated since the
// snapshot must BLOCK (never be recreated, never fall back to user). Every governed write resolves this
// EARLY and bubbles the error, so a project-scope existence probe never masks the missing-config error.
func (m *Manager) writeTarget() (string, error) {
	// Go validates the scope (the SPA only submits a string): reject anything other than the two known
	// scopes rather than silently treating an unknown value as a user write.
	if m.WriteScope != "" && m.WriteScope != writeScopeUser && m.WriteScope != writeScopeProject {
		return "", fault.New(fault.Usage, fmt.Sprintf("unknown config write scope %q (expected %q or %q)", m.WriteScope, writeScopeUser, writeScopeProject))
	}
	if m.scope() == writeScopeProject {
		p := m.Layers.ProjectPath
		if p == "" {
			return "", fault.New(fault.Usage, "no project config for this folder to edit — create a project config first")
		}
		if _, err := config.LoadRawMap(p); err != nil {
			return "", fault.New(fault.Usage, "the project config is missing or unreadable now — reload the workbench (it may have been deleted or edited outside the app)")
		}
		return p, nil
	}
	return m.userConfigTarget()
}

// sharedWriteTarget resolves the SHARED adapters.yaml FILE (.aimesh/adapters.yaml) for the selected
// write scope — the destination for adapter binary PATHS, which no longer go into config.yaml. Project
// scope is ROOT-anchored: it BLOCKS with guidance when the folder is not inside a repo, never silently
// falling back to a cwd-relative path (paths are a repo-wide fact). User scope is AIMESH_HOME-anchored.
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

// scopeConfigPath resolves the write-scope config FILE for READ-ONLY layer probes (no write-time
// revalidation, no error): project → ProjectPath; user → the discovered/canonical user config. Used by
// the layer-analysis helpers so "is this defined in the layer I'm editing?" is scope-correct.
func (m *Manager) scopeConfigPath() string {
	if m.scope() == writeScopeProject {
		return m.Layers.ProjectPath
	}
	if p := config.DiscoverUserConfig(); p != "" {
		return p
	}
	if p, err := config.UserConfigPath(); err == nil {
		return p
	}
	return ""
}

// layerRef names a loaded config layer + its file path.
type layerRef struct{ layer, path string }

// higherWriteLayers returns the loaded layers strictly HIGHER in precedence than the write scope — they
// would SHADOW a write to the scope (precedence: shipped < user < project < explicit). user scope →
// {project, explicit}; project scope → {explicit} only (the user layer is LOWER, never shadows a project
// write). Returned highest-first.
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

// lowerWriteFileLayers returns the loaded FILE layers strictly LOWER in precedence than the write scope
// — a delete/clear could REVEAL them (field-by-field merge). shipped (config.Default) is always lowest
// and is handled by callers separately; for project scope the USER layer is also lower. Returned
// NEAREST-first (the highest lower layer wins the reveal).
func (m *Manager) lowerWriteFileLayers() []layerRef {
	var out []layerRef
	if m.scope() == writeScopeProject && m.Layers.UserLoaded && m.Layers.UserPath != "" {
		out = append(out, layerRef{"user/global", m.Layers.UserPath})
	}
	return out
}

// rawLayerHasProfile / rawLayerHasLane test whether a raw config file defines a profile / a specific lane.
func rawLayerHasProfile(path, name string) bool {
	raw, err := config.LoadRawMap(path)
	if err != nil {
		return false
	}
	profs, ok := raw["profiles"].(map[string]any)
	if !ok {
		return false
	}
	_, present := profs[name]
	return present
}

func rawLayerHasLane(path, profile, role string) bool {
	raw, err := config.LoadRawMap(path)
	if err != nil {
		return false
	}
	profs, _ := raw["profiles"].(map[string]any)
	pm, _ := profs[profile].(map[string]any)
	lanes, _ := pm["lanes"].(map[string]any)
	_, present := lanes[role]
	return present
}

// lowerLaneReveal reports whether a layer LOWER than the write scope defines
// `profiles.<profile>.lanes.<role>` — so after clearing it in the write scope, that layer's lane is
// REVEALED (field-by-field merge). Checks the lower file layers nearest-first (user before shipped for
// a project write), then the shipped built-in default.
func (m *Manager) lowerLaneReveal(profile, role string) (layer string, revealed bool) {
	for _, l := range m.lowerWriteFileLayers() {
		if rawLayerHasLane(l.path, profile, role) {
			return l.layer, true
		}
	}
	if bi, ok := config.Default().Profiles[profile]; ok {
		if _, ok := bi.Lanes[role]; ok {
			return "built-in default", true
		}
	}
	return "", false
}

// lowerProfileReveal is the profile-level analogue (for DeleteProfile): a lower layer that would reveal
// `profiles.<name>` after deleting it from the write scope.
func (m *Manager) lowerProfileReveal(name string) (layer string, revealed bool) {
	for _, l := range m.lowerWriteFileLayers() {
		if rawLayerHasProfile(l.path, name) {
			return l.layer, true
		}
	}
	if _, ok := config.Default().Profiles[name]; ok {
		return "built-in default", true
	}
	return "", false
}

// profileContent projects a resolved profile to a plain map (json-tag keys) so it can be the
// value of a full-subtree copy SetOp — serialization-library-independent and re-validated by
// the write path.
func profileContent(p config.Profile) (map[string]any, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, "marshal profile", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fault.Wrap(fault.Internal, "decode profile", err)
	}
	return out, nil
}

// CopyProfile copies the resolved source profile to target (a NEW name or an EXISTING one),
// writing to the user config. Copying onto an existing profile ALWAYS requires that the caller
// echo the EXACT confirmation string (ReplaceProfileConfirm(target, source)) as confirm — a bare
// "yes" flag is intentionally insufficient, so a caller can only confirm a prompt it actually
// received. Without the exact echo the method makes NO change and returns ReplaceRequired with
// the confirmation prompt. defaultProfile is never touched — "copy to default" is just this with
// target `default` (the profile name is not conflated with the default role).
func (m *Manager) CopyProfile(source, target, confirm string) (CopyResult, error) {
	res := CopyResult{Source: source, Target: target}
	if source == "" || target == "" {
		return res, fault.New(fault.Usage, "both a source and a target profile name are required")
	}
	if source == target {
		return res, fault.New(fault.Usage, fmt.Sprintf("source and destination are the same profile (%q) — copying a profile onto itself does nothing", source))
	}
	src, ok := m.Cfg.Profiles[source]
	if !ok {
		return res, fault.New(fault.Usage, fmt.Sprintf("source profile %q does not exist", source))
	}
	_, exists := m.Cfg.Profiles[target]
	if exists {
		// A copy writes profiles.<target> to the USER layer only, but profile merge is
		// field-granular, so a "replace" is faithful only when the user-layer write actually
		// determines the whole merged target. Two cases where it would NOT:
		//   (a) the target is also defined in a HIGHER layer (project/explicit) — that layer
		//       overrides the user write, so the copy would be shadowed and not take effect;
		//   (b) the merged target has lane roles the SOURCE lacks — those lanes come from a
		//       lower layer (shipped) and survive the merge, so the copy would not be a true
		//       replace (it would silently keep the target's extra lanes).
		// Block both with a clear reason instead of writing a misleading partial copy.
		if layer, p := m.higherLayerDefining(target); layer != "" {
			res.Blocked = true
			res.Message = fmt.Sprintf("profile %q is also defined in the %s config layer (%s), which this workbench does not edit — a %s copy would be shadowed by it; edit that file directly", ProfileDisplayName(target), layer, p, m.scopeLabel())
			return res, nil
		}
		// Compare against the LOWER layers only (below the write scope), NOT the merged view: a
		// full-subtree write cleanly replaces the write-layer target (and higher layers are already
		// blocked above), so the only lanes that survive a copy are those a LOWER layer contributes for
		// roles the source lacks. For a user write that is the shipped seed; for a PROJECT write it is the
		// user layer AND shipped (a user-layer lane survives a project copy exactly like a shipped one).
		if extra := m.lowerTargetLanesMissing(target, src); len(extra) > 0 {
			res.Blocked = true
			res.Message = fmt.Sprintf("profile %q has lower-layer lane(s) %s that %q does not define — copying would leave those inherited lanes in place, so it would not be a faithful replace; choose a source that covers all its roles", ProfileDisplayName(target), strings.Join(extra, ", "), ProfileDisplayName(source))
			return res, nil
		}
	}
	if exists && confirm != ReplaceProfileConfirm(target, source) {
		res.ReplaceRequired = true
		res.Message = ReplaceProfileConfirm(target, source)
		return res, nil
	}
	content, err := profileContent(src)
	if err != nil {
		return res, err
	}
	patch, err := m.engine().PlanProfileCopy(target, content)
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
	res.Written, res.Path = true, path
	if exists {
		// Honest about the layered model: we write the copy to the user layer. For a user-defined
		// target this is a clean replace; for a shipped-seed target the seed still contributes any
		// keys the source lacks (documented limitation), so we don't claim a global wipe-and-replace.
		res.Message = fmt.Sprintf("Wrote a copy of %q over profile %q in the %s.", ProfileDisplayName(source), ProfileDisplayName(target), m.scopeLabel())
	} else {
		res.Message = fmt.Sprintf("Created profile %q as a copy of %q in the %s.", ProfileDisplayName(target), ProfileDisplayName(source), m.scopeLabel())
	}
	m.log(res.Message + " (" + path + ")")
	return res, nil
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

// DeleteProfileConfirm is the exact confirmation a caller must echo to delete a profile.
func DeleteProfileConfirm(name string) string {
	return fmt.Sprintf("This will delete the profile %q from your user config. Do you want to continue?", ProfileDisplayName(name))
}

// DeleteProfile deletes profiles.<name> from the USER config layer — the web-UI governed
// deletion. Guards, in order: the fixed `default` profile can never be deleted; the profile
// must exist in the merged config; deleting the profile the saved defaultProfile points at is
// blocked (it would leave a dangling pointer — repair the pointer first); a profile that is
// not present in the user layer is blocked (it comes from the delivered defaults or another
// config layer, so deleting the user-layer key would not remove it). The caller must echo the
// EXACT confirmation string (DeleteProfileConfirm(name)) — a bare flag is insufficient. Layered
// honesty: deleting a user-layer key that shadows a lower-layer (delivered `default`) profile
// restores that lower-layer profile after merge, and the message says so.
func (m *Manager) DeleteProfile(name, confirm string) (DeleteResult, error) {
	res := DeleteResult{Name: name}
	if name == "" {
		return res, fault.New(fault.Usage, "a profile name is required")
	}
	if name == "default" {
		return res, fault.New(fault.Usage, "the `default` profile cannot be deleted")
	}
	if _, ok := m.Cfg.Profiles[name]; !ok {
		return res, fault.New(fault.Usage, fmt.Sprintf("profile %q does not exist", name))
	}
	if m.Cfg.DefaultProfile == name {
		res.Blocked = true
		res.Message = fmt.Sprintf("the saved defaultProfile currently points at %q — deleting it would leave a dangling default pointer; repair the default pointer (back to `default`) first", ProfileDisplayName(name))
		return res, nil
	}
	path, err := m.writeTarget()
	if err != nil {
		return res, err
	}
	inUserLayer := false
	if raw, rerr := config.LoadRawMap(path); rerr == nil {
		if profs, ok := raw["profiles"].(map[string]any); ok {
			_, inUserLayer = profs[name]
		}
	}
	if !inUserLayer {
		res.Blocked = true
		res.Message = fmt.Sprintf("profile %q is not in the %s — it comes from another config layer (the delivered defaults or a different layer), so it cannot be deleted here; you can override it by copying another profile onto it", ProfileDisplayName(name), m.scopeLabel())
		return res, nil
	}
	// Layered honesty: this surface writes ONLY the user layer. If a higher-precedence layer
	// (project / explicit --config) also defines the profile, deleting the user-layer key would
	// not remove it from the merged view — block with the explanation instead of a false
	// "deleted" that the UI would render as the profile disappearing.
	if layer, p := m.higherLayerDefining(name); layer != "" {
		res.Blocked = true
		res.Message = fmt.Sprintf("profile %q is also defined in the %s config layer (%s), which this workbench does not edit — deleting the %s key would not remove it; edit that file directly", ProfileDisplayName(name), layer, p, m.scopeLabel())
		return res, nil
	}
	if confirm != DeleteProfileConfirm(name) {
		res.ConfirmRequired = true
		res.Message = DeleteProfileConfirm(name)
		return res, nil
	}
	patch, err := m.engine().PlanProfileDeletion(name)
	if err != nil {
		return res, err
	}
	if err := config.ApplyPatchToFile(path, patch); err != nil {
		return res, err
	}
	res.Deleted, res.Path = true, path
	if revealed, ok := m.lowerProfileReveal(name); ok {
		res.Message = fmt.Sprintf("Deleted the %s override of %q; the %s profile of the same name is now in effect after merge.", m.scopeLabel(), ProfileDisplayName(name), revealed)
	} else {
		res.Message = fmt.Sprintf("Deleted profile %q from the %s.", ProfileDisplayName(name), m.scopeLabel())
	}
	m.log(res.Message + " (" + path + ")")
	return res, nil
}

// higherLayerDefining reports which loaded layer of STRICTLY HIGHER precedence than the write scope, if
// any, defines profiles.<name> — a write to the scope would be shadowed by it (so the mutation blocks).
// Scope-aware: for user scope the higher layers are {project, explicit}; for project scope only
// {explicit} (the user layer is lower and never shadows a project write).
func (m *Manager) higherLayerDefining(name string) (layer, path string) {
	for _, l := range m.higherWriteLayers() {
		if rawLayerHasProfile(l.path, name) {
			return l.layer, l.path
		}
	}
	return "", ""
}

// higherLayerDefiningLane is the LANE-level analogue: which STRICTLY-HIGHER layer defines
// `profiles.<profile>.lanes.<role>` specifically (a higher layer defining the PROFILE but not this ROLE
// does not shadow the clear). Scope-aware higher set (see higherLayerDefining).
func (m *Manager) higherLayerDefiningLane(profile, role string) (layer, path string) {
	for _, l := range m.higherWriteLayers() {
		if rawLayerHasLane(l.path, profile, role) {
			return l.layer, l.path
		}
	}
	return "", ""
}

// RemoveAdapter deletes adapters.<name> from the user config. It is BLOCKED while any
// profile/lane references the adapter (the blocking uses are returned so the UI can list them
// first), and the built-in `fake` adapter can never be removed. It never edits a profile. For a
// seed-registered adapter this clears the user's configuration (e.g. a recorded path); a
// purely user-added adapter is removed outright.
func (m *Manager) RemoveAdapter(name string) (RemoveResult, error) {
	res := RemoveResult{Name: name}
	if name == "" {
		return res, fault.New(fault.Usage, "an adapter name is required")
	}
	if name == "fake" {
		return res, fault.New(fault.Usage, "the built-in `fake` adapter cannot be removed")
	}
	if uses := m.adapterUsage()[name]; len(uses) > 0 {
		res.Blocked, res.UsedBy = true, uses
		res.Message = fmt.Sprintf("%s is in use by %d profile lane(s); reconfigure those profiles first", AdapterDisplayName(name), len(uses))
		return res, nil
	}

	// The saved binary PATH lives ONLY in the shared adapters.yaml. Resolve the scope target first so a
	// project-scope-with-no-root case blocks with guidance before any check/write.
	shared, err := m.sharedWriteTarget()
	if err != nil {
		return res, err
	}
	if !config.SharedHasAdapterPath(shared, name) {
		// Nothing to clear at this scope: no saved path. Visibly a no-op — block with the explanation
		// (the UI disables the button for this case).
		res.Blocked = true
		res.Message = fmt.Sprintf("%s has no saved path in the %s to clear; it still ships with reviewmesh and remains available to configure.", AdapterDisplayName(name), m.scopeLabel())
		return res, nil
	}
	if _, serr := config.ClearSharedAdapterPath(shared, name); serr != nil {
		return res, serr
	}
	res.Removed, res.Path = true, shared
	res.Message = fmt.Sprintf("Cleared the saved path for %s from the %s; the adapter is still available to configure again.", AdapterDisplayName(name), m.scopeLabel())
	m.log(res.Message)
	return res, nil
}

// laneExecutionFor is the schema's execution value for a role when the lane editor CREATES a
// lane that did not exist yet (an existing lane's execution is never changed by an edit):
// the author lane is the host's own adjudication call; every other role is a spawned adapter.
func laneExecutionFor(role string) string {
	if role == "author_remediator" {
		return "host"
	}
	return "adapter"
}

// EditProfileLane sets one lane's adapter + model (+ effort where the adapter supports it)
// on a saved profile — the web-UI lane editor's governed write path. The choice is EITHER an
// existing catalog entry OR a discovered/manual model argument; either way it is re-validated
// server-side against the adapter-owned rules (validateLaneChoice) before any write. A
// discovered/manual choice is materialized as a first-class modelCatalog entry (effort stays
// a CATALOG field — no per-lane effort field is invented) and the lane points at it. Patches
// are planned by the pure SetupEngine and written through the single config-write path (user
// scope). defaultProfile is never touched — editing the profile named `default` is ordinary
// profile editing, not a default-pointer operation.
func (m *Manager) EditProfileLane(profile, role, adapter string, in LaneChoiceInput, disc *DiscoveryResult) (Result, error) {
	var res Result
	if err := m.laneTargetGuards(profile, role, adapter); err != nil {
		return res, err
	}
	// Layer guard (like Copy/Delete): if a higher-precedence layer (project/explicit) defines this
	// profile, a user-layer lane write would be SHADOWED by it — the edit would silently not take
	// effect. Block it with the honest reason instead of writing a phantom override.
	if layer, p := m.higherLayerDefining(profile); layer != "" {
		return res, fault.New(fault.Usage, fmt.Sprintf("profile %q is also defined in the %s config layer (%s), which this workbench does not edit — a %s lane change would be shadowed by it; edit that file directly", ProfileDisplayName(profile), layer, p, m.scopeLabel()))
	}
	choice, errs, _ := m.validateLaneChoice(adapter, in, disc)
	if len(errs) > 0 {
		return res, fault.New(fault.Usage, strings.Join(errs, "; "))
	}

	eng := m.engine()
	var ops []config.SetOp
	// Resolve the materialization via the SHARED planner (identical to CheckLane, so Check
	// predicts this exactly). create → write a new catalog entry; reuse → no catalog op;
	// conflict → a governed Replace (never a silent overwrite).
	plan := m.planMaterialization(adapter, choice)
	catalogKey := plan.CatalogKey
	switch plan.Action {
	case "create":
		entryPatch, err := eng.PlanCatalogEntry(catalogKey, catalogEntryFor(adapter, choice.ModelArg, choice.Effort))
		if err != nil {
			return res, err
		}
		ops = append(ops, entryPatch.Ops...)
	case "conflict":
		if !in.Replace {
			// Return the governed conflict for the UI to resolve (Replace/Cancel + used-by) —
			// not an opaque fault. ConfigWritten stays false; nothing is written.
			res.Conflict = plan.Conflict
			return res, nil
		}
		if !plan.Conflict.Replaceable {
			return res, fault.New(fault.Usage, plan.Conflict.Reason)
		}
		// Explicit Replace of a user-owned, unshadowed generated key: overwrite the full subtree.
		entryPatch, err := eng.PlanCatalogEntry(catalogKey, catalogEntryFor(adapter, choice.ModelArg, choice.Effort))
		if err != nil {
			return res, err
		}
		ops = append(ops, entryPatch.Ops...)
	}

	execution := ""
	if _, exists := m.Cfg.Profiles[profile].Lanes[role]; !exists {
		execution = laneExecutionFor(role) // new lane: set execution; existing lanes keep theirs
	}
	lanePatch, err := eng.PlanLaneEdit(profile, role, adapter, catalogKey, execution)
	if err != nil {
		return res, err
	}
	ops = append(ops, lanePatch.Ops...)

	path, err := m.writeTarget()
	if err != nil {
		return res, err
	}
	if err := config.ApplyPatchToFile(path, config.ConfigPatch{Ops: ops}); err != nil {
		return res, err
	}
	res.ConfigWritten, res.Path = true, path
	detail := m.ModelDisplayLabel(catalogKey)
	if choice.EffectiveModelArg != "" && choice.EffectiveModelArg != catalogKey {
		detail += " (effective: " + choice.EffectiveModelArg + ")"
	}
	res.Messages = append(res.Messages,
		fmt.Sprintf("Set the %s lane of %s to %s / %s in the %s.", role, ProfileDisplayName(profile), AdapterDisplayName(adapter), detail, m.scopeLabel()))
	m.log(res.Messages[len(res.Messages)-1] + " (" + path + ")")
	return res, nil
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

// ClearProfileLane removes a profile lane's EXPLICIT user-layer configuration — the governed "Clear
// configuration" action. It deletes `profiles.<profile>.lanes.<role>` from the user layer only (never a
// saved `modelCatalog` entry), so a lower layer's lane is revealed or the lane becomes unset/skipped.
// Layer- and no-op-aware: a lane with no user-layer override is a no-op (nothing to clear); a lane a
// higher (project/explicit) layer defines is blocked (the user-layer clear would be shadowed). Requires
// an echoed confirm token (the role), since clearing removes config and can expose inherited behavior
// or make a required lane not-ready. Writes through the one ConfigAccess path (user scope).
func (m *Manager) ClearProfileLane(profile, role, confirm string) (ClearLaneResult, error) {
	res := ClearLaneResult{Profile: profile, Role: role}
	if profile == "" || role == "" {
		return res, fault.New(fault.Usage, "a profile name and role are required")
	}
	if _, ok := m.Cfg.Profiles[profile]; !ok {
		return res, fault.New(fault.Usage, fmt.Sprintf("profile %q does not exist", profile))
	}
	validRole := false
	for _, r := range laneRolePurposes {
		if r.Role == role {
			validRole = true
			break
		}
	}
	if !validRole {
		return res, fault.New(fault.Usage, fmt.Sprintf("unknown lane role %q", role))
	}
	path, err := m.writeTarget()
	if err != nil {
		return res, err
	}
	// No-op honesty: only a lane EXPLICITLY present in the user layer can be cleared here. If the
	// effective lane comes only from a lower/shipped layer (or is already absent), a user-layer delete
	// changes nothing — report that rather than a phantom "cleared".
	userHasLane := false
	if raw, rerr := config.LoadRawMap(path); rerr == nil {
		if profs, ok := raw["profiles"].(map[string]any); ok {
			if pm, ok := profs[profile].(map[string]any); ok {
				if lanes, ok := pm["lanes"].(map[string]any); ok {
					_, userHasLane = lanes[role]
				}
			}
		}
	}
	if !userHasLane {
		res.NoOp = true
		res.Message = fmt.Sprintf("The %s lane of %s has no %s configuration to clear (it is inherited from a lower config layer or already unset).", role, ProfileDisplayName(profile), m.scopeLabel())
		return res, nil
	}
	// Lane-level layer guard: if a higher (project/explicit) layer defines THIS lane, the user-layer
	// lane is already shadowed — clearing it would not change the effective lane. Block honestly.
	if layer, p := m.higherLayerDefiningLane(profile, role); layer != "" {
		res.Blocked = true
		res.Message = fmt.Sprintf("the %s lane of %q is also defined in the %s config layer (%s), which this workbench does not edit — clearing the %s lane would be shadowed by it; edit that file directly", role, ProfileDisplayName(profile), layer, p, m.scopeLabel())
		return res, nil
	}
	if confirm != role {
		res.ConfirmRequired = true
		res.Message = role
		return res, nil
	}
	patch, err := m.engine().PlanLaneClear(profile, role)
	if err != nil {
		return res, err
	}
	if err := config.ApplyPatchToFile(path, patch); err != nil {
		return res, err
	}
	res.Cleared, res.Path = true, path
	// Post-clear state: does a LOWER layer (user before shipped for a project write, else shipped) define
	// this lane? If so it is revealed; else the lane is now unset (a required lane may become not-ready).
	if revealed, ok := m.lowerLaneReveal(profile, role); ok {
		res.RevealedLower = true
		res.Message = fmt.Sprintf("Cleared the %s lane override from %s (%s); the %s lane is now in effect.", role, ProfileDisplayName(profile), m.scopeLabel(), revealed)
	} else {
		res.Message = fmt.Sprintf("Cleared the %s lane from %s (%s); the lane is now unset — re-check readiness for the resulting state.", role, ProfileDisplayName(profile), m.scopeLabel())
	}
	m.log(res.Message + " (" + path + ")")
	return res, nil
}

// DeleteSavedModelResult is the outcome of a saved-model delete (governed, layer/used-by aware).
type DeleteSavedModelResult struct {
	Key           string
	ConfigWritten bool
	Path          string
	Blocked       bool
	UsedBy        []string
	Message       string
}

// DeleteSavedModel deletes a user-layer, non-default modelCatalog entry (a "saved model") the
// lane editor materialized. It is layer- and usage-aware: it refuses adapter-default/authoritative
// entries (those are not saved models), an entry defined only in a higher layer (project/explicit)
// or not in the user layer at all (the UI can't effectively remove it), an entry a lower (shipped)
// layer would REVEAL, and any entry currently referenced by a profile lane (the lanes are named).
// The write goes through the one config path (user scope).
func (m *Manager) DeleteSavedModel(key string) (DeleteSavedModelResult, error) {
	res := DeleteSavedModelResult{Key: key}
	if key == "" {
		return res, fault.New(fault.Usage, "a saved-model key is required")
	}
	entry, ok := m.Cfg.ModelCatalog[key]
	if !ok {
		return res, fault.New(fault.Usage, fmt.Sprintf("saved model %q does not exist", m.ModelDisplayLabel(key)))
	}
	if entry.IsAdapterDefault() || (entry.Authoritative != nil && *entry.Authoritative) {
		return res, fault.New(fault.Usage, fmt.Sprintf("%q is an adapter default / authoritative catalog entry, not a deletable saved model", m.ModelDisplayLabel(key)))
	}
	if uses := m.catalogKeyUsedBy(key); len(uses) > 0 {
		res.Blocked, res.UsedBy = true, uses
		res.Message = fmt.Sprintf("saved model %q is in use by %d lane(s); re-point those lanes first", m.ModelDisplayLabel(key), len(uses))
		return res, nil
	}
	userDefines, higher, lower := m.catalogKeyLayers(key)
	if higher != "" {
		res.Blocked = true
		res.Message = fmt.Sprintf("saved model %q is defined in the %s config layer — the web UI only edits the user layer; edit it in that config file", m.ModelDisplayLabel(key), higher)
		return res, nil
	}
	if !userDefines {
		res.Blocked = true
		res.Message = fmt.Sprintf("saved model %q is not defined in the %s, so there is nothing to delete here", m.ModelDisplayLabel(key), m.scopeLabel())
		return res, nil
	}
	if lower {
		res.Blocked = true
		res.Message = fmt.Sprintf("deleting the user override for %q would reveal the shipped entry of the same key; leave it in place", m.ModelDisplayLabel(key))
		return res, nil
	}
	patch, err := m.engine().PlanCatalogEntryDeletion(key)
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
	res.Message = fmt.Sprintf("Deleted saved model %q from the %s.", m.ModelDisplayLabel(key), m.scopeLabel())
	m.log(res.Message + " (" + path + ")")
	return res, nil
}

// writeAdapterPathShared persists a validated adapter binary path to the scope's SHARED
// adapters.yaml (`.aimesh/adapters.yaml`) — the single seam SetAdapterPath + ConfigureAdapterPath
// share. Adapter binary paths live ONLY here (config.yaml no longer carries them), so this is a single
// atomic write to the resolved scope target.
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

// ConfigureAdapterPath records a validated binary path for an adapter — the web-UI "add / configure
// adapter" action. It reuses the SAME validation and shared-file write seam as the CLI
// `setup --adapter <name> --path`; `fake` needs no path and is rejected.
func (m *Manager) ConfigureAdapterPath(name, binPath string) (Result, error) {
	if err := m.engine().ValidateAdapterForPathCapture(name, m.configurableNames()); err != nil {
		return Result{}, err
	}
	if err := validateBinaryPath(binPath); err != nil {
		return Result{}, err
	}
	return m.writeAdapterPathShared(name, binPath)
}
