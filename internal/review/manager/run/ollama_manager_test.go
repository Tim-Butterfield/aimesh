package run

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
)

const validReviewerResult = `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","summary":"one issue","verdict":"request_changes","findings":[{"id":"F-1","kind":"fail","severity":"high","title":"Example","file":"main.go","location":"1","source":"reviewer"}]}`

// The Manager re-keys findings to unique host ids (f1..fN) before the host call, so
// host fixtures adjudicate "f1" (the single reviewer finding), not the reviewer's id.
const validHostAdjudication = `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"f1","validity":"valid","decisionState":"reported_valid","severityAdjusted":"high","reasoning":"supported by main.go"}]}`

const invalidHostAdjudication = `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"f1","validity":"invalid","decisionState":"invalid","reasoning":"not supported by the shown files"}]}`

// fakeOllamaBin writes an executable /bin/sh that ignores its args and prints body.
func fakeOllamaBin(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake POSIX ollama binary; the real adapter path is verify-on-windows")
	}
	p := filepath.Join(t.TempDir(), "ollama")
	if err := os.WriteFile(p, []byte("#!/bin/sh\ncat <<'JSON'\n"+body+"\nJSON\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeOllamaTwoCall writes a /bin/sh that returns hostJSON for an adjudication prompt
// (the prompt contains "HOST ADJUDICATOR") and reviewerJSON otherwise — so one binary
// serves both the reviewer and host-adjudication calls of the local two-call path.
func fakeOllamaTwoCall(t *testing.T, reviewerJSON, hostJSON string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake POSIX ollama binary; the real adapter path is verify-on-windows")
	}
	p := filepath.Join(t.TempDir(), "ollama")
	script := "#!/bin/sh\ncase \"$*\" in\n  *'HOST ADJUDICATOR'*) cat <<'HOSTJSON'\n" + hostJSON + "\nHOSTJSON\n  ;;\n  *) cat <<'REVJSON'\n" + reviewerJSON + "\nREVJSON\n  ;;\nesac\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func ollamaManager(t *testing.T, recipe shell.Recipe, binPath, tag string) *Manager {
	t.Helper()
	return &Manager{
		Cfg: config.WithOllamaModel(config.WithExampleProfiles(config.Default()), tag),
		Adapters: map[string]model.Adapter{
			"fake":   fake.New(fake.Valid),
			"ollama": shell.New(recipe, binPath, time.Minute),
		},
		ArtifactDir: t.TempDir(),
		TempBase:    t.TempDir(),
	}
}

func TestOllamaManagerPath_FakeBinaryReportSucceeds(t *testing.T) {
	bin := fakeOllamaTwoCall(t, validReviewerResult, validHostAdjudication)
	m := ollamaManager(t, shell.Recipes()["ollama"], bin, "stub-model:tag")
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "fully-local-ollama"})
	if err != nil {
		t.Fatalf("ollama Manager-path report failed: %v", err)
	}
	if out.Status != "stable" {
		t.Errorf("status = %q, want stable", out.Status)
	}
	if len(out.Findings) != 1 {
		t.Errorf("findings = %d, want 1", len(out.Findings))
	}
	// audit artifacts are written, including the separate host-adjudication call
	for _, p := range []string{
		"resolved-plan.json",
		filepath.Join("calls", "c-0001-host", "call-status.json"),
		filepath.Join("decisions", "host-adjudication.json"),
	} {
		if _, err := os.Stat(filepath.Join(out.RunDir, p)); err != nil {
			t.Errorf("expected audit artifact %s: %v", p, err)
		}
	}
}

// hostModelManager wires reviewer→fake and author_remediator→ollama(shell) so the
// host-adjudication MODEL call is exercised deterministically via a fake binary.
func hostModelManager(t *testing.T, hostRecipe shell.Recipe, hostBin string, reviewerScenario fake.Scenario) *Manager {
	t.Helper()
	cfg := config.WithOllamaModel(config.WithExampleProfiles(config.Default()), "stub:tag")
	cfg.Profiles["host-test"] = config.Profile{
		Lanes: map[string]config.Lane{
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
			"author_remediator": {Execution: "host", Adapter: "ollama", Model: "ollama-local"},
		},
	}
	return &Manager{
		Cfg: cfg,
		Adapters: map[string]model.Adapter{
			"fake":   fake.New(reviewerScenario),
			"ollama": shell.New(hostRecipe, hostBin, time.Minute),
		},
		ArtifactDir: t.TempDir(),
		TempBase:    t.TempDir(),
	}
}

