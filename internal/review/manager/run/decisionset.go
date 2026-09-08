package run

// THE DURABLE DECISION SET — a completed REPORT run's already-adjudicated output, written to that
// run's own directory so a LATER, SEPARATE write can apply exactly the set a human inspected.
//
// WHY ON DISK RATHER THAN IN A REGISTRY. `Manager.Remediate` has always been able to apply a
// decision set it was handed; what varied was where the caller got one. The MCP surface holds it in
// an in-memory registry (`surface/mcp/runs.go`), which is honest but dies with the process. A run
// directory does not: it is the artifact every surface already promises a caller, it survives a
// restart, and it is the same file whether the run was produced by the CLI, by ACP or by MCP. So the
// FROM-RUN input is stored where the run already is, and BOTH agent surfaces read it — ACP always,
// MCP whenever its registry no longer holds the handle. What still dies with the MCP process is its
// source-run guard, not the set; see that surface's own note on what a second apply gets.
//
// WHAT MAKES A HANDLE TRUSTWORTHY, given that it arrives from a peer. A `runDir` on an agent surface
// is a peer-supplied absolute path, and treating one as "the run to apply" would let any local
// process point this at an arbitrary directory. So the handle is VERIFIED rather than trusted, in
// this order and for these reasons:
//
//  1. LEXICALLY a direct child of THIS agent's artifact directory — checked before anything is
//     stat-ed, so a handle naming a path outside it is refused without the filesystem being
//     consulted at all. That matters on a surface whose whole job is to refuse arbitrary paths: a
//     check that stat-ed first would answer "does this exist" for every path a peer cared to name.
//  2. CANONICALLY the same, after symlink resolution. `<artifacts>/x` where `x` is a symlink to
//     somewhere else is SPELLED like a run of ours and is not one.
//  3. Carrying a decision set at the expected schema version whose recorded `runId` is the
//     directory's own name — so a file copied in from another run does not answer for this one.
//
// Every one of those failures is the SAME refusal with the same reason code. A caller learns "that
// is not a run handle of mine" and nothing else; which of the three failed would be an oracle.
//
// WHAT IT DOES NOT DO. It is not an access-control boundary for the artifact directory itself: a
// process that can write there can write a decision set naming any workspace. That is not a new
// exposure — the same process can write the run's findings, its receipt and its journal — and the
// answer is the artifact directory's own permissions, not a check inside this file.

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
)

// DecisionSetArtifact is the run-relative path a report run records its decision set at. It sits
// beside `decisions/host-adjudication.json`, which is the human-readable half of the same fact.
const DecisionSetArtifact = "decisions/decision-set.json"

// decisionSetSchema is the artifact's schema version. A set at any other version is not read: a
// governed write must not be driven by a record whose shape this build does not know.
const decisionSetSchema = 1

