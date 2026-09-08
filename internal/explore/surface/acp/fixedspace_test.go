package acp_test

// ACP tests for the FIXED-SPACE modes: the task inputs arrive through `_meta.exploremesh`, an under-declared space is
// invalid-params (never a run over a space nobody declared), and the response echo carries the honest
// fixed-space facts — the frontier as a SET, the disagreed-cell count beside it, the withheld ranking, and
// the explicit statement that there is no partition because no entity resolution was performed.

import (
	"context"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"

	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// fixedSpaceExplorer records the task it received and returns a canned FIXED-SPACE result, so these tests
// exercise the SURFACE (parsing, validation, echo) rather than the pipeline.
type fixedSpaceExplorer struct {
	gotRaw *schema.RawTask
	result pipeline.Result
}

func (f *fixedSpaceExplorer) Run(_ context.Context, _ roster.Plan, raw schema.RawTask, _ pipeline.Options, _ func(audit.EventLine)) (pipeline.Result, error) {
	cp := raw
	f.gotRaw = &cp
	return f.result, nil
}

// compareResult builds a minimal but honest CompareOutput result for the echo test: one SPLIT cell, a
// one-option frontier, a withheld ranking, and a claim carrying the no-partition statement.
func compareResult() pipeline.Result {
	pan, _ := govern.Freeze([]schema.ExplorerIdentity{{Adapter: "a", Model: "m1"}, {Adapter: "b", Model: "m2"}}, govern.DefaultCountingPolicy())
	pan = pan.WithOutcome(govern.Outcome{Dispatched: 2, Eligible: 2})
	ledger := &govern.ClaimLedger{}
	ledger.Emit(govern.Claim{
		Query: govern.AgreementQuery, Subject: govern.CellSubject("alpha", "speed"),
		SubjectLabel: "alpha on speed", Value: 1,
		KOfPanel:              govern.Denominator{K: 1, M: 2, Basis: govern.BasisPanel},
		KOfRespondents:        govern.Denominator{K: 1, M: 2, Basis: govern.BasisRespondents},
		PartitionRevisionHash: govern.FixedSpaceNoPartition, RulesVersion: govern.RulesVersion,
		BaselineRoundID: "round-1", Label: govern.LabelWithheldDisagreement,
	})
	report := govern.NewReport(pan, ledger, nil)
	return pipeline.Result{
		Mode:           mode.Compare,
		Formulation:    schema.Formulation{Source: schema.FormulationFreeMap},
		CollatorStatus: schema.IdentityVerified,
		Envelopes:      []schema.Envelope{{}, {}},
		Panel:          pan,
		Governance:     &report,
		Output: mode.CompareOutput{
			Space:                 string(schema.FixedSpace),
			SpaceNote:             "FIXED-SPACE comparison: no entity resolution was performed.",
			PartitionRevisionHash: govern.FixedSpaceNoPartition,
			Options:               []string{"alpha", "beta"},
			Criteria:              []schema.CompareCriterion{{Name: "speed", Direction: schema.HigherIsBetter}},
			Cells: []govern.CompareCell{
				{Option: "alpha", Criterion: "speed", Agreement: govern.AgreementSplit, Sources: 2},
				{Option: "beta", Criterion: "speed", Agreement: govern.AgreementUnanimous, Sources: 2},
			},
			Pareto:          []govern.ParetoEntry{{Option: "alpha"}},
			Dominated:       []govern.DominatedOption{{Option: "beta"}},
			RankingWithheld: "NO SCALAR RANKING was computed: no weights were declared.",
		},
	}
}

// TestServer_FixedSpaceTaskInputsViaMeta pins the task-input half: the declared option set, the criteria
// (with direction, role and weight) and the estimation target arrive verbatim through `_meta.exploremesh`.
func TestServer_FixedSpaceTaskInputsViaMeta(t *testing.T) {
	exp := &fixedSpaceExplorer{result: compareResult()}
	client, stop := serve(t, exp)
	defer stop()
	if _, err := client.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sn, _ := client.Call("session/new", map[string]any{"cwd": "/tmp/x"})
	sid, _ := sn.Result["sessionId"].(string)

	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "choose a store"}},
		"_meta": map[string]any{"exploremesh": map[string]any{
			"criteria": []string{"operability"},
			"mode":     "compare",
			"options":  []string{"alpha", "beta"},
			"compareCriteria": []any{
				map[string]any{"name": "speed", "direction": "higher_is_better", "role": "dimension", "weight": 2},
				map[string]any{"name": "on-prem", "role": "filter"},
			},
		}},
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("compare prompt: err=%v rpc=%+v", err, resp.Error)
	}
	if exp.gotRaw == nil {
		t.Fatal("the explorer received no task")
	}
	got := *exp.gotRaw
	if got.Mode != mode.Compare || len(got.Options) != 2 {
		t.Fatalf("the declared option set was not carried: %+v", got)
	}
	if len(got.CompareCriteria) != 2 {
		t.Fatalf("the declared criteria were not carried: %+v", got.CompareCriteria)
	}
	if got.CompareCriteria[0].Direction != schema.HigherIsBetter || got.CompareCriteria[0].Weight != 2 {
		t.Errorf("a criterion's DIRECTION and WEIGHT must survive the wire — the host's Pareto rule reads one and the ranking rule the other: %+v", got.CompareCriteria[0])
	}
	if got.CompareCriteria[1].EffectiveRole() != schema.RoleFilter {
		t.Errorf("a filter GATE must survive the wire: %+v", got.CompareCriteria[1])
	}
}

