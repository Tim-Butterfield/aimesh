package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// TestMain pins AIMESH_HOME to an isolated empty dir so no test reads the developer's real
// ~/.aimesh/adapters.yaml (LoadLayered now resolves the shared adapter-location layer). Tests that
// need a populated shared file override this with t.Setenv.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "reviewmesh-config-aimesh-home")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("AIMESH_HOME", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func TestLoadYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("schemaVersion: 1\ndefaultProfile: custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load yaml: %v", err)
	}
	if cfg.DefaultProfile != "custom" {
		t.Errorf("defaultProfile = %q, want custom", cfg.DefaultProfile)
	}
}

func TestWriteAndLoadYAMLRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".aimesh", "review", "config.yaml")
	if err := Default().WriteYAMLFile(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "defaultProfile") {
		t.Errorf("YAML should use camelCase keys (json tags):\n%s", b)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("reload yaml: %v", err)
	}
	if cfg.DefaultProfile != "default" {
		t.Errorf("round-trip defaultProfile = %q, want default", cfg.DefaultProfile)
	}
}

func TestInvalidYAMLFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("defaultProfile: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("invalid YAML should fail to load")
	}
}

func TestLoadJSONCompat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"schemaVersion":1,"defaultProfile":"jsoncfg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load json: %v", err)
	}
	if cfg.DefaultProfile != "jsoncfg" {
		t.Errorf("defaultProfile = %q, want jsoncfg", cfg.DefaultProfile)
	}
}

