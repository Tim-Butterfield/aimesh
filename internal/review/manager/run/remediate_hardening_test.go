package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// These tests pin the write path's HARDENING invariants.
// Each names the property rather than the implementation, because the property is what
// must survive a refactor: a write that is unrecorded, or recorded falsely, is worse than a refused
// write, and every assertion below is a form of that one sentence.

// --- a scripted host remediator, so a test can propose the edits a real model could ---

// scriptedRemediator is an author_remediator lane that returns a fixed remediation result. It
// exists because the deterministic marker engine proposes exactly one same-file hunk: the failure
// modes worth pinning (a group whose second hunk fails, a hunk aimed at another file) are
// MODEL-AUTHORED shapes, and a test that cannot produce them cannot prove they are refused.
type scriptedRemediator struct {
	name string
	body string
}

func (s scriptedRemediator) Name() string              { return s.name }
func (s scriptedRemediator) Available() (bool, string) { return true, "scripted" }
func (s scriptedRemediator) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	return model.Result{Stdout: []byte(s.body), ExitCode: 0}, nil
}

// remediationJSON renders an author_remediator result carrying the given edits.
func remediationJSON(t *testing.T, edits ...review.Edit) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "role": "author_remediator", "phase": "remediate", "edits": edits,
	})
	if err != nil {
		t.Fatalf("render remediation result: %v", err)
	}
	return string(b)
}

// scriptedFixture is remediateFixture with a REAL (non-fake) host lane wired to a scripted
// remediator, so `modelRemediation` is true and the scripted edits are what gets planned.
func scriptedFixture(t *testing.T, body string) (*Manager, string) {
	t.Helper()
	m, ws := remediateFixture(t, true)
	cfg := m.Cfg
	cfg.Adapters["scripted"] = config.Adapter{ModelIdentity: "self_report"}
	cfg.ModelCatalog["scripted-model"] = config.CatalogEntry{
		Provider: "scripted", Runtime: "local", CanonicalModel: "scripted-1",
		DisplayName: "Scripted remediator",
		Adapters:    map[string]config.AdapterModel{"scripted": {ModelArg: "scripted-1"}},
	}
	cfg.Profiles["remediate-scripted"] = config.Profile{
		AdapterPreference: []string{"scripted"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "scripted", Model: "scripted-model"},
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		},
	}
	m.Cfg = cfg
	m.Adapters = map[string]model.Adapter{
		"fake":     fake.New(fake.Valid),
		"scripted": scriptedRemediator{name: "scripted", body: body},
	}
	return m, ws
}

func scriptedRequest(t *testing.T, ws string, mode review.Mode) RemediateRequest {
	t.Helper()
	req := baseRequest(t, ws, mode)
	req.Profile = "remediate-scripted"
	return req
}

// --- W1: an edit may only touch what was reviewed ---

// TestAuthorizeEditTarget_OnlyTheReviewedFile is the rule itself, exhaustively. Root confinement
// answers WHERE a write may land; none of these three refusals is a confinement question, which is
// exactly why a scope check alone let them through.
func TestAuthorizeEditTarget_OnlyTheReviewedFile(t *testing.T) {
	f := review.Finding{ID: "F1", File: "a.go"}
	shown := map[string]bool{"a.go": true, "b.go": true}
	pins := map[string]string{"a.go": "sha256:aa", "b.go": "sha256:bb", "gone.go": BaseHashAbsent}
	cases := []struct {
		name string
		edit review.Edit
		want string
	}{
		{"the finding's own file", review.Edit{File: "a.go"}, ""},
		{"another file, shown and pinned", review.Edit{File: "b.go"}, ReasonEditOutsideFinding},
		{"another file, neither shown nor pinned", review.Edit{File: "c.go"}, ReasonEditOutsideFinding},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fault.ReasonOf(authorizeEditTarget(f, tc.edit, shown, pins)); got != tc.want {
				t.Fatalf("reasonCode = %q, want %q", got, tc.want)
			}
		})
	}
	// The two remaining limbs, stated on the finding's OWN file so the same-file rule cannot be
	// what refuses them: a target nobody was shown, and a target with no recorded base content.
	if got := fault.ReasonOf(authorizeEditTarget(
		review.Finding{ID: "F1", File: "u.go"}, review.Edit{File: "u.go"},
		map[string]bool{}, map[string]string{"u.go": "sha256:uu"},
	)); got != ReasonEditTargetUnreviewed {
		t.Fatalf("an unshown target must be refused, got %q", got)
	}
	if got := fault.ReasonOf(authorizeEditTarget(
		review.Finding{ID: "F1", File: "u.go"}, review.Edit{File: "u.go"},
		map[string]bool{"u.go": true}, map[string]string{},
	)); got != ReasonEditTargetUnreviewed {
		t.Fatalf("an unpinned target must be refused, got %q", got)
	}
	if got := fault.ReasonOf(authorizeEditTarget(
		review.Finding{ID: "F1", File: "gone.go"}, review.Edit{File: "gone.go"},
		map[string]bool{"gone.go": true}, pins,
	)); got != ReasonEditTargetUnreviewed {
		t.Fatalf("a target pinned as ABSENT must be refused, got %q", got)
	}
}

