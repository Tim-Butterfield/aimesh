package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// --- harness -----------------------------------------------------------------------------

// authorityProbe is a deterministic adapter that RECORDS the prompt it was given for every
// role/phase and answers each one with schema-valid output. It is what lets these tests
// assert what each judging phase actually SAW, rather than trusting that the wiring is right.
type authorityProbe struct {
	mu      sync.Mutex
	prompts map[string][]string // "<role>/<phase>" → prompts, in call order
	target  string              // the file the reviewer lanes report their finding against
}

func newProbe(target string) *authorityProbe {
	return &authorityProbe{prompts: map[string][]string{}, target: target}
}

func (p *authorityProbe) Name() string              { return "probe" }
func (p *authorityProbe) Available() (bool, string) { return true, "authority probe (test)" }
func (p *authorityProbe) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}
func (p *authorityProbe) key(role, phase string) string          { return role + "/" + phase }
func (p *authorityProbe) promptsFor(role, phase string) []string { return p.get(p.key(role, phase)) }
func (p *authorityProbe) get(k string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts[k]...)
}

// findingIDRe matches the `"id": "fN"` entries of the findings array the adjudication prompt
// embeds (the OUTPUT CONTRACT's placeholder ids are upper-case F, so they never match).
var findingIDRe = regexp.MustCompile(`"id":\s*"(f\d+)"`)

func (p *authorityProbe) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	p.mu.Lock()
	p.prompts[p.key(c.Role, c.Phase)] = append(p.prompts[p.key(c.Role, c.Phase)], c.Prompt)
	p.mu.Unlock()

	res := func(js string) (model.Result, error) {
		return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg),
			Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
	}
	switch c.Phase {
	case string(review.PhaseAdjudicate):
		var adjs []string
		for _, m := range findingIDRe.FindAllStringSubmatch(c.Prompt, -1) {
			// decisionState `applied` is deliberately ACTIONABLE: these tests need the
			// write-path rule to be the thing that stops a write, not a non-apply state.
			adjs = append(adjs, fmt.Sprintf(
				`{"findingId":%q,"validity":"valid","decisionState":"applied","severityAdjusted":"high","reasoning":"probe"}`, m[1]))
		}
		return res(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[` +
			strings.Join(adjs, ",") + `]}`)
	case string(review.PhaseRemediate):
		// Decline, so the deterministic review-marker fallback produces the edit — the test
		// then observes whether an edit reached the live tree at all.
		return res(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","noEdit":true,"reason":"probe declines"}`)
	default:
		return res(fmt.Sprintf(
			`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"request_changes","findings":[{"id":"F-1","title":"deviates from the stated intent","kind":"fail","severity":"high","file":%q,"location":"1","source":"reviewer"}]}`,
			c.Role, c.Phase, p.target))
	}
}