func writeCfg(t *testing.T, dir, body string) {
	t.Helper()
	p := filepath.Join(dir, ".aimesh", "review", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLayered_Precedence(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("AIMESH_HOME", home)

	// user/global only → overrides shipped default
	writeCfg(t, home, "schemaVersion: 1\ndefaultProfile: userprof\n")
	cfg, ly, err := LoadLayered(cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProfile != "userprof" || !ly.UserLoaded || ly.ProjectLoaded {
		t.Fatalf("user layer: profile=%q layers=%+v", cfg.DefaultProfile, ly)
	}

	// project overrides user
	writeCfg(t, cwd, "schemaVersion: 1\ndefaultProfile: projprof\n")
	cfg, ly, _ = LoadLayered(cwd, "")
	if cfg.DefaultProfile != "projprof" || !ly.ProjectLoaded {
		t.Fatalf("project should beat user: profile=%q layers=%+v", cfg.DefaultProfile, ly)
	}

	// explicit (--config) beats project
	exp := filepath.Join(t.TempDir(), "explicit.yaml")
	if err := os.WriteFile(exp, []byte("schemaVersion: 1\ndefaultProfile: expprof\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, ly, _ = LoadLayered(cwd, exp)
	if cfg.DefaultProfile != "expprof" || !ly.ExplicitLoaded {
		t.Fatalf("explicit should beat project: profile=%q layers=%+v", cfg.DefaultProfile, ly)
	}
}

// writeAimesh writes a shared adapters.yaml under dir/.aimesh/.
func writeAimesh(t *testing.T, dir, body string) {
	t.Helper()
	p := filepath.Join(dir, ".aimesh", "adapters.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadLayered_AdapterPathPrecedence exercises the shared-only adapter-path merge
// (shared-user < shared-project) with provenance + shadow diagnostics + a presence-aware clear, and
// proves a path sitting in a legacy config.yaml is IGNORED (paths come solely from adapters.yaml).
func TestLoadLayered_AdapterPathPrecedence(t *testing.T) {
	// ONE user home carries both files — the legacy review config (.aimesh/review/config.yaml) and the
	// shared adapters file (.aimesh/adapters.yaml) — which is the whole point of the shared state root.
	home := t.TempDir()
	proj := t.TempDir() // cwd == project root (shared-project lives here)
	t.Setenv("AIMESH_HOME", home)
	if err := os.Mkdir(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err) // make proj a VCS root so the shared-project layer (root-anchored) resolves
	}

	// A legacy config.yaml sets claude-code's path — it must be IGNORED. codex-cli is set at both
	// shared scopes (project wins, shadowing user). ollama is set at shared-user then CLEARED at
	// shared-project.
	writeCfg(t, home, "schemaVersion: 1\nadapters:\n  claude-code: {path: /legacy-user/claude}\n")
	writeAimesh(t, home, "schemaVersion: 1\nadapters:\n  codex-cli: {path: /shared-user/codex}\n  ollama: {path: /shared-user/ollama}\n")
	writeAimesh(t, proj, "schemaVersion: 1\nadapters:\n  codex-cli: {path: /shared-proj/codex}\n  ollama: {path: \"\"}\n")

	cfg, ly, err := LoadLayered(proj, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Adapters["codex-cli"].Path; got != "/shared-proj/codex" {
		t.Errorf("codex-cli: shared-project must win, got %q", got)
	}
	if got := cfg.Adapters["claude-code"].Path; got != "" {
		t.Errorf("claude-code: a legacy config.yaml path must be ignored, got %q", got)
	}
	if got := cfg.Adapters["ollama"].Path; got != "" {
		t.Errorf("ollama: shared-project clear must win (use PATH), got %q", got)
	}
	if src := ly.AdapterPathSource["codex-cli"]; src != adapterPathSourceProjectShared {
		t.Errorf("codex-cli provenance = %q, want %q", src, adapterPathSourceProjectShared)
	}
	if _, ok := ly.AdapterPathSource["claude-code"]; ok {
		t.Errorf("claude-code has no shared path — it must have no effective source")
	}
	if _, ok := ly.AdapterPathSource["ollama"]; ok {
		t.Errorf("ollama was cleared — it must have no effective source")
	}
	// codex-cli's shared-user path was overridden by shared-project → shadowed. ollama was set at
	// shared-user then cleared at shared-project → shadowed.
	shadowed := map[string]bool{}
	for _, n := range ly.AdapterPathShadowed {
		shadowed[n] = true
	}
	if !shadowed["codex-cli"] || !shadowed["ollama"] {
		t.Errorf("expected codex-cli + ollama shadowed, got %v", ly.AdapterPathShadowed)
	}
	if !ly.SharedUserLoaded || !ly.SharedProjectLoaded || !ly.SharedProjectHasRoot {
		t.Errorf("shared layers should be loaded+rooted: %+v", ly)
	}
}

// TestLoadLayered_SynthesizesACPInstances proves a user-defined ACP instance in the shared
// adapters.yaml is synthesized into a typed adapter (cli_status identity + path) + a default
// model-catalog entry, and captured on Layers — so profiles/doctor/views treat it like any adapter.
func TestLoadLayered_SynthesizesACPInstances(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	writeAimesh(t, home, "schemaVersion: 1\nacpAdapters:\n  acp-claude:\n    title: \"ACP: Claude Code\"\n    path: /opt/bin/claude\n    args: [--acp]\n    model: claude-opus-4-8\n")

	cfg, ly, err := LoadLayered(t.TempDir(), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ad, ok := cfg.Adapters["acp-claude"]
	if !ok {
		t.Fatal("ACP instance was not synthesized into cfg.Adapters")
	}
	if ad.ModelIdentity != "cli_status" {
		t.Errorf("ModelIdentity = %q, want cli_status", ad.ModelIdentity)
	}
	if ad.Path != "/opt/bin/claude" {
		t.Errorf("Path = %q, want /opt/bin/claude", ad.Path)
	}
	cat, ok := cfg.ModelCatalog["acp-claude-default"]
	if !ok || cat.DisplayName != "ACP: Claude Code" {
		t.Errorf("default catalog entry missing/mislabeled: %+v", cat)
	}
	// The captured model becomes the EXPECTED model (modelArg + canonical) for identity verification.
	if cat.CanonicalModel != "claude-opus-4-8" || cat.Adapters["acp-claude"].ModelArg != "claude-opus-4-8" {
		t.Errorf("captured model must flow to the catalog modelArg/canonical: %+v", cat)
	}
	if got := ly.ACPInstances["acp-claude"]; got.Title != "ACP: Claude Code" || len(got.Args) != 1 || got.Args[0] != "--acp" {
		t.Errorf("Layers.ACPInstances not captured: %+v", got)
	}
}

// TestLoadLayered_MalformedSharedFileRejected proves a present-but-broken shared adapters.yaml is an
// error (not silently ignored), mirroring legacy-layer strictness.
func TestLoadLayered_MalformedSharedFileRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	writeAimesh(t, home, "schemaVersion: 99\nadapters: {}\n") // unsupported schemaVersion
	if _, _, err := LoadLayered(t.TempDir(), ""); err == nil {
		t.Error("a malformed shared adapters.yaml must be rejected")
	}
}

func TestLoadLayered_MissingLayersOK(t *testing.T) {
	t.Setenv("AIMESH_HOME", t.TempDir()) // empty home, empty cwd → only shipped defaults
	cfg, ly, err := LoadLayered(t.TempDir(), "")
	if err != nil {
		t.Fatalf("missing user/project layers must not error: %v", err)
	}
	if cfg.DefaultProfile != "default" {
		t.Errorf("shipped default expected, got %q", cfg.DefaultProfile)
	}
	if ly.UserLoaded || ly.ProjectLoaded {
		t.Errorf("no layers should be loaded: %+v", ly)
	}
}

func TestUserConfigPath_HonorsHomeOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AIMESH_HOME", home)
	p, err := UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(home, ".aimesh", "review", "config.yaml") {
		t.Errorf("user config path = %q", p)
	}
}

func TestResolve_Effort(t *testing.T) {
	cfg := Default()
	cfg.ModelCatalog["eff"] = CatalogEntry{
		Provider: "x", CanonicalModel: "e", Effort: "medium",
		Adapters: map[string]AdapterModel{"codex-cli": {ModelArg: "gpt", Effort: "high"}, "agy-cli": {ModelArg: "g"}},
	}
	cfg.Profiles["eff"] = Profile{Lanes: map[string]Lane{
		"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
		"reviewer":          {Execution: "adapter", Adapter: "codex-cli", Model: "eff"},
		"cross_check":       {Execution: "adapter", Adapter: "agy-cli", Model: "eff"},
	}}
	plan, err := cfg.Resolve(ResolveRequest{Profile: "eff"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := plan.Lanes[review.RoleReviewer].Effort; got != "high" {
		t.Errorf("per-adapter effort should win: got %q, want high", got)
	}
	if got := plan.Lanes[review.RoleCrossCheck].Effort; got != "medium" {
		t.Errorf("effort should fall back to the catalog entry: got %q, want medium", got)
	}
}

func TestStrict_RejectsUnknownStructField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("schemaVersion: 1\nbogusField: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("an unknown struct field should be rejected (strict)")
	}
}

func TestStrict_AllowsFreeMapKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	y := "schemaVersion: 1\n" +
		"profiles:\n  my-custom-profile:\n    lanes:\n      reviewer: {execution: adapter, adapter: my-adapter, model: my-model}\n" +
		"adapters:\n  my-adapter: {modelIdentity: self_report}\n" +
		"modelCatalog:\n  my-model: {provider: x, canonicalModel: y, adapters: {my-adapter: {modelArg: z}}}\n"
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("free map keys (profile/adapter/catalog/lane names) should be allowed: %v", err)
	}
	if _, ok := cfg.Profiles["my-custom-profile"]; !ok {
		t.Error("custom profile key not preserved")
	}
}

// TestStrict_RejectsInertSections is the inverse of the old accept-and-ignore contract: sections and
// keys that NOTHING reads (the former reserved `policy`/`validation`/`containment`/`audit`/`timeouts`
// maps, the review cross-check/stabilization knobs, and the inert adapter/catalog metadata) are no
// longer part of the schema, so strict parsing REFUSES them instead of silently doing nothing. Each
// case is loaded on its own so one accidental re-addition cannot hide behind another.
func TestStrict_RejectsInertSections(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"policy", "policy: {providerDiversity: required}\n"},
		{"validation", "validation: {modelVerifyMaxAttempts: 3}\n"},
		{"containment", "containment: {isolatedCopy: true}\n"},
		{"audit", "audit: {keepRuns: 50}\n"},
		{"timeouts", "timeouts: {callSeconds: 600}\n"},
		{"review.crossCheck", "review: {crossCheck: per_outer_cycle}\n"},
		{"review.stabilizeOn", "review: {stabilizeOn: set_stable}\n"},
		{"review.feedDispositionsToReviewers", "review: {feedDispositionsToReviewers: true}\n"},
		{"review.rejectedFindingsAreSticky", "review: {rejectedFindingsAreSticky: true}\n"},
		{"adapters.detect", "adapters:\n  claude-code: {detect: claude}\n"},
		{"adapters.defaultAuthoritative", "adapters:\n  claude-code: {defaultAuthoritative: true}\n"},
		{"adapters.needsCapture", "adapters:\n  claude-code: {needsCapture: true}\n"},
		{"modelCatalog.excludeFromProviderDiversity", "modelCatalog:\n  fake-model: {excludeFromProviderDiversity: true}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte("schemaVersion: 1\n"+tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Errorf("%s is read by nothing — it must be rejected, not accepted-and-ignored", tc.name)
			}
		})
	}
	// The review knobs that ARE enforced must still load (the rejection above is about the inert
	// siblings, not the section).
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("schemaVersion: 1\nreview: {maxInnerIterations: 2, maxOuterCycles: 1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("the enforced review caps must still load: %v", err)
	}
	if cfg.Review.MaxInnerIterations == nil || *cfg.Review.MaxInnerIterations != 2 ||
		cfg.Review.MaxOuterCycles == nil || *cfg.Review.MaxOuterCycles != 1 {
		t.Errorf("review caps not applied: %+v", cfg.Review)
	}
}

func TestSetAdapterPathInFile_CreatesAndPreserves(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".aimesh", "review", "config.yaml")
	if err := SetAdapterPathInFile(p, "claude-code", "/bin/claude"); err != nil {
		t.Fatalf("create: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Adapters["claude-code"].Path != "/bin/claude" {
		t.Errorf("path = %q", cfg.Adapters["claude-code"].Path)
	}
	if err := SetAdapterPathInFile(p, "codex-cli", "/bin/codex"); err != nil {
		t.Fatalf("second: %v", err)
	}
	cfg2, _ := Load(p)
	if cfg2.Adapters["claude-code"].Path != "/bin/claude" || cfg2.Adapters["codex-cli"].Path != "/bin/codex" {
		t.Errorf("paths not both preserved: %+v", cfg2.Adapters)
	}
}

func TestDiscoverProjectConfig_PrefersYAML(t *testing.T) {
	dir := t.TempDir()
	rm := filepath.Join(dir, ".aimesh", "review")
	if err := os.MkdirAll(rm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rm, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverProjectConfig(dir); !strings.HasSuffix(got, "config.json") {
		t.Errorf("json-only discovery = %q", got)
	}
	if err := os.WriteFile(filepath.Join(rm, "config.yaml"), []byte("schemaVersion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverProjectConfig(dir); !strings.HasSuffix(got, "config.yaml") {
		t.Errorf("with both present, discovery should prefer yaml, got %q", got)
	}
}

func TestResolve_HiddenFakeProfile(t *testing.T) {
	// The shipped `default` profile is now UNCONFIGURED; the deterministic fake coverage lives in
	// the shipped-but-hidden FakeProfile, which resolves to the fake adapter — only under the
	// internal test-harness gate.
	t.Setenv(fake.EnvVar, "1")
	plan, err := Default().Resolve(ResolveRequest{Profile: FakeProfile, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.Mode != review.ModeReport {
		t.Errorf("mode = %q, want report", plan.Mode)
	}
	lane, ok := plan.Lanes[review.RoleReviewer]
	if !ok {
		t.Fatal("no reviewer lane")
	}
	if lane.Adapter != "fake" || lane.Model != "fake-model" || lane.ModelArg != "fake-1" {
		t.Errorf("reviewer lane = %+v", lane)
	}
}

func TestResolve_ModeDegradation_CISurface(t *testing.T) {
	// ci surface caps the mode at report; an apply request degrades to report.
	t.Setenv(fake.EnvVar, "1") // the hidden fake profile resolves only under the internal gate
	plan, err := Default().Resolve(ResolveRequest{Profile: FakeProfile, Mode: review.ModeApply, Surface: "ci"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.Mode != review.ModeReport {
		t.Errorf("ci effective mode = %q, want report", plan.Mode)
	}
}

// The ACP surface's shipped policy ceiling is `report`: an apply request over ACP resolves to
// report on the seed config. Without the seed entry the surface fell through to `apply`, so an
// ACP host could reach live workspace writes with no config opt-in — this pins that closed.
func TestResolve_ModeDegradation_ACPSurface(t *testing.T) {
	t.Setenv(fake.EnvVar, "1") // the hidden fake profile resolves only under the internal gate
	plan, err := Default().Resolve(ResolveRequest{Profile: FakeProfile, Mode: review.ModeApply, Surface: "acp"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.Mode != review.ModeReport {
		t.Errorf("acp effective mode = %q, want report (shipped surface ceiling)", plan.Mode)
	}
}

// The ceiling is config-visible POLICY, not a hard-coded refusal: a user who widens
// `surfaces.defaultModeBySurface.acp` to apply gets apply.
func TestResolve_ACPSurface_WidenedByConfig(t *testing.T) {
	t.Setenv(fake.EnvVar, "1")
	cfg := Default()
	cfg.Surfaces.DefaultModeBySurface["acp"] = "apply"
	plan, err := cfg.Resolve(ResolveRequest{Profile: FakeProfile, Mode: review.ModeApply, Surface: "acp"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.Mode != review.ModeApply {
		t.Errorf("widened acp mode = %q, want apply", plan.Mode)
	}
}

func TestResolve_UnknownProfile_ConfigFault(t *testing.T) {
	_, err := Default().Resolve(ResolveRequest{Profile: "does-not-exist", Surface: "cli"})
	if err == nil {
		t.Fatal("expected a config fault for unknown profile")
	}
	if fault.CodeOf(err) != fault.Config {
		t.Errorf("code = %d, want %d (config)", fault.CodeOf(err), fault.Config)
	}
}

func TestSelectProfile_Default(t *testing.T) {
	if got := Default().selectProfile("", nil); got != "default" {
		t.Errorf("selectProfile = %q, want default", got)
	}
	if got := Default().selectProfile("override", nil); got != "override" {
		t.Errorf("invocation profile = %q, want override", got)
	}
}

// When defaultProfile is empty, selection falls back to the adapter-derived profile
// (defaults.profileForAdapter). That fallback must resolve to a profile that actually exists in
// the seed — a regression guard for the fake→default profile rename.
func TestSelectProfile_AdapterDerivedResolvesExistingProfile(t *testing.T) {
	cfg := Default()
	cfg.DefaultProfile = "" // force the adapter-derived fallback
	got := cfg.selectProfile("", nil)
	if got != "default" {
		t.Errorf("adapter-derived selectProfile = %q, want default", got)
	}
	if _, ok := cfg.Profiles[got]; !ok {
		t.Errorf("adapter-derived profile %q does not exist in the shipped seed", got)
	}
}

func TestResolve_NoCloudAutoSelection(t *testing.T) {
	// even with cloud adapters configured, resolving the (fake) hidden profile stays on fake — no
	// cloud adapter is ever auto-selected.
	t.Setenv(fake.EnvVar, "1") // the hidden fake profile resolves only under the internal gate
	plan, err := Default().Resolve(ResolveRequest{Profile: FakeProfile, Surface: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Lanes[review.RoleReviewer].Adapter != "fake" {
		t.Errorf("reviewer adapter = %q, want fake (no cloud auto-selection)", plan.Lanes[review.RoleReviewer].Adapter)
	}
}

// The shipped `default` profile now ships UNCONFIGURED: resolving it fails (Doctor flags it),
// rather than silently running the fake adapter.
func TestResolve_UnconfiguredDefaultProfileFails(t *testing.T) {
	if _, err := Default().Resolve(ResolveRequest{Surface: "cli"}); err == nil {
		t.Error("resolving the unconfigured shipped default profile must fail (no adapter configured)")
	}
}

func TestResolve_AvailabilityFallback(t *testing.T) {
	// a lane with no explicit adapter falls back over adapterPreference, skipping a
	// preferred-but-unavailable adapter (devin-cli) to the available one (fake).
	cfg := Default()
	cfg.Profiles["pref-test"] = Profile{
		AdapterPreference: []string{"devin-cli", "fake"},
		Lanes:             map[string]Lane{"reviewer": {Execution: "adapter", Model: "fake-model"}},
	}
	plan, err := cfg.Resolve(ResolveRequest{Profile: "pref-test", Surface: "cli", Available: map[string]bool{"fake": true}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := plan.Lanes[review.RoleReviewer].Adapter; got != "fake" {
		t.Errorf("fallback adapter = %q, want fake (devin-cli unavailable)", got)
	}
}

func TestResolve_OllamaProfile_EmptyTagFails(t *testing.T) {
	if _, err := Default().Resolve(ResolveRequest{Profile: "fully-local-ollama", Surface: "cli"}); err == nil {
		t.Error("fully-local-ollama with no model tag must fail resolution with guidance")
	}
}

func TestResolve_OllamaProfile_WithTagSelectsOllama(t *testing.T) {
	cfg := WithOllamaModel(WithExampleProfiles(Default()), "qwen2.5-coder:14b")
	plan, err := cfg.Resolve(ResolveRequest{Profile: "fully-local-ollama", Surface: "cli"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	lane := plan.Lanes[review.RoleReviewer]
	if lane.Adapter != "ollama" || string(lane.ModelArg) != "qwen2.5-coder:14b" {
		t.Errorf("reviewer lane = %+v, want ollama / qwen2.5-coder:14b", lane)
	}
}

func TestMerge_FieldGranular(t *testing.T) {
	// Merge over the shipped-but-configured fake profile so a field-granular override (model only)
	// preserves the base lane's sibling fields (adapter/execution). The `default` profile ships with
	// unconfigured lanes, so the hidden fake profile is the configured base to exercise here.
	over := Config{
		Profiles: map[string]Profile{FakeProfile: {Lanes: map[string]Lane{"reviewer": {Model: "other-model"}}}},
		Adapters: map[string]Adapter{"fake": {Path: "/opt/fake"}},
	}
	merged := merge(Default(), over)

	lane := merged.Profiles[FakeProfile].Lanes["reviewer"]
	if lane.Model != "other-model" {
		t.Errorf("lane model = %q, want other-model", lane.Model)
	}
	if lane.Adapter != "fake" || lane.Execution != "adapter" {
		t.Errorf("sibling lane fields lost: %+v", lane)
	}
	a := merged.Adapters["fake"]
	if a.Path != "/opt/fake" {
		t.Errorf("adapter path = %q, want /opt/fake", a.Path)
	}
	if a.ModelIdentity != "self_report" {
		t.Errorf("sibling adapter field lost: modelIdentity = %q", a.ModelIdentity)
	}
}

func TestMerge_BoolPreservedWhenOmitted(t *testing.T) {
	// override changes only DisplayName; the omitted Authoritative must stay true.
	over := Config{ModelCatalog: map[string]CatalogEntry{"fake-model": {DisplayName: "Renamed"}}}
	merged := merge(Default(), over)
	e := merged.ModelCatalog["fake-model"]
	if e.Authoritative == nil || !*e.Authoritative {
		t.Error("Authoritative should be preserved (true) when the override omits it")
	}
	if e.DisplayName != "Renamed" {
		t.Errorf("DisplayName = %q, want Renamed", e.DisplayName)
	}
}

func TestMerge_BoolExplicitFalseOverrides(t *testing.T) {
	f := false
	over := Config{ModelCatalog: map[string]CatalogEntry{"fake-model": {Authoritative: &f}}}
	merged := merge(Default(), over)
	e := merged.ModelCatalog["fake-model"]
	if e.Authoritative == nil || *e.Authoritative {
		t.Error("explicit false should override the base true")
	}
}

// TestParse_InertKeysAreRejectedNotIgnored pins the rule the whole schema is built on: a key
// nothing reads is a LOAD ERROR, not a setting that quietly does nothing. `defaults.autoDetect` and
// `lanes.<role>.optional` are read by no code anywhere, so neither is accepted here: a config that
// names one must fail rather than look configured.
func TestParse_InertKeysAreRejectedNotIgnored(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "defaults.autoDetect",
			yaml: "schemaVersion: 1\ndefaults:\n  autoDetect: true\n",
			want: "autoDetect",
		},
		{
			name: "lanes.<role>.optional",
			yaml: "schemaVersion: 1\nprofiles:\n  p:\n    lanes:\n      verifier:\n        execution: adapter\n        optional: true\n",
			want: "optional",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfigBytes("config.yaml", []byte(tc.yaml))
			if err == nil {
				t.Fatalf("%s parsed successfully — an inert key must be refused, not accepted-and-ignored", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal must NAME the offending key; got %v", err)
			}
		})
	}
}

func TestSeed_AdapterDefaultsAreMarked(t *testing.T) {
	// Every shipped per-adapter default entry must be flagged adapterDefault: true so the
	// lane editor projects it under "Adapter default", never as a saved-model preset.
	cat := Default().ModelCatalog
	for _, key := range []string{"fake-model", "ollama-local", "claude-code-default", "codex-cli-default", "agy-cli-default", "devin-cli-default"} {
		e, ok := cat[key]
		if !ok {
			t.Fatalf("seed catalog missing %q", key)
		}
		if !e.IsAdapterDefault() {
			t.Errorf("seed entry %q must be an adapter default (adapterDefault: true)", key)
		}
		if len(e.Adapters) != 1 {
			t.Errorf("adapter-default entry %q must be single-adapter (invariant), got %d adapters", key, len(e.Adapters))
		}
	}
}

func TestMerge_AdapterDefaultPreservedAndOverridable(t *testing.T) {
	// Omitted → preserved.
	over := Config{ModelCatalog: map[string]CatalogEntry{"claude-code-default": {DisplayName: "X"}}}
	if e := merge(Default(), over).ModelCatalog["claude-code-default"]; !e.IsAdapterDefault() {
		t.Error("adapterDefault should be preserved (true) when the override omits it")
	}
	// Explicit false → a user can reclassify a seed default as a saved model.
	f := false
	over2 := Config{ModelCatalog: map[string]CatalogEntry{"claude-code-default": {AdapterDefault: &f}}}
	if e := merge(Default(), over2).ModelCatalog["claude-code-default"]; e.IsAdapterDefault() {
		t.Error("explicit adapterDefault=false should override the seed true (reclassify as saved model)")
	}
	// Explicit true → a user can promote a user entry to an adapter default.
	tr := true
	base := Config{ModelCatalog: map[string]CatalogEntry{"my-model": {Provider: "x", Adapters: map[string]AdapterModel{"codex-cli": {ModelArg: "m"}}}}}
	over3 := Config{ModelCatalog: map[string]CatalogEntry{"my-model": {AdapterDefault: &tr}}}
	if e := merge(base, over3).ModelCatalog["my-model"]; !e.IsAdapterDefault() {
		t.Error("explicit adapterDefault=true should mark a user entry as an adapter default")
	}
}

func TestStrict_AcceptsAdapterDefaultField(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("schemaVersion: 1\nmodelCatalog:\n  m1:\n    provider: openai\n    adapterDefault: true\n    adapters:\n      codex-cli:\n        modelArg: gpt-5.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("strict load must accept adapterDefault: %v", err)
	}
	if !cfg.ModelCatalog["m1"].IsAdapterDefault() {
		t.Error("adapterDefault: true should parse to a true marker")
	}
}

func TestLoad_NoOverride(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DefaultProfile != "default" {
		t.Errorf("defaultProfile = %q", cfg.DefaultProfile)
	}
}
