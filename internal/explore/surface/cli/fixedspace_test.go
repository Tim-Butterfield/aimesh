package cli

// CLI tests for the FIXED-SPACE modes, plus the derived SQLite export command. What they
// pin, beyond the plumbing, is that the RENDERING is honest — a comparison must not read like a scoreboard,
// a disagreed cell must be visible as a disagreement, and the absence of a scalar ranking must be stated
// rather than merely left out.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// compareArgs is a full `--mode compare` invocation over a declared space (weights omitted unless given).
func compareArgs(extra ...string) []string {
	base := []string{"explore",
		"--purpose", "choose a store", "--criteria", "operability",
		"--mode", "compare",
		"--options", "alpha,beta,gamma",
		"--criterion", "name=throughput,direction=higher_is_better,role=dimension",
		"--criterion", "name=cost,direction=lower_is_better,role=dimension",
	}
	return append(base, extra...)
}

// forecastArgs is a full `--mode forecast` invocation over a declared target. It uses an AD-HOC three-model
// panel rather than the two-slot demo roster: the demo roster is two explorers, and an outlier rule needs a
// majority to be far from — with two estimates neither is the outlier, which is the honest behavior but not
// the one this rendering test is about.
func forecastArgs(extra ...string) []string {
	base := []string{"explore",
		"--purpose", "size the migration", "--criteria", "account for the trend",
		"--mode", "forecast",
		"--target", "peak requests per second", "--unit", "requests/second", "--horizon", "the next 12 months",
		"--explorer", "adapter=fake,model=panel-a,effort=high",
		"--explorer", "adapter=fake,model=panel-b,effort=high",
		"--explorer", "adapter=fake,model=panel-c,effort=high",
		"--collator", "adapter=fake,model=panel-collator,effort=high",
	}
	return append(base, extra...)
}

// TestExplore_CompareMode_RendersMatrixParetoAndDisagreement runs `--mode compare` end to end and pins the
// rendering: the Pareto frontier (not a winner), the per-cell disagreement in capitals with every value
// retained, the filter-gate exclusion, and the stated absence of a scalar ranking.
func TestExplore_CompareMode_RendersMatrixParetoAndDisagreement(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run(compareArgs("--criterion", "name=on-prem support,role=filter"), &out, &errb)
	if code != 0 {
		t.Fatalf("--mode compare exit %d, stderr: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Compare:", "FIXED-SPACE comparison", "no entity resolution",
		"partition revision: NONE",
		"Pareto frontier", "not a ranking",
		"DISAGREEMENT across",
		"Excluded by a FILTER GATE", "gamma",
		"NO SCALAR RANKING", "weighting",
		"Matrix (every cell keeps EVERY explorer's value",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("compare rendering missing %q:\n%s", want, s)
		}
	}
	// Both disputed values must be printed — the disagreement is the finding, and an average would erase it.
	if strings.Count(s, "· 9 ←") == 0 || strings.Count(s, "· 5 ←") == 0 {
		t.Errorf("both values of the disagreed cell must appear verbatim:\n%s", s)
	}
}

// TestExplore_CompareMode_WeightsLicenseARanking is the other half of the ranking rule at the surface: with
// a weight on every scored criterion the CLI prints the ranking, still labeled as a view of the same matrix.
func TestExplore_CompareMode_WeightsLicenseARanking(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"explore",
		"--purpose", "choose a store", "--criteria", "operability", "--mode", "compare",
		"--options", "alpha,beta,gamma",
		"--criterion", "name=throughput,direction=higher_is_better,weight=3",
		"--criterion", "name=cost,direction=lower_is_better,weight=1",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("weighted compare exit %d, stderr: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "Weighted ranking") || !strings.Contains(s, "not a verdict") {
		t.Errorf("a weighted ranking must be printed AND labeled:\n%s", s)
	}
	if strings.Contains(s, "NO SCALAR RANKING") {
		t.Errorf("a computed ranking must not also print the refusal:\n%s", s)
	}
}