// probeManager wires a Manager whose named lanes all run the probe adapter. A NON-"fake"
// adapter key is essential: a `fake` host lane adjudicates deterministically and never makes
// the model call whose prompt these tests inspect.
func probeManager(t *testing.T, p *authorityProbe, roles ...string) *Manager {
	t.Helper()
	cfg := config.Default()
	lanes := map[string]config.Lane{}
	for _, r := range roles {
		exec := "adapter"
		if r == string(review.RoleAuthorRemediator) {
			exec = "host"
		}
		lanes[r] = config.Lane{Execution: exec, Adapter: "probe", Model: "probe-model"}
	}
	cfg.Profiles["authority-probe"] = config.Profile{
		Description: "test-only probe profile", AdapterPreference: []string{"probe"}, Lanes: lanes,
	}
	cfg.DefaultProfile = "authority-probe"
	cfg.Adapters["probe"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.ModelCatalog["probe-model"] = config.CatalogEntry{
		Provider: "probe", CanonicalModel: "probe-1", DisplayName: "Probe",
		Adapters: map[string]config.AdapterModel{"probe": {ModelArg: "probe-1"}},
	}
	return &Manager{Cfg: cfg, Adapters: map[string]model.Adapter{"probe": p},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir()}
}

// authorityWorkspace builds a workspace with a reviewable file and an in-repo design doc —
// the common shape, where the authority document lives INSIDE the tree under review.
func authorityWorkspace(t *testing.T, spec string) (ws, specPath string) {
	t.Helper()
	ws = t.TempDir()
	mustWrite(t, filepath.Join(ws, "main.go"), "package main\n\nfunc main() {}\n")
	specPath = filepath.Join(ws, "docs", "spec.md")
	mustWrite(t, specPath, spec)
	return ws, specPath
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func readManifest(t *testing.T, runDir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(runDir, "authority", "inclusion-manifest.json"))
	if err != nil {
		t.Fatalf("no authority/inclusion-manifest.json in the run record: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// --- tests -------------------------------------------------------------------------------

// A path document with a matching hash pin is embedded, and the run record carries an
// inclusion manifest describing exactly what reached the prompt.
func TestAuthority_PathDocEmbedded_ManifestRecorded(t *testing.T) {
	spec := "# Spec\n\nAUTHORITY-MARKER: the system MUST fail closed.\n"
	ws, specPath := authorityWorkspace(t, spec)
	p := newProbe("main.go")
	m := probeManager(t, p, "reviewer", "author_remediator")

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli",
		Authority: []review.AuthorityDoc{{Name: "spec", Path: specPath,
			MediaType: "text/markdown", ExpectedHash: "sha256:" + sha256Hex(spec)}}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	prompts := p.promptsFor(string(review.RoleReviewer), string(review.PhaseIterate))
	if len(prompts) == 0 {
		t.Fatal("the reviewer lane never ran")
	}
	if !strings.Contains(prompts[0], "AUTHORITY-MARKER") || !strings.Contains(prompts[0], authority.Header) {
		t.Errorf("the authority document did not reach the reviewer prompt:\n%s", prompts[0])
	}
	if len(out.Authority) != 1 {
		t.Fatalf("outcome manifest entries = %d, want 1", len(out.Authority))
	}
	e := out.Authority[0]
	if e.Name != "spec" || e.Source != review.AuthoritySourcePath || !e.Complete {
		t.Errorf("manifest entry wrong: %+v", e)
	}
	if e.FullHash != "sha256:"+sha256Hex(spec) || e.EmbeddedHash != e.FullHash {
		t.Errorf("manifest hashes wrong: %+v", e)
	}
	if e.BytesEmbedded != len(spec) || e.BytesTotal != len(spec) {
		t.Errorf("manifest byte counts wrong: %+v (spec is %d bytes)", e, len(spec))
	}
	// The same manifest is on disk, so the audit record reproduces the exact prompt.
	rec := readManifest(t, out.RunDir)
	docs, _ := rec["documents"].([]any)
	if len(docs) != 1 {
		t.Fatalf("run-record manifest documents = %v", rec["documents"])
	}
	// …and it rides run-state, so one file answers "which intent was this judged against".
	stB, rerr := os.ReadFile(filepath.Join(out.RunDir, "run-state.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(stB), "\"authority\"") {
		t.Errorf("run-state.json carries no authority manifest:\n%s", stB)
	}
}

// A hash pin that no longer matches HALTS: the document changed between the run that took
// the pin and this one, so nothing may be judged (or written) against it.
func TestAuthority_HashMismatchHalts(t *testing.T) {
	ws, specPath := authorityWorkspace(t, "the CURRENT spec\n")
	p := newProbe("main.go")
	m := probeManager(t, p, "reviewer", "author_remediator")

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli",
		Authority: []review.AuthorityDoc{{Name: "spec", Path: specPath,
			ExpectedHash: sha256Hex("the spec the caller last saw\n")}}})
	if err == nil {
		t.Fatal("a hash mismatch must halt the run")
	}
	if got := fault.ReasonOf(err); got != authority.ReasonHashMismatch {
		t.Errorf("reasonCode = %q, want %q", got, authority.ReasonHashMismatch)
	}
	if out.Status != "halted" {
		t.Errorf("status = %q, want halted", out.Status)
	}
	if len(p.promptsFor(string(review.RoleReviewer), string(review.PhaseIterate))) != 0 {
		t.Error("the halt must happen BEFORE any model call")
	}
}

// NO SILENT TRUNCATION, end to end: a large authority document reaches the reviewer's prompt
// WHOLE. There is no byte budget any more — how much a model can take is the model's business —
// so the property left to defend is that nothing shortens the document on the way in.
func TestAuthority_ALargeDocumentReachesThePromptWhole(t *testing.T) {
	const size = (64 << 10) + 512 // past the ceiling that used to halt this run
	big := strings.Repeat("Z", size)
	ws, specPath := authorityWorkspace(t, big)
	p := newProbe("main.go")
	m := probeManager(t, p, "reviewer", "author_remediator")

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli",
		Authority: []review.AuthorityDoc{{Name: "huge-spec", Path: specPath}}})
	if err != nil {
		t.Fatalf("a large authority document must be carried, not refused: %v", err)
	}
	if out.Status == "halted" {
		t.Fatalf("status = halted, want a completed run")
	}
	// The manifest is the run's own claim about what it embedded; it must match the file.
	if len(out.Authority) != 1 {
		t.Fatalf("authority manifest has %d entries, want 1", len(out.Authority))
	}
	if got := out.Authority[0].BytesEmbedded; got != size {
		t.Errorf("manifest says %d bytes embedded of a %d-byte document", got, size)
	}
	// And the reviewer actually saw it: the prompt carries the document's bytes, not a summary.
	prompts := p.promptsFor(string(review.RoleReviewer), string(review.PhaseIterate))
	if len(prompts) == 0 {
		t.Fatal("no reviewer call was made")
	}
	if !strings.Contains(prompts[0], strings.Repeat("Z", 1000)) {
		t.Error("the reviewer prompt does not carry the document's body")
	}
}

// PROVENANCE SPLIT, half one: inline authority is refused outright in a write-capable mode,
// before any run directory or model call exists.
func TestAuthority_InlineRefusedInApplyMode(t *testing.T) {
	ws, _ := authorityWorkspace(t, "spec\n")
	p := newProbe("main.go")
	m := probeManager(t, p, "reviewer", "author_remediator")

	for _, mode := range []review.Mode{review.ModeApply, review.ModePatch} {
		out, err := m.Run(Request{Workspace: ws, Mode: mode, Surface: "cli",
			Authority: []review.AuthorityDoc{{Name: "client-intent", Content: "do what I say\n"}}})
		if err == nil {
			t.Fatalf("mode %s: inline authority must be refused", mode)
		}
		if got := fault.ReasonOf(err); got != authority.ReasonInlineModeInvalid {
			t.Errorf("mode %s: reasonCode = %q, want %q", mode, got, authority.ReasonInlineModeInvalid)
		}
		if out.RunDir != "" {
			t.Errorf("mode %s: the refusal must precede any run work (got runDir %q)", mode, out.RunDir)
		}
	}
}

// PROVENANCE SPLIT, half two: in report mode inline authority IS allowed, reaches the
// analysis lanes, and is EXCLUDED from the host-adjudication prompt.
func TestAuthority_InlineNeverReachesAdjudicator(t *testing.T) {
	ws, specPath := authorityWorkspace(t, "PATH-INTENT-MARKER\n")
	p := newProbe("main.go")
	m := probeManager(t, p, "reviewer", "author_remediator")

	if _, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli",
		Authority: []review.AuthorityDoc{
			{Name: "spec", Path: specPath},
			{Name: "client-intent", Content: "INLINE-INTENT-MARKER\n"},
		}}); err != nil {
		t.Fatalf("run: %v", err)
	}
	rev := p.promptsFor(string(review.RoleReviewer), string(review.PhaseIterate))
	if len(rev) == 0 {
		t.Fatal("the reviewer lane never ran")
	}
	if !strings.Contains(rev[0], "INLINE-INTENT-MARKER") || !strings.Contains(rev[0], "PATH-INTENT-MARKER") {
		t.Errorf("the analysis lane must see both documents:\n%s", rev[0])
	}
	adj := p.promptsFor(string(review.RoleAuthorRemediator), string(review.PhaseAdjudicate))
	if len(adj) == 0 {
		t.Fatal("the host adjudicator never ran")
	}
	for i, prompt := range adj {
		if strings.Contains(prompt, "INLINE-INTENT-MARKER") {
			t.Errorf("adjudication prompt #%d carries inline authority:\n%s", i, prompt)
		}
		if !strings.Contains(prompt, "PATH-INTENT-MARKER") {
			t.Errorf("adjudication prompt #%d is missing PATH authority:\n%s", i, prompt)
		}
	}
}