// Stable MACHINE reason codes for resolving a run handle (lower_snake, never sentences).
const (
	// ReasonRunHandleUnknown — the handle does not name a run directory this agent produced, or
	// that run carries no decision set. It is deliberately ONE code for every way of failing;
	// see the file comment.
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

// StoredDecisionSet is the on-disk form of a report run's accepted set — the exact input a from-run
// remediation writes from.
//
// It carries every input `RemediateRequest` needs and nothing a later run could re-derive: the
// findings and decisions AS ADJUDICATED, the files the reviewers were actually shown, and the base
// hashes of every targeted file AS REVIEWED. Recomputing any of those at apply time would be a
// second adjudication wearing the first one's name — which is the whole failure this exists to
// close.
type StoredDecisionSet struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         string `json:"runId"`
	Surface       string `json:"surface,omitempty"`
	// Workspace is the live directory the findings were judged against, as the run recorded it
	// (absolute). WorkspaceCanonical is its symlink-resolved form at capture time, and is what a
	// later write resolves against — see BindWorkspace.
	Workspace          string `json:"workspace"`
	WorkspaceCanonical string `json:"workspaceCanonical"`
	// WorkspaceIdentityKey is the DURABLE form of the reviewed root's filesystem identity
	// (device + inode). `WorkspaceIdentity.Info` is an `fs.FileInfo` and cannot be serialized, so
	// this is what carries the identity binding across the report→apply gap. It is EMPTY on a
	// platform that cannot express one through `fs.FileInfo.Sys()` (Windows: the file index
	// `os.SameFile` compares is not reachable there) — see BindWorkspace for what still holds.
	WorkspaceIdentityKey string `json:"workspaceIdentityKey,omitempty"`
	// Inline marks a workspace the run's own process materialized and deletes when the run ends
	// (an ACP `inlineWorkspace`, MCP's inline branch). Such a set is never remediable.
	Inline bool `json:"inline,omitempty"`
	// Profile and Panel are carried so the remediation resolves the SAME configuration the report
	// run used (it selects the author_remediator lane). No reviewer seat is executed from them.
	Profile string            `json:"profile,omitempty"`
	Panel   []review.SeatSpec `json:"panel,omitempty"`
	// Findings and Decisions are the adjudicated set, index-aligned exactly as RunOutcome carries
	// them. Nothing here is re-judged; the write path reconciles them BY FINDING ID.
	Findings  []review.Finding  `json:"findings"`
	Decisions []review.Decision `json:"decisions"`
	// Shown is the sorted set of workspace-relative files a reviewer was actually shown. It gates
	// the write exactly as it gated the run that produced the decisions.
	Shown []string `json:"shown"`
	// BaseHashes is path → digest AS REVIEWED, for every file the accepted set targets.
	// Re-verifying it is what makes a stale decision set a halt rather than a silent write over a
	// file nobody judged.
	BaseHashes map[string]string `json:"baseHashes"`
	// Accepted is how many findings are in the accepted set, computed once, here, by the same
	// predicate the write path uses.
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

// NamesWorkspace reports whether `path` names the same tree this set was adjudicated against.
//
// It is for a caller that wants to check a workspace a REQUEST asserted, before handing the write
// the set's own workspace. It compares canonical forms — the same comparison, with the same
// Windows case folding, that the identity binding uses — so two spellings of one directory are one
// directory and a symlink is judged by what it resolves to.
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

// Remediable reports the one thing a stored set can say about itself before any workspace is
// touched: whether there is anything real to write, and anywhere real to write it.
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

// BindWorkspace re-establishes the reviewed root's identity binding ACROSS THE REPORT→APPLY GAP and
// returns the live WorkspaceIdentity the write path verifies with.
//
// Two checks, and they answer different questions:
//
//   - The CANONICAL PATH must still resolve to what it resolved to when the review ran. A symlink
//     re-pointed between the two turns is caught here.
//   - The DURABLE IDENTITY KEY (device + inode on unix, volume serial + file index on Windows) must
//     still match. This is the check a pathname cannot make: point the same canonical path at a
//     different checkout whose targeted files happen to carry the same bytes and every other check
//     passes.
//
// A stored key that is EMPTY means the platform that captured it could not express one (see
// rootIdentityKey). The binding then rests on the canonical path plus the content pins — which are
// verified twice, once before the window and once per destination inside the commit — and that is
// stated rather than papered over. It is not a hole a caller opens: a set with no key is a set some
// other platform wrote.
//
// The returned identity is a LIVE capture, so `governedWrite`'s own `verifyAgainst` re-checks the
// identity over the (much shorter) interval between this call and the commit.
func (s *StoredDecisionSet) BindWorkspace() (WorkspaceIdentity, error) {
	changed := func(detail string) error {
		return fault.New(fault.Policy, fmt.Sprintf(
			"remediation refused: %s. The accepted set was adjudicated against a specific directory, not against a path string — re-run the report turn against the tree you mean to change.",
			detail)).WithReason(ReasonWorkspaceIdentityChanged)
	}
	if strings.TrimSpace(s.WorkspaceCanonical) == "" {
		// The run that produced this set could not capture its root's identity, so there is
		// nothing to verify against. It is refused HERE rather than passed on as an unbound
		// identity, because `CaptureWorkspaceIdentity("")` would resolve the empty path to the
		// PROCESS WORKING DIRECTORY — a silent substitution of a tree nobody reviewed.
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

// recordDecisionSet persists a completed REPORT run's accepted set to its run directory.
//
// It runs for REPORT runs only, and that is the whole rule: `AcceptedForApply` acts on
// `reported_valid`, which is the state report mode finalizes an actionable finding into. A
// patch/apply run has already written its own accepted set, so a second, later application of it
// would be a second write of decisions nobody re-read.
//
// A failure here does NOT fail the review. The report is a complete, correct answer whether or not
// a later turn can write from it; the fail-closed half is on the other side — a from-run write with
// no decision set to read is refused, not guessed at.
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
		Findings:      out.Findings,
		Decisions:     out.Decisions,
		Shown:         append([]string(nil), out.ShownFiles...),
	}
	// The reviewed root's identity, captured HERE for the same reason the base hashes are: a later
	// write must be able to prove it is writing to the tree that was judged, and a path string
	// cannot prove that. A capture failure is not a review failure — the report stands — but it
	// leaves the set unbound, and BindWorkspace refuses an unbound set rather than writing on the
	// strength of a pathname.
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

// acceptedCount counts the findings a from-run write would consider, by the SAME predicate the
// write path applies. One definition, so a caller cannot be told a run has an accepted set that the
// write path then declines to see.
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

// ReadDecisionSet resolves a caller-supplied RUN HANDLE to the decision set that run produced,
// returning the set and the canonical run directory it was read from.
//
// The handle is VERIFIED against this agent's own artifact directory rather than trusted as a path;
// see the file comment for the order of the checks and why every failure answers alike.
func (m *Manager) ReadDecisionSet(handle string) (*StoredDecisionSet, string, error) {
	dir, err := m.resolveRunDir(handle)
	if err != nil {
		return nil, "", err
	}
	b, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(DecisionSetArtifact)))
	if rerr != nil {
		return nil, "", unknownRunHandle(handle)
	}
	var set StoredDecisionSet
	dec := json.NewDecoder(bytes.NewReader(b))
	// STRICT. An unknown field means this file was written by a build that recorded something this
	// one would silently drop — and what it would drop is a governance input to a live write.
	dec.DisallowUnknownFields()
	if jerr := dec.Decode(&set); jerr != nil {
		return nil, "", unknownRunHandle(handle)
	}
	// The recorded run id must be the directory's own name: a decision set copied in from another
	// run is a set that does not describe this one.
	if set.SchemaVersion != decisionSetSchema || set.RunID != filepath.Base(dir) {
		return nil, "", unknownRunHandle(handle)
	}
	return &set, dir, nil
}

