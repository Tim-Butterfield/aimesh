package run

// These tests are about the FULL-CYCLE write path — the one `review --apply` on the CLI and an ACP
// `apply` prompt reach through Manager.RunContext. Until this convergence it was a second,
// unguarded implementation: it built its own remediation copy and committed with a plain, unpinned
// `ws.Commit`, so none of the guarantees the MCP remediation path advertises were true here. Every
// test in this file fails against that code, and each one says how in its own comment.
//
// The Manager is exercised with `Surface: "cli"` because that is literally what
// `internal/surface/cli` passes: the CLI parses flags and calls RunContext. The ACP half of the
// same guarantees is proven end to end, over real JSON-RPC frames, in
// internal/surface/acp/writepath_test.go.

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// readReceiptAt reads a write window's receipt from a run directory.
func readReceiptAt(t *testing.T, runDir, rel string) Receipt {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("the receipt must be persisted on every path: %v", err)
	}
	var r Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("receipt is not valid JSON: %v", err)
	}
	return r
}

// TestCLIApply_ConcurrentInPlaceEditIsRefused is the data-loss defect itself, on the surface a
// human actually uses.
//
// The file is edited IN PLACE after the remediation copy was taken and before the commit — same
// inode, same length, only the bytes differ, which is exactly what an editor's save does. The
// staged bytes derive from the pre-edit content, so committing them silently discards the human's
// work.
//
// AGAINST THE OLD CODE THIS TEST FAILS: `handleMode` committed with `ws.Commit(rcopy)` — i.e.
// `CommitExpecting(h, nil)`, no pins — so the destination's identity still matched (an in-place
// save keeps the inode) and the concurrent edit was overwritten by the marker. The run reported
// success. Reverting `PinsFromCopy`/`CommitExpecting` alone reproduces it: the assertions on the
// error, on the receipt and on the file contents all fail together.
func TestCLIApply_ConcurrentInPlaceEditIsRefused(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)
	const concurrent = "package main\n\nfunc main() { /* a human was here */ }\n"

	// The window is entered; the commit has not begun. This is the narrowest possible staging of
	// the race, and it is deterministic (see testHookInsideWriteWindow).
	testHookInsideWriteWindow = func() {
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			t.Errorf("open for in-place edit: %v", err)
			return
		}
		if _, err := f.WriteString(concurrent); err != nil {
			t.Errorf("in-place edit: %v", err)
		}
		f.Close()
	}
	t.Cleanup(func() { testHookInsideWriteWindow = nil })

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err == nil {
		t.Fatal("a concurrent in-place edit must be REFUSED at the commit boundary, not clobbered")
	}
	if got := fault.ReasonOf(err); got != ReasonStaleDecisionSet {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonStaleDecisionSet, err)
	}
	if got := read(t, file); got != concurrent {
		t.Fatalf("the concurrent edit was overwritten:\nwant: %q\ngot:  %q", concurrent, got)
	}
	// And the refusal is RECORDED: a receipt that says the commit was entered and did not complete
	// is what distinguishes a rollback from a run that never tried.
	r := readReceiptAt(t, out.RunDir, "remediation/receipt.json")
	if r.Status != "halted" || r.Committed || !r.CommitAttempted {
		t.Fatalf("receipt = %+v, want halted with commitAttempted and no commit", r)
	}
	if len(r.Files) != 0 {
		t.Fatalf("receipt.files = %v, want empty — nothing was written", r.Files)
	}
}

