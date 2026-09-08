package setup

// Governed cleanup of user-layer generated model-catalog keys that embed an adapter-key prefix
// (e.g. `codex-cli-gpt-5.5-high`) — a shape older web-UI batches materialized. The repair renames
// them off the adapter prefix (`gpt-5.5-high`) and repoints the user-layer profile lanes that use
// them, through SetupManager → SetupEngine → ConfigAccess. It is preview-first, idempotent, and
// SAFE: it touches ONLY user-layer-owned, generated-shape, non-authoritative/non-default entries
// whose lane references are ALL in the writable user layer, and it never coalesces onto a target
// key that already holds different content.

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	esetup "github.com/Tim-Butterfield/aimesh/internal/review/engine/setup"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// ModelKeyRename is one applyable rename in the plan.
type ModelKeyRename struct {
	OldKey string   `json:"oldKey"`
	NewKey string   `json:"newKey"`
	Label  string   `json:"label"`  // readable model label (for the preview)
	UsedBy []string `json:"usedBy"` // "profile.role" refs (all user-layer)
	Reuses bool     `json:"reuses"` // true = NewKey already exists with identical content (merge into it)
}

// ModelKeyCleanupSkip is a candidate that was intentionally NOT auto-repaired, with the reason.
type ModelKeyCleanupSkip struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// ModelKeyCleanupPlan is the previewed old→new mapping + the safely-skipped candidates.
type ModelKeyCleanupPlan struct {
	Renames []ModelKeyRename      `json:"renames"`
	Skipped []ModelKeyCleanupSkip `json:"skipped"`
}

// ModelKeyCleanupConfirm is the exact confirmation string an apply must echo.
func ModelKeyCleanupConfirm() string {
	return "Rename these generated model keys off their adapter-key prefix and repoint the lanes that use them?"
}

