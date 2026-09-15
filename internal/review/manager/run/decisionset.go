package run

// The durable decision set is a report run's adjudicated output, written to its run directory so a
// later, separate write can apply exactly the set that was inspected, from any surface and across
// restarts.
//
// A run handle arrives from a peer, so it is verified rather than trusted: it must lexically be a
// direct child of the artifact directory (checked before touching the filesystem, so the check
// discloses nothing), remain one after symlink resolution, and hold a decision set at the expected
// schema version whose runId is the directory's name. Every failure returns the same refusal, so the
// checks are not an oracle. Protecting the artifact directory itself is left to its permissions.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
)

// DecisionSetArtifact is the run-relative path a report run records its decision set at. It sits
// beside `decisions/host-adjudication.json`, which is the human-readable half of the same fact.
const DecisionSetArtifact = "decisions/decision-set.json"

// decisionSetSchema is the artifact's schema version. A set at any other version is not read: a
// governed write must not be driven by a record whose shape this build does not know.
const decisionSetSchema = 1

// Reason codes for resolving a run handle.
const (
	// ReasonRunHandleUnknown — the handle does not name a run directory this agent produced, or
	// that run carries no decision set. It is one code for every way of failing.
	ReasonRunHandleUnknown = "run_handle_unknown"
	// ReasonInlineWorkspaceNotRemediable — the source run reviewed content supplied over the
	// wire and materialized into a directory this process has since deleted. There is nothing
	// real to write to. (The same code the MCP surface reports for the same fact.)
	ReasonInlineWorkspaceNotRemediable = "inline_workspace_not_remediable"
	// ReasonNoAcceptedFindings — the source run adjudicated nothing into its accepted set, so
	// there is nothing to write. (Also the MCP surface's code for it.)
	ReasonNoAcceptedFindings = "no_accepted_findings"
	// ReasonInvalidRunID — a surface supplied a Request.RunID that is not a single path element.
	// It names a directory, so it cannot be a path. See validateRunID.
	ReasonInvalidRunID = "invalid_run_id"
)

// StoredDecisionSet is the on-disk form of a report run's accepted set: the findings and decisions as
// adjudicated, the files reviewers were shown, and base hashes of targeted files as reviewed.
// Recomputing any of these at apply time would amount to a second adjudication.
type StoredDecisionSet struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         string `json:"runId"`
	Surface       string `json:"surface,omitempty"`
	// Workspace is the live directory the findings were judged against, as the run recorded it
	// (absolute). WorkspaceCanonical is its symlink-resolved form at capture time, and is what a
	// later write resolves against — see BindWorkspace.
	Workspace          string `json:"workspace"`
	WorkspaceCanonical string `json:"workspaceCanonical"`
	// WorkspaceIdentityKey is the serialized identity key of the reviewed root (see
	// WorkspaceIdentity.Key); empty when the capturing platform could not express one.
	WorkspaceIdentityKey string `json:"workspaceIdentityKey,omitempty"`
	// Inline marks a workspace the run's own process materialized and deletes when the run ends
	// (an ACP `inlineWorkspace`, MCP's inline branch). Such a set is never remediable.
	Inline bool `json:"inline,omitempty"`
	// Profile, Panel and Roles are carried so the remediation resolves the same configuration the
	// report run used (they select the author_remediator lane). Roles holds a call-composed run's
	// single-slot role seats. No reviewer seat is executed from any of them.
	Profile string                          `json:"profile,omitempty"`
	Panel   []review.SeatSpec               `json:"panel,omitempty"`
	Roles   map[review.Role]review.SeatSpec `json:"roles,omitempty"`
	// Findings and Decisions are the adjudicated set, index-aligned as in RunOutcome; the write path
	// reconciles them by finding id.
	Findings  []review.Finding  `json:"findings"`
	Decisions []review.Decision `json:"decisions"`
	// Shown is the sorted set of workspace-relative files a reviewer was shown.
	Shown []string `json:"shown"`
	// BaseHashes maps each file the accepted set targets to its digest as reviewed.
	BaseHashes map[string]string `json:"baseHashes"`
	// Accepted is how many findings are in the accepted set, by the write path's own predicate.
	Accepted int `json:"accepted"`
}

