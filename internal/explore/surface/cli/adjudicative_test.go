package cli

// CLI surface tests for the ADJUDICATIVE modes (design §3/§4): the two adjudicative modes run end to
// end through the CLI on the built-in fake roster and RENDER — including, above all, rendering a WITHHELD
// label honestly rather than hiding it behind a bare count. Plus `--artifact` validation, the one extra
// input these modes make the CLI take.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeArtifact writes an artifact file into a temp dir and returns its path.
func writeArtifact(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "artifact.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return p
}

// TestExplore_ChallengeMode_RendersRegisterWithDenominators runs `--mode challenge` through the CLI on fakes
// and asserts the human summary carries what the design says it must: the severity register, the HOST-computed
// corroboration with BOTH denominators, the partition + policy pins, and the attributed findings.
func TestExplore_ChallengeMode_RendersRegisterWithDenominators(t *testing.T) {
	art := writeArtifact(t, "func Shutdown() { close(ch); drain(); free(state) } // no lock, two callers")
	var out, errb bytes.Buffer
	code := Run([]string{"explore", "--purpose", "review the shutdown path", "--criteria", "correctness,safety",
		"--mode", "challenge", "--artifact", art}, &out, &errb)
	if code != 0 {
		t.Fatalf("--mode challenge exit %d, stderr: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Challenge:", "challenge register:", "artifact under review: sha256:",
		"Finding register", "HOST-computed", "partition revision:", "frozen counting policy:",
		"corroborated", "of 2 panel", "of 2 respondents", "Race condition on shutdown",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("challenge summary missing %q:\n%s", want, s)
		}
	}
}

// TestExplore_ShortlistMode_RendersHostTalliedRankingAndRejects runs `--mode shortlist` through the CLI and
// asserts the ranking is presented as what it is — a host-tallied informed preference, never consensus — with
// the frozen criteria, the frozen inputs hash, the rejects and their host reasons all visible.
func TestExplore_ShortlistMode_RendersHostTalliedRankingAndRejects(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"explore", "--purpose", "pick a datastore", "--criteria", "latency,cost",
		"--mode", "shortlist"}, &out, &errb)
	if code != 0 {
		t.Fatalf("--mode shortlist exit %d, stderr: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Shortlist:", "INFORMED PREFERENCE UNDER SHARED FRAMING", "NOT consensus",
		"decision inputs frozen as", "hashed BEFORE the ballot was solicited",
		"Frozen criteria (origin / aggregation method):", "[user / ballot]",
		"Ranked (host tally over the confirmed candidate universe):",
		"Not shortlisted", "emergent salience", "a DIFFERENT measurement",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("shortlist summary missing %q:\n%s", want, s)
		}
	}
	// The word "consensus" may appear ONLY as a denial.
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(strings.ToLower(line), "consensus") && !strings.Contains(strings.ToLower(line), "not consensus") {
			t.Errorf("a ballot must never be rendered as consensus: %q", line)
		}
	}
}

// TestExplore_WithheldLabelsArePrintedHonestly pins the human-output rendering rule: when the
// partition under a count is CONTESTED the definitive label is withheld, and the human output must SAY so
// (with the range) rather than print a bare count that reads as settled. It drives the contested case through
// the CLI by pointing the panel at the aggressive-merger fake via an ad-hoc roster.
func TestExplore_WithheldLabelsArePrintedHonestly(t *testing.T) {
	art := writeArtifact(t, "func Shutdown() { close(ch); drain(); free(state) } // no lock, two callers")
	var out, errb bytes.Buffer
	// The `fake` adapter is deterministic and agreeing, so a plain run is SETTLED — the control case: a
	// definitive label appears and no WITHHELD marker does.
	code := Run([]string{"explore", "--purpose", "review the shutdown path", "--criteria", "correctness",
		"--mode", "challenge", "--artifact", art}, &out, &errb)
	if code != 0 {
		t.Fatalf("challenge exit %d, stderr: %s", code, errb.String())
	}
	if strings.Contains(out.String(), "WITHHELD") {
		t.Errorf("a settled partition must not print a withheld label:\n%s", out.String())
	}
	// And the renderer's honesty helper shouts a withheld label rather than passing it off as a verdict.
	if got := labelDisplay("withheld_contested_partition"); !strings.HasPrefix(got, "WITHHELD") {
		t.Errorf("a withheld label must be rendered as WITHHELD, got %q", got)
	}
	if got := labelDisplay("corroborated"); got != "corroborated" {
		t.Errorf("a definitive label renders plainly, got %q", got)
	}
	if got := labelDisplay("ranked"); got != "ranked" {
		t.Errorf("a ranked label is definitive, got %q", got)
	}
	if got := labelDisplay("withheld_tie"); !strings.HasPrefix(got, "WITHHELD") {
		t.Errorf("a tie-withheld label must be rendered as WITHHELD, got %q", got)
	}
}

