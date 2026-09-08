package shell

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Parser fixtures are sanitized captures of the real CLIs (2026-07-03). Default tests
// never run the real discovery commands — ListModels is exercised only against fake
// executables below.

const ollamaListFixture = `NAME                 ID              SIZE      MODIFIED
gemma3:27b           a418f5838eaf    17 GB     8 weeks ago
qwen2.5-coder:14b    9ec8897f747e    9.0 GB    8 weeks ago
`

const codexBundledFixture = `{"models":[
  {"slug":"gpt-5.5","display_name":"GPT-5.5","default_reasoning_level":"medium",
   "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"}],
   "visibility":"list"},
  {"slug":"gpt-5.4-mini","display_name":"GPT-5.4 mini","default_reasoning_level":"low",
   "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"}],
   "visibility":"list"},
  {"slug":"internal-test","display_name":"hidden","default_reasoning_level":"low",
   "supported_reasoning_levels":[{"effort":"low"}],"visibility":"hidden"}
]}`

const agyModelsFixture = `Gemini 3.5 Flash (Medium)
Gemini 3.1 Pro (High)
Claude Opus 4.6 (Thinking)
GPT-OSS 120B (Medium)
`

func TestParseOllamaList(t *testing.T) {
	models, err := parseOllamaList([]byte(ollamaListFixture), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 2 || models[0].Arg != "gemma3:27b" || models[1].Arg != "qwen2.5-coder:14b" {
		t.Errorf("models=%+v", models)
	}
	if _, err := parseOllamaList([]byte("garbage output\n"), nil); err == nil {
		t.Error("headerless output must fail the parse (fallback with reason, never a guess)")
	}
}

func TestParseCodexBundled(t *testing.T) {
	models, err := parseCodexBundled([]byte(codexBundledFixture), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("hidden entries must be skipped; models=%+v", models)
	}
	m := models[0]
	if m.Arg != "gpt-5.5" || m.DefaultEffort != "medium" || len(m.Efforts) != 4 || m.Efforts[3] != "xhigh" {
		t.Errorf("gpt-5.5 projection=%+v", m)
	}
	if _, err := parseCodexBundled([]byte("not json"), nil); err == nil {
		t.Error("non-JSON output must fail the parse")
	}
	if _, err := parseCodexBundled([]byte(`{"other":1}`), nil); err == nil {
		t.Error("JSON without a models array must fail the parse")
	}
}

func TestParseAgyModels(t *testing.T) {
	models, err := parseAgyModels([]byte(agyModelsFixture), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 4 || models[1].Arg != "Gemini 3.1 Pro (High)" {
		t.Errorf("models=%+v", models)
	}
	if models[0].Efforts != nil || models[0].DefaultEffort != "" {
		t.Error("agy reasoning tiers are name-bound — no separate effort may be reported")
	}
	if _, err := parseAgyModels([]byte("Error: not signed in\n"), nil); err == nil {
		t.Error("error-looking output must fail the parse")
	}
}

// fakeListerBinary writes an executable that prints the given stdout for any args.
func fakeListerBinary(t *testing.T, stdout string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake /bin/sh binaries are POSIX-only (exec mechanics verify-on-windows)")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "fakecli")
	if !strings.HasSuffix(stdout, "\n") {
		stdout += "\n" // keep the heredoc terminator on its own line
	}
	script := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "EOF\nexit " + map[bool]string{true: "0", false: "3"}[exitCode == 0] + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListModels_FakeBinary(t *testing.T) {
	a := New(Recipes()["ollama"], fakeListerBinary(t, ollamaListFixture, 0), time.Minute)
	models, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("models=%+v", models)
	}
	cmdName, kind := a.DiscoveryMechanism()
	if cmdName != "ollama list" || kind != "local" {
		t.Errorf("mechanism=%q kind=%q", cmdName, kind)
	}
}

func TestListModels_FailuresAreDescriptive(t *testing.T) {
	// Non-zero exit → "discovery command failed".
	a := New(Recipes()["agy-cli"], fakeListerBinary(t, "boom", 3), time.Minute)
	if _, err := a.ListModels(context.Background()); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Errorf("want a descriptive failure, got %v", err)
	}
	// Missing binary → "discovery unavailable".
	b := New(Recipes()["ollama"], "/no/such/binary", time.Minute)
	if _, err := b.ListModels(context.Background()); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("want unavailable, got %v", err)
	}
	// No mechanism (claude) → explicit unsupported error.
	c := New(Recipes()["claude-code"], "", time.Minute)
	if _, err := c.ListModels(context.Background()); err == nil || !strings.Contains(err.Error(), "no model-discovery mechanism") {
		t.Errorf("want unsupported, got %v", err)
	}
}

func TestPreviewArgs_MatchesRecipes(t *testing.T) {
	cases := []struct {
		recipe, modelArg, effort string
		want                     []string
	}{
		// The piped recipes render the prompt after a `<`: it is still an input, and a preview that
		// showed a command with no prompt in it would read as a broken preview.
		{"codex-cli", "gpt-5.5", "high",
			[]string{"codex", "exec", "-s", "read-only", "--skip-git-repo-check", "-m", "gpt-5.5", "-c", "model_reasoning_effort=high", "<", "<prompt>"}},
		{"claude-code", "opus", "medium",
			[]string{"claude", "-p", "--permission-mode", "plan", "--disallowedTools", "Edit Write NotebookEdit", "--output-format", "json", "--model", "opus", "--effort", "medium", "<", "<prompt>"}},
		{"devin-cli", "claude-opus-4-8-medium", "",
			[]string{"devin", "--model", "claude-opus-4-8-medium", "-p", "<prompt>"}},
		{"agy-cli", "Gemini 3.1 Pro (High)", "",
			[]string{"agy", "--model", "Gemini 3.1 Pro (High)", "-p", "<prompt>"}},
		{"ollama", "qwen2.5-coder:14b", "",
			[]string{"ollama", "run", "--nowordwrap", "qwen2.5-coder:14b", "<prompt>"}},
		{"cursor-cli", "composer-2.5", "",
			[]string{"agent", "--print", "--mode", "plan", "--trust", "--output-format", "json", "--model", "composer-2.5", "<prompt>"}},
	}
	for _, c := range cases {
		a := New(Recipes()[c.recipe], "", time.Minute)
		got := a.PreviewArgs(c.modelArg, c.effort)
		if len(got) != len(c.want) {
			t.Errorf("%s: argv=%q want %q", c.recipe, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s[%d]=%q want %q", c.recipe, i, got[i], c.want[i])
			}
		}
	}
	// Note: claude's `--effort` is a REQUESTED level — the CLI may silently downgrade and
	// does not report the effective level, so reviewmesh labels it requested/unverified.
}