// TestServer_ForecastTargetViaMeta pins the same for the declared estimation target.
func TestServer_ForecastTargetViaMeta(t *testing.T) {
	exp := &fixedSpaceExplorer{result: pipeline.Result{Mode: mode.Forecast, Formulation: schema.Formulation{Source: schema.FormulationFreeMap}}}
	client, stop := serve(t, exp)
	defer stop()
	if _, err := client.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sn, _ := client.Call("session/new", map[string]any{"cwd": "/tmp/x"})
	sid, _ := sn.Result["sessionId"].(string)
	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "size the migration"}},
		"_meta": map[string]any{"exploremesh": map[string]any{
			"criteria": []string{"trend"}, "mode": "forecast",
			"target": "peak rps", "unit": "requests/second", "horizon": "12 months",
			"conditioningEvent": "the capacity constraint lifts",
		}},
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("forecast prompt: err=%v rpc=%+v", err, resp.Error)
	}
	got := *exp.gotRaw
	if got.Target != "peak rps" || got.Unit != "requests/second" || got.Horizon != "12 months" ||
		got.ConditioningEvent != "the capacity constraint lifts" {
		t.Errorf("the declared estimation target must survive the wire: %+v", got)
	}
}

// TestServer_UnderDeclaredFixedSpaceTaskIsInvalidParams pins the fail-closed posture: a fixed-space mode
// whose space was not declared is invalid-params with a message naming what to supply — the same posture
// absent criteria and an unknown profile already take, and the refusal happens before any dispatch.
func TestServer_UnderDeclaredFixedSpaceTaskIsInvalidParams(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]any
		want string
	}{
		{"compare with no option set", map[string]any{
			"criteria": []string{"c"}, "mode": "compare",
			"compareCriteria": []any{map[string]any{"name": "speed", "direction": "higher_is_better"}},
		}, "OPTION SET"},
		{"compare with no criteria", map[string]any{
			"criteria": []string{"c"}, "mode": "compare", "options": []string{"a", "b"},
		}, "declared CRITERIA"},
		{"compare dimension with no direction", map[string]any{
			"criteria": []string{"c"}, "mode": "compare", "options": []string{"a", "b"},
			"compareCriteria": []any{map[string]any{"name": "speed"}},
		}, "must declare its direction"},
		{"forecast with no horizon", map[string]any{
			"criteria": []string{"c"}, "mode": "forecast", "target": "t", "unit": "u",
		}, "horizon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp := &fixedSpaceExplorer{}
			client, stop := serve(t, exp)
			defer stop()
			if _, err := client.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			sn, _ := client.Call("session/new", map[string]any{"cwd": "/tmp/x"})
			sid, _ := sn.Result["sessionId"].(string)
			resp, err := client.Call("session/prompt", map[string]any{
				"sessionId": sid,
				"prompt":    []any{map[string]any{"type": "text", "text": "do the thing"}},
				"_meta":     map[string]any{"exploremesh": tc.meta},
			})
			if err != nil {
				t.Fatalf("session/prompt: %v", err)
			}
			if resp.Error == nil || resp.Error.Code != -32602 {
				t.Fatalf("an under-declared fixed-space task must be invalid-params, got %+v", resp.Error)
			}
			if !contains(resp.Error.Message, tc.want) {
				t.Errorf("the error must name what to supply (%q): %q", tc.want, resp.Error.Message)
			}
			if exp.gotRaw != nil {
				t.Error("the refusal must happen BEFORE the exploration is dispatched")
			}
		})
	}
}