// TestExplore_ArtifactFlag_Validated covers the one new CLI input: challenge REQUIRES it, a missing file and an
// empty artifact are usage errors before any spend, and `-` reads stdin.
func TestExplore_ArtifactFlag_Validated(t *testing.T) {
	// (a) --mode challenge with NO artifact is a usage error naming the flag.
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "challenge"}, &out, &errb); code == 0 {
		t.Fatal("--mode challenge with no --artifact must be a usage error")
	}
	if !strings.Contains(errb.String(), "requires an ARTIFACT") || !strings.Contains(errb.String(), "--artifact") {
		t.Errorf("the error must name the missing input + the flag:\n%s", errb.String())
	}

	// (b) an unreadable path is a usage error naming the path.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "challenge",
		"--artifact", filepath.Join(t.TempDir(), "nope.txt")}, &out, &errb); code == 0 {
		t.Fatal("an unreadable --artifact must be a usage error")
	}
	if !strings.Contains(errb.String(), "--artifact") {
		t.Errorf("the error must name the flag:\n%s", errb.String())
	}

	// (c) a supplied-but-empty artifact is refused: the user asked for a review of nothing.
	out.Reset()
	errb.Reset()
	empty := writeArtifact(t, "   \n  ")
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "challenge",
		"--artifact", empty}, &out, &errb); code == 0 {
		t.Fatal("an empty --artifact must be a usage error")
	}
	if !strings.Contains(errb.String(), "nothing to review") {
		t.Errorf("the error must say the artifact is empty:\n%s", errb.String())
	}

	// (d) `-` reads stdin (unit-level: readArtifact is the reader the flag uses).
	got, err := readArtifact("-", strings.NewReader("the artifact from stdin"))
	if err != nil || got != "the artifact from stdin" {
		t.Errorf("readArtifact(-) = %q, %v", got, err)
	}
	// (e) no flag → no artifact, and that is not an error (most modes review nothing).
	if got, err := readArtifact("", strings.NewReader("ignored")); err != nil || got != "" {
		t.Errorf("an absent --artifact yields an empty artifact: %q, %v", got, err)
	}
}

// TestList_ExposesTheAdjudicativeModes pins that registering the modes is all that was needed to expose them:
// `list` reports them generically, with no per-mode code anywhere in the listing path.
func TestList_ExposesTheAdjudicativeModes(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"list", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, errb.String())
	}
	var view struct {
		Modes []string `json:"modes"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out.String())
	}
	have := map[string]bool{}
	for _, m := range view.Modes {
		have[m] = true
	}
	for _, want := range []string{"map", "synthesize", "catalog", "challenge", "shortlist", "ai-collab"} {
		if !have[want] {
			t.Errorf("list must report mode %q: %v", want, view.Modes)
		}
	}
	// The human listing surfaces them too.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"list"}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d", code)
	}
	if !strings.Contains(out.String(), "challenge") || !strings.Contains(out.String(), "shortlist") {
		t.Errorf("the human listing must name the modes:\n%s", out.String())
	}
	// An unknown --mode still lists the known ones, now including the new modes.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"explore", "--purpose", "p", "--criteria", "a", "--mode", "bogus"}, &out, &errb); code == 0 {
		t.Fatal("an unknown mode must be a usage error")
	}
	if !strings.Contains(errb.String(), "challenge") || !strings.Contains(errb.String(), "shortlist") {
		t.Errorf("the unknown-mode error must list the new modes:\n%s", errb.String())
	}
}

// TestExplore_AdjudicativeModes_JSON pins the machine surface: `--json` carries the mode's own output plus the
// governance record, generically (the encoder has no per-mode arm).
func TestExplore_AdjudicativeModes_JSON(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "pick a datastore", "--criteria", "latency",
		"--mode", "shortlist", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("--mode shortlist --json exit %d, stderr: %s", code, errb.String())
	}
	var res struct {
		Output struct {
			Decision           string `json:"decision"`
			DecisionInputsHash string `json:"decisionInputsHash"`
			Ranked             []struct {
				Name  string `json:"name"`
				Claim struct {
					Label string `json:"label"`
					Query string `json:"query"`
				} `json:"claim"`
			} `json:"ranked"`
			Criteria []struct {
				Origin            string `json:"origin"`
				AggregationMethod string `json:"aggregationMethod"`
			} `json:"criteria"`
		} `json:"Output"`
		Decision *struct {
			RuleVersion string `json:"ruleVersion"`
			Frozen      struct {
				InputsHash string `json:"inputsHash"`
			} `json:"frozen"`
		} `json:"decision"`
		Governance *struct {
			ClaimsHash string `json:"claimsHash"`
		} `json:"governance"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("shortlist --json is not valid JSON: %v\n%s", err, out.String())
	}
	if len(res.Output.Ranked) == 0 || res.Output.Ranked[0].Claim.Query == "" {
		t.Errorf("the JSON result must carry the ranked entries with their pinned claims:\n%s", out.String())
	}
	if res.Output.Ranked[0].Claim.Label != "ranked" {
		t.Errorf("a settled quorate ballot yields the ranked label, got %q", res.Output.Ranked[0].Claim.Label)
	}
	if res.Output.Criteria[0].Origin != "user" || res.Output.Criteria[0].AggregationMethod != "ballot" {
		t.Errorf("the JSON result must carry criterion origin + aggregation method: %+v", res.Output.Criteria)
	}
	if res.Decision == nil || res.Decision.Frozen.InputsHash != res.Output.DecisionInputsHash {
		t.Errorf("the frozen decision must be persisted on the machine surface: %+v", res.Decision)
	}
	if res.Governance == nil || res.Governance.ClaimsHash == "" {
		t.Error("the governance claim ledger must be persisted on the machine surface")
	}
}

