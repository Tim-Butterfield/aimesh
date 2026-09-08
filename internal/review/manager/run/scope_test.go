package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// targetFake is a deterministic reviewer that reports ONE finding against a caller-chosen
// path. It exists so a test can drive the write layer at an arbitrary target — including
// the protected ones a well-behaved model would never name — without a real CLI.
type targetFake struct{ target string }

func (f targetFake) Name() string              { return "fake" }
func (f targetFake) Available() (bool, string) { return true, "target fake (test)" }
func (f targetFake) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}
func (f targetFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"request_changes","findings":[{"id":"F-1","kind":"fail","severity":"high","title":"targeted","file":%q,"location":"1","source":"reviewer"}]}`,
		string(c.Role), string(c.Phase), f.target)
	return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg),
		Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
}

// mixedFake is a deterministic reviewer that reports one finding per caller-chosen path. It
// exists for the partial-refusal contract: a refusal only matters if the OTHER findings in the
// same set are still applied, and a single-finding fake cannot demonstrate that.
type mixedFake struct{ targets []string }

func (f mixedFake) Name() string              { return "fake" }
func (f mixedFake) Available() (bool, string) { return true, "mixed target fake (test)" }
func (f mixedFake) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}
func (f mixedFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	findings := make([]string, 0, len(f.targets))
	for i, target := range f.targets {
		// Distinct kind+location per finding so the host's fingerprint dedup keeps both.
		findings = append(findings, fmt.Sprintf(
			`{"id":"F-%d","kind":"fail","severity":"high","title":"targeted %d","file":%q,"location":"%d","source":"reviewer"}`,
			i+1, i+1, target, (i+1)*10))
	}
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"request_changes","findings":[%s]}`,
		string(c.Role), string(c.Phase), strings.Join(findings, ","))
	return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg),
		Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
}

// targetManager wires a manager whose reviewer reports a finding against `target`.
func targetManager(t *testing.T, target string) *Manager {
	t.Helper()
	return managerWith(t, targetFake{target: target})
}

// mixedTargetManager wires a manager whose reviewer reports one finding per target, in order.
func mixedTargetManager(t *testing.T, targets ...string) *Manager {
	t.Helper()
	return managerWith(t, mixedFake{targets: targets})
}

func managerWith(t *testing.T, a model.Adapter) *Manager {
	t.Helper()
	t.Setenv(fake.EnvVar, "1")
	cfg := config.Default()
	cfg.DefaultProfile = config.FakeProfile
	return &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": a},
		ArtifactDir: t.TempDir(),
		TempBase:    t.TempDir(),
	}
}

// scopeWorkspace builds a repo-like workspace carrying the protected files a real tree has.
func scopeWorkspace(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	ws := filepath.Join(parent, "ws")
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(ws, "main.go"), "package main\n\nfunc main() {}\n")
	mustWrite(t, filepath.Join(ws, ".env"), "SECRET=super-secret-value\n")
	mustWrite(t, filepath.Join(ws, ".git", "config"), "ORIGINAL-GIT-CONFIG\n")
	mustWrite(t, filepath.Join(parent, "outside.go"), "package outside\n")
	return ws
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestApply_OutOfRootTarget_Halts is what remains of the original blanket halt test after D8-C
// split it in two. An ESCAPE — a target that resolves outside the workspace the caller consented
// to — is still a halt with the containment exit code and a halt record on disk. It is not a
// policy refusal: the denylist never got to speak, the write was aimed somewhere nobody
// authorized, and a proposal that reached outside its authorization is not one whose other hunks
// can be assumed sound.
//
// (The protected-path half of the original table moved to
// TestApply_ProtectedTarget_RecordedRefusal below — a deliberate, reviewed reversal, argued
// there.)
func TestApply_OutOfRootTarget_Halts(t *testing.T) {
	const wantReason = scope.ReasonOutsideRoot
	ws := scopeWorkspace(t)
	m := targetManager(t, filepath.ToSlash(filepath.Join("..", "outside.go")))

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err == nil {
		t.Fatal("an apply targeting a path outside the workspace must halt")
	}
	if out.Status != "halted" {
		t.Errorf("status = %q, want halted", out.Status)
	}
	if got := fault.CodeOf(err); got != fault.Containment {
		t.Errorf("exit code = %d, want %d (containment; no new exit code is introduced)", got, fault.Containment)
	}
	if got := fault.ReasonOf(err); got != string(wantReason) {
		t.Errorf("reasonCode = %q, want %q", got, wantReason)
	}
	if out.Halt == nil || string(*out.Halt) != ScopeHaltClass {
		t.Errorf("halt class = %v, want %s", out.Halt, ScopeHaltClass)
	}
	if out.HaltReason != string(wantReason) {
		t.Errorf("outcome.HaltReason = %q, want %q", out.HaltReason, wantReason)
	}
	if len(out.Refusals) != 0 {
		t.Errorf("an escape must NOT be recorded as a refusal; got %+v", out.Refusals)
	}
	rec := readHaltRecord(t, out.RunDir)
	if rec["reasonCode"] != string(wantReason) {
		t.Errorf("halt-record reasonCode = %v, want %q", rec["reasonCode"], wantReason)
	}
	if rec["haltClass"] != ScopeHaltClass {
		t.Errorf("halt-record haltClass = %v, want %s", rec["haltClass"], ScopeHaltClass)
	}
}