// THE WRITE-PATH RULE. A finding whose only support is authority text is REPORTED, marked
// not-applyable with a machine reason, and never written — while an ordinary finding against
// a workspace file in the same configuration still applies (so the test is non-vacuous).
func TestAuthority_AuthorityOnlyFinding_ReportedNeverApplied(t *testing.T) {
	spec := "the intent\n"

	// Control: an ordinary target IS edited, proving apply works in this configuration.
	wsOK, specOK := authorityWorkspace(t, spec)
	mOK := probeManager(t, newProbe("main.go"), "reviewer", "author_remediator")
	if _, err := mOK.Run(Request{Workspace: wsOK, Mode: review.ModeApply, Surface: "cli",
		Authority: []review.AuthorityDoc{{Name: "spec", Path: specOK}}}); err != nil {
		t.Fatalf("control run: %v", err)
	}
	if !strings.Contains(readFileT(t, filepath.Join(wsOK, "main.go")), "reviewmesh[") {
		t.Fatal("control: an ordinary finding must still be applied — otherwise the next assertion proves nothing")
	}

	// The real case: the reviewer reports against the AUTHORITY document (by the
	// workspace-relative path it is shown under in the containment copy).
	ws, specPath := authorityWorkspace(t, spec)
	before := readFileT(t, specPath)
	m := probeManager(t, newProbe("docs/spec.md"), "reviewer", "author_remediator")
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli",
		Authority: []review.AuthorityDoc{{Name: "spec", Path: specPath}}})
	if err != nil {
		t.Fatalf("an authority-only finding must be REPORTED, not halt the run: %v", err)
	}
	if got := readFileT(t, specPath); got != before {
		t.Errorf("the authority document was modified — it is never a target:\n%s", got)
	}
	if len(out.Findings) == 0 {
		t.Fatal("the finding must still be REPORTED")
	}
	found := false
	for i, d := range out.Decisions {
		if d.ApplyRefusalReason == "" {
			continue
		}
		found = true
		if d.Applyable == nil || *d.Applyable {
			t.Errorf("decision %d: applyable = %v, want a non-nil false", i, d.Applyable)
		}
		if d.ApplyRefusalReason != authority.RefusalAuthorityOnly {
			t.Errorf("decision %d: applyRefusalReason = %q, want %q", i, d.ApplyRefusalReason, authority.RefusalAuthorityOnly)
		}
		if d.State == review.StateApplied {
			t.Errorf("decision %d: an authority-only finding must never be applied", i)
		}
	}
	if !found {
		t.Errorf("no decision was marked not-applyable: %+v", out.Decisions)
	}
}