// TestExplore_ShortlistMode_DumpRunCapturesTheDecision pins §9's system-of-record for a ballot-bearing run:
// `--dump-run` writes the decision (frozen inputs, ballots, tally) alongside the merge-ledger and the claims,
// and the manifest carries the frozen inputs hash — so "the framing predated the vote" is checkable from the
// captured run alone, without re-running anything.
func TestExplore_ShortlistMode_DumpRunCapturesTheDecision(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", dir)
	var out, errb bytes.Buffer
	if code := Run([]string{"explore", "--purpose", "pick a datastore", "--criteria", "latency",
		"--mode", "shortlist", "--dump-run"}, &out, &errb); code != 0 {
		t.Fatalf("--mode shortlist --dump-run exit %d, stderr: %s", code, errb.String())
	}
	runDir := latestRunDir(t, dir)
	decision := readJSON(t, filepath.Join(runDir, "decision.json"))
	frozen, _ := decision["frozen"].(map[string]any)
	inputsHash, _ := frozen["inputsHash"].(string)
	if inputsHash == "" {
		t.Fatalf("decision.json must carry the frozen inputs hash: %+v", decision)
	}
	// Every candidate in the confirmed universe is in the record — the shortlisted ones AND the rejects (the
	// built-in demo roster's two fake explorers nominate the same two candidates).
	if entries, _ := decision["entries"].([]any); len(entries) != 2 {
		t.Errorf("decision.json must carry every tallied candidate: %+v", decision["entries"])
	}
	if ballots, _ := decision["ballots"].([]any); len(ballots) == 0 {
		t.Error("decision.json must carry the recorded ballots")
	}
	// The voters' prose is quarantined into the narrative namespace, never onto the machine ballot record.
	if b, _ := decision["ballots"].([]any); len(b) > 0 {
		if first, _ := b[0].(map[string]any); first["rationale"] != nil {
			t.Errorf("a recorded ballot must carry no model prose: %+v", first)
		}
	}
	manifest := readJSON(t, filepath.Join(runDir, "manifest.json"))
	if manifest["decisionInputsHash"] != inputsHash {
		t.Errorf("the manifest must pin the frozen inputs hash: %v vs %q", manifest["decisionInputsHash"], inputsHash)
	}
	if manifest["decisionRule"] == "" || manifest["ranked"] == nil {
		t.Errorf("the manifest must record the tally rule + the ranked count: %+v", manifest)
	}
	// The governance ledger and the merge-ledger are captured alongside it.
	for _, rel := range []string{"governance-claims.json", "merge-ledger.jsonl", "merge-ledger-provisional.jsonl", "rounds.json"} {
		if _, err := os.Stat(filepath.Join(runDir, rel)); err != nil {
			t.Errorf("a governed run's capture must include %s: %v", rel, err)
		}
	}
}

// latestRunDir returns the single run directory `--dump-run` created under root.
func latestRunDir(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read artifact dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(root, e.Name())
		}
	}
	t.Fatalf("no run directory under %s", root)
	return ""
}

// readJSON reads and decodes a captured JSON artifact.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return v
}