// TestCLIApply_JournalIsDurableBeforeTheFirstEdit checks the ORDER, not just the existence: at the
// moment the journal event fires, the journal is on disk and the live workspace is still untouched.
//
// AGAINST THE OLD CODE THIS TEST FAILS: the full-cycle path emitted no `remediation_journaled`
// event and wrote no journal at all, so the hook never fires and the final journal assertion finds
// no file.
func TestCLIApply_JournalIsDurableBeforeTheFirstEdit(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)
	before := read(t, file)

	journaled := 0
	req := Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"}
	req.OnEvent = func(ev audit.EventLine) {
		if ev.EventType != "remediation_journaled" {
			return
		}
		journaled++
		if got := read(t, file); got != before {
			t.Errorf("the workspace was written BEFORE the journal was durable; file=%q", got)
		}
		if _, err := os.Stat(filepath.Join(m.ArtifactDir, ev.RunID, "remediation", "journal.json")); err != nil {
			t.Errorf("the journal must be on disk when this event fires: %v", err)
		}
	}
	out, err := m.Run(req)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if journaled != 1 {
		t.Fatalf("remediation_journaled fired %d times, want exactly 1", journaled)
	}
	var j Journal
	b, rerr := os.ReadFile(filepath.Join(out.RunDir, "remediation", "journal.json"))
	if rerr != nil {
		t.Fatalf("the journal must be persisted: %v", rerr)
	}
	if uerr := json.Unmarshal(b, &j); uerr != nil {
		t.Fatalf("journal is not valid JSON: %v", uerr)
	}
	if len(j.Hunks) == 0 || j.Hunks[0].File != "main.go" || j.IntentSHA256 == "" {
		t.Fatalf("journal does not describe the intended write: %+v", j)
	}
}

// TestCLIApply_UnwritableJournalHaltsWithNothingWritten makes the journal impossible to write (its
// path is occupied by a DIRECTORY — what a full or read-only disk amounts to for this purpose) and
// requires the write window to stay shut.
//
// AGAINST THE OLD CODE THIS TEST FAILS: there was no journal, so the obstruction was irrelevant and
// the run happily wrote the marker and returned success.
func TestCLIApply_UnwritableJournalHaltsWithNothingWritten(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)
	before := read(t, file)

	req := Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"}
	req.OnEvent = func(ev audit.EventLine) {
		if ev.EventType != "run_started" {
			return
		}
		if err := os.MkdirAll(filepath.Join(m.ArtifactDir, ev.RunID, "remediation", "journal.json"), 0o755); err != nil {
			t.Errorf("plant obstruction: %v", err)
		}
	}
	out, err := m.Run(req)
	if got := fault.ReasonOf(err); got != ReasonJournalUnwritable {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonJournalUnwritable, err)
	}
	if got := read(t, file); got != before {
		t.Fatalf("the workspace was written although the journal never became durable; file=%q", got)
	}
	// The receipt is the one thing that must still exist — a halt nobody can read is the failure
	// this whole path exists to prevent.
	if r := readReceiptAt(t, out.RunDir, "remediation/receipt.json"); r.Status != "halted" || r.CommitAttempted || r.Committed {
		t.Fatalf("receipt = %+v, want a halted receipt with no commit attempted", r)
	}
}

// TestCLIApply_ReceiptRecordsTheCommittedReality is the positive half: an ordinary apply leaves a
// receipt whose applied set, file list and `committed` flag describe what actually reached the
// tree.
//
// AGAINST THE OLD CODE THIS TEST FAILS: the full-cycle path wrote no receipt, so
// remediation/receipt.json does not exist.
func TestCLIApply_ReceiptRecordsTheCommittedReality(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(read(t, file), "// reviewmesh[") {
		t.Fatal("the fixture apply must actually write, or this test proves nothing")
	}
	r := readReceiptAt(t, out.RunDir, "remediation/receipt.json")
	if r.Status != "complete" || !r.Committed || !r.CommitAttempted {
		t.Fatalf("receipt = %+v, want a completed, committed receipt", r)
	}
	if len(r.Files) != 1 || r.Files[0] != "main.go" {
		t.Fatalf("receipt.files = %v, want [main.go]", r.Files)
	}
	if len(r.Applied) != 1 {
		t.Fatalf("receipt.applied = %+v, want exactly the one finding", r.Applied)
	}
	if r.IntentSHA256 == "" || len(r.Intended) == 0 {
		t.Fatalf("the receipt must be tied to the journal by intent digest: %+v", r)
	}
	// The commit-attempt marker is fsynced BEFORE the first live write, so an interrupted window
	// has a signature rather than being indistinguishable from a run that did nothing.
	if _, serr := os.Stat(filepath.Join(out.RunDir, "remediation", "commit-attempt.json")); serr != nil {
		t.Fatalf("the commit-attempt marker must exist: %v", serr)
	}
}

