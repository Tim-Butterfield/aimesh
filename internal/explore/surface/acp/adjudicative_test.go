package acp_test

// ACP surface tests for the ADJUDICATIVE modes (design §3/§4): the ARTIFACT input a reviewing mode
// needs, and the GOVERNANCE SUMMARY echoed back in the PromptResponse `_meta.exploremesh` — including the
// withheld/contested signal, which is the part a driver would otherwise present as a settled result.

import (
	"context"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"

	"github.com/Tim-Butterfield/aimesh/internal/explore/canon"
	"github.com/Tim-Butterfield/aimesh/internal/explore/govern"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// governedExplorer is a deterministic in-process Explorer returning a GOVERNED result: a frozen panel, a
// confirmed-but-CONTESTED partition, one corroborated and one withheld claim, and a host tally. It exists so
// the `_meta` echo can be asserted without running a pipeline inside the ACP test.
type governedExplorer struct {
	gotRaw *schema.RawTask
}

func (g *governedExplorer) Run(_ context.Context, _ roster.Plan, raw schema.RawTask, _ pipeline.Options, _ func(audit.EventLine)) (pipeline.Result, error) {
	g.gotRaw = &raw
	pan, _ := govern.Freeze([]schema.ExplorerIdentity{{Adapter: "a", Model: "m1"}, {Adapter: "b", Model: "m2"}}, govern.DefaultCountingPolicy())
	pan = pan.WithOutcome(govern.Outcome{Dispatched: 2, Eligible: 2})
	ledger := &govern.ClaimLedger{}
	ledger.Emit(govern.Claim{Query: govern.CorroborationQuery, Subject: "canon-0", Value: 2, Label: govern.LabelCorroborated})
	ledger.Emit(govern.Claim{
		Query: govern.CorroborationQuery, Subject: "canon-1", Value: 1, Label: govern.LabelWithheldContested,
		Sensitivity: &govern.Sensitivity{Low: 1, High: 2, ContestedCanonicalIDs: []string{"canon-1"}},
	})
	report := govern.NewReport(pan, ledger, []govern.Narrative{{Phase: schema.PhaseCanonicalize, Prose: "coverage notes"}})
	return pipeline.Result{
		Formulation:    schema.Formulation{Source: schema.FormulationFreeMap},
		CollatorStatus: schema.IdentityVerified,
		Envelopes:      []schema.Envelope{{}, {}},
		Panel:          pan,
		Governance:     &report,
		Canonicalization: &canon.Result{
			PartitionRevisionHash: "rev-hash-1", AgreementRuleVersion: canon.DualRuleVersion,
		},
		Confirmation: &canon.Confirmation{
			RuleVersion: canon.HostConfirmationRuleVersion,
			Challenges:  []canon.Challenge{{Type: canon.ChallengeWrongSplit}},
			Contested:   []canon.ContestedMapping{{HeldCanonicalIDs: []string{"canon-1"}}},
		},
		Output: mode.ChallengeOutput{
			ArtifactDigest: "abc123", PartitionRevisionHash: "rev-hash-1",
			Register: []mode.ChallengeEntry{{CanonicalID: "canon-0", Statement: "a finding"}},
		},
	}, nil
}

// TestServer_ArtifactViaMeta_RequiredByChallenge pins the ACP half of Challenge's task contract: the artifact
// arrives inline under `_meta.exploremesh.artifact`, and a challenge prompt without one is invalid-params —
// fail-closed, exactly like absent criteria, so a driver is told what to supply instead of getting a review of
// nothing.
func TestServer_ArtifactViaMeta_RequiredByChallenge(t *testing.T) {
	exp := &governedExplorer{}
	client, stop := serve(t, exp)
	defer stop()

	if _, err := client.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sn, _ := client.Call("session/new", map[string]any{"cwd": "/tmp/x"})
	sid, _ := sn.Result["sessionId"].(string)

	// (a) challenge with NO artifact → invalid-params naming the missing input.
	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "review the shutdown path"}},
		"_meta":     map[string]any{"exploremesh": map[string]any{"criteria": []string{"correctness"}, "mode": "challenge"}},
	})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("a challenge prompt with no artifact must be invalid-params, got %+v", resp.Error)
	}
	if !contains(resp.Error.Message, "requires an ARTIFACT") || !contains(resp.Error.Message, "_meta.exploremesh.artifact") {
		t.Errorf("the error must name the missing input + where to put it: %q", resp.Error.Message)
	}
	if exp.gotRaw != nil {
		t.Error("an invalid-params rejection must happen BEFORE the exploration is dispatched")
	}

	// (b) challenge WITH an artifact runs, and the artifact reaches the pipeline verbatim.
	const artifact = "func Shutdown() { close(ch); free(state) }"
	resp, err = client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "review the shutdown path"}},
		"_meta": map[string]any{"exploremesh": map[string]any{
			"criteria": []string{"correctness"}, "mode": "challenge", "artifact": artifact,
		}},
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("session/prompt with an artifact: err=%v rpc=%+v", err, resp.Error)
	}
	if exp.gotRaw == nil || exp.gotRaw.Artifact != artifact {
		t.Errorf("the server must pass the artifact through verbatim: %+v", exp.gotRaw)
	}
	em := exploreMetaOf(t, resp.Result)
	if supplied, _ := em["artifactSupplied"].(bool); !supplied {
		t.Errorf("the echo must state whether an artifact was supplied: %+v", em)
	}
}

