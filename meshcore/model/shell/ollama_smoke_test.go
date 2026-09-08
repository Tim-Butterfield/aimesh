package shell

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// TestOllama_RealSmoke is a real, local-only Ollama invocation (no cloud, no
// tokens). It skips unless both `ollama` is on PATH and REVIEWMESH_OLLAMA_MODEL
// names an installed tag, e.g.:
//
//	REVIEWMESH_OLLAMA_MODEL=qwen2.5-coder:14b go test ./internal/access/model/shell -run Ollama -v
//
// The argv shape (`run <model> <prompt>`) was verified locally in Batch 4; Ollama
// exposes no model-identity envelope, so the self-report tag (the invocation tag)
// is the honest identity signal — confirmed here.
func TestOllama_RealSmoke(t *testing.T) {
	tag := os.Getenv("REVIEWMESH_OLLAMA_MODEL")
	if tag == "" {
		t.Skip("set REVIEWMESH_OLLAMA_MODEL=<installed tag> to run the real ollama smoke")
	}
	if _, err := exec.LookPath("ollama"); err != nil {
		t.Skip("ollama not installed")
	}
	a := New(ollamaRecipe(), "", 180*time.Second)
	res, err := a.Invoke(context.Background(), model.Call{
		ModelArg: core.ModelArg(tag),
		Prompt:   "Reply with exactly two words: hello reviewmesh",
	})
	if err != nil {
		t.Fatalf("ollama invoke: %v (stderr: %s)", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d (stderr: %s)", res.ExitCode, res.Stderr)
	}
	if len(res.Stdout) == 0 {
		t.Error("expected some model output")
	}
	if res.ActualModel != tag {
		t.Errorf("identity = %q, want the self-report tag %q", res.ActualModel, tag)
	}
}
