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

// These tests exercise the WRITE WINDOW itself — the from-run remediation path — with no model,
// no adapter and no network: the deterministic remediation engine supplies the edits, so what is
// under test is the mechanism (staleness, journal, receipt, cancellation, commit), not a model.

// remediateFixture builds a hermetic workspace + Manager. `granted` controls whether the `mcp`
// surface holds the allowRemediate capability, because the ceiling that permits a write is exactly
// what these tests must not be able to sidestep.
func remediateFixture(t *testing.T, granted bool) (*Manager, string) {
	t.Helper()
	t.Setenv("AIMESH_HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "sample.go"), []byte("package sample\n\nfunc Sample() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg := config.Default()
	// A deterministic, fully in-process profile: every lane is the internal fake adapter, so the
	// remediation falls back to the deterministic marker engine and no model is ever called.
	cfg.Profiles["remediate-fixture"] = config.Profile{
		AdapterPreference: []string{"fake"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		},
	}
	if granted {
		cfg = config.WithSurfaceCapability(cfg, "mcp", config.CapabilityAllowRemediate)
	}
	return &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": fake.New(fake.Valid)},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}, ws
}

// acceptedSet is one accepted finding about sample.go, in exactly the shape a completed report run
// carries: valid, `reported_valid`, with no apply refusal.
func acceptedSet() ([]review.Finding, []review.Decision) {
	f := review.Finding{
		ID: "F1", Title: "sample returns a magic number", Kind: review.KindRisk,
		Severity: review.SeverityMedium, File: "sample.go",
	}
	d := review.Decision{FindingID: "F1", Valid: true, State: review.StateReportedValid}
	return []review.Finding{f}, []review.Decision{d}
}

func baseRequest(t *testing.T, ws string, mode review.Mode) RemediateRequest {
	t.Helper()
	findings, decisions := acceptedSet()
	// The workspace identity is captured exactly where a surface captures it: on the run that
	// produced the decision set, BEFORE anything moves. A request without it is refused (see
	// TestRemediate_UnboundWorkspaceIsRefused), which is why every fixture carries one.
	id, err := CaptureWorkspaceIdentity(ws)
	if err != nil {
		t.Fatalf("capture workspace identity: %v", err)
	}
	return RemediateRequest{
		Workspace: ws, WorkspaceIdentity: id, Mode: mode, Surface: "mcp", Profile: "remediate-fixture",
		SourceRunID: "run-source", Findings: findings, Decisions: decisions,
		Shown:      map[string]bool{"sample.go": true},
		BaseHashes: BaseHashes(ws, []string{"sample.go"}),
	}
}

func readReceipt(t *testing.T, runDir string) Receipt {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(runDir, "remediation", "receipt.json"))
	if err != nil {
		t.Fatalf("receipt must be persisted on every path: %v", err)
	}
	var r Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("receipt is not valid JSON: %v", err)
	}
	return r
}

// The CEILING is authoritative. Without the capability the `mcp` surface resolves apply down to
// report, and the remediation refuses rather than writing at the lower mode — a silent downgrade
// here would be a write nobody authorized... or, worse, an apply that quietly did nothing.
func TestRemediate_RefusedWithoutTheCapability(t *testing.T) {
	m, ws := remediateFixture(t, false)
	_, err := m.Remediate(context.Background(), baseRequest(t, ws, review.ModeApply))
	if err == nil {
		t.Fatal("apply must be refused when the mcp surface has no allowRemediate capability")
	}
	if got := fault.ReasonOf(err); got != ReasonRemediateModeRefused {
		t.Fatalf("reasonCode = %q, want %q", got, ReasonRemediateModeRefused)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "sample.go")); strings.Contains(string(body), "reviewmesh[") {
		t.Fatal("a refused remediation must not have written anything")
	}
}