// TestApply_ProtectedTarget_RecordedRefusal is D8-C: a finding aimed at a protected path is a
// RECORDED REFUSAL, not a halt of the whole run.
//
// Halting the run on one protected target makes every OTHER finding in it pay: a caller paid for a
// full panel, one out-of-bounds target discards all of it, and — the decisive part — the halt
// happens at authorization time, so a caller who does not already know which finding is protected
// has to burn a run to find out. The workaround that invites is hand-editing the patch, which
// abandons containment, rollback and the receipt at once.
//
// It is not a silent skip either: a silent skip would let a run that tried to rewrite `.env` finish
// reporting success. The finding is marked not-applied with its reason, named in the receipt,
// counted, rendered first in the human text, and carried in the run's coarse not-clean signal on
// every surface. A recorded, surfaced refusal answers both objections.
func TestApply_ProtectedTarget_RecordedRefusal(t *testing.T) {
	cases := []struct {
		name      string
		target    string
		untouched string // a file whose content must be unchanged afterwards
	}{
		{"env file", ".env", ".env"},
		{"git config", filepath.Join(".git", "config"), filepath.Join(".git", "config")},
		{"agent client config", ".mcp.json", ""},
		{"local state dir", filepath.Join(".aimesh", "state.json"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := scopeWorkspace(t)
			target := filepath.ToSlash(tc.target)
			m := targetManager(t, target)

			out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
			if err != nil {
				t.Fatalf("a protected-path target must be REFUSED, not halt the run: %v", err)
			}
			if out.Status != "stable" {
				t.Errorf("status = %q, want stable — the run completed", out.Status)
			}
			// The refusal is a first-class fact on the outcome, keyed on the HOST-COMPUTED
			// fingerprint (never the model-authored finding id).
			if len(out.Refusals) != 1 {
				t.Fatalf("refusals = %+v, want exactly one", out.Refusals)
			}
			ref := out.Refusals[0]
			if ref.File != target {
				t.Errorf("refusal file = %q, want %q", ref.File, target)
			}
			if ref.Reason != review.ApplyRefusalProtectedPath {
				t.Errorf("refusal reason = %q, want %q", ref.Reason, review.ApplyRefusalProtectedPath)
			}
			if !strings.HasPrefix(ref.Fingerprint, "sha1:") {
				t.Errorf("refusal fingerprint = %q, want the host-computed fingerprint", ref.Fingerprint)
			}
			// The decision carries the same refusal in the same two fields the write-path rule's
			// other refusals use, so every existing projection renders it without knowing it
			// exists.
			found := false
			for i := range out.Decisions {
				if out.Decisions[i].ApplyRefusalReason == review.ApplyRefusalProtectedPath {
					found = true
					if out.Decisions[i].Applyable == nil || *out.Decisions[i].Applyable {
						t.Errorf("decision %d: applyable = %v, want a non-nil false", i, out.Decisions[i].Applyable)
					}
				}
			}
			if !found {
				t.Error("no decision carries applyRefusalReason=protected_path")
			}
			// NOT ONE BYTE reached the protected path. This is the part that is unchanged and
			// non-negotiable: the denylist is not overridable, and the refusal is about what
			// happens to the OTHER findings.
			if tc.untouched != "" {
				got := readFileT(t, filepath.Join(ws, tc.untouched))
				if strings.Contains(got, "reviewmesh[") {
					t.Errorf("%s was modified: %q", tc.untouched, got)
				}
			}
			// The receipt says the write completed and names the refused finding.
			r := readReceipt(t, out.RunDir)
			if r.Status != "complete" || !r.Committed {
				t.Errorf("receipt status=%q committed=%v, want complete/true — the write DID commit", r.Status, r.Committed)
			}
			na := 0
			for _, e := range r.NotApplied {
				if e.Reason == review.ApplyRefusalProtectedPath {
					na++
					if e.File != target {
						t.Errorf("receipt notApplied file = %q, want %q", e.File, target)
					}
				}
			}
			if na != 1 {
				t.Errorf("receipt.notApplied carries %d protected_path row(s), want 1: %+v", na, r.NotApplied)
			}
			// NO HALT RECORD. There was no halt — the run completed — and a halt record would
			// make an audit reader believe otherwise.
			if p := findFile(out.RunDir, "halt-record.json"); p != "" {
				t.Errorf("a recorded refusal must not write a halt record; found %s", p)
			}
		})
	}
}

