package run

// THE RUN HANDLE IS EVIDENCE, NOT AUTHORITY.
//
// `fromRun` arrives from a peer process. Treating it as "the run directory to apply" would let any
// local caller point a governed write at an arbitrary directory — and, just as bad, would turn the
// resolver into an existence oracle over every absolute path a peer cared to name. These tests hold
// the resolution to both halves: it accepts only a run this agent produced, and it answers every
// failure alike.
//
// AGAINST THE OLD CODE they do not compile: there was no on-disk decision set and no reader for one.
// The behavioural claim they encode — that a handle is verified rather than trusted — had no
// implementation to be true of.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// decisionSetFixture records a report run's decision set the way RunContext does, and returns the
// manager plus that run's directory.
func decisionSetFixture(t *testing.T) (*Manager, string, string) {
	t.Helper()
	art, ws := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ArtifactDir: art}
	run, err := audit.NewRun(art, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	out := review.RunOutcome{
		Findings:   []review.Finding{{ID: "f1", Kind: "bug", File: "a.go", Title: "boom"}},
		Decisions:  []review.Decision{{FindingID: "f1", Valid: true, State: review.StateReportedValid}},
		ShownFiles: []string{"a.go"},
	}
	m.recordDecisionSet(run, Request{Workspace: ws}, review.RunPlan{Mode: review.ModeReport, Surface: "acp"}, out)
	return m, run.Dir, ws
}

func TestReadDecisionSet_AcceptsARunThisAgentProduced(t *testing.T) {
	m, runDir, ws := decisionSetFixture(t)
	set, canon, err := m.ReadDecisionSet(runDir)
	if err != nil {
		t.Fatalf("a run this agent produced must resolve: %v", err)
	}
	if set.Accepted != 1 || len(set.Findings) != 1 {
		t.Fatalf("set = %+v, want one accepted finding", set)
	}
	if set.RunID != filepath.Base(runDir) {
		t.Errorf("set.RunID = %q, want the run directory's own name", set.RunID)
	}
	// The pins are the SOURCE run's, captured as reviewed — not re-derived at apply time, which
	// would make a changed file look unchanged.
	if set.BaseHashes["a.go"] == "" || set.BaseHashes["a.go"] == BaseHashAbsent {
		t.Errorf("baseHashes = %v, want a real digest for the accepted finding's target", set.BaseHashes)
	}
	if !set.NamesWorkspace(ws) {
		t.Errorf("the set must name the workspace it was adjudicated against (%q vs %q)", ws, set.WorkspaceCanonical)
	}
	if canon == "" {
		t.Error("the canonical run directory must be returned: it is what was actually read")
	}
}

// TestReadDecisionSet_RefusesAnythingElse is the fail-closed half. Each case fails for a DIFFERENT
// reason and every one answers with the SAME code — a resolver that distinguished them would tell a
// peer which absolute paths exist.
func TestReadDecisionSet_RefusesAnythingElse(t *testing.T) {
	m, runDir, _ := decisionSetFixture(t)
	art := m.ArtifactDir

	// A directory outside the artifact dir, a nested path inside it, a traversal, a bare name, and
	// a run of ours that recorded nothing.
	outside := t.TempDir()
	nested := filepath.Join(runDir, "calls")
	empty, err := audit.NewRun(art, "", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// A SYMLINK that is SPELLED like a run of ours and is not one. This is the case the lexical
	// check alone would admit, which is why the canonical check exists.
	link := filepath.Join(art, "20990101T000000-0000")
	if lerr := os.Symlink(outside, link); lerr != nil {
		t.Skipf("symlinks unavailable: %v", lerr)
	}

	for _, handle := range []string{
		"", "   ", outside, nested, filepath.Join(runDir, ".."), filepath.Join(art, ".."),
		art, filepath.Base(runDir), "/nonexistent/run", empty.Dir, link,
	} {
		set, _, rerr := m.ReadDecisionSet(handle)
		if rerr == nil {
			t.Errorf("handle %q must be refused; got set %+v", handle, set)
			continue
		}
		if fault.ReasonOf(rerr) != ReasonRunHandleUnknown {
			t.Errorf("handle %q: reasonCode = %q, want %q — one code for every failure, or the refusal is an oracle",
				handle, fault.ReasonOf(rerr), ReasonRunHandleUnknown)
		}
	}
}

// TestReadDecisionSet_RefusesATamperedRecord. The artifact is read strictly: a record whose runId is
// not the directory's own name was copied in from somewhere else, and an unknown field means a build
// recorded a governance input this one would silently drop.
func TestReadDecisionSet_RefusesATamperedRecord(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"foreign runId":  func(raw map[string]any) { raw["runId"] = "some-other-run" },
		"schema version": func(raw map[string]any) { raw["schemaVersion"] = 99 },
		"unknown field":  func(raw map[string]any) { raw["applyEverything"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			m, runDir, _ := decisionSetFixture(t)
			p := filepath.Join(runDir, filepath.FromSlash(DecisionSetArtifact))
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if jerr := json.Unmarshal(b, &raw); jerr != nil {
				t.Fatal(jerr)
			}
			mutate(raw)
			out, _ := json.Marshal(raw)
			if werr := os.WriteFile(p, out, 0o644); werr != nil {
				t.Fatal(werr)
			}
			if _, _, rerr := m.ReadDecisionSet(runDir); rerr == nil {
				t.Fatal("a tampered or unrecognized record must not drive a governed write")
			}
		})
	}
}