// TestRemediate_ModelCrossFileProposalNeverReachesAnotherFile is the same rule end to end, and it
// documents the DEPTH of the defence rather than pretending there is only one layer.
//
// A model that proposes a hunk in a file its finding does not name is refused twice over. The
// remediation PARSER refuses the result outright (schema.ParseRemediationResult: "remediation may
// only edit the finding's file"), which is why this test observes a marker fallback rather than a
// halt — the model's proposal never becomes an edit at all. authorizeEditTarget is the second
// layer, at the point where a write is AUTHORIZED rather than parsed: it is what still holds if the
// parser is relaxed, if a future engine proposes multi-file fixes, or if edits ever arrive by some
// other route. The invariant both layers exist for is the assertion below: other.go is untouched.
func TestRemediate_ModelCrossFileProposalNeverReachesAnotherFile(t *testing.T) {
	const other = "other.go"
	m, ws := scriptedFixture(t, remediationJSON(t, review.Edit{
		File: other, Anchor: "func Other() {}", Replacement: "func Other() { /* fixed */ }",
	}))
	if err := os.WriteFile(filepath.Join(ws, other), []byte("package sample\n\nfunc Other() {}\n"), 0o644); err != nil {
		t.Fatalf("write second file: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(ws, other))

	req := scriptedRequest(t, ws, review.ModeApply)
	// The other file is shown AND pinned, so nothing but the "inside its finding" rule could
	// possibly stand between the proposal and the file.
	req.Shown[other] = true
	req.BaseHashes = BaseHashes(ws, []string{"sample.go", other})

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(ws, other))
	if string(before) != string(after) {
		t.Fatalf("a hunk aimed outside its finding reached %s:\n%s", other, after)
	}
	r := readReceipt(t, out.RunDir)
	for _, h := range r.Intended {
		if h.File != "sample.go" {
			t.Fatalf("an intended hunk targets %q, outside finding F1's file", h.File)
		}
	}
	for _, f := range r.Files {
		if f != "sample.go" {
			t.Fatalf("the receipt reports writing %q, outside finding F1's file", f)
		}
	}
}

// TestRemediate_UnpinnedTargetIsRefused is the limb that is reachable with NO model at all: a
// caller whose Shown set is wider than its base-hash set. Before this rule, such a finding was
// applied and committed having never been hash-verified — the staleness check simply had nothing
// to say about a file it was never given.
func TestRemediate_UnpinnedTargetIsRefused(t *testing.T) {
	m, ws := remediateFixture(t, true)
	before, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	req := baseRequest(t, ws, review.ModeApply)
	req.BaseHashes = map[string]string{} // the file is shown, but nothing pinned it

	out, err := m.Remediate(context.Background(), req)
	if got := fault.ReasonOf(err); got != ReasonEditTargetUnreviewed {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonEditTargetUnreviewed, err)
	}
	if r := readReceipt(t, out.RunDir); r.Committed || r.CommitAttempted {
		t.Fatalf("receipt = %+v, want nothing committed", r)
	}
	after, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	if string(before) != string(after) {
		t.Fatal("a file that was never base-hashed was written")
	}
}

// --- W2: a finding is all-applied or none-applied ---