// TestServer_GovernanceSummaryInMeta pins the ACP rendering: the PromptResponse `_meta.exploremesh`
// carries the governance summary — the claim tally, the rules/policy/partition hashes, and the
// withheld/contested signal. The last of those is the point: a driver reading only a claim COUNT would present a
// contested result as settled, so the withheld tally travels next to it.
func TestServer_GovernanceSummaryInMeta(t *testing.T) {
	client, stop := serve(t, &governedExplorer{})
	defer stop()

	if _, err := client.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sn, _ := client.Call("session/new", map[string]any{"cwd": "/tmp/x"})
	sid, _ := sn.Result["sessionId"].(string)
	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "review it"}},
		"_meta": map[string]any{"exploremesh": map[string]any{
			"criteria": []string{"correctness"}, "mode": "challenge", "artifact": "some artifact",
		}},
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("session/prompt: err=%v rpc=%+v", err, resp.Error)
	}
	em := exploreMetaOf(t, resp.Result)
	gov, _ := em["governance"].(map[string]any)
	if gov == nil {
		t.Fatalf("a governed run must echo its governance summary: %+v", em)
	}
	if n, _ := gov["claims"].(float64); int(n) != 2 {
		t.Errorf("governance.claims = %v, want 2", gov["claims"])
	}
	// The withheld/contested signal — the field a driver must not be able to miss.
	if n, _ := gov["claimsWithheld"].(float64); int(n) != 1 {
		t.Errorf("governance.claimsWithheld = %v, want 1", gov["claimsWithheld"])
	}
	if n, _ := gov["claimsContested"].(float64); int(n) != 1 {
		t.Errorf("governance.claimsContested = %v, want 1", gov["claimsContested"])
	}
	if settled, ok := gov["settled"].(bool); !ok || settled {
		t.Errorf("an unsettled confirmation must be echoed as such: %v", gov["settled"])
	}
	if n, _ := gov["contestedMappings"].(float64); int(n) != 1 {
		t.Errorf("governance.contestedMappings = %v, want 1", gov["contestedMappings"])
	}
	byLabel, _ := gov["claimsByLabel"].(map[string]any)
	if byLabel == nil || byLabel[string(govern.LabelWithheldContested)] == nil {
		t.Errorf("the per-label tally must name the withheld label: %+v", gov["claimsByLabel"])
	}
	// The pins every count rides on.
	for _, key := range []string{"rulesVersion", "countingPolicyHash", "claimsHash", "partitionRevisionHash",
		"agreementRuleVersion", "confirmationRule"} {
		if s, _ := gov[key].(string); s == "" {
			t.Errorf("the governance echo must carry %q: %+v", key, gov)
		}
	}
	if n, _ := gov["panelSize"].(float64); int(n) != 2 {
		t.Errorf("governance.panelSize = %v, want 2", gov["panelSize"])
	}
	// The mode's own structured detail rides alongside it.
	if n, _ := em["registerEntries"].(float64); int(n) != 1 {
		t.Errorf("a challenge result must echo its register size: %+v", em)
	}
	if s, _ := em["artifactDigest"].(string); s != "abc123" {
		t.Errorf("a challenge result must echo the artifact digest it reviewed: %+v", em)
	}
	if m, _ := em["mode"].(string); m != mode.Challenge {
		t.Errorf("the applied mode must be echoed: %v", em["mode"])
	}
}

// TestServer_AdjudicativeModesAreSelectable pins that registering the modes is all that was needed on this
// surface too: `_meta.exploremesh.mode` accepts them, and an unknown mode still fails closed while listing them.
func TestServer_AdjudicativeModesAreSelectable(t *testing.T) {
	exp := &governedExplorer{}
	client, stop := serve(t, exp)
	defer stop()

	if _, err := client.Call("initialize", map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sn, _ := client.Call("session/new", map[string]any{"cwd": "/tmp/x"})
	sid, _ := sn.Result["sessionId"].(string)

	// shortlist needs no artifact and is accepted.
	resp, err := client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "pick a datastore"}},
		"_meta":     map[string]any{"exploremesh": map[string]any{"criteria": []string{"latency"}, "mode": "shortlist"}},
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("shortlist prompt: err=%v rpc=%+v", err, resp.Error)
	}
	if exp.gotRaw == nil || exp.gotRaw.Mode != mode.Shortlist {
		t.Errorf("the server must apply the selected mode: %+v", exp.gotRaw)
	}
	// ai-collab likewise (the shipped composition is a first-class selectable mode).
	resp, err = client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "plan the migration"}},
		"_meta":     map[string]any{"exploremesh": map[string]any{"criteria": []string{"reversible"}, "mode": "ai-collab"}},
	})
	if err != nil || resp.Error != nil {
		t.Fatalf("ai-collab prompt: err=%v rpc=%+v", err, resp.Error)
	}
	// An unknown mode still fails closed, listing the known ones (including the adjudicative modes).
	resp, err = client.Call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "x"}},
		"_meta":     map[string]any{"exploremesh": map[string]any{"criteria": []string{"c"}, "mode": "bogus"}},
	})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("an unknown mode must be invalid-params, got %+v", resp.Error)
	}
	for _, want := range []string{"challenge", "shortlist", "ai-collab"} {
		if !contains(resp.Error.Message, want) {
			t.Errorf("the unknown-mode error must list %q: %q", want, resp.Error.Message)
		}
	}
}

// exploreMetaOf extracts `_meta.exploremesh` from a PromptResponse result.
func exploreMetaOf(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	meta, _ := result["_meta"].(map[string]any)
	em, _ := meta["exploremesh"].(map[string]any)
	if em == nil {
		t.Fatalf("no _meta.exploremesh in PromptResponse: %+v", result)
	}
	return em
}

// contains is strings.Contains, kept local so this file's imports stay to the packages under test.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