// TestServer_FixedSpaceEchoIsHonest pins the response echo: the frontier as a SET (never a winner), the
// disagreed-cell count beside it, the withheld ranking carried verbatim, and the explicit statement that
// there is no partition. A driver that read only "pareto: 1" would otherwise present a trade-off with a
// disputed cell under it as a settled comparison.
func TestServer_FixedSpaceEchoIsHonest(t *testing.T) {
	client, stop := serve(t, &fixedSpaceExplorer{result: compareResult()})
	defer stop()
	if _, err := client.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sn, _ := client.Call("session/new", map[string]any{"cwd": "/tmp/x"})
	sid, _ := sn.Result["sessionId"].(string)
	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "choose a store"}},
		"_meta": map[string]any{"exploremesh": map[string]any{
			"criteria": []string{"operability"}, "mode": "compare", "options": []string{"alpha", "beta"},
			"compareCriteria": []any{map[string]any{"name": "speed", "direction": "higher_is_better"}},
		}},
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("prompt: err=%v rpc=%+v", err, resp.Error)
	}
	em := exploreMetaOf(t, resp.Result)
	pareto, ok := em["pareto"].([]any)
	if !ok || len(pareto) != 1 || pareto[0] != "alpha" {
		t.Errorf("the frontier must be echoed as a SET of options, not a winner: %v", em["pareto"])
	}
	if em["disagreedCells"] != float64(1) {
		t.Errorf("the disagreed-cell count must travel beside the frontier, got %v", em["disagreedCells"])
	}
	if s, _ := em["rankingWithheld"].(string); !contains(s, "NO SCALAR RANKING") {
		t.Errorf("the withheld ranking must be echoed, got %v", em["rankingWithheld"])
	}
	if s, _ := em["spaceNote"].(string); !contains(s, "no entity resolution") {
		t.Errorf("the space note must be echoed, got %v", em["spaceNote"])
	}
	gov, ok := em["governance"].(map[string]any)
	if !ok {
		t.Fatalf("no governance echo: %v", em["governance"])
	}
	if s, _ := gov["partitionRevisionHash"].(string); s != govern.FixedSpaceNoPartition {
		t.Errorf("the governance echo must STATE the absent partition rather than omitting the field, got %v", gov["partitionRevisionHash"])
	}
	if s, _ := gov["entityResolution"].(string); !contains(s, "none") {
		t.Errorf("the governance echo must say no entity resolution happened, got %v", gov["entityResolution"])
	}
	if gov["claimsWithheld"] != float64(1) {
		t.Errorf("a withheld cell claim must be counted as withheld, got %v", gov["claimsWithheld"])
	}
}

// TestServer_FixedSpaceModesAreSelectable pins that both modes flow generically into `_meta.mode` — the
// surface learned no names.
func TestServer_FixedSpaceModesAreSelectable(t *testing.T) {
	for _, name := range []string{mode.Compare, mode.Forecast} {
		if _, ok := mode.Lookup(name); !ok {
			t.Errorf("mode %q must be registered and therefore selectable over ACP", name)
		}
	}
}
