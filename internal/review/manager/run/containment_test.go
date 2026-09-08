package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// --- G2: every model call runs IN the isolated copy ---

// callRecorder is a deterministic adapter that RECORDS the model.Call it was handed. It
// answers every phase with schema-valid output (reviewer/cross-check/verifier findings, a host
// adjudication, and a remediation decline) so one apply run exercises every lane.
type callRecorder struct {
	mu    sync.Mutex
	calls []model.Call
}

func (r *callRecorder) Name() string              { return "recorder" }
func (r *callRecorder) Available() (bool, string) { return true, "call recorder (test)" }
func (r *callRecorder) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}

func (r *callRecorder) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	res := func(js string) (model.Result, error) {
		return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg),
			Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
	}
	switch review.Phase(c.Phase) {
	case review.PhaseAdjudicate:
		// An adjudication must cover exactly the ids it was asked about, so read them back
		// out of the prompt rather than hard-coding one (the self-critique pass asks about a
		// different set).
		var adj []string
		for _, id := range findingIDsInPrompt(c.Prompt) {
			adj = append(adj, fmt.Sprintf(`{"findingId":%q,"validity":"valid","decisionState":"applied","reasoning":"ok"}`, id))
		}
		return res(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[` +
			strings.Join(adj, ",") + `]}`)
	case review.PhaseRemediate:
		// Decline: the marker fallback still applies the edit, and the call is recorded.
		return res(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","file":"main.go","noEdit":true,"reason":"declined"}`)
	}
	return res(fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"request_changes","findings":[{"id":"R1","kind":"fail","severity":"high","title":"rec","file":"main.go","location":"1","source":"reviewer"}]}`,
		c.Role, c.Phase))
}

var recFindingIDRe = regexp.MustCompile(`"id"\s*:\s*"([^"]+)"`)