// RunHandle spells a RUN ID as the handle ReadDecisionSet resolves — `<artifactDir>/<runId>`.
//
// It exists for a surface that hands its caller an OPAQUE RUN ID rather than a path. ACP returns
// the run DIRECTORY and a host passes that same string back, so it needs nothing here; MCP
// deliberately withholds host paths and returns only the id, so the directory has to be spelled for
// it. The join lives HERE, beside the artifact directory it joins to, rather than on a surface that
// would then hold a second opinion about where this agent's runs live.
//
// It GRANTS NOTHING. The result faces ReadDecisionSet's four checks unchanged, so an id spelled as a
// traversal, as an absolute path, or as a nested path is refused by the lexical containment check
// exactly as the same string handed in as a handle would be. It is a spelling, not a bypass.
func (m *Manager) RunHandle(runID string) string {
	base, id := strings.TrimSpace(m.ArtifactDir), strings.TrimSpace(runID)
	if base == "" || id == "" {
		return ""
	}
	return filepath.Join(base, id)
}

// validateRunID guards Request.RunID — the one request field that becomes a filesystem NAME.
//
// The value is server-generated (see Request.RunID), so this cannot fire for any client of any
// surface today. It exists because the field is joined to the artifact directory, and a check that
// is only true by the current caller's good behaviour is a check that is one caller away from being
// false. A run id is a single path element or it is refused.
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

// resolveRunDir turns a peer-supplied handle into the canonical run directory it names, or refuses.
func (m *Manager) resolveRunDir(handle string) (string, error) {
	base, h := strings.TrimSpace(m.ArtifactDir), strings.TrimSpace(handle)
	if base == "" || h == "" {
		return "", unknownRunHandle(handle)
	}
	absBase, berr := filepath.Abs(base)
	absRun, herr := filepath.Abs(h)
	if berr != nil || herr != nil {
		return "", unknownRunHandle(handle)
	}
	// LEXICAL FIRST, before the filesystem is consulted at all. See the file comment: a check that
	// stat-ed first would answer "does this exist" for any path a peer named.
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

// directChildPath reports whether child is an IMMEDIATE child of parent. Immediacy is the point,
// not containment: run directories are created as `<artifactDir>/<runId>` and nothing else is, so
// "somewhere under the artifact directory" would admit a nested path this agent never made.
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

// unknownRunHandle is the ONE refusal every handle failure answers with. The handle is quoted back
// because the caller supplied it; nothing about this agent's filesystem is disclosed.
func unknownRunHandle(handle string) error {
	return fault.New(fault.Usage, fmt.Sprintf(
		"run handle %q does not name a run directory this agent produced, or that run recorded no decision set (only a completed `report` run does). "+
			"`fromRun` is the `runDir` this agent returned in the RESPONSE to an earlier `report` turn — it is verified against this agent's own artifact directory rather than trusted as a path, so an arbitrary directory is refused whether or not it exists. "+
			"Run the report turn, read `runDir` from its response, and pass that.",
		handle)).WithReason(ReasonRunHandleUnknown)
}