// TestApply_MixedSet_AppliesTheRestAndRefusesOne is the assertion §13.2 calls owed and the one
// the halt could never satisfy: a decision set mixing a protected-path target with a valid one
// APPLIES the valid one. Everything else in this file could pass with a run that refused
// everything; this is the test that says the paid work survives.
//
// AGAINST THE OLD CODE IT FAILS AT THE FIRST ASSERTION (the run halted, so nothing was applied).
func TestApply_MixedSet_AppliesTheRestAndRefusesOne(t *testing.T) {
	ws := scopeWorkspace(t)
	m := mixedTargetManager(t, "main.go", ".env")

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("a mixed set must complete: %v", err)
	}
	if out.Applied != 1 {
		t.Errorf("outcome.Applied = %d, want 1 — the valid finding must still be written", out.Applied)
	}
	if len(out.Refusals) != 1 || out.Refusals[0].File != ".env" {
		t.Fatalf("refusals = %+v, want exactly the .env target", out.Refusals)
	}
	if got := review.ApplyOutcome(out.Applied, len(out.Refusals)); got != review.OutcomePartialRefusal {
		t.Errorf("outcome = %q, want %q", got, review.OutcomePartialRefusal)
	}
	if !strings.Contains(readFileT(t, filepath.Join(ws, "main.go")), "reviewmesh[") {
		t.Error("the VALID finding was not applied — one out-of-bounds target must not discard the rest of a paid run")
	}
	if strings.Contains(readFileT(t, filepath.Join(ws, ".env")), "reviewmesh[") {
		t.Error(".env was modified; the denylist is not overridable")
	}
}

// findFile returns the first path under dir with the given base name, or "".
func findFile(dir, name string) string {
	var found string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name && found == "" {
			found = p
		}
		return nil
	})
	return found
}

// TestReport_ProtectedTarget_NoHalt pins the blast radius: report mode writes nothing, so a
// finding merely NAMING a protected path is reported, not halted. Only the write layer
// enforces — the read path must not become newly fragile.
func TestReport_ProtectedTarget_NoHalt(t *testing.T) {
	ws := scopeWorkspace(t)
	m := targetManager(t, ".env")
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("report mode must not halt on a finding that merely names a protected path: %v", err)
	}
	if out.Status != "stable" {
		t.Errorf("status = %q, want stable", out.Status)
	}
}