// ShownSet projects Shown into the map shape RemediateRequest takes.
func (s *StoredDecisionSet) ShownSet() map[string]bool {
	out := make(map[string]bool, len(s.Shown))
	for _, f := range s.Shown {
		out[f] = true
	}
	return out
}

// NamesWorkspace reports whether path names the tree this set was adjudicated against, comparing
// canonical forms with the same case folding as the identity binding.
func (s *StoredDecisionSet) NamesWorkspace(path string) bool {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(s.WorkspaceCanonical) == "" {
		return false
	}
	canon, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return sameCanonicalPath(canon, s.WorkspaceCanonical)
}

// Remediable returns an error when the set has no accepted findings or reviewed an inline workspace,
// which does not persist after its run.
func (s *StoredDecisionSet) Remediable() error {
	if s.Inline {
		return fault.New(fault.Policy, fmt.Sprintf(
			"run %q reviewed an INLINE workspace: the content was supplied over the wire and materialized into a directory this agent has since deleted, so there is nothing real to write to.",
			s.RunID)).WithReason(ReasonInlineWorkspaceNotRemediable)
	}
	if s.Accepted == 0 {
		return fault.New(fault.Policy, fmt.Sprintf(
			"run %q has no accepted findings to apply (%d finding(s), none in the accepted set). There is nothing to write.",
			s.RunID, len(s.Findings))).WithReason(ReasonNoAcceptedFindings)
	}
	return nil
}

// BindWorkspace re-establishes the reviewed root's identity between report and apply and returns a
// live WorkspaceIdentity. The canonical path must resolve as it did at review time, catching a
// re-pointed symlink, and a stored identity key must still match, catching a different tree at the
// same path. Without a stored key the binding rests on the canonical path and the content pins.
// governedWrite re-verifies the returned identity before committing.
func (s *StoredDecisionSet) BindWorkspace() (WorkspaceIdentity, error) {
	changed := func(detail string) error {
		return fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: %s. The accepted set was adjudicated against a specific directory, not against a path string — re-run the report turn against the tree you mean to change.",
			detail)).WithReason(ReasonWorkspaceIdentityChanged)
	}
	if strings.TrimSpace(s.WorkspaceCanonical) == "" {
		// Refuse here: CaptureWorkspaceIdentity("") would resolve to the process working directory.
		return WorkspaceIdentity{}, fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: run %q recorded no canonical identity for the workspace it reviewed, so there is no way to verify that a path still names that tree. Re-run the report turn.",
			s.RunID)).WithReason(ReasonWorkspaceUnbound)
	}
	now, err := CaptureWorkspaceIdentity(s.WorkspaceCanonical)
	if err != nil {
		return WorkspaceIdentity{}, changed(fmt.Sprintf(
			"the reviewed workspace root %q cannot be resolved now (%v)", s.WorkspaceCanonical, err))
	}
	if !sameCanonicalPath(now.Canonical, s.WorkspaceCanonical) {
		return WorkspaceIdentity{}, changed(fmt.Sprintf(
			"%q resolves to %q now, but the review resolved it to %q",
			s.WorkspaceCanonical, now.Canonical, s.WorkspaceCanonical))
	}
	if s.WorkspaceIdentityKey != "" {
		if now.Key != s.WorkspaceIdentityKey {
			return WorkspaceIdentity{}, changed(fmt.Sprintf(
				"%q no longer names the directory that was reviewed (it names a different filesystem object now)",
				s.WorkspaceCanonical))
		}
	}
	return now, nil
}