func TestRemediate_AppliesTheAcceptedSetAndWritesAReceipt(t *testing.T) {
	m, ws := remediateFixture(t, true)
	var journal Journal
	req := baseRequest(t, ws, review.ModeApply)
	req.OnJournal = func(j Journal) { journal = j }

	out, err := m.Remediate(context.Background(), req)
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	// The JOURNAL exists, is durable, and describes what was about to be written.
	if len(journal.Hunks) == 0 {
		t.Fatal("the journal callback must receive the intended hunks BEFORE any write")
	}
	if _, jerr := os.Stat(filepath.Join(out.RunDir, "remediation", "journal.json")); jerr != nil {
		t.Fatalf("the journal must be persisted: %v", jerr)
	}
	if journal.Hunks[0].File != "sample.go" {
		t.Fatalf("journal hunk file = %q, want sample.go", journal.Hunks[0].File)
	}
	// The RECEIPT is the durable answer, and it agrees with the live tree.
	r := readReceipt(t, out.RunDir)
	if r.Status != "complete" || !r.Committed {
		t.Fatalf("receipt status=%q committed=%v, want complete/true", r.Status, r.Committed)
	}
	if len(r.Applied) != 1 || r.Applied[0].FindingID != "F1" {
		t.Fatalf("receipt.applied = %+v, want the one accepted finding", r.Applied)
	}
	if r.BaseHashesVerified != 1 {
		t.Fatalf("baseHashesVerified = %d, want 1", r.BaseHashesVerified)
	}
	if r.PatchArtifact == "" || !strings.HasPrefix(r.PatchSHA256, "sha256:") {
		t.Fatalf("a patch artifact and its hash must be recorded, got %q / %q", r.PatchArtifact, r.PatchSHA256)
	}
	body, rerr := os.ReadFile(filepath.Join(ws, "sample.go"))
	if rerr != nil {
		t.Fatalf("read applied file: %v", rerr)
	}
	if !strings.Contains(string(body), "reviewmesh[F1]") {
		t.Fatalf("apply mode must reach the live workspace; got:\n%s", body)
	}
}

// patch mode produces the diff and touches nothing.
func TestRemediate_PatchModeWritesNothingLive(t *testing.T) {
	m, ws := remediateFixture(t, true)
	before, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	out, err := m.Remediate(context.Background(), baseRequest(t, ws, review.ModePatch))
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if r := readReceipt(t, out.RunDir); r.Committed {
		t.Fatal("patch mode must never commit")
	}
	after, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	if string(before) != string(after) {
		t.Fatal("patch mode changed the live workspace")
	}
}

// STALENESS. The decision set describes a workspace that has moved on, so applying it would be
// acting on a judgment about a file that no longer exists in that form.
func TestRemediate_StaleBaseHashHalts(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := baseRequest(t, ws, review.ModeApply)
	// Someone edits the file between the review and the apply.
	if err := os.WriteFile(filepath.Join(ws, "sample.go"), []byte("package sample\n\nfunc Sample() int { return 2 }\n"), 0o644); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	out, err := m.Remediate(context.Background(), req)
	if err == nil {
		t.Fatal("a stale decision set must halt")
	}
	if got := fault.ReasonOf(err); got != ReasonStaleDecisionSet {
		t.Fatalf("reasonCode = %q, want %q", got, ReasonStaleDecisionSet)
	}
	if r := readReceipt(t, out.RunDir); r.Status != "halted" || r.Committed || len(r.Applied) != 0 {
		t.Fatalf("halted receipt = %+v, want halted/uncommitted/empty", r)
	}
	body, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	if strings.Contains(string(body), "reviewmesh[") {
		t.Fatal("a stale halt must not write")
	}
}

// A file that appears AFTER the review is as much a change as one that was edited: the base hash
// records its absence, so it cannot be silently written to.
func TestRemediate_FileCreatedAfterTheReviewIsStale(t *testing.T) {
	m, ws := remediateFixture(t, true)
	req := baseRequest(t, ws, review.ModeApply)
	req.BaseHashes["new.go"] = BaseHashAbsent
	if err := os.WriteFile(filepath.Join(ws, "new.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Remediate(context.Background(), req); fault.ReasonOf(err) != ReasonStaleDecisionSet {
		t.Fatalf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonStaleDecisionSet)
	}
}

// CANCELLATION AT THE COMMIT BOUNDARY. The copy has already been edited and the patch written when
// the cancellation lands — and the live tree must still be untouched, with a receipt that says so.
// This is the case that gets no response to answer with, so the receipt is the answer.
func TestRemediate_CancelledBeforeCommitDoesNotWrite(t *testing.T) {
	m, ws := remediateFixture(t, true)
	before, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := baseRequest(t, ws, review.ModeApply)
	// Cancel exactly when the patch artifact has been written: the last boundary before commit.
	req.OnEvent = func(ev audit.EventLine) {
		if ev.EventType == "patch_written" {
			cancel()
		}
	}
	out, err := m.Remediate(ctx, req)
	if err == nil {
		t.Fatal("a cancelled remediation must return a cancellation halt")
	}
	if got := fault.ReasonOf(err); got != ReasonRemediateCancelled {
		t.Fatalf("reasonCode = %q, want %q", got, ReasonRemediateCancelled)
	}
	if !out.Cancelled {
		t.Fatal("the outcome must report the cancellation")
	}
	r := readReceipt(t, out.RunDir)
	if r.Status != "cancelled" || r.Committed {
		t.Fatalf("receipt = %+v, want cancelled/uncommitted", r)
	}
	if len(r.Intended) == 0 {
		t.Fatal("the receipt of a cancelled call must still say what it intended to write")
	}
	after, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	if string(before) != string(after) {
		t.Fatal("a cancelled call committed to the live workspace")
	}
}