// TestApply_NormalTree_Unchanged pins "no behavior change for normal reviews of normal
// trees": an ordinary target still applies.
func TestApply_NormalTree_Unchanged(t *testing.T) {
	ws := scopeWorkspace(t)
	m := targetManager(t, "main.go")
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("apply on a normal target: %v", err)
	}
	if out.Status != "stable" {
		t.Errorf("status = %q, want stable", out.Status)
	}
	if !strings.Contains(readFileT(t, filepath.Join(ws, "main.go")), "reviewmesh[") {
		t.Error("the ordinary target was not edited — confinement must not block normal work")
	}
}

// TestSecretsNeverReachThePrompt pins the read denylist end-to-end: the reviewer prompt
// captured in the run directory must not contain `.env` content.
func TestSecretsNeverReachThePrompt(t *testing.T) {
	ws := scopeWorkspace(t)
	m := targetManager(t, "main.go")
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	var leaked []string
	_ = filepath.WalkDir(out.RunDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr == nil && strings.Contains(string(b), "super-secret-value") {
			leaked = append(leaked, p)
		}
		return nil
	})
	if len(leaked) > 0 {
		t.Errorf("secret content reached run artifacts: %v", leaked)
	}
}

// TestHaltRecord_ReasonCodeIsMachineShaped pins the halt record's machine shape: reasonCode is a stable code,
// never the fault's human sentence.
func TestHaltRecord_ReasonCodeIsMachineShaped(t *testing.T) {
	m := newManager(t, fake.ExitError) // a non-zero adapter exit → Class A halt
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err == nil {
		t.Fatal("a non-zero adapter exit must halt")
	}
	if got := fault.ReasonOf(err); got != "adapter_exited_nonzero" {
		t.Errorf("reasonCode = %q, want adapter_exited_nonzero", got)
	}
	rec := readHaltRecord(t, out.RunDir)
	code, _ := rec["reasonCode"].(string)
	if code == "" {
		t.Fatal("halt-record has no reasonCode")
	}
	if strings.ContainsAny(code, " .\"") || code != strings.ToLower(code) {
		t.Errorf("reasonCode %q is not code-shaped (it must not be a sentence)", code)
	}
	// The human message is still available, in its own field.
	if detail, _ := rec["detail"].(string); !strings.Contains(detail, "exited") {
		t.Errorf("halt-record detail = %q, want the human message", detail)
	}
	// The classified actionability signal is persisted: the ExitError fixture's stderr
	// carries a `codex login` hint.
	if sig, _ := rec["signal"].(string); sig != "login_required" {
		t.Errorf("halt-record signal = %q, want login_required", sig)
	}
	if out.HaltSignal != "login_required" {
		t.Errorf("outcome.HaltSignal = %q, want login_required", out.HaltSignal)
	}
}

// TestRunState_HaltSummaryCarriesCodes pins that the run-state halt block is machine-usable
// too — it carries the codes, not only the halt class letter.
func TestRunState_HaltSummaryCarriesCodes(t *testing.T) {
	m := newManager(t, fake.ExitError)
	ws, _ := makeWorkspace(t)
	out, _ := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	b, err := os.ReadFile(filepath.Join(out.RunDir, "run-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	halt, ok := st["halt"].(map[string]any)
	if !ok {
		t.Fatal("run-state has no halt block")
	}
	if halt["haltClass"] != "A" {
		t.Errorf("haltClass = %v, want A", halt["haltClass"])
	}
	if halt["reasonCode"] != "adapter_exited_nonzero" {
		t.Errorf("reasonCode = %v, want adapter_exited_nonzero", halt["reasonCode"])
	}
}

// readHaltRecord finds the run's halt record (top-level or per-call) and decodes it.
func readHaltRecord(t *testing.T, runDir string) map[string]any {
	t.Helper()
	var found string
	_ = filepath.WalkDir(runDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "halt-record.json" && found == "" {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no halt-record.json under %s", runDir)
	}
	b, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