// TestCLIApply_UnshownAndSkippedFindingsKeepTheirStates pins the decision-state mapping across the
// delegation. The write path reports outcomes; handleMode still owns what a finding's terminal
// state becomes, and a regression here would silently mislabel findings in every surface's report.
func TestCLIApply_UnshownAndSkippedFindingsKeepTheirStates(t *testing.T) {
	m := newManager(t, fake.UnshownFile)
	ws, file := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if strings.Contains(read(t, file), "// reviewmesh[") {
		t.Fatal("a finding for an unshown file must not edit a real workspace file")
	}
	if len(out.Decisions) == 0 {
		t.Fatal("the outcome must carry the decisions")
	}
	for i, d := range out.Decisions {
		if d.State != review.StateSkipped {
			t.Errorf("decision %d state = %q, want skipped (finding %+v)", i, d.State, out.Findings[i])
		}
	}
}

// TestFullCycleApply_WorkspaceIdentityIsVerifiedBeforeTheWrite proves the reviewed root is bound by
// IDENTITY and not by pathname on the full-cycle path too: the directory the review ran against is
// swapped for a different one whose file happens to carry the same bytes.
//
// AGAINST THE OLD CODE THIS TEST FAILS: `handleMode` never captured or checked an identity, so the
// substitute tree was copied, edited and committed without complaint.
func TestFullCycleApply_WorkspaceIdentityIsVerifiedBeforeTheWrite(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)
	body := read(t, file)

	// Swap the reviewed directory for a DIFFERENT directory reached by the same path, carrying
	// byte-identical content, at the last moment before the commit.
	substitute := t.TempDir()
	if err := os.WriteFile(filepath.Join(substitute, "main.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	moved := false
	testHookInsideWriteWindow = func() { moved = true }
	t.Cleanup(func() { testHookInsideWriteWindow = nil })

	// The identity check runs before the copy, so the swap has to happen earlier than the window:
	// do it as soon as the run has started.
	req := Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"}
	req.OnEvent = func(ev audit.EventLine) {
		if ev.EventType != "remediation_started" {
			return
		}
		if err := os.RemoveAll(ws); err != nil {
			t.Errorf("remove reviewed root: %v", err)
			return
		}
		// A real directory moved into place, not a symlink to one. Creating a symlink on Windows
		// needs SeCreateSymbolicLinkPrivilege, so a symlink vector would make this test unrunnable
		// on a supported platform; a rename needs no privilege anywhere. It is also the harder
		// vector, because a symlinked root can be refused by meshcore's reparse rule — which would
		// let this pass without the identity check ever running.
		if err := os.Rename(substitute, ws); err != nil {
			t.Errorf("substitute reviewed root: %v", err)
		}
	}
	_, err := m.Run(req)
	if got := fault.ReasonOf(err); got != ReasonWorkspaceIdentityChanged {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonWorkspaceIdentityChanged, err)
	}
	if moved {
		t.Fatal("the write window must never have opened")
	}
	// The substitute now OCCUPIES ws, so that is where it is read back from: it must be untouched.
	if got, _ := os.ReadFile(filepath.Join(ws, "main.go")); string(got) != body {
		t.Fatalf("a substituted tree was written: %q", got)
	}
}

// --- the structural guarantee: there is ONE write path, and it cannot silently re-diverge ---

