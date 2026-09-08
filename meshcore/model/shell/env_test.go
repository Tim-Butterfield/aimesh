package shell

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// envDumpBin returns a fake binary that prints its whole environment to stdout, one KEY=VALUE per
// line — so a test can assert what the adapter actually spawned the CLI with.
func envDumpBin(t *testing.T) string {
	t.Helper()
	return fakeBin(t, `env`)
}

// spawnedEnv runs a recipe against the env-dumping fake binary and returns the child's environment
// as a lookup. It goes through Adapter.Invoke rather than calling HardenedEnvWith directly: the
// contract under test is "the recipe's env reaches the spawned process", not "a helper concatenates".
func spawnedEnv(t *testing.T, rec Recipe) map[string]string {
	t.Helper()
	rec.Detect = "definitely-not-on-path-xyz"
	if rec.BuildArgs == nil {
		rec.BuildArgs = func(model.Call) []string { return nil }
	}
	res, err := New(rec, envDumpBin(t), time.Minute).Invoke(context.Background(),
		model.Call{Role: "reviewer", Phase: "semantic_iterate", ModelArg: "m", Prompt: "p"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

// TestRecipeEnv_AppliedPerRecipe proves the per-recipe environment reaches the spawned CLI: a recipe
// that declares one gets it, a recipe that declares none does not, and the universal hardened base
// (NO_UPDATE_NOTIFIER) applies to BOTH — a per-recipe override must not replace the base.
func TestRecipeEnv_AppliedPerRecipe(t *testing.T) {
	with := spawnedEnv(t, Recipe{Name: "with-env", Env: map[string]string{"CI": "1", "AIMESH_TEST_ENV": "yes"}})
	if with["CI"] != "1" {
		t.Errorf("recipe env CI = %q, want 1 (the recipe's env must reach the spawned CLI)", with["CI"])
	}
	if with["AIMESH_TEST_ENV"] != "yes" {
		t.Errorf("recipe env AIMESH_TEST_ENV = %q, want yes", with["AIMESH_TEST_ENV"])
	}
	if with["NO_UPDATE_NOTIFIER"] != "1" {
		t.Errorf("the hardened base must still apply under a recipe env: NO_UPDATE_NOTIFIER = %q, want 1", with["NO_UPDATE_NOTIFIER"])
	}

	without := spawnedEnv(t, Recipe{Name: "no-env"})
	if _, set := without["CI"]; set {
		t.Errorf("a recipe with no Env must not get CI; got %q", without["CI"])
	}
	if without["NO_UPDATE_NOTIFIER"] != "1" {
		t.Errorf("hardened base missing without a recipe env: NO_UPDATE_NOTIFIER = %q, want 1", without["NO_UPDATE_NOTIFIER"])
	}
}

// TestRecipeEnv_PreservesInheritedAuthVars proves the merge is an APPEND, not a from-scratch build:
// an inherited, auth-bearing variable survives into the child even when the recipe sets its own env.
// This is the invariant that keeps every provider CLI's authentication working.
func TestRecipeEnv_PreservesInheritedAuthVars(t *testing.T) {
	t.Setenv("AIMESH_FAKE_API_KEY", "sk-inherited-secret")
	env := spawnedEnv(t, Recipe{Name: "with-env", Env: map[string]string{"CI": "1"}})
	if env["AIMESH_FAKE_API_KEY"] != "sk-inherited-secret" {
		t.Errorf("an inherited auth var must survive the recipe env merge: got %q", env["AIMESH_FAKE_API_KEY"])
	}
	if env["PATH"] == "" || env["PATH"] != os.Getenv("PATH") {
		t.Errorf("inherited PATH must be preserved verbatim, got %q", env["PATH"])
	}
}

// TestRecipeEnv_RecipeWinsOverInherited proves last-wins resolution: a recipe override beats an
// inherited value for the SAME key (os/exec resolves duplicates to the last entry).
func TestRecipeEnv_RecipeWinsOverInherited(t *testing.T) {
	t.Setenv("CI", "inherited-value")
	env := spawnedEnv(t, Recipe{Name: "with-env", Env: map[string]string{"CI": "1"}})
	if env["CI"] != "1" {
		t.Errorf("the recipe's override must win over an inherited value: CI = %q, want 1", env["CI"])
	}
}

// TestCodexRecipe_HasNoCI is the regression guard for the deliberate omission: codex-cli's model
// identity is parsed from its stderr status banner, and CI mode is precisely where a CLI suppresses
// that chrome — so a "helpful" CI=1 here would turn a verified lane into a Class-E halt. It also
// pins the whole shipped policy so enabling CI on a recipe is always a deliberate, reviewed edit.
func TestCodexRecipe_HasNoCI(t *testing.T) {
	if _, set := codexRecipe().Env["CI"]; set {
		t.Fatal("codex-cli must NOT set CI=1: its identity is the stderr status banner, which CI mode suppresses (→ unknown identity → Class-E halt)")
	}
	wantCI := map[string]bool{"gemini-cli": true, "cursor-cli": true}
	for name, rec := range Recipes() {
		_, set := rec.Env["CI"]
		if set != wantCI[name] {
			t.Errorf("recipe %q CI=1 set=%v, want %v — CI is opt-in only where the recipe reads no identity from the CLI's own output (see Recipes())", name, set, wantCI[name])
		}
	}
}

// TestCIRecipes_SpawnWithCI proves the shipped CI recipes really do spawn with CI=1 (the recipe
// table and the spawn path agree), and that the hardened base still applies to them.
func TestCIRecipes_SpawnWithCI(t *testing.T) {
	for _, rec := range []Recipe{geminiRecipe(), cursorRecipe()} {
		env := spawnedEnv(t, rec)
		if env["CI"] != "1" {
			t.Errorf("%s: spawned CI = %q, want 1", rec.Name, env["CI"])
		}
		if env["NO_UPDATE_NOTIFIER"] != "1" {
			t.Errorf("%s: spawned NO_UPDATE_NOTIFIER = %q, want 1", rec.Name, env["NO_UPDATE_NOTIFIER"])
		}
	}
}

// TestHardenedEnvWith_Deterministic pins the helper's own contract: a nil/empty override map returns
// the base unchanged, and overrides are appended in sorted key order so the produced environment is
// reproducible (a map iteration order leaking into a spawned process would be untestable).
func TestHardenedEnvWith_Deterministic(t *testing.T) {
	base := model.HardenedEnv()
	if got := model.HardenedEnvWith(nil); len(got) != len(base) {
		t.Errorf("nil overrides changed the environment: %d entries, want %d", len(got), len(base))
	}
	got := model.HardenedEnvWith(map[string]string{"B_KEY": "2", "A_KEY": "1"})
	tail := got[len(got)-2:]
	if tail[0] != "A_KEY=1" || tail[1] != "B_KEY=2" {
		t.Errorf("overrides must be appended in sorted key order, got %v", tail)
	}
}