// TestRemediate_PartiallyFailedFindingLeavesNothing is the receipt-lies case. The remediator
// proposes two hunks for one finding; the second anchor does not exist. The finding is reported NOT
// applied — and the bytes must agree, which means the FIRST hunk must not survive into the commit.
func TestRemediate_PartiallyFailedFindingLeavesNothing(t *testing.T) {
	m, ws := scriptedFixture(t, remediationJSON(t,
		review.Edit{File: "sample.go", Anchor: "return 1", Replacement: "return 2"},
		review.Edit{File: "sample.go", Anchor: "no such anchor anywhere", Replacement: "x"},
	))
	before, _ := os.ReadFile(filepath.Join(ws, "sample.go"))

	out, err := m.Remediate(context.Background(), scriptedRequest(t, ws, review.ModeApply))
	if err != nil {
		t.Fatalf("a finding whose hunks fail is a recorded outcome, not a run failure: %v", err)
	}
	r := readReceipt(t, out.RunDir)
	if len(r.Applied) != 0 {
		t.Fatalf("receipt.applied = %+v, want empty (the finding did not fully apply)", r.Applied)
	}
	if len(r.NotApplied) != 1 || r.NotApplied[0].FindingID != "F1" {
		t.Fatalf("receipt.notApplied = %+v, want the one finding", r.NotApplied)
	}
	after, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	if string(before) != string(after) {
		t.Fatalf("a finding recorded NOT applied still changed the workspace:\nbefore: %s\nafter:  %s", before, after)
	}
	if len(r.Files) != 0 {
		t.Fatalf("receipt.files = %v, want empty — nothing was written", r.Files)
	}
}

// --- W3: content, not identity, is what catches a concurrent edit ---

// TestRemediate_ConcurrentInPlaceEditIsRefusedAtCommit stages the exact race the pre-window
// staleness check cannot see: the file is edited AFTER the check and BEFORE the commit, in place,
// so its inode never changes and os.SameFile is satisfied. The staged bytes derive from the stale
// base, so writing them would silently discard the concurrent edit.
func TestRemediate_ConcurrentInPlaceEditIsRefusedAtCommit(t *testing.T) {
	m, ws := remediateFixture(t, true)
	target := filepath.Join(ws, "sample.go")
	req := baseRequest(t, ws, review.ModeApply)
	// The pre-window check has already passed by the time the patch is written; edit IN PLACE
	// there, which is what an editor's save does.
	req.OnJournal = func(Journal) {
		f, err := os.OpenFile(target, os.O_WRONLY, 0o644)
		if err != nil {
			t.Errorf("open for in-place edit: %v", err)
			return
		}
		// Same length, same inode: only the CONTENT changed.
		if _, err := f.WriteAt([]byte("package sample\n\nfunc Sample() int { return 9 }\n"), 0); err != nil {
			t.Errorf("in-place edit: %v", err)
		}
		f.Close()
	}
	concurrent := "package sample\n\nfunc Sample() int { return 9 }\n"

	out, err := m.Remediate(context.Background(), req)
	if err == nil {
		t.Fatal("a concurrent in-place edit must be refused at the commit boundary")
	}
	if got := fault.ReasonOf(err); got != ReasonStaleDecisionSet {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonStaleDecisionSet, err)
	}
	r := readReceipt(t, out.RunDir)
	if r.Committed {
		t.Fatal("the receipt claims a commit that was refused")
	}
	if !r.CommitAttempted {
		t.Fatal("the receipt must record that the commit WAS entered — that is what distinguishes a rollback from a run that never tried")
	}
	body, _ := os.ReadFile(target)
	if string(body) != concurrent {
		t.Fatalf("the concurrent edit was overwritten:\nwant: %s\ngot:  %s", concurrent, body)
	}
}

// --- W4: no write without a durable journal ---