// TestExplore_ForecastMode_RendersPooledEstimateAndOutlier pins the forecast rendering: who computed the
// aggregate, the dispersion beside it, and the outlier WITH its forecaster's own reasoning.
func TestExplore_ForecastMode_RendersPooledEstimateAndOutlier(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run(forecastArgs(), &out, &errb)
	if code != 0 {
		t.Fatalf("--mode forecast exit %d, stderr: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Forecast:", "FIXED-SPACE forecast", "partition revision: NONE",
		"peak requests per second = 110 requests/second",
		"interval:", "dispersion:", "method: median",
		"computed by the HOST",
		"Individual estimates", "Outliers (IDENTIFIED, not discarded",
		"forecaster's own reasoning:", "step change",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("forecast rendering missing %q:\n%s", want, s)
		}
	}
}

// TestExplore_FixedSpaceModes_ValidateTheDeclaredSpace pins the flag-level refusals: an under-declared
// fixed-space task fails as a usage error naming what to supply, before any panel is touched.
func TestExplore_FixedSpaceModes_ValidateTheDeclaredSpace(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"compare without options", []string{"explore", "--purpose", "p", "--criteria", "c", "--mode", "compare",
			"--criterion", "name=cost,direction=lower_is_better"}, "OPTION SET"},
		{"compare without criteria", []string{"explore", "--purpose", "p", "--criteria", "c", "--mode", "compare",
			"--options", "a,b"}, "declared CRITERIA"},
		{"criterion with no direction", []string{"explore", "--purpose", "p", "--criteria", "c", "--mode", "compare",
			"--options", "a,b", "--criterion", "name=cost"}, "must declare its direction"},
		{"criterion with an unknown key", []string{"explore", "--purpose", "p", "--criteria", "c", "--mode", "compare",
			"--options", "a,b", "--criterion", "name=cost,scale=log"}, "unknown key"},
		{"forecast without a unit", []string{"explore", "--purpose", "p", "--criteria", "c", "--mode", "forecast",
			"--target", "t", "--horizon", "h"}, "unit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := Run(tc.args, &out, &errb); code != 2 {
				t.Fatalf("expected a usage error (exit 2), got %d; stderr: %s", code, errb.String())
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("the error must name what to supply (%q): %s", tc.want, errb.String())
			}
		})
	}
}

// TestList_ExposesTheFixedSpaceModes pins that both modes flow generically into `list` and `--mode` — no
// surface had to learn their names.
func TestList_ExposesTheFixedSpaceModes(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"list", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("list --json exit %d: %s", code, errb.String())
	}
	var view struct {
		Modes []string `json:"modes"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out.String())
	}
	names := strings.Join(view.Modes, ",")
	for _, want := range []string{"map", "synthesize", "catalog", "challenge", "shortlist", "ai-collab", "compare", "forecast"} {
		if !strings.Contains(names, want) {
			t.Errorf("list is missing mode %q: %s", want, names)
		}
	}
}

// TestExplore_FixedSpaceModes_JSON pins the machine surface: the terminal output serializes with the
// host-computed structures and the honest no-partition statement.
func TestExplore_FixedSpaceModes_JSON(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(compareArgs("--json"), &out, &errb); code != 0 {
		t.Fatalf("compare --json exit %d: %s", code, errb.String())
	}
	var res struct {
		Mode   string `json:"mode"`
		Output struct {
			PartitionRevisionHash string           `json:"partitionRevisionHash"`
			Cells                 []map[string]any `json:"cells"`
			Pareto                []map[string]any `json:"pareto"`
			RankingWithheld       string           `json:"rankingWithheld"`
			Rules                 map[string]any   `json:"rules"`
		} `json:"Output"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("compare --json is not valid JSON: %v\n%s", err, out.String())
	}
	if res.Mode != "compare" {
		t.Errorf("the executed mode must be stamped on the result, got %q", res.Mode)
	}
	if len(res.Output.Cells) != 6 || len(res.Output.Pareto) == 0 {
		t.Errorf("compare JSON: %d cell(s), %d frontier entr(ies)", len(res.Output.Cells), len(res.Output.Pareto))
	}
	if !strings.Contains(res.Output.PartitionRevisionHash, "no entity resolution") {
		t.Errorf("the machine surface must carry the no-partition STATEMENT, got %q", res.Output.PartitionRevisionHash)
	}
	if res.Output.RankingWithheld == "" || res.Output.Rules["paretoRule"] == "" {
		t.Errorf("the withheld ranking and the rule versions must be on the machine surface: %+v", res.Output)
	}
}