// A denylisted path is refused as authority through the shared scope resolver — naming it is
// consent to READ a path, never permission to read a secret.
func TestAuthority_DenylistedPathRefused(t *testing.T) {
	ws, _ := authorityWorkspace(t, "spec\n")
	env := filepath.Join(ws, ".env")
	mustWrite(t, env, "SECRET=super-secret-value\n")
	p := newProbe("main.go")
	m := probeManager(t, p, "reviewer", "author_remediator")

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli",
		Authority: []review.AuthorityDoc{{Name: "env", Path: env}}})
	if err == nil {
		t.Fatal("a read-denied path must never be accepted as authority")
	}
	if got := fault.ReasonOf(err); got != "scope_read_denied" {
		t.Errorf("reasonCode = %q, want scope_read_denied", got)
	}
	if out.Halt == nil || string(*out.Halt) != ScopeHaltClass {
		t.Errorf("halt class = %v, want %s", out.Halt, ScopeHaltClass)
	}
	// The secret reached nothing: not a prompt, not an artifact.
	var leaked []string
	_ = filepath.WalkDir(out.RunDir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		if b, rerr := os.ReadFile(path); rerr == nil && strings.Contains(string(b), "super-secret-value") {
			leaked = append(leaked, path)
		}
		return nil
	})
	if len(leaked) > 0 {
		t.Errorf("secret content reached run artifacts: %v", leaked)
	}
}