// TestRemediate_JournalWriteFailureHaltsBeforeAnyEdit makes the journal unwritable (its path is
// occupied by a DIRECTORY, which is what a full or read-only disk amounts to for this purpose) and
// requires the write window to stay shut. The contract this path advertises is "journal before
// write"; a dropped journal error made that a hope rather than a rule.
func TestRemediate_JournalWriteFailureHaltsBeforeAnyEdit(t *testing.T) {
	m, ws := remediateFixture(t, true)
	before, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	req := baseRequest(t, ws, review.ModeApply)
	// The run directory is created inside Remediate, so the obstruction is planted from the FIRST
	// event — a DIRECTORY exactly where journal.json must be a file. Only the journal's path is
	// obstructed: the receipt must still be written, because a halt nobody can read is the failure
	// this whole path exists to prevent.
	req.OnEvent = func(ev audit.EventLine) {
		if ev.EventType != "remediation_started" {
			return
		}
		if err := os.MkdirAll(filepath.Join(m.ArtifactDir, ev.RunID, "remediation", "journal.json"), 0o755); err != nil {
			t.Errorf("plant obstruction: %v", err)
		}
	}

	out, err := m.Remediate(context.Background(), req)
	if got := fault.ReasonOf(err); got != ReasonJournalUnwritable {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonJournalUnwritable, err)
	}
	after, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	if string(before) != string(after) {
		t.Fatal("the workspace was written although the journal never became durable")
	}
	if r := readReceipt(t, out.RunDir); r.Status != "halted" || r.CommitAttempted || r.Committed {
		t.Fatalf("receipt = %+v, want a halted receipt with no commit attempted", r)
	}
}

// --- W5: the receipt reports the committed reality ---

// TestRemediate_FailedCommitIsReportedAsNotApplied removes the workspace's write permission after
// the staleness check, so the commit fails and rolls back. The receipt must describe THAT — not the
// copy, which did contain the edit.
func TestRemediate_FailedCommitIsReportedAsNotApplied(t *testing.T) {
	m, ws := remediateFixture(t, true)
	target := filepath.Join(ws, "sample.go")
	before, _ := os.ReadFile(target)
	req := baseRequest(t, ws, review.ModeApply)
	// Block the commit only AFTER the staleness check, which is what makes this a failed commit
	// rather than a refused one. How a write is blocked differs per platform; blockCommit owns that.
	var release func()
	req.OnJournal = func(Journal) { release = blockCommit(t, ws, target) }

	out, err := m.Remediate(context.Background(), req)
	if err == nil {
		t.Fatal("a commit that could not be performed must halt")
	}
	if release != nil {
		release()
	}
	r := readReceipt(t, out.RunDir)
	if r.Committed {
		t.Fatalf("receipt claims committed for a failed commit: %+v", r)
	}
	if len(r.Applied) != 0 {
		t.Fatalf("receipt.applied = %+v, want empty — nothing reached the live tree", r.Applied)
	}
	if len(r.NotApplied) != 1 {
		t.Fatalf("receipt.notApplied = %+v, want the finding that did not reach the tree", r.NotApplied)
	}
	if len(r.Files) != 0 {
		t.Fatalf("receipt.files = %v, want empty", r.Files)
	}
	if after, _ := os.ReadFile(target); string(after) != string(before) {
		t.Fatal("a failed commit left the workspace changed")
	}
}

// --- W6: the workspace is bound by identity, not by pathname ---

func TestRemediate_UnboundWorkspaceIsRefused(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := baseRequest(t, ws, review.ModeApply)
	req.WorkspaceIdentity = WorkspaceIdentity{}
	if _, err := m.Remediate(context.Background(), req); fault.ReasonOf(err) != ReasonWorkspaceUnbound {
		t.Fatalf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonWorkspaceUnbound)
	}
}