// TestExport_SQLiteFromADumpedRun is the export command end to end: `explore --dump-run` writes the system
// of record, `export --sqlite --verify` derives the database from it and proves the derivation.
func TestExport_SQLiteFromADumpedRun(t *testing.T) {
	artifactDir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", artifactDir)
	var out, errb bytes.Buffer
	if code := Run(compareArgs("--dump-run"), &out, &errb); code != 0 {
		t.Fatalf("compare --dump-run exit %d: %s", code, errb.String())
	}
	runDir := latestRunDir(t, artifactDir)
	// The DECLARED task is part of the system of record — a fixed-space result cannot be reproduced without
	// the space it was computed over.
	task := readJSON(t, filepath.Join(runDir, "task.json"))
	if task["mode"] != "compare" || task["options"] == nil || task["compareCriteria"] == nil {
		t.Errorf("task.json must record the declared space: %+v", task)
	}

	dbPath := filepath.Join(t.TempDir(), "evidence.db")
	out.Reset()
	errb.Reset()
	if code := Run([]string{"export", "--sqlite", dbPath, "--run", runDir, "--verify"}, &out, &errb); code != 0 {
		t.Fatalf("export exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("the export wrote no database: %v", err)
	}
	for _, want := range []string{"Exported", "mode compare", "DERIVED, rebuildable view", "governance_claims"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("export output missing %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(errb.String(), "byte-identical to a fresh export") {
		t.Errorf("--verify must report the derivability check: %s", errb.String())
	}
	// The JSON projection carries the same summary. This second export targets the SAME path, so it needs
	// the explicit opt-in: a destination that already exists is never replaced silently, not even by a
	// rebuild of a derived database.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"export", "--sqlite", dbPath, "--run", runDir, "--force", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("export --json exit %d: %s", code, errb.String())
	}
	var sum struct {
		ExplorationID string         `json:"explorationId"`
		Mode          string         `json:"mode"`
		Rows          map[string]int `json:"rows"`
	}
	if err := json.Unmarshal(out.Bytes(), &sum); err != nil {
		t.Fatalf("export --json is not valid JSON: %v\n%s", err, out.String())
	}
	if sum.ExplorationID == "" || sum.Mode != "compare" || sum.Rows["governance_claims"] == 0 {
		t.Errorf("export summary: %+v", sum)
	}
}

// TestExport_RequiresBothFlags pins the usage contract: the export is explicit about what it reads and what
// it writes, and never guesses either.
func TestExport_RequiresBothFlags(t *testing.T) {
	for _, args := range [][]string{
		{"export"},
		{"export", "--sqlite", "x.db"},
		{"export", "--run", "somewhere"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 2 {
			t.Errorf("%v: expected a usage error, got %d", args, code)
		}
	}
	// A run directory that is not one fails cleanly rather than writing a half-built database. The
	// user-supplied `--run` value naming nothing exportable is a CONFIGURATION fault (exit 3), not the
	// taxonomy's internal catch-all — the input was wrong, the export was not broken.
	var out, errb bytes.Buffer
	if code := Run([]string{"export", "--sqlite", filepath.Join(t.TempDir(), "x.db"), "--run", t.TempDir()}, &out, &errb); code != int(fault.Config) {
		t.Errorf("an empty directory must fail the export with the config code %d, got exit %d", int(fault.Config), code)
	}
	if !strings.Contains(errb.String(), "did not finish writing") {
		t.Errorf("the failure must explain what a missing manifest means: %s", errb.String())
	}
}