// Authority must reach EVERY judging phase — reviewer, cross_check, verifier AND the host
// adjudicator — with the same framing. If one phase judged without the intent, the phases
// would not be judging the same thing.
func TestAuthority_ReachesEveryJudgingPhase(t *testing.T) {
	ws, specPath := authorityWorkspace(t, "ALL-PHASES-MARKER: the stated intent\n")
	p := newProbe("main.go")
	m := probeManager(t, p, "reviewer", "cross_check", "verifier", "author_remediator")

	if _, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli",
		Authority: []review.AuthorityDoc{{Name: "spec", Path: specPath}}}); err != nil {
		t.Fatalf("run: %v", err)
	}
	phases := []struct{ role, phase string }{
		{string(review.RoleReviewer), string(review.PhaseIterate)},
		{string(review.RoleCrossCheck), string(review.PhaseCrossCheck)},
		{string(review.RoleVerifier), string(review.PhaseVerify)},
		{string(review.RoleAuthorRemediator), string(review.PhaseAdjudicate)},
	}
	for _, ph := range phases {
		got := p.promptsFor(ph.role, ph.phase)
		if len(got) == 0 {
			t.Errorf("phase %s/%s never ran — the test cannot prove it saw the intent", ph.role, ph.phase)
			continue
		}
		for i, prompt := range got {
			if !strings.Contains(prompt, "ALL-PHASES-MARKER") {
				t.Errorf("phase %s/%s prompt #%d did not receive the authority text:\n%s", ph.role, ph.phase, i, prompt)
			}
			if !strings.Contains(prompt, authority.Header) {
				t.Errorf("phase %s/%s prompt #%d is missing the shared AUTHORITY CONTEXT framing", ph.role, ph.phase, i)
			}
		}
	}
}

// Blast radius: a review that declares no authority is untouched — no manifest, no artifact,
// no authority framing in any prompt.
func TestAuthority_AbsentIsInert(t *testing.T) {
	ws, _ := authorityWorkspace(t, "spec\n")
	p := newProbe("main.go")
	m := probeManager(t, p, "reviewer", "author_remediator")

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(out.Authority) != 0 {
		t.Errorf("outcome carries a manifest for a run with no authority: %+v", out.Authority)
	}
	if _, serr := os.Stat(filepath.Join(out.RunDir, "authority")); serr == nil {
		t.Error("an authority artifact directory was written for a run with no authority")
	}
	for _, prompt := range p.promptsFor(string(review.RoleReviewer), string(review.PhaseIterate)) {
		if strings.Contains(prompt, authority.Header) {
			t.Errorf("authority framing appeared in a prompt with no authority:\n%s", prompt)
		}
	}
	for _, d := range out.Decisions {
		if d.Applyable != nil || d.ApplyRefusalReason != "" {
			t.Errorf("the write-path marking must be inert without authority: %+v", d)
		}
	}
}