// TestRemediate_SubstitutedWorkspaceIsRefused SWAPS the directory the path names — the reviewed
// tree is moved aside and a different one is moved into its place, holding a byte-identical copy of
// the targeted file. This is the substitution that defeats every other check: the path resolves,
// the base hashes match (same bytes), and scope.New happily authorizes the substitute as its own
// root. Only the root's IDENTITY disagrees.
//
// A symlinked root is deliberately not the vector used here: meshcore refuses a symlink as a
// workspace root outright (Copy's reparse rule), so it would prove a rule that already held.
func TestRemediate_SubstitutedWorkspaceIsRefused(t *testing.T) {
	m, real := remediateFixture(t, true)
	body, err := os.ReadFile(filepath.Join(real, "sample.go"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	req := baseRequest(t, real, review.ModeApply)

	impostor := filepath.Join(t.TempDir(), "impostor")
	if merr := os.MkdirAll(impostor, 0o755); merr != nil {
		t.Fatalf("make impostor: %v", merr)
	}
	if werr := os.WriteFile(filepath.Join(impostor, "sample.go"), body, 0o644); werr != nil {
		t.Fatalf("write impostor: %v", werr)
	}
	moved := real + ".moved"
	if rerr := os.Rename(real, moved); rerr != nil {
		t.Skipf("cannot stage a directory swap: %v", rerr)
	}
	if rerr := os.Rename(impostor, real); rerr != nil {
		t.Fatalf("swap in the impostor: %v", rerr)
	}

	out, err := m.Remediate(context.Background(), req)
	if got := fault.ReasonOf(err); got != ReasonWorkspaceIdentityChanged {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonWorkspaceIdentityChanged, err)
	}
	if r := readReceipt(t, out.RunDir); r.Status != "halted" || r.CommitAttempted {
		t.Fatalf("receipt = %+v, want halted with no commit attempted", r)
	}
	if b, _ := os.ReadFile(filepath.Join(real, "sample.go")); strings.Contains(string(b), "reviewmesh[") {
		t.Fatalf("the substituted tree was written:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(moved, "sample.go")); strings.Contains(string(b), "reviewmesh[") {
		t.Fatalf("the reviewed tree was written by a request whose path no longer names it:\n%s", b)
	}
}

// --- W7: decisions bind by finding ID ---

func TestRemediate_DecisionSetMustReconcileByID(t *testing.T) {
	f1 := review.Finding{ID: "F1", File: "a.go", Kind: review.KindRisk, Severity: review.SeverityMedium}
	f2 := review.Finding{ID: "F2", File: "b.go", Kind: review.KindRisk, Severity: review.SeverityMedium}
	accept := func(id string) review.Decision {
		return review.Decision{FindingID: id, Valid: true, State: review.StateReportedValid}
	}
	reject := func(id string) review.Decision {
		return review.Decision{FindingID: id, Valid: false, State: review.StateInvalid}
	}
	cases := []struct {
		name      string
		findings  []review.Finding
		decisions []review.Decision
		wantErr   bool
	}{
		{"aligned", []review.Finding{f1, f2}, []review.Decision{accept("F1"), reject("F2")}, false},
		{"reordered decisions", []review.Finding{f1, f2}, []review.Decision{accept("F2"), reject("F1")}, true},
		{"truncated decisions", []review.Finding{f1, f2}, []review.Decision{accept("F1")}, true},
		{"extra decisions", []review.Finding{f1}, []review.Decision{accept("F1"), reject("F2")}, true},
		{"empty finding id", []review.Finding{{File: "a.go"}}, []review.Decision{accept("")}, true},
		{"duplicate finding id", []review.Finding{f1, f1}, []review.Decision{accept("F1"), accept("F1")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reconcileDecisions(tc.findings, tc.decisions, AcceptedForApply)
			if (err != nil) != tc.wantErr {
				t.Fatalf("reconcileDecisions err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestRemediate_ReorderedDecisionSetHalts is the same rule end to end. Positionally, the accepted
// decision lines up with a finding it does not name — so a positional pairing would have applied
// the WRONG finding, silently.
func TestRemediate_ReorderedDecisionSetHalts(t *testing.T) {
	m, ws := remediateFixture(t, true)
	if err := os.WriteFile(filepath.Join(ws, "other.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	req := baseRequest(t, ws, review.ModeApply)
	req.Findings = []review.Finding{
		{ID: "F1", File: "sample.go", Kind: review.KindRisk, Severity: review.SeverityMedium},
		{ID: "F2", File: "other.go", Kind: review.KindRisk, Severity: review.SeverityMedium},
	}
	// The decisions arrive in the other order — each still perfectly well-formed on its own.
	req.Decisions = []review.Decision{
		{FindingID: "F2", Valid: false, State: review.StateInvalid},
		{FindingID: "F1", Valid: true, State: review.StateReportedValid},
	}
	out, err := m.Remediate(context.Background(), req)
	if got := fault.ReasonOf(err); got != ReasonDecisionSetUnreconciled {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonDecisionSetUnreconciled, err)
	}
	if r := readReceipt(t, out.RunDir); r.Status != "halted" || r.Committed {
		t.Fatalf("receipt = %+v, want halted/uncommitted", r)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "other.go")); strings.Contains(string(body), "reviewmesh[") {
		t.Fatal("a mis-bound decision authorized a write")
	}
}

// --- W8: cancelled and committing are mutually exclusive ---

// TestRemediate_CancellationInsideTheWindowDoesNotReportCancelled cancels AFTER the gate has been
// entered. The window is deliberately uncancellable from that point, so the one thing that must NOT
// happen is a call reporting itself cancelled while its commit ran: that receipt would state the
// opposite of what the repository holds.
func TestRemediate_CancellationInsideTheWindowDoesNotReportCancelled(t *testing.T) {
	m, ws := remediateFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	testHookInsideWriteWindow = cancel
	t.Cleanup(func() { testHookInsideWriteWindow = nil })

	out, err := m.Remediate(ctx, baseRequest(t, ws, review.ModeApply))
	if err != nil {
		t.Fatalf("a commit that had already been entered must complete: %v", err)
	}
	if out.Cancelled {
		t.Fatal("the call reported itself CANCELLED although its commit had already started")
	}
	r := readReceipt(t, out.RunDir)
	if r.Status != "complete" || !r.Committed || !r.CommitAttempted {
		t.Fatalf("receipt = %+v, want complete/committed", r)
	}
	body, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	if !strings.Contains(string(body), "reviewmesh[F1]") {
		t.Fatal("the receipt says committed but the workspace was not written")
	}
}

// --- W10: the journal says what was intended, not merely how much of it ---

func TestRemediate_JournalRecordsReplacementAndIntentDigests(t *testing.T) {
	m, ws := remediateFixture(t, true)
	var got Journal
	req := baseRequest(t, ws, review.ModePatch)
	req.OnJournal = func(j Journal) { got = j }
	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if len(got.Hunks) != 1 {
		t.Fatalf("journal hunks = %+v, want one", got.Hunks)
	}
	if !strings.HasPrefix(got.Hunks[0].ReplacementSHA256, "sha256:") {
		t.Fatalf("hunk replacement digest = %q, want a sha256 — a byte count identifies no replacement", got.Hunks[0].ReplacementSHA256)
	}
	if !strings.HasPrefix(got.IntentSHA256, "sha256:") {
		t.Fatalf("intent digest = %q, want a sha256", got.IntentSHA256)
	}
	// The persisted journal and the receipt agree BY VALUE, not merely by both existing.
	b, rerr := os.ReadFile(filepath.Join(out.RunDir, "remediation", "journal.json"))
	if rerr != nil {
		t.Fatalf("read journal: %v", rerr)
	}
	var onDisk Journal
	if uerr := json.Unmarshal(b, &onDisk); uerr != nil {
		t.Fatalf("journal is not valid JSON: %v", uerr)
	}
	if onDisk.IntentSHA256 != got.IntentSHA256 {
		t.Fatalf("persisted intent digest %q != callback's %q", onDisk.IntentSHA256, got.IntentSHA256)
	}
	if r := readReceipt(t, out.RunDir); r.IntentSHA256 != got.IntentSHA256 {
		t.Fatalf("receipt intent digest %q != journal's %q", r.IntentSHA256, got.IntentSHA256)
	}
	// The digest is over the WHOLE ordered intent: a different order is a different intent.
	a := IntendedHunk{FindingID: "F1", File: "a.go", Bytes: 1, Origin: "marker", ReplacementSHA256: hunkDigest("a")}
	bb := IntendedHunk{FindingID: "F2", File: "b.go", Bytes: 1, Origin: "marker", ReplacementSHA256: hunkDigest("b")}
	if intentDigest([]IntendedHunk{a, bb}) == intentDigest([]IntendedHunk{bb, a}) {
		t.Fatal("the intent digest is order-insensitive; a reordered write is a different write")
	}
	// The point of the per-hunk digest: two replacements of the SAME LENGTH — indistinguishable
	// to a byte count alone — are distinguishable by digest.
	if hunkDigest("aa") == hunkDigest("bb") {
		t.Fatal("two same-length replacements share a digest")
	}
}