// The write-path rule's refusals survive into the remediation: a quarantined finding is never part
// of the accepted set, whatever a later caller asks for.
func TestAcceptedForApply_ExcludesRefusedAndUndecided(t *testing.T) {
	no := false
	cases := []struct {
		name string
		d    review.Decision
		want bool
	}{
		{"accepted", review.Decision{Valid: true, State: review.StateReportedValid}, true},
		{"invalid", review.Decision{Valid: false, State: review.StateReportedValid}, false},
		{"skipped", review.Decision{Valid: true, State: review.StateSkipped}, false},
		{"already addressed", review.Decision{Valid: true, State: review.StateAlreadyAddr}, false},
		{"no workspace evidence", review.Decision{Valid: true, State: review.StateReportedValid,
			Applyable: &no, ApplyRefusalReason: "no_workspace_evidence"}, false},
		{"authority only", review.Decision{Valid: true, State: review.StateReportedValid,
			ApplyRefusalReason: "authority_only"}, false},
		{"undecided", review.Decision{Valid: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AcceptedForApply(tc.d); got != tc.want {
				t.Fatalf("AcceptedForApply = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAcceptedForApply_NonApplyableWithoutAReasonIsStillRefused pins the SECOND half of the
// write-path gate, which a test-strength sweep found unguarded: every existing case that sets
// `Applyable: &false` also sets `ApplyRefusalReason`, so deleting the `Applyable` check changed
// nothing and no test noticed.
//
// The two fields are not one fact. `ApplyRefusalReason` is the host's explanation; `Applyable`
// is the verdict. A decision set does not always arrive straight from the adjudicator that
// wrote both — `review_remediate --fromRun` replays a set recorded by an earlier run, and the
// MCP surface carries decisions across a process boundary. If a verdict of "not applyable"
// ever arrives without its prose, the write path must still honour the verdict. Reading only
// the reason string would turn a missing explanation into permission to write.
func TestAcceptedForApply_NonApplyableWithoutAReasonIsStillRefused(t *testing.T) {
	no := false
	d := review.Decision{
		Valid: true, State: review.StateReportedValid,
		Applyable: &no, // the verdict…
		// …and deliberately NO ApplyRefusalReason.
	}
	if AcceptedForApply(d) {
		t.Fatal("a decision marked NOT applyable must never join the accepted set, even when it carries no refusal reason: the verdict is the gate, the prose is only the explanation")
	}
	// The control: the same decision with the verdict ABSENT (nil, meaning the write-path rule
	// never refused it) is accepted — so the refusal above comes from the verdict, not from the
	// test having built an unacceptable decision some other way.
	d.Applyable = nil
	if !AcceptedForApply(d) {
		t.Fatal("an unrefused reported_valid decision must still be accepted; otherwise this test proves nothing about the Applyable check")
	}
}

func TestBaseHashes_RecordAbsenceAndDetectChange(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := BaseHashes(ws, []string{"a.txt", "missing.txt"})
	if !strings.HasPrefix(got["a.txt"], "sha256:") {
		t.Fatalf("a.txt digest = %q", got["a.txt"])
	}
	if got["missing.txt"] != BaseHashAbsent {
		t.Fatalf("missing.txt = %q, want %q", got["missing.txt"], BaseHashAbsent)
	}
	if stale := StaleBaseHashes(ws, got); len(stale) != 0 {
		t.Fatalf("unchanged workspace reported stale files: %v", stale)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("two"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if stale := StaleBaseHashes(ws, got); len(stale) != 1 || stale[0] != "a.txt" {
		t.Fatalf("stale = %v, want [a.txt]", stale)
	}
}

// The surface ceiling is ONE function, and a granted capability RAISES it rather than bypassing it.
func TestSurfaceCeiling_CapabilityRaisesRatherThanBypasses(t *testing.T) {
	cfg := config.Default()
	if got := cfg.SurfaceCeiling("mcp"); got != review.ModeReport {
		t.Fatalf("shipped mcp ceiling = %q, want report", got)
	}
	granted := config.WithSurfaceCapability(cfg, "mcp", config.CapabilityAllowRemediate)
	if got := granted.SurfaceCeiling("mcp"); got != review.ModeApply {
		t.Fatalf("granted mcp ceiling = %q, want apply", got)
	}
	// The grant is a COPY: the snapshot other components hold is untouched.
	if cfg.SurfaceCeiling("mcp") != review.ModeReport {
		t.Fatal("WithSurfaceCapability mutated the original config")
	}
	// It is scoped to the surface it names.
	if granted.SurfaceCeiling("acp") != review.ModeReport {
		t.Fatal("granting mcp changed the acp ceiling")
	}
}