// recordDecisionSet persists a report run's accepted set to its run directory. Patch and apply runs
// have already written their set, so they record none. A failure does not fail the review; a later
// fromRun write without a set is refused.
func (m *Manager) recordDecisionSet(run *audit.Run, req Request, plan review.RunPlan, out review.RunOutcome) {
	if plan.Mode != review.ModeReport {
		return
	}
	abs, err := filepath.Abs(req.Workspace)
	if err != nil {
		return
	}
	set := StoredDecisionSet{
		SchemaVersion: decisionSetSchema,
		RunID:         run.ID,
		Surface:       plan.Surface,
		Workspace:     abs,
		Inline:        req.WorkspaceEphemeral,
		Profile:       req.Profile,
		Panel:         req.ReviewerPanel,
		Roles:         req.ComposedRoles,
		Findings:      out.Findings,
		Decisions:     out.Decisions,
		Shown:         append([]string(nil), out.ShownFiles...),
	}
	// Capture the root's identity so a later write can prove it targets the reviewed tree. On failure
	// the set stays unbound, and BindWorkspace refuses it.
	if id, ierr := CaptureWorkspaceIdentity(abs); ierr == nil {
		set.WorkspaceCanonical, set.WorkspaceIdentityKey = id.Canonical, id.Key
	}
	files := acceptedTargetFiles(out)
	set.Accepted = acceptedCount(out)
	if !set.Inline && set.WorkspaceCanonical != "" {
		set.BaseHashes = BaseHashes(abs, files)
	}
	if set.BaseHashes == nil {
		set.BaseHashes = map[string]string{}
	}
	_ = run.WriteJSON(DecisionSetArtifact, set)
}

// acceptedCount counts the findings a fromRun write would consider, using the write path's predicate.
func acceptedCount(out review.RunOutcome) int {
	n := 0
	for i := range out.Findings {
		if i < len(out.Decisions) && AcceptedForApply(out.Decisions[i]) {
			n++
		}
	}
	return n
}

// acceptedTargetFiles is the sorted set of workspace-relative files the accepted findings target —
// exactly the files whose base hashes must still match when a later remediation runs.
func acceptedTargetFiles(out review.RunOutcome) []string {
	seen := map[string]bool{}
	var files []string
	for i, f := range out.Findings {
		if i >= len(out.Decisions) || !AcceptedForApply(out.Decisions[i]) {
			continue
		}
		if f.File == "" || seen[f.File] {
			continue
		}
		seen[f.File] = true
		files = append(files, f.File)
	}
	sort.Strings(files)
	return files
}

// ReadDecisionSet resolves a caller-supplied run handle to its decision set and canonical run
// directory, verifying the handle as described in the file comment.
func (m *Manager) ReadDecisionSet(handle string) (*StoredDecisionSet, string, error) {
	return m.readDecisionSetIn(m.ArtifactDir, handle)
}