func TestHostAdjudication_InvalidPreventsApply(t *testing.T) {
	bin := fakeOllamaTwoCall(t, validReviewerResult, invalidHostAdjudication)
	m := ollamaManager(t, shell.Recipes()["ollama"], bin, "stub:tag")
	ws, file := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli", Profile: "fully-local-ollama"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Status != "stable" {
		t.Errorf("status = %q, want stable", out.Status)
	}
	if strings.Contains(read(t, file), "// reviewmesh[") {
		t.Error("a host-invalidated finding must not be applied")
	}
}

func TestHostAdjudication_DeferredNotApplied(t *testing.T) {
	// validity "valid" but a non-apply decisionState (deferred) must NOT be applied.
	deferred := `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"f1","validity":"valid","decisionState":"upstream_conflict_deferred","reasoning":"conflicts with upstream authority"}]}`
	bin := fakeOllamaTwoCall(t, validReviewerResult, deferred)
	m := ollamaManager(t, shell.Recipes()["ollama"], bin, "stub:tag")
	ws, file := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli", Profile: "fully-local-ollama"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Status != "stable" {
		t.Errorf("status = %q, want stable", out.Status)
	}
	if strings.Contains(read(t, file), "// reviewmesh[") {
		t.Error("a deferred (valid but non-apply) host verdict must not be applied")
	}
}

func TestHostAdjudication_MissingHaltsClassG(t *testing.T) {
	missing := `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[]}`
	bin := fakeOllamaTwoCall(t, validReviewerResult, missing)
	m := ollamaManager(t, shell.Recipes()["ollama"], bin, "stub:tag")
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "fully-local-ollama"})
	if fault.CodeOf(err) != fault.Internal {
		t.Fatalf("exit code = %d, want %d (Class G)", fault.CodeOf(err), fault.Internal)
	}
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001-host", "retry-1", "prompt.md")) // host retried
}

// The ADJUDICATING host is the strictest lane there is — it decides which findings are valid — and even
// there a mismatched identity only produces a caveat. The adjudication itself is what a reader assesses.
func TestHostAdjudication_IdentityMismatchIsACaveat(t *testing.T) {
	recipe := shell.Recipes()["ollama"]
	recipe.ParseIdentity = func(_, _ []byte, _ model.Call) string { return "wrong-host-model" }
	bin := fakeOllamaBin(t, validHostAdjudication)
	m := hostModelManager(t, recipe, bin, fake.Valid)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "host-test"})
	if err != nil {
		t.Fatalf("a mismatched host-adjudication identity must not halt: %v", err)
	}
	found := false
	for _, c := range out.IdentityCaveats {
		if c.Status == review.VerifMismatch {
			found = true
		}
	}
	if !found {
		t.Errorf("the host lane's mismatch must be recorded, got %+v", out.IdentityCaveats)
	}
}

func TestHostAdjudication_NoFindingsSkipsHostCall(t *testing.T) {
	bin := fakeOllamaBin(t, validHostAdjudication)
	m := hostModelManager(t, shell.Recipes()["ollama"], bin, fake.Empty)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "host-test"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001-host")); !os.IsNotExist(statErr) {
		t.Error("no findings ⇒ host-adjudication model call must be skipped")
	}
}

// TestOllamaTwoCall_RealHostSmoke proves the local two-call path with a REAL Ollama
// host adjudicator: a deterministic fake reviewer emits one finding and the real local
// model adjudicates it. Gated (REVIEWMESH_RUN_REAL_OLLAMA_MANAGER_SMOKE=1 +
// REVIEWMESH_OLLAMA_MODEL=<tag>) — success depends on the model emitting schema-valid
// host-adjudication JSON; the deterministic fake-binary tests are the acceptance proof.
func TestOllamaTwoCall_RealHostSmoke(t *testing.T) {
	if os.Getenv("REVIEWMESH_RUN_REAL_OLLAMA_MANAGER_SMOKE") == "" || os.Getenv("REVIEWMESH_OLLAMA_MODEL") == "" {
		t.Skip("gated: set REVIEWMESH_RUN_REAL_OLLAMA_MANAGER_SMOKE=1 and REVIEWMESH_OLLAMA_MODEL=<tag>")
	}
	tag := os.Getenv("REVIEWMESH_OLLAMA_MODEL")
	cfg := config.WithOllamaModel(config.WithExampleProfiles(config.Default()), tag)
	cfg.Profiles["host-test"] = config.Profile{
		Lanes: map[string]config.Lane{
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
			"author_remediator": {Execution: "host", Adapter: "ollama", Model: "ollama-local"},
		},
	}
	m := &Manager{
		Cfg: cfg,
		Adapters: map[string]model.Adapter{
			"fake":   fake.New(fake.Valid),
			"ollama": shell.New(shell.Recipes()["ollama"], "", 3*time.Minute), // real ollama on PATH
		},
		ArtifactDir: t.TempDir(),
		TempBase:    t.TempDir(),
	}
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "host-test"})
	t.Logf("real two-call: status=%q err=%v runDir=%s", out.Status, err, out.RunDir)
}