// TestRecordDecisionSet_OnlyForReportRuns. A patch/apply run has already applied its own accepted
// set; offering it as a from-run source would be a second application of decisions nobody re-read.
func TestRecordDecisionSet_OnlyForReportRuns(t *testing.T) {
	for _, mode := range []review.Mode{review.ModePatch, review.ModeApply} {
		art := t.TempDir()
		m := &Manager{ArtifactDir: art}
		run, err := audit.NewRun(art, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		m.recordDecisionSet(run, Request{Workspace: t.TempDir()}, review.RunPlan{Mode: mode}, review.RunOutcome{})
		if _, serr := os.Stat(filepath.Join(run.Dir, filepath.FromSlash(DecisionSetArtifact))); serr == nil {
			t.Errorf("mode %q recorded a decision set; only a report run may be a from-run source", mode)
		}
	}
}

// TestBindWorkspace_RefusesATreeThatIsNoLongerTheOneReviewed. The stored canonical path plus the
// durable device+inode key are what carry the identity binding across the report→apply gap, because
// an fs.FileInfo cannot be serialized. This is the check a pathname cannot make.
func TestBindWorkspace_RefusesATreeThatIsNoLongerTheOneReviewed(t *testing.T) {
	m, runDir, ws := decisionSetFixture(t)
	set, _, err := m.ReadDecisionSet(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, berr := set.BindWorkspace(); berr != nil {
		t.Fatalf("the unchanged tree must still bind: %v", berr)
	}

	// The same path, a different directory. On a platform with a durable key this is caught by the
	// key; everywhere it is caught by the pins, which is what the empty-key case below relies on.
	if runtime.GOOS == "windows" {
		t.Skip("the durable identity key is not expressible on this platform; see rootidentity_windows.go")
	}
	replacement := t.TempDir()
	if rerr := os.RemoveAll(ws); rerr != nil {
		t.Fatal(rerr)
	}
	if rerr := os.Symlink(replacement, ws); rerr != nil {
		t.Fatal(rerr)
	}
	if _, berr := set.BindWorkspace(); berr == nil {
		t.Fatal("a path re-pointed at a different tree must not bind: a path is not a repository")
	} else if fault.ReasonOf(berr) != ReasonWorkspaceIdentityChanged {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(berr), ReasonWorkspaceIdentityChanged)
	}
}

// TestBindWorkspace_RefusesAnUnboundSet. A set whose run could not capture an identity has nothing to
// verify against — and an empty canonical path would resolve to the PROCESS WORKING DIRECTORY, which
// is a silent substitution of a tree nobody reviewed. It is refused rather than resolved.
func TestBindWorkspace_RefusesAnUnboundSet(t *testing.T) {
	set := &StoredDecisionSet{RunID: "r", Accepted: 1}
	if _, err := set.BindWorkspace(); err == nil {
		t.Fatal("an unbound set must not bind to anything")
	} else if fault.ReasonOf(err) != ReasonWorkspaceUnbound {
		t.Errorf("reasonCode = %q, want %q", fault.ReasonOf(err), ReasonWorkspaceUnbound)
	}
}

// TestBindWorkspace_NoDurableKeyStillBinds is the Windows branch, exercised on any platform: a set
// captured where `fs.FileInfo.Sys()` carries no file index records no key, and the binding then
// rests on the canonical path plus the content pins. It must still BIND rather than fail closed on
// a platform difference — the alternative is an agent that can never apply its own decisions there.
func TestBindWorkspace_NoDurableKeyStillBinds(t *testing.T) {
	m, runDir, _ := decisionSetFixture(t)
	set, _, err := m.ReadDecisionSet(runDir)
	if err != nil {
		t.Fatal(err)
	}
	set.WorkspaceIdentityKey = ""
	if _, berr := set.BindWorkspace(); berr != nil {
		t.Fatalf("a set with no durable key must still bind by canonical path: %v", berr)
	}
}

// TestRunHandle_IsASpellingNotABypass. A surface that hands out an OPAQUE RUN ID and no path (MCP)
// cannot form a handle itself, so RunHandle spells one for it. What it must NOT do is extend any
// trust: the joined path faces the same four checks, so an id that is really a traversal, an
// absolute path or a nested path is refused exactly as the same string handed in as a handle is.
//
// AGAINST THE OLD CODE this does not compile: there was no RunHandle, and MCP's `fromRun` therefore
// had no way to name a run directory at all.
func TestRunHandle_IsASpellingNotABypass(t *testing.T) {
	m, runDir, _ := decisionSetFixture(t)
	id := filepath.Base(runDir)

	if got := m.RunHandle(id); got != runDir {
		t.Fatalf("RunHandle(%q) = %q, want the run's own directory %q", id, got, runDir)
	}
	set, canon, err := m.ReadDecisionSet(m.RunHandle(id))
	if err != nil {
		t.Fatalf("a run id this agent produced must resolve through its spelled handle: %v", err)
	}
	if set.RunID != id || canon == "" {
		t.Errorf("set = %+v canon = %q, want the run named by %q", set, canon, id)
	}

	// An empty artifact directory has no runs to name, and neither does an empty id.
	if got := (&Manager{}).RunHandle(id); got != "" {
		t.Errorf("RunHandle with no artifact directory = %q, want \"\"", got)
	}
	if got := m.RunHandle("   "); got != "" {
		t.Errorf("RunHandle(blank) = %q, want \"\"", got)
	}

	// And every other spelling is refused, with the one code.
	for _, bad := range []string{
		".", "..", "../" + filepath.Base(m.ArtifactDir), "/etc", id + "/calls", "nope",
	} {
		_, _, rerr := m.ReadDecisionSet(m.RunHandle(bad))
		if rerr == nil {
			t.Errorf("run id %q must be refused", bad)
			continue
		}
		if fault.ReasonOf(rerr) != ReasonRunHandleUnknown {
			t.Errorf("run id %q: reasonCode = %q, want %q", bad, fault.ReasonOf(rerr), ReasonRunHandleUnknown)
		}
	}
}

// TestRunID_NamesTheRunDirectory. A surface that must hand its caller a durable handle BEFORE the run
// exists supplies the run id, and the run's directory is named for it — which is the whole reason
// MCP's `review_remediate {fromRun}` can read a set it recorded: the only handle a client holds is
// the directory's own name.
//
// AGAINST THE OLD CODE this fails: the id was generated inside RunContext from the start time, so a
// caller-supplied one was ignored and the two names had nothing to do with each other.
func TestRunID_NamesTheRunDirectory(t *testing.T) {
	m := syntheticAdjManager(t, fake.Empty)
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{
		Workspace: ws, Mode: review.ModeReport, Surface: "mcp", Profile: "synth", RunID: "run-deadbeef",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.RunID != "run-deadbeef" || filepath.Base(out.RunDir) != "run-deadbeef" {
		t.Fatalf("runId = %q, runDir = %q, want both to be the supplied id", out.RunID, out.RunDir)
	}
	// And the set that run recorded is readable BY THAT NAME, which is the property the whole
	// arrangement exists for.
	if _, _, rerr := m.ReadDecisionSet(m.RunHandle("run-deadbeef")); rerr != nil {
		t.Fatalf("the run must be resolvable from the id its caller was handed: %v", rerr)
	}
}

// TestValidateRunID_RefusesAnythingThatIsNotASingleName. The field becomes a filesystem name under
// the artifact directory. It is server-generated today, so this cannot fire for any client — which is
// exactly why it is asserted rather than assumed: a guard that is only true by the current caller's
// good behaviour is one caller away from being false.
//
// AGAINST THE OLD CODE this does not compile: there was no such field and no such check.
func TestValidateRunID_RefusesAnythingThatIsNotASingleName(t *testing.T) {
	for _, bad := range []string{".", "..", "a/b", "/abs", `a\b`, "../escape", "x/"} {
		err := validateRunID(bad)
		if err == nil {
			t.Errorf("run id %q must be refused: it NAMES a directory under the artifact dir", bad)
			continue
		}
		if fault.ReasonOf(err) != ReasonInvalidRunID {
			t.Errorf("run id %q: reasonCode = %q, want %q", bad, fault.ReasonOf(err), ReasonInvalidRunID)
		}
	}
	// "" is how every other surface asks for a generated one, and a plain name is the whole point.
	for _, ok := range []string{"", "run-9f3c1a", "20260101T000000-0001"} {
		if err := validateRunID(ok); err != nil {
			t.Errorf("run id %q must be accepted: %v", ok, err)
		}
	}
}

// TestRemediable_NamesWhyASetCannotBeApplied. Both refusals reuse the MCP surface's codes for the
// same facts, so a host that learned one vocabulary does not need a second.
func TestRemediable_NamesWhyASetCannotBeApplied(t *testing.T) {
	if err := (&StoredDecisionSet{RunID: "r", Inline: true, Accepted: 1}).Remediable(); fault.ReasonOf(err) != ReasonInlineWorkspaceNotRemediable {
		t.Errorf("inline: reasonCode = %q, want %q", fault.ReasonOf(err), ReasonInlineWorkspaceNotRemediable)
	}
	if err := (&StoredDecisionSet{RunID: "r"}).Remediable(); fault.ReasonOf(err) != ReasonNoAcceptedFindings {
		t.Errorf("empty: reasonCode = %q, want %q", fault.ReasonOf(err), ReasonNoAcceptedFindings)
	}
	if err := (&StoredDecisionSet{RunID: "r", Accepted: 1}).Remediable(); err != nil {
		t.Errorf("a set with an accepted finding must be remediable, got %v", err)
	}
}