// ReadDecisionSetFor resolves a run handle (a run directory path) produced for a run over workspace.
// It checks, in order, each location that workspace's runs can live in — its own project state
// directory, then the temp run directory — applying ReadDecisionSet's checks against each.
func (m *Manager) ReadDecisionSetFor(workspace, handle string) (*StoredDecisionSet, string, error) {
	var lastErr error = unknownRunHandle(handle)
	for _, base := range m.runBases(workspace) {
		set, dir, err := m.readDecisionSetIn(base, handle)
		if err == nil {
			return set, dir, nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

// ReadDecisionSetByID resolves an opaque run ID for a run over workspace, trying each location that
// workspace's runs can live in.
func (m *Manager) ReadDecisionSetByID(workspace, runID string) (*StoredDecisionSet, string, error) {
	id := strings.TrimSpace(runID)
	var lastErr error = unknownRunHandle(runID)
	for _, base := range m.runBases(workspace) {
		if base == "" || id == "" {
			continue
		}
		set, dir, err := m.readDecisionSetIn(base, filepath.Join(base, id))
		if err == nil {
			return set, dir, nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

// runBases lists the artifact directories a workspace's runs can live in, most specific first.
func (m *Manager) runBases(workspace string) []string {
	bases := []string{m.artifactDirFor(workspace)}
	if temp := localstate.TempRunDir(reviewComponent); temp != bases[0] {
		bases = append(bases, temp)
	}
	return bases
}

// artifactDirFor is the artifact directory for a run over workspace: ArtifactDirFor's answer when the
// Manager serves many workspaces, else the fixed ArtifactDir.
func (m *Manager) artifactDirFor(workspace string) string {
	if m.ArtifactDirFor != nil && strings.TrimSpace(workspace) != "" {
		if d := m.ArtifactDirFor(workspace); d != "" {
			return d
		}
	}
	return m.ArtifactDir
}

func (m *Manager) readDecisionSetIn(base, handle string) (*StoredDecisionSet, string, error) {
	dir, err := resolveRunDirIn(base, handle)
	if err != nil {
		return nil, "", err
	}
	b, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(DecisionSetArtifact)))
	if rerr != nil {
		return nil, "", unknownRunHandle(handle)
	}
	var set StoredDecisionSet
	dec := json.NewDecoder(bytes.NewReader(b))
	// Reject unknown fields: they would be write inputs this build silently drops.
	dec.DisallowUnknownFields()
	if jerr := dec.Decode(&set); jerr != nil {
		return nil, "", unknownRunHandle(handle)
	}
	// The recorded run id must match the directory, so a set copied from another run is refused.
	if set.SchemaVersion != decisionSetSchema || set.RunID != filepath.Base(dir) {
		return nil, "", unknownRunHandle(handle)
	}
	return &set, dir, nil
}

// RunHandle joins a run id to the artifact directory, giving the handle ReadDecisionSet resolves. It
// serves surfaces that return opaque run ids rather than paths. The result still faces every handle
// check, so it grants nothing.
func (m *Manager) RunHandle(runID string) string {
	base, id := strings.TrimSpace(m.ArtifactDir), strings.TrimSpace(runID)
	if base == "" || id == "" {
		return ""
	}
	return filepath.Join(base, id)
}

// validateRunID refuses a Request.RunID that is not a single path element. The id is server-generated,
// but it becomes a directory name, so it is checked regardless.
func validateRunID(id string) error {
	if id == "" {
		return nil
	}
	if id != filepath.Base(id) || id == "." || id == ".." ||
		filepath.IsAbs(id) || strings.ContainsAny(id, `/\`) {
		return fault.New(fault.Usage, fmt.Sprintf(
			"run id %q is not a single path element. A run id NAMES this run's directory under the artifact directory, so it may not contain a separator, resolve to a parent, or be an absolute path.",
			id)).WithReason(ReasonInvalidRunID)
	}
	return nil
}

// resolveRunDirIn turns a peer-supplied handle into the canonical run directory it names under base,
// or refuses.
func resolveRunDirIn(artifactDir, handle string) (string, error) {
	base, h := strings.TrimSpace(artifactDir), strings.TrimSpace(handle)
	if base == "" || h == "" {
		return "", unknownRunHandle(handle)
	}
	absBase, berr := filepath.Abs(base)
	absRun, herr := filepath.Abs(h)
	if berr != nil || herr != nil {
		return "", unknownRunHandle(handle)
	}
	// Check lexically before consulting the filesystem (see the file comment).
	if !directChildPath(absBase, absRun) {
		return "", unknownRunHandle(handle)
	}
	canonBase, berr := filepath.EvalSymlinks(absBase)
	canonRun, herr := filepath.EvalSymlinks(absRun)
	if berr != nil || herr != nil {
		return "", unknownRunHandle(handle)
	}
	if !directChildPath(canonBase, canonRun) {
		return "", unknownRunHandle(handle)
	}
	fi, serr := os.Stat(canonRun)
	if serr != nil || !fi.IsDir() {
		return "", unknownRunHandle(handle)
	}
	return canonRun, nil
}

// directChildPath reports whether child is an immediate child of parent; run directories are only
// created one level below the artifact directory.
func directChildPath(parent, child string) bool {
	parent, child = filepath.Clean(parent), filepath.Clean(child)
	if parent == child {
		return false
	}
	name := filepath.Base(child)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return false
	}
	return sameCanonicalPath(filepath.Dir(child), parent)
}

// unknownRunHandle is the single refusal for every handle failure. It quotes the handle and discloses
// nothing else.
func unknownRunHandle(handle string) error {
	return fault.New(fault.Usage, fmt.Sprintf(
		"run handle %q does not name a run directory this agent produced, or that run recorded no decision set (only a completed `report` run does). "+
			"`fromRun` is the `runDir` this agent returned in the RESPONSE to an earlier `report` turn — it is verified against this agent's own artifact directory rather than trusted as a path, so an arbitrary directory is refused whether or not it exists. "+
			"Run the report turn, read `runDir` from its response, and pass that.",
		handle)).WithReason(ReasonRunHandleUnknown)
}
