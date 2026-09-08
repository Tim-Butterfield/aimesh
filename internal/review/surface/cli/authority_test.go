package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/authority"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/runview"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// authorityWorkspace returns a reviewable workspace plus a design doc kept OUTSIDE it (the
// other common shape: the spec lives in a docs repo, not the tree under review).
func authorityWorkspace(t *testing.T, spec string) (ws, specPath string) {
	t.Helper()
	ws = jsonWorkspace(t)
	specPath = filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, specPath
}

// `--authority` + a matching `--authority-hash` runs, and the machine projection carries the
// inclusion manifest — the CLI half of "authority lands on CLI and ACP together".
func TestCLIAuthority_JSONProjectionCarriesManifest(t *testing.T) {
	hermeticReview(t, "valid")
	spec := "# Spec\n\nfail closed.\n"
	ws, specPath := authorityWorkspace(t, spec)

	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke",
		"--authority", specPath, "--authority-hash", "spec.md="+sha256Hex(spec), ws)
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("stdout is not the projection: %v\n%s", err, out)
	}
	if len(view.Authority) != 1 {
		t.Fatalf("projection authority = %+v, want one entry", view.Authority)
	}
	e := view.Authority[0]
	if e.Name != "spec.md" || e.Source != review.AuthoritySourcePath {
		t.Errorf("manifest entry wrong: %+v", e)
	}
	if !e.Complete || e.BytesTotal != len(spec) || e.FullHash != "sha256:"+sha256Hex(spec) {
		t.Errorf("manifest entry does not describe the document: %+v", e)
	}
}

// The projection ALWAYS carries the authority array, so "judged against nothing" is stated
// rather than inferred from a missing key.
func TestCLIAuthority_ProjectionAlwaysPresent(t *testing.T) {
	hermeticReview(t, "valid")
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke", jsonWorkspace(t))
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	if !strings.Contains(out, `"authority": []`) {
		t.Errorf("the projection must always state the authority set:\n%s", out)
	}
}

// A hash pin that does not match HALTS with the machine reason code — the CLI's error
// carrier for the same refusal ACP reports as -32602.
func TestCLIAuthority_HashMismatchHalts(t *testing.T) {
	hermeticReview(t, "valid")
	ws, specPath := authorityWorkspace(t, "current\n")
	code, _, errs := run(t, "review", "--report", "--profile", "fake-smoke",
		"--authority", specPath, "--authority-hash", "spec.md="+sha256Hex("stale\n"), ws)
	if code != int(fault.Policy) {
		t.Errorf("exit = %d, want %d", code, fault.Policy)
	}
	if !strings.Contains(errs, authority.ReasonHashMismatch) && !strings.Contains(errs, "expectedHash") {
		t.Errorf("the refusal must explain the pin mismatch:\n%s", errs)
	}
}

// A read-denied path is refused as authority at the CLI too.
func TestCLIAuthority_DenylistedPathRefused(t *testing.T) {
	hermeticReview(t, "valid")
	ws := jsonWorkspace(t)
	env := filepath.Join(ws, ".env")
	if err := os.WriteFile(env, []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "review", "--report", "--profile", "fake-smoke", "--authority", env, ws)
	if code != int(fault.Containment) {
		t.Errorf("exit = %d, want %d (containment)", code, fault.Containment)
	}
	if !strings.Contains(errs, "scope_read_denied") && !strings.Contains(errs, "protected path rule") {
		t.Errorf("the refusal must name the rule:\n%s", errs)
	}
}

// Inline authority in a write-capable mode is refused at the CLI boundary, before any run.
func TestCLIAuthority_InlineRefusedInApplyMode(t *testing.T) {
	hermeticReview(t, "valid")
	ws := jsonWorkspace(t)
	manifest := filepath.Join(t.TempDir(), "authority.json")
	if err := os.WriteFile(manifest,
		[]byte(`[{"name":"client-intent","content":"do what I say"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "review", "--apply", "--profile", "fake-smoke",
		"--authority-manifest", manifest, ws)
	if code != int(fault.Config) {
		t.Errorf("exit = %d, want %d", code, fault.Config)
	}
	if !strings.Contains(errs, "report-mode only") {
		t.Errorf("the refusal must explain the provenance split:\n%s", errs)
	}
}

// The manifest form carries what a flag cannot: byte ranges. Partial inclusion is recorded as
// incomplete, with a different embedded hash.
func TestCLIAuthority_ManifestRanges(t *testing.T) {
	hermeticReview(t, "valid")
	ws, specPath := authorityWorkspace(t, "AAAAABBBBBCCCCC")
	manifest := filepath.Join(t.TempDir(), "authority.json")
	body := `{"authority":[{"name":"spec","path":` + strconvQuote(specPath) +
		`,"completeness":"ranges","ranges":[{"start":0,"end":5}]}]}`
	if err := os.WriteFile(manifest, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "review", "--report", "--json", "--profile", "fake-smoke",
		"--authority-manifest", manifest, ws)
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Authority) != 1 {
		t.Fatalf("authority = %+v", view.Authority)
	}
	e := view.Authority[0]
	if e.Complete || e.BytesEmbedded != 5 || e.BytesTotal != 15 {
		t.Errorf("ranged inclusion recorded wrong: %+v", e)
	}
	if e.EmbeddedHash == e.FullHash {
		t.Error("an incomplete inclusion must hash differently from the full document")
	}
}

// Usage guards: the two declaration forms do not mix, and a pin that names nothing is a typo
// — and a typo'd pin pins NOTHING, which is exactly what a pin exists to prevent.
func TestCLIAuthority_UsageGuards(t *testing.T) {
	hermeticReview(t, "valid")
	ws, specPath := authorityWorkspace(t, "spec\n")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"manifest with flags", []string{"--authority", specPath, "--authority-manifest", "m.json"}, "cannot be combined"},
		{"hash without authority", []string{"--authority-hash", "spec.md=" + sha256Hex("x")}, "applies only with --authority"},
		{"malformed hash flag", []string{"--authority", specPath, "--authority-hash", "no-equals-sign"}, "want <name>=<sha256>"},
		{"unmatched pin", []string{"--authority", specPath, "--authority-hash", "other.md=" + sha256Hex("x")}, "which is not one of the declared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"review", "--report", "--profile", "fake-smoke"}, tc.args...)
			code, _, errs := run(t, append(args, ws)...)
			if code != int(fault.Usage) {
				t.Errorf("exit = %d, want %d; stderr=%s", code, fault.Usage, errs)
			}
			if !strings.Contains(errs, tc.want) {
				t.Errorf("stderr should explain the problem (%q):\n%s", tc.want, errs)
			}
		})
	}
}

// Flags may appear before OR after the positional path — the authority flags take a value, so
// splitArgs must know about them or the path would be swallowed.
func TestCLIAuthority_FlagOrderIndependent(t *testing.T) {
	hermeticReview(t, "valid")
	ws, specPath := authorityWorkspace(t, "spec\n")
	code, out, errs := run(t, "review", ws, "--report", "--json", "--profile", "fake-smoke", "--authority", specPath)
	if code != int(fault.OK) {
		t.Fatalf("exit=%d stderr=%s", code, errs)
	}
	var view runview.View
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Authority) != 1 {
		t.Errorf("authority flag after the path was not honored: %+v", view.Authority)
	}
}

// strconvQuote JSON-quotes a path (Windows separators need escaping in a JSON string).
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