func TestOllamaProfile_UnavailableFailsPreflight(t *testing.T) {
	m := ollamaManager(t, shell.Recipes()["ollama"], "/no/such/ollama-binary", "stub:tag")
	ws, _ := makeWorkspace(t)
	_, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "fully-local-ollama"})
	if err == nil {
		t.Fatal("an unavailable ollama binary must fail preflight")
	}
	if fault.CodeOf(err) != fault.Adapter {
		t.Errorf("exit code = %d, want %d (adapter)", fault.CodeOf(err), fault.Adapter)
	}
}

func TestPreflight_AllLanesChecked(t *testing.T) {
	// reviewer on fake (available), author_remediator on ollama (unavailable):
	// preflight must fail because SOME lane selects an unavailable adapter.
	cfg := config.WithOllamaModel(config.WithExampleProfiles(config.Default()), "x:tag")
	cfg.Profiles["mixed-test"] = config.Profile{
		Lanes: map[string]config.Lane{
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
			"author_remediator": {Execution: "host", Adapter: "ollama", Model: "ollama-local"},
		},
	}
	m := &Manager{
		Cfg: cfg,
		Adapters: map[string]model.Adapter{
			"fake":   fake.New(fake.Valid),
			"ollama": shell.New(shell.Recipes()["ollama"], "/no/such/ollama", time.Minute),
		},
		ArtifactDir: t.TempDir(),
		TempBase:    t.TempDir(),
	}
	ws, _ := makeWorkspace(t)
	_, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "mixed-test"})
	if err == nil {
		t.Fatal("a non-reviewer lane on an unavailable adapter must fail preflight")
	}
	if fault.CodeOf(err) != fault.Adapter {
		t.Errorf("exit code = %d, want %d (adapter)", fault.CodeOf(err), fault.Adapter)
	}
}

func TestOllamaShell_IdentityMismatchIsACaveat(t *testing.T) {
	// Two-call: the mismatch no longer stops the run, so it proceeds through host adjudication.
	bin := fakeOllamaTwoCall(t, validReviewerResult, validHostAdjudication)
	// a recipe whose identity extraction reports a different model than requested
	recipe := shell.Recipes()["ollama"]
	recipe.ParseIdentity = func(_, _ []byte, _ model.Call) string { return "some-other-model" }
	m := ollamaManager(t, recipe, bin, "stub-model:tag")
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "fully-local-ollama"})
	if err != nil {
		t.Fatalf("an identity mismatch must not halt the shell path: %v", err)
	}
	found := false
	for _, c := range out.IdentityCaveats {
		if c.Status == review.VerifMismatch && c.ReportedModel == "some-other-model" {
			found = true
		}
	}
	if !found {
		t.Errorf("the caveat must carry the model the adapter actually reported, got %+v", out.IdentityCaveats)
	}
}

// TestOllamaManagerPath_RealSmoke runs the full Manager path against a real local
// Ollama. It is gated (skips unless both env vars are set) because a general model
// will not reliably emit schema-valid ReviewerResult JSON without the full prompt
// template (deferred). Run with:
//
//	REVIEWMESH_OLLAMA_MODEL=<tag> REVIEWMESH_RUN_REAL_OLLAMA_MANAGER_SMOKE=1 \
//	  go test ./internal/manager/review -run OllamaManagerPath_RealSmoke -v
func TestOllamaManagerPath_RealSmoke(t *testing.T) {
	if os.Getenv("REVIEWMESH_RUN_REAL_OLLAMA_MANAGER_SMOKE") == "" || os.Getenv("REVIEWMESH_OLLAMA_MODEL") == "" {
		t.Skip("gated: set REVIEWMESH_RUN_REAL_OLLAMA_MANAGER_SMOKE=1 and REVIEWMESH_OLLAMA_MODEL=<tag>")
	}
	tag := os.Getenv("REVIEWMESH_OLLAMA_MODEL")
	m := ollamaManager(t, shell.Recipes()["ollama"], "", tag) // "" → resolve `ollama` on PATH
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "fully-local-ollama"})
	t.Logf("real ollama Manager-path: status=%q err=%v runDir=%s", out.Status, err, out.RunDir)
	// We do not assert success: a general model may not emit schema-valid JSON
	// (→ Class G). The full prompt template that makes this reliable is deferred.
}