// TestOneGovernedWritePath is the test that makes this convergence durable rather than momentary.
//
// The defect this whole change exists to fix was not a wrong line of code; it was a SECOND write
// path that drifted from the first. So the invariant is asserted structurally, over the package's
// own source: exactly one function commits to a live workspace, and every caller that writes goes
// through it. A future edit that reintroduces a `ws.Commit` beside a surface — the exact shape of
// the original defect — fails here, in this package, rather than silently on one surface.
//
// AGAINST THE OLD CODE THIS TEST FAILS on its first assertion: review.go's handleMode contained
// `ws.Commit(rcopy)`, a second commit call site outside the governed path.
func TestOneGovernedWritePath(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	pkg, ok := pkgs["run"]
	if !ok {
		t.Fatal("package run not found")
	}

	// commitSites maps "file:function" → the commit selector it calls.
	commitSites := map[string]string{}
	// calls maps a function name → the set of functions it calls.
	calls := map[string]map[string]bool{}
	for name, f := range pkg.Files {
		file := filepath.Base(name)
		ast.Inspect(f, func(n ast.Node) bool {
			fn, isFn := n.(*ast.FuncDecl)
			if !isFn {
				return true
			}
			calls[fn.Name.Name] = map[string]bool{}
			ast.Inspect(fn.Body, func(in ast.Node) bool {
				call, isCall := in.(*ast.CallExpr)
				if !isCall {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					switch fun.Sel.Name {
					case "Commit", "CommitExpecting":
						commitSites[file+":"+fn.Name.Name] = fun.Sel.Name
					}
					calls[fn.Name.Name][fun.Sel.Name] = true
				case *ast.Ident:
					calls[fn.Name.Name][fun.Name] = true
				}
				return true
			})
			return false
		})
	}

	if len(commitSites) != 1 {
		t.Fatalf("there must be exactly ONE live-workspace commit call site in this package; found %v", commitSites)
	}
	for site, sel := range commitSites {
		if site != "writepath.go:governedWrite" {
			t.Errorf("the commit call site is %q; it must be writepath.go:governedWrite", site)
		}
		if sel != "CommitExpecting" {
			t.Errorf("the commit is %q; an unpinned Commit is exactly the defect this converged away", sel)
		}
	}
	// Both writing entry points delegate. `handleMode` is the CLI and ACP path; `Remediate` is the
	// MCP path. Neither may grow its own copy of the write window again.
	for _, entry := range []string{"handleMode", "Remediate"} {
		if !calls[entry]["governedWrite"] {
			t.Errorf("%s must delegate to governedWrite — a surface with its own write path is the defect", entry)
		}
	}
}

// TestGovernedWriteRefusesAnUnpinnedDestination pins the fail-closed direction of the pin
// contract at the unit level: a nil pin map must never mean "write everything unchecked".
func TestGovernedWriteRefusesAnUnpinnedDestination(t *testing.T) {
	m, ws := remediateFixture(t, true)
	before, _ := os.ReadFile(filepath.Join(ws, "sample.go"))
	req := baseRequest(t, ws, review.ModeApply)
	req.BaseHashes = nil // no pins recorded at all

	out, err := m.Remediate(context.Background(), req)
	if got := fault.ReasonOf(err); got != ReasonEditTargetUnreviewed {
		t.Fatalf("reasonCode = %q, want %q (err=%v)", got, ReasonEditTargetUnreviewed, err)
	}
	if r := readReceipt(t, out.RunDir); r.Committed || r.CommitAttempted {
		t.Fatalf("receipt = %+v, want nothing committed", r)
	}
	if after, _ := os.ReadFile(filepath.Join(ws, "sample.go")); string(after) != string(before) {
		t.Fatal("a destination with no recorded pin was written")
	}
}

// TestUniqueFindingIDs_LeavesUniqueIdsAlone is the narrow contract the write path's ID binding
// depends on: ids only change when they MUST, because a finding's id is user-visible (the
// remediation marker quotes it).
func TestUniqueFindingIDs_LeavesUniqueIdsAlone(t *testing.T) {
	findings := []review.Finding{{ID: "F-001"}, {ID: "F-002"}}
	decisions := []review.Decision{{FindingID: "F-001"}, {FindingID: "F-002"}}
	uniqueFindingIDs(findings, decisions)
	if findings[0].ID != "F-001" || findings[1].ID != "F-002" {
		t.Fatalf("unique ids must be untouched, got %q/%q", findings[0].ID, findings[1].ID)
	}

	dup := []review.Finding{{ID: "F-001"}, {ID: "F-001"}, {ID: ""}}
	dupD := []review.Decision{{FindingID: "F-001"}, {FindingID: "F-001"}, {}}
	uniqueFindingIDs(dup, dupD)
	seen := map[string]bool{}
	for i, f := range dup {
		if f.ID == "" {
			t.Fatalf("finding %d still has no id", i)
		}
		if seen[f.ID] {
			t.Fatalf("finding %d repeats id %q", i, f.ID)
		}
		seen[f.ID] = true
		if dupD[i].FindingID != f.ID {
			t.Fatalf("decision %d is bound to %q but its finding is %q", i, dupD[i].FindingID, f.ID)
		}
	}
	if dup[0].ID != "F-001" {
		t.Fatalf("the FIRST holder of an id keeps it, got %q", dup[0].ID)
	}
}