// PlanModelKeyCleanup projects the governed cleanup plan (no writes). Idempotent: once applied it
// finds nothing.
func (m *Manager) PlanModelKeyCleanup() ModelKeyCleanupPlan {
	var plan ModelKeyCleanupPlan
	path, err := m.writeTarget()
	if err != nil {
		return plan
	}
	raw, err := config.LoadRawMap(path)
	if err != nil {
		return plan
	}
	userCatalog, _ := raw["modelCatalog"].(map[string]any)
	userProfiles, _ := raw["profiles"].(map[string]any)
	if len(userCatalog) == 0 {
		return plan
	}
	// Keys referenced (in a lane) OR defined (in modelCatalog) by any NON-user config layer. A key
	// touched by a lower/higher layer cannot be safely renamed/deleted from the user layer.
	otherLayer := m.nonUserLayerModelKeys()

	keys := make([]string, 0, len(userCatalog))
	for k := range userCatalog {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	claimed := map[string]bool{} // newKey targets already planned this pass (avoid same-target coalescing)
	for _, oldKey := range keys {
		e, ok := m.Cfg.ModelCatalog[oldKey]
		if !ok {
			continue
		}
		// Never touch adapter-default / authoritative / non-single-adapter entries.
		if e.IsAdapterDefault() || (e.Authoritative != nil && *e.Authoritative) || len(e.Adapters) != 1 {
			continue
		}
		adapter := ""
		for a := range e.Adapters {
			adapter = a
		}
		// Only the generated shape (advisory) + must actually carry the adapter-key prefix.
		if !isGeneratedCatalogEntry(oldKey, adapter, e) || !strings.HasPrefix(oldKey, adapter+"-") {
			continue
		}
		newKey := strings.TrimPrefix(oldKey, adapter+"-")
		if newKey == "" || newKey == oldKey {
			continue
		}

		// (a) A non-user layer references/defines oldKey → deleting it from the user layer could leave
		// that layer dangling (e.g. a lower-layer lane the user layer currently shadows). Skip.
		if otherLayer[oldKey] {
			plan.Skipped = append(plan.Skipped, ModelKeyCleanupSkip{Key: oldKey,
				Reason: "referenced or defined by a project/explicit config layer — edit that file directly"})
			continue
		}
		// (b) Any EFFECTIVE (merged) lane referencing oldKey must be user-layer-defined, else a
		// user-layer write can't repoint it.
		if !m.mergedRefsAllUserLayer(oldKey, userProfiles) {
			plan.Skipped = append(plan.Skipped, ModelKeyCleanupSkip{Key: oldKey,
				Reason: "referenced by a profile lane outside your user config (project/explicit/shipped layer) — edit that file directly"})
			continue
		}
		// (c) Two generated keys stripping to the same new target would overwrite each other's entry.
		if claimed[newKey] {
			plan.Skipped = append(plan.Skipped, ModelKeyCleanupSkip{Key: oldKey,
				Reason: fmt.Sprintf("another generated key also maps to %q — rename this one manually", newKey)})
			continue
		}
		// (d) Existing-target collision: same content merges; different content is manual.
		reuses := false
		if target, exists := m.Cfg.ModelCatalog[newKey]; exists {
			if !reflect.DeepEqual(target, e) {
				plan.Skipped = append(plan.Skipped, ModelKeyCleanupSkip{Key: oldKey,
					Reason: fmt.Sprintf("a different model is already saved as %q — rename manually", newKey)})
				continue
			}
			reuses = true // same content → merge lanes onto the existing target, delete the old key
		}

		// Repoint the RAW user-layer lanes referencing oldKey (this catches user-layer refs that are
		// shadowed by a higher layer in the merged view, which mergedRefsAllUserLayer cannot see).
		refs := m.userLayerRefsForModel(oldKey, userProfiles)
		claimed[newKey] = true
		plan.Renames = append(plan.Renames, ModelKeyRename{
			OldKey: oldKey, NewKey: newKey, Label: m.ModelDisplayLabel(oldKey), UsedBy: refs, Reuses: reuses,
		})
	}
	return plan
}

// nonUserLayerModelKeys returns the set of catalog keys referenced by a lane OR defined in the
// modelCatalog of any layer STRICTLY HIGHER than the write scope (for user scope: project + explicit;
// for project scope: explicit only). Such a key cannot be safely renamed/deleted from the write layer —
// a higher layer still references/defines the old name. Keys in LOWER layers are safe to rename in the
// write layer (renaming there does not disturb the lower layer), so they are NOT excluded here.
func (m *Manager) nonUserLayerModelKeys() map[string]bool {
	out := map[string]bool{}
	for _, hl := range m.higherWriteLayers() {
		path := hl.path
		if path == "" {
			continue
		}
		raw, err := config.LoadRawMap(path)
		if err != nil {
			continue
		}
		if cat, ok := raw["modelCatalog"].(map[string]any); ok {
			for k := range cat {
				out[k] = true
			}
		}
		if profs, ok := raw["profiles"].(map[string]any); ok {
			for _, p := range profs {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				lanes, ok := pm["lanes"].(map[string]any)
				if !ok {
					continue
				}
				for _, lane := range lanes {
					lm, ok := lane.(map[string]any)
					if !ok {
						continue
					}
					if model, _ := lm["model"].(string); model != "" {
						out[model] = true
					}
				}
			}
		}
	}
	return out
}

// userLayerRefsForModel returns the "profile.role" refs whose RAW user-layer lane model == key.
func (m *Manager) userLayerRefsForModel(key string, userProfiles map[string]any) []string {
	var refs []string
	names := make([]string, 0, len(userProfiles))
	for p := range userProfiles {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		pm, ok := userProfiles[p].(map[string]any)
		if !ok {
			continue
		}
		lanes, ok := pm["lanes"].(map[string]any)
		if !ok {
			continue
		}
		roles := make([]string, 0, len(lanes))
		for r := range lanes {
			roles = append(roles, r)
		}
		sort.Strings(roles)
		for _, r := range roles {
			lm, ok := lanes[r].(map[string]any)
			if !ok {
				continue
			}
			if model, _ := lm["model"].(string); model == key {
				refs = append(refs, p+"."+r)
			}
		}
	}
	return refs
}

// mergedRefsAllUserLayer reports whether EVERY effective (merged) lane referencing key is defined in
// the user-layer raw profiles (so a user-layer write can repoint it). A merged ref not present in the
// user layer comes from a project/explicit/shipped layer and must not be silently orphaned.
func (m *Manager) mergedRefsAllUserLayer(key string, userProfiles map[string]any) bool {
	for p, prof := range m.Cfg.Profiles {
		for r, lane := range prof.Lanes {
			if lane.Model == key && !userLayerLaneHasModel(userProfiles, p, r, key) {
				return false
			}
		}
	}
	return true
}

func userLayerLaneHasModel(userProfiles map[string]any, profile, role, key string) bool {
	pm, ok := userProfiles[profile].(map[string]any)
	if !ok {
		return false
	}
	lanes, ok := pm["lanes"].(map[string]any)
	if !ok {
		return false
	}
	lane, ok := lanes[role].(map[string]any)
	if !ok {
		return false
	}
	model, _ := lane["model"].(string)
	return model == key
}

// ApplyModelKeyCleanup applies the (freshly recomputed) cleanup plan behind an exact-echo confirm,
// as ONE governed ConfigPatch to the user layer. Idempotent.
func (m *Manager) ApplyModelKeyCleanup(confirm string) (Result, error) {
	var res Result
	if confirm != ModelKeyCleanupConfirm() {
		return res, fault.New(fault.Usage, "confirmation required: echo the exact cleanup confirmation string")
	}
	plan := m.PlanModelKeyCleanup()
	if len(plan.Renames) == 0 {
		res.Messages = append(res.Messages, "No generated model keys with adapter-key prefixes to clean.")
		return res, nil
	}
	renames := make([]esetup.CatalogRename, 0, len(plan.Renames))
	for _, r := range plan.Renames {
		cr := esetup.CatalogRename{OldKey: r.OldKey, NewKey: r.NewKey}
		if !r.Reuses { // write the entry content at NewKey only when the target does not already exist
			entryMap, err := catalogEntryToMap(m.Cfg.ModelCatalog[r.OldKey])
			if err != nil {
				return res, err
			}
			cr.Entry = entryMap
		}
		for _, ref := range r.UsedBy {
			parts := strings.SplitN(ref, ".", 2)
			if len(parts) == 2 {
				cr.LaneRefs = append(cr.LaneRefs, [2]string{parts[0], parts[1]})
			}
		}
		renames = append(renames, cr)
	}
	patch := m.engine().PlanModelKeyCleanup(renames)
	if patch.IsEmpty() {
		res.Messages = append(res.Messages, "Nothing to write.")
		return res, nil
	}
	path, err := m.writeTarget()
	if err != nil {
		return res, err
	}
	// Back up the exact current file before mutating the real user config, so the user can recover
	// from any unforeseen write/collision issue.
	if backup, err := backupConfigFile(path); err != nil {
		return res, err
	} else if backup != "" {
		res.Backup = backup
	}
	if err := config.ApplyPatchToFile(path, patch); err != nil {
		return res, err
	}
	res.ConfigWritten, res.Path = true, path
	res.Messages = append(res.Messages, fmt.Sprintf("Cleaned %d generated model key(s) in the user config.", len(plan.Renames)))
	if res.Backup != "" {
		res.Messages = append(res.Messages, "Backup written to "+res.Backup)
	}
	m.log(res.Messages[0] + " (" + path + ")")
	return res, nil
}

// backupConfigFile copies path to a timestamped sibling `<path>.<ts>.bak` before a mutation. A
// missing source file is a no-op (returns ""); any other error is surfaced so the caller can abort
// before writing.
func backupConfigFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	base := path + "." + time.Now().UTC().Format("20060102T150405Z") + ".bak"
	// O_EXCL so a second cleanup in the same wall-clock second never overwrites an earlier backup.
	for i := 0; ; i++ {
		backup := base
		if i > 0 {
			backup = fmt.Sprintf("%s.%d", base, i)
		}
		f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil {
			return "", werr
		}
		if cerr != nil {
			return "", cerr
		}
		return backup, nil
	}
}

// catalogEntryToMap serializes a CatalogEntry to a map[string]any (matching its json tags) so it can
// be written verbatim under the new key.
func catalogEntryToMap(e config.CatalogEntry) (map[string]any, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