// findingIDsInPrompt reads back the finding ids an adjudication prompt asks about. It reads
// only the REVIEWER FINDINGS block (cut before the workspace files), so file content can never
// be mistaken for a finding.
func findingIDsInPrompt(prompt string) []string {
	block := prompt
	if i := strings.Index(block, "REVIEWER FINDINGS"); i >= 0 {
		block = block[i:]
	}
	if i := strings.Index(block, "WORKSPACE FILES"); i >= 0 {
		block = block[:i]
	}
	var ids []string
	seen := map[string]bool{}
	for _, m := range recFindingIDRe.FindAllStringSubmatch(block, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	return ids
}

// recorderManager wires EVERY lane (reviewer, cross_check, verifier, author_remediator) to one
// callRecorder, so a single run covers every model call reviewmesh makes.
func recorderManager(t *testing.T, rec *callRecorder) *Manager {
	t.Helper()
	cfg := config.Default()
	cfg.Adapters["recorder"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.ModelCatalog["rec-model"] = config.CatalogEntry{
		Provider: "x", CanonicalModel: "rm",
		Adapters: map[string]config.AdapterModel{"recorder": {ModelArg: "rm"}},
	}
	lane := config.Lane{Execution: "adapter", Adapter: "recorder", Model: "rec-model"}
	host := config.Lane{Execution: "host", Adapter: "recorder", Model: "rec-model"}
	cfg.Profiles["recorder"] = config.Profile{
		Description: "call recorder", AdapterPreference: []string{"recorder"},
		Lanes: map[string]config.Lane{
			"reviewer": lane, "cross_check": lane, "verifier": lane, "author_remediator": host,
		},
	}
	return &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"recorder": rec},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
}

// TestModelCalls_RunInTheIsolatedCopy is the G2 pin: EVERY model call reviewmesh makes carries
// a WorkDir that is the isolated copy, never the empty value.
//
// An empty WorkDir is not neutral — meshcore/model/shell only sets cmd.Dir when WorkDir is
// set, so the CLI inherits the ORCHESTRATOR's working directory. An agentic reviewer CLI
// resolves "the current project" from there, so leaving it empty pointed every reviewer at the
// real tree that invoked reviewmesh, whatever the prompt showed it. Prompt filtering cannot fix
// that: the containment copy is only a containment boundary if the process runs inside it.
func TestModelCalls_RunInTheIsolatedCopy(t *testing.T) {
	rec := &callRecorder{}
	m := recorderManager(t, rec)
	ws, _ := makeWorkspace(t)
	if _, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli", Profile: "recorder"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rec.calls) == 0 {
		t.Fatal("no model calls were recorded")
	}
	seen := map[string]bool{}
	for _, c := range rec.calls {
		seen[c.Role+"/"+c.Phase] = true
		if c.WorkDir == "" {
			t.Errorf("%s/%s ran with an EMPTY WorkDir — the CLI would inherit the orchestrator cwd", c.Role, c.Phase)
			continue
		}
		if c.WorkDir != c.CopyRoot {
			t.Errorf("%s/%s WorkDir = %q, want the isolated copy %q", c.Role, c.Phase, c.WorkDir, c.CopyRoot)
		}
		// The copy is under the manager's temp base, and is NOT the live workspace.
		if !strings.HasPrefix(c.WorkDir, m.TempBase) {
			t.Errorf("%s/%s WorkDir = %q, want a directory under the temp base %q", c.Role, c.Phase, c.WorkDir, m.TempBase)
		}
		if c.WorkDir == ws {
			t.Errorf("%s/%s ran in the LIVE workspace %q", c.Role, c.Phase, ws)
		}
	}
	// Every kind of call reviewmesh makes is covered by this one run.
	for _, want := range []string{
		string(review.RoleReviewer) + "/" + string(review.PhaseIterate),
		string(review.RoleCrossCheck) + "/" + string(review.PhaseCrossCheck),
		string(review.RoleVerifier) + "/" + string(review.PhaseVerify),
		string(review.RoleAuthorRemediator) + "/" + string(review.PhaseAdjudicate),
		string(review.RoleAuthorRemediator) + "/" + string(review.PhaseRemediate),
	} {
		if !seen[want] {
			t.Errorf("no %s call was recorded; covered: %v", want, sortedKeys(seen))
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- G10: a snippet-collection refusal SURFACES ---

// TestCollectRefusal_HaltsAndIsRecorded pins the fail-LOUD contract. meshcore's legacy
// CollectSnippets fails CLOSED by returning an empty set on a containment refusal — safe for
// the prompt, but indistinguishable in the audit record from "this workspace had nothing to
// show". A refusal must be a recorded fact.
//
// The refusal is reached the way an operator would really reach it: an artifact/temp base that
// sits under a protected directory name, so the isolated copy the collector is pointed at is
// itself a read-denied path (the denylist matches EVERY component of a path). The run must
// halt with exit 6 / class M6 / the machine reason, and the halt record must say so.
func TestCollectRefusal_HaltsAndIsRecorded(t *testing.T) {
	t.Setenv(fake.EnvVar, "1")
	cfg := config.Default()
	cfg.DefaultProfile = config.FakeProfile
	// A temp base whose path contains a protected component: every isolated copy made under
	// it is a read-denied root.
	base := filepath.Join(t.TempDir(), ".env")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": fake.New(fake.Valid)},
		ArtifactDir: t.TempDir(), TempBase: base,
	}
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err == nil {
		t.Fatal("a containment refusal while assembling the prompt must HALT, not yield an empty snippet set")
	}
	if out.Status != "halted" {
		t.Errorf("status = %q, want halted", out.Status)
	}
	if got := fault.CodeOf(err); got != fault.Containment {
		t.Errorf("exit code = %d, want %d (containment)", got, fault.Containment)
	}
	if got := fault.ReasonOf(err); got != "workspace_root_denied" {
		t.Errorf("reasonCode = %q, want workspace_root_denied", got)
	}
	if out.Halt == nil || string(*out.Halt) != ScopeHaltClass {
		t.Errorf("halt class = %v, want %s", out.Halt, ScopeHaltClass)
	}
	rec := readHaltRecord(t, out.RunDir)
	if rec["reasonCode"] != "workspace_root_denied" || rec["haltClass"] != ScopeHaltClass {
		t.Errorf("halt-record = %v, want the machine reason + class %s", rec, ScopeHaltClass)
	}
}

// --- G1 (manager half): trusted roots govern authority documents ---

// TestTrustedRoots_ConfineAuthorityPaths pins that an agent surface's out-of-band roots reach
// the Manager's AUTHORITATIVE authority resolution: a declared `path` outside them is refused
// even though the declaration named it. Without this, the surface's pre-spend check would be
// the only enforcement, and the core would still treat "the peer named it" as consent.
func TestTrustedRoots_ConfineAuthorityPaths(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, _ := makeWorkspace(t)
	outside := t.TempDir()
	spec := filepath.Join(outside, "spec.md")
	if err := os.WriteFile(spec, []byte("someone else's intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	docs := []review.AuthorityDoc{{Name: "spec", Path: spec, Completeness: review.CompletenessRequireFull}}

	// With trusted roots that do NOT contain the document: refused, class M6.
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "acp",
		Authority: docs, TrustedRoots: []string{ws}})
	if err == nil {
		t.Fatal("an authority path outside the trusted roots must be refused")
	}
	if got := fault.ReasonOf(err); got != "scope_outside_root" {
		t.Errorf("reasonCode = %q, want scope_outside_root", got)
	}
	if out.Halt == nil || string(*out.Halt) != ScopeHaltClass {
		t.Errorf("halt class = %v, want %s", out.Halt, ScopeHaltClass)
	}

	// The CLI's human-consent model is untouched: with no trusted roots, naming the path IS
	// the consent and the same document resolves.
	m2 := newManager(t, fake.Valid)
	if _, err := m2.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Authority: docs}); err != nil {
		t.Fatalf("the CLI path (human-named authority) must still resolve: %v", err)
	}

	// And a document INSIDE a trusted root resolves over the agent surface too.
	inside := filepath.Join(ws, "spec.md")
	if err := os.WriteFile(inside, []byte("the intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m3 := newManager(t, fake.Valid)
	out3, err := m3.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "acp",
		Authority:    []review.AuthorityDoc{{Name: "spec", Path: inside, Completeness: review.CompletenessRequireFull}},
		TrustedRoots: []string{ws}})
	if err != nil {
		t.Fatalf("an authority document inside a trusted root must resolve: %v", err)
	}
	if len(out3.Authority) != 1 {
		t.Errorf("inclusion manifest = %+v, want the one document", out3.Authority)
	}
}
