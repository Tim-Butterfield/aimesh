package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/internal/review/engine/adjudication"
	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// matrixFake is a deterministic in-process stand-in for ANY adapter name. It returns
// schema-valid reviewer output for reviewer/cross_check/verifier phases and schema-valid
// host-adjudication output for semantic_adjudicate, and echoes the requested model so
// identity verifies — so every adapter/role combination can be exercised with NO real
// CLI, network, or auth. (It stands in for the shell adapters at the ReviewManager layer;
// real provider auth/identity is verify-on-provision and not exercised here.)
type matrixFake struct{ name string }

func (f matrixFake) Name() string              { return f.name }
func (f matrixFake) Available() (bool, string) { return true, "matrix fake (test)" }
func (f matrixFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	if c.Phase == string(review.PhaseAdjudicate) {
		js := `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"f1","validity":"valid","decisionState":"reported_valid","reasoning":"matrix host ok"}]}`
		return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
	}
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"request_changes","findings":[{"id":"M1","kind":"fail","severity":"high","title":"matrix finding","file":"main.go","location":"1","source":"reviewer"}]}`,
		string(c.Role), string(c.Phase))
	return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
}

// matrixWantActual is the model the matrixFake echoes back for adapter `name` — "mm",
// except devin-cli whose display name renders to the name-bound slug.
func matrixWantActual(name string) string {
	if name == "devin-cli" {
		return "claude-opus-4-8-medium"
	}
	return "mm"
}

// matrixManager pins exactly one role to adapter `name` (a matrixFake), keeping the
// other lanes on the deterministic `fake` adapter.
func matrixManager(t *testing.T, role review.Role, name string) *Manager {
	t.Helper()
	cfg := config.Default()
	// devin-cli is name-bound: its modelArg is a Devin display name that the resolver
	// renders into a slug, so use a valid brand-prefixed display name there.
	am := config.AdapterModel{ModelArg: "mm"}
	if name == "devin-cli" {
		am = config.AdapterModel{ModelArg: "Claude Opus 4.8", Effort: "medium"}
	}
	cfg.ModelCatalog["matrix-model"] = config.CatalogEntry{
		Provider: "x", CanonicalModel: "mm",
		Adapters: map[string]config.AdapterModel{name: am},
	}
	if _, ok := cfg.Adapters[name]; !ok {
		cfg.Adapters[name] = config.Adapter{ModelIdentity: "self_report"}
	}
	pin := func(r review.Role, exec string) config.Lane {
		if r == role {
			return config.Lane{Execution: exec, Adapter: name, Model: "matrix-model"}
		}
		return config.Lane{Execution: exec, Adapter: "fake", Model: "fake-model"}
	}
	lanes := map[string]config.Lane{
		"author_remediator": pin(review.RoleAuthorRemediator, "host"),
		"reviewer":          pin(review.RoleReviewer, "adapter"),
	}
	if role == review.RoleCrossCheck {
		lanes["cross_check"] = config.Lane{Execution: "adapter", Adapter: name, Model: "matrix-model"}
	}
	if role == review.RoleVerifier {
		lanes["verifier"] = config.Lane{Execution: "adapter", Adapter: name, Model: "matrix-model"}
	}
	cfg.Profiles["matrix"] = config.Profile{Description: "matrix", AdapterPreference: []string{name, "fake"}, Lanes: lanes}
	adapters := map[string]model.Adapter{"fake": fake.New(fake.Valid)}
	if name != "fake" {
		adapters[name] = matrixFake{name: name}
	}
	return &Manager{Cfg: cfg, Adapters: adapters, ArtifactDir: t.TempDir(), TempBase: t.TempDir()}
}

// syntheticAdjManager pins reviewer to the deterministic `fake` (its scenario controls whether the
// reviewer yields findings) and author_remediator to a NON-fake in-process host (matrixFake) — the
// shape ACP validation exercises. cross_check/verifier are unconfigured.
func syntheticAdjManager(t *testing.T, reviewerScenario fake.Scenario) *Manager {
	t.Helper()
	cfg := config.Default()
	cfg.Adapters["host-cli"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.ModelCatalog["host-model"] = config.CatalogEntry{
		Provider: "x", CanonicalModel: "mm", Adapters: map[string]config.AdapterModel{"host-cli": {ModelArg: "mm"}},
	}
	cfg.Profiles["synth"] = config.Profile{
		Description: "synthetic adjudication test", AdapterPreference: []string{"host-cli", "fake"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "host-cli", Model: "host-model"},
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		},
	}
	return &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": fake.New(reviewerScenario), "host-cli": matrixFake{name: "host-cli"}},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
}

// TestValidateHostAdjudication_SyntheticProbeOnZeroFindings: with ValidateHostAdjudication + zero
// reviewer findings, the manager runs a synthetic host adjudication so the configured real host lane is
// exercised — a real author_remediator call-status (phase validate_adjudicate) is written, identity is
// projected from the call, and the synthetic finding never pollutes the real review outcome.
func TestValidateHostAdjudication_SyntheticProbeOnZeroFindings(t *testing.T) {
	m := syntheticAdjManager(t, fake.Empty) // reviewer approves → zero findings
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "acp", Profile: "synth", ValidateHostAdjudication: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	cs := readCallStatus(t, out.RunDir, "c-0001-hostvalidate")
	if cs.Role != review.RoleAuthorRemediator || cs.Phase != review.PhaseValidateAdjudicate {
		t.Errorf("synthetic host call-status = role %q phase %q, want author_remediator/validate_adjudicate", cs.Role, cs.Phase)
	}
	if cs.VerificationStatus != "verified" {
		t.Errorf("host identity should be verified (invocation_tag match), got %q", cs.VerificationStatus)
	}
	if len(out.Findings) != 0 {
		t.Errorf("no-pollution: the synthetic finding must never appear in outcome.Findings, got %+v", out.Findings)
	}
	for _, artifact := range []string{"review-summary.md", "decisions/host-adjudication.json"} {
		if b, _ := os.ReadFile(filepath.Join(out.RunDir, artifact)); strings.Contains(string(b), "readiness probe") {
			t.Errorf("no-pollution: the synthetic finding must not appear in %s", artifact)
		}
	}
}

// failHostFake is a NON-fake host adapter whose adjudication call exits non-zero (Class A) — to prove a
// failing synthetic probe fails ACP validation.
type failHostFake struct{}

func (failHostFake) Name() string              { return "failhost" }
func (failHostFake) Available() (bool, string) { return true, "fail host (test)" }
func (failHostFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	return model.Result{ExitCode: 1, Stderr: []byte("failhost: simulated host adjudication failure")}, nil
}

// TestValidateHostAdjudication_SyntheticFailureFailsValidation: a failing synthetic host adjudication
// (host adapter exits non-zero) marks the host lane failed and fails the ACP validation.
func TestValidateHostAdjudication_SyntheticFailureFailsValidation(t *testing.T) {
	m := syntheticAdjManager(t, fake.Empty)
	m.Adapters["host-cli"] = failHostFake{} // the configured host FAILS its synthetic adjudication
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "acp", Profile: "synth", ValidateHostAdjudication: true})
	if err == nil {
		t.Fatal("a failing synthetic host adjudication must fail ACP validation")
	}
	if out.Failure == nil || out.Failure.Role != string(review.RoleAuthorRemediator) {
		t.Errorf("the failure must be attributed to the author_remediator host lane, got %+v", out.Failure)
	}
	cs := readCallStatus(t, out.RunDir, "c-0001-hostvalidate")
	if cs.Phase != review.PhaseValidateAdjudicate || cs.ExitCode != 1 {
		t.Errorf("failed synthetic host call-status = phase %q exit %d, want validate_adjudicate/1", cs.Phase, cs.ExitCode)
	}
}

// TestValidateHostAdjudication_NaturalCrossCheckSuppresses: a natural adjudication of CROSS_CHECK
// findings (reviewer had none) also exercises the host lane, so no synthetic probe runs.
func TestValidateHostAdjudication_NaturalCrossCheckSuppresses(t *testing.T) {
	m := syntheticAdjManager(t, fake.Empty) // reviewer yields no findings
	// Add a cross_check lane on the non-fake host adapter so it produces findings → natural adjudication.
	p := m.Cfg.Profiles["synth"]
	p.Lanes["cross_check"] = config.Lane{Execution: "adapter", Adapter: "host-cli", Model: "host-model"}
	m.Cfg.Profiles["synth"] = p
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "acp", Profile: "synth", ValidateHostAdjudication: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001-hostvalidate", "call-status.json")); serr == nil {
		t.Error("no synthetic probe may run when a natural cross_check adjudication already exercised the host")
	}
	if cs := readCallStatus(t, out.RunDir, "c-0001-cc-host"); cs.Phase != review.PhaseAdjudicate {
		t.Errorf("the natural cross_check host adjudication phase = %q, want semantic_adjudicate", cs.Phase)
	}
}

// TestValidateHostAdjudication_NaturalSuppressesSynthetic: when the reviewer produces findings, a
// NATURAL host adjudication exercises the lane, so no synthetic probe runs (no double call).
func TestValidateHostAdjudication_NaturalSuppressesSynthetic(t *testing.T) {
	m := syntheticAdjManager(t, fake.Valid) // reviewer emits a finding → natural host adjudication
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "acp", Profile: "synth", ValidateHostAdjudication: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001-hostvalidate", "call-status.json")); serr == nil {
		t.Error("no synthetic probe may run when a natural host adjudication already exercised the lane")
	}
	if cs := readCallStatus(t, out.RunDir, "c-0001-host"); cs.Phase != review.PhaseAdjudicate {
		t.Errorf("the natural host adjudication call phase = %q, want semantic_adjudicate", cs.Phase)
	}
}

// TestValidateHostAdjudication_OffByDefault: without the flag, no synthetic probe runs (normal CLI).
func TestValidateHostAdjudication_OffByDefault(t *testing.T) {
	m := syntheticAdjManager(t, fake.Empty)
	ws, _ := makeWorkspace(t)
	out, err := m.RunContext(context.Background(), Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "synth"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001-hostvalidate", "call-status.json")); serr == nil {
		t.Error("the synthetic probe must NOT run when ValidateHostAdjudication is false")
	}
}

// TestHostAdjudicationOccurred_IgnoresSelfReview: a host SELF-REVIEW call-status (semantic_author_review)
// is NOT adjudication readiness, so it must not suppress the synthetic probe; a semantic_adjudicate one does.
func TestHostAdjudicationOccurred_IgnoresSelfReview(t *testing.T) {
	m := &Manager{}
	dir := t.TempDir()
	write := func(callID string, phase review.Phase) {
		cs := review.CallStatus{SchemaVersion: 1, CallID: callID, Role: review.RoleAuthorRemediator, Phase: phase}
		b, _ := json.Marshal(cs)
		cd := filepath.Join(dir, "calls", callID)
		if err := os.MkdirAll(cd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cd, "call-status.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("c-sr", review.PhaseAuthorReview)
	if m.hostAdjudicationOccurred(dir) {
		t.Error("a semantic_author_review host call must NOT count as adjudication readiness")
	}
	write("c-adj", review.PhaseAdjudicate)
	if !m.hostAdjudicationOccurred(dir) {
		t.Error("a semantic_adjudicate host call must count as adjudication readiness")
	}
}

// TestDefault_NoSmokeEnvOrRealCLIRequired proves the default test path needs no
// REVIEWMESH_SMOKE_* gate and no real provider CLI — it runs entirely on in-process
// fakes even for a real adapter NAME like claude-code.
func TestDefault_NoSmokeEnvOrRealCLIRequired(t *testing.T) {
	for _, e := range []string{"REVIEWMESH_SMOKE_CLAUDE", "REVIEWMESH_SMOKE_CODEX", "REVIEWMESH_SMOKE_AGY", "REVIEWMESH_SMOKE_DEVIN"} {
		t.Setenv(e, "") // explicitly unset; the path must not depend on these
	}
	m := matrixManager(t, review.RoleReviewer, "claude-code")
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "matrix"})
	if err != nil {
		t.Fatalf("default path must run with no smoke env and no real CLI: %v", err)
	}
	// the "claude-code" lane resolved to the in-process matrixFake, not a real binary
	if cs := readCallStatus(t, out.RunDir, "c-0001"); cs.Adapter != "claude-code" || cs.ActualModel != matrixWantActual("claude-code") {
		t.Errorf("expected in-process matrix fake for claude-code, got adapter=%q model=%q", cs.Adapter, cs.ActualModel)
	}
}

// selfReportFake reports self_report evidence with a matching model — the weaker
// disposition that must be recorded as self_reported (not verified) and must proceed.
type selfReportFake struct{}

func (selfReportFake) Name() string              { return "selfreport" }
func (selfReportFake) Available() (bool, string) { return true, "self-report fake" }
func (selfReportFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"approve","findings":[]}`, string(c.Role), string(c.Phase))
	return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceSelfReport, Stdout: []byte(js)}, nil
}

func TestIdentity_SelfReportedProceedsAndIsRecorded(t *testing.T) {
	cfg := config.Default()
	cfg.ModelCatalog["sr-model"] = config.CatalogEntry{Provider: "x", CanonicalModel: "sr", Adapters: map[string]config.AdapterModel{"selfreport": {ModelArg: "sr"}}}
	cfg.Adapters["selfreport"] = config.Adapter{ModelIdentity: "self_report"}
	cfg.Profiles["sr"] = config.Profile{AdapterPreference: []string{"selfreport"}, Lanes: map[string]config.Lane{
		"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
		"reviewer":          {Execution: "adapter", Adapter: "selfreport", Model: "sr-model"},
	}}
	m := &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": fake.New(fake.Valid), "selfreport": selfReportFake{}},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "sr"})
	if err != nil {
		t.Fatalf("a self-reported (matching) identity must proceed, not halt: %v", err)
	}
	cs := readCallStatus(t, out.RunDir, "c-0001")
	if cs.VerificationStatus != review.VerifSelfReported {
		t.Errorf("verificationStatus = %q, want self_reported", cs.VerificationStatus)
	}
	if cs.IdentityEvidence != review.EvidenceSelfReport {
		t.Errorf("identityEvidence = %q, want self_report", cs.IdentityEvidence)
	}
}

// TestWantsSelfReport proves the prompt gating: ONLY a self_report-ceiling adapter is asked for the
// inline identity wrapper — strong-evidence adapters and adapters that declare no ceiling are not.
func TestWantsSelfReport(t *testing.T) {
	if !wantsSelfReport(selfReportFake{}) {
		t.Error("a self_report-ceiling adapter must receive the identity wrapper prompt")
	}
	if wantsSelfReport(envelopeFake{}) {
		t.Error("a strong-evidence (envelope) adapter must NOT receive the wrapper prompt")
	}
	if wantsSelfReport(unknownIdentityFake{}) {
		t.Error("an adapter with no declared Evidence() ceiling must NOT receive the wrapper prompt")
	}
}

// selfReportMalformedFake carries a VALID self-reported identity but a MALFORMED inner result on every
// attempt — proving identity metadata can NEVER rescue a schema-invalid result.
type selfReportMalformedFake struct{}

func (selfReportMalformedFake) Name() string              { return "selfreport" }
func (selfReportMalformedFake) Available() (bool, string) { return true, "self-report malformed fake" }
func (selfReportMalformedFake) Evidence() review.IdentityEvidence {
	return review.EvidenceSelfReport
}
func (selfReportMalformedFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceSelfReport, Payload: []byte("this is not a valid result object")}, nil
}

// TestIdentity_SelfReportDoesNotRescueMalformedResult proves a self-report (even a matching one) does
// NOT make a schema-invalid result acceptable: it still corrective-retries and halts Class G, with no
// caveat committed.
func TestIdentity_SelfReportDoesNotRescueMalformedResult(t *testing.T) {
	cfg := config.Default()
	cfg.ModelCatalog["sr-model"] = config.CatalogEntry{Provider: "x", CanonicalModel: "sr", Adapters: map[string]config.AdapterModel{"selfreport": {ModelArg: "sr"}}}
	cfg.Adapters["selfreport"] = config.Adapter{ModelIdentity: "self_report"}
	cfg.Profiles["sr"] = config.Profile{AdapterPreference: []string{"selfreport"}, Lanes: map[string]config.Lane{
		"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
		"reviewer":          {Execution: "adapter", Adapter: "selfreport", Model: "sr-model"},
	}}
	m := &Manager{Cfg: cfg, Adapters: map[string]model.Adapter{"fake": fake.New(fake.Valid), "selfreport": selfReportMalformedFake{}}, ArtifactDir: t.TempDir(), TempBase: t.TempDir()}
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "sr"})
	if err == nil {
		t.Fatal("a malformed result must halt even with a valid self-report present")
	}
	if len(out.IdentityCaveats) != 0 {
		t.Errorf("no caveat may be committed when the result never parses: %+v", out.IdentityCaveats)
	}
}

// unknownIdentityFake runs fine and returns schema-valid output but reports NO model (EvidenceNone) —
// exactly like Devin/Antigravity. Its identity is UNKNOWN, which is NOT a mismatch.
type unknownIdentityFake struct{}

func (unknownIdentityFake) Name() string              { return "unknownid" }
func (unknownIdentityFake) Available() (bool, string) { return true, "unknown-identity fake" }
func (unknownIdentityFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"approve","findings":[]}`, string(c.Role), string(c.Phase))
	return model.Result{ExitCode: 0, ActualModel: "", Evidence: review.EvidenceNone, Stdout: []byte(js)}, nil
}

// mismatchFake returns schema-valid output but reports a DIFFERENT model than requested — a proven
// mismatch that must ALWAYS halt (even under the ACP caveat policy).
type mismatchFake struct{}

func (mismatchFake) Name() string              { return "mismatchid" }
func (mismatchFake) Available() (bool, string) { return true, "mismatch fake" }
func (mismatchFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"approve","findings":[]}`, string(c.Role), string(c.Phase))
	return model.Result{ExitCode: 0, ActualModel: "totally-different-model", Evidence: review.EvidenceEnvelope, Stdout: []byte(js)}, nil
}

// unknownThenValidFake returns unknown identity + MALFORMED output on attempt 1, then unknown identity
// + schema-valid output on attempt 2 — so the pending caveat must be committed only for the ACCEPTED
// (parsed) attempt, never the discarded malformed one.
type unknownThenValidFake struct{ n int }

func (f *unknownThenValidFake) Name() string              { return "unknownid" }
func (f *unknownThenValidFake) Available() (bool, string) { return true, "unknown-then-valid fake" }
func (f *unknownThenValidFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	f.n++
	if f.n == 1 {
		return model.Result{ExitCode: 0, ActualModel: "", Evidence: review.EvidenceNone, Stdout: []byte("not json at all")}, nil
	}
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"approve","findings":[]}`, string(c.Role), string(c.Phase))
	return model.Result{ExitCode: 0, ActualModel: "", Evidence: review.EvidenceNone, Stdout: []byte(js)}, nil
}

// unknownAlwaysMalformedFake returns unknown identity + malformed output on EVERY attempt → the review
// halts Class G (unparseable) after retry exhaustion, and NO caveat may be committed.
type unknownAlwaysMalformedFake struct{}

func (unknownAlwaysMalformedFake) Name() string              { return "unknownid" }
func (unknownAlwaysMalformedFake) Available() (bool, string) { return true, "always-malformed fake" }
func (unknownAlwaysMalformedFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	return model.Result{ExitCode: 0, ActualModel: "", Evidence: review.EvidenceNone, Stdout: []byte("still not json")}, nil
}

// Evidence() ceilings for the test fakes — the manager's evidence-authority cap FAILS CLOSED (an
// adapter without Evidence() is treated as EvidenceNone), so any fake that must classify above `none`
// declares its ceiling here. The unknown-* fakes intentionally omit it (their ceiling IS none).
func (matrixFake) Evidence() review.IdentityEvidence     { return review.EvidenceInvocationTag }
func (selfReportFake) Evidence() review.IdentityEvidence { return review.EvidenceSelfReport }
func (mismatchFake) Evidence() review.IdentityEvidence   { return review.EvidenceEnvelope }
func (envelopeFake) Evidence() review.IdentityEvidence   { return review.EvidenceEnvelope }
func (laneFake) Evidence() review.IdentityEvidence       { return review.EvidenceInvocationTag }

// unknownThenVerifiedFake returns unknown identity + MALFORMED output on attempt 1, then a STRONG
// (invocation_tag) match + schema-valid output on attempt 2. The ACCEPTED attempt must be audited as
// `verified` with NO stale unknown reasonCode/caveat from the rejected first attempt.
type unknownThenVerifiedFake struct{ n int }

func (f *unknownThenVerifiedFake) Name() string { return "unknownid" }
func (f *unknownThenVerifiedFake) Available() (bool, string) {
	return true, "unknown-then-verified fake"
}
func (f *unknownThenVerifiedFake) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}
func (f *unknownThenVerifiedFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	f.n++
	if f.n == 1 {
		return model.Result{ExitCode: 0, ActualModel: "", Evidence: review.EvidenceNone, Stdout: []byte("not json at all")}, nil
	}
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"approve","findings":[]}`, string(c.Role), string(c.Phase))
	return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
}

// strongUnconfirmedFake returns schema-valid output under a STRONG evidence ceiling but reports NO
// model (empty actual) — the no-silent-fallback case: a strong-evidence adapter that cannot confirm
// its model must HALT Class E, not pass with a caveat.
type strongUnconfirmedFake struct{}

func (strongUnconfirmedFake) Name() string              { return "strongunconfirmed" }
func (strongUnconfirmedFake) Available() (bool, string) { return true, "strong-unconfirmed fake" }
func (strongUnconfirmedFake) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}
func (strongUnconfirmedFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"approve","findings":[]}`, string(c.Role), string(c.Phase))
	return model.Result{ExitCode: 0, ActualModel: "", Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
}

// TestIdentity_SharedCaveatPolicy is the crux of the SHARED identity policy: an adapter that RAN and
// returned schema-valid output but did not verify its exact model passes with a caveat on EVERY
// surface (CLI and ACP alike — no ACP escape hatch); a weak matching self-report passes as
// self_reported (never verified); only a STRONG-evidence MISMATCH halts Class E. The caveat is
// committed only after a schema-valid parse. Fully deterministic (fake binaries; no real provider calls).
func TestIdentity_SharedCaveatPolicy(t *testing.T) {
	build := func(reviewerAdapter string, adapters map[string]model.Adapter) *Manager {
		cfg := config.Default()
		cfg.ModelCatalog["u-model"] = config.CatalogEntry{Provider: "x", CanonicalModel: "u", Adapters: map[string]config.AdapterModel{reviewerAdapter: {ModelArg: "opus"}}}
		cfg.Adapters[reviewerAdapter] = config.Adapter{ModelIdentity: "trace"}
		cfg.Profiles["u"] = config.Profile{AdapterPreference: []string{reviewerAdapter}, Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
			"reviewer":          {Execution: "adapter", Adapter: reviewerAdapter, Model: "u-model"},
		}}
		adapters["fake"] = fake.New(fake.Valid)
		return &Manager{Cfg: cfg, Adapters: adapters, ArtifactDir: t.TempDir(), TempBase: t.TempDir()}
	}
	unknownCaveatRun := func(t *testing.T, surface string) {
		m := build("unknownid", map[string]model.Adapter{"unknownid": unknownIdentityFake{}})
		ws, _ := makeWorkspace(t)
		out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: surface, Profile: "u"})
		if err != nil {
			t.Fatalf("%s unknown identity must pass with caveat, not halt: %v", surface, err)
		}
		found := false
		for _, c := range out.IdentityCaveats {
			if c.Role == "reviewer" && c.Status == review.VerifUnknown {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a reviewer unknown-identity caveat on %s, got %+v", surface, out.IdentityCaveats)
		}
		cs := readCallStatus(t, out.RunDir, "c-0001")
		if cs.VerificationStatus != review.VerifUnknown {
			t.Errorf("verificationStatus = %q, want unknown", cs.VerificationStatus)
		}
	}

	// The Devin-only-CLI fix: unknown identity is a pass-with-caveat on CLI, not only ACP.
	t.Run("cli unknown → pass with caveat", func(t *testing.T) { unknownCaveatRun(t, "cli") })
	t.Run("acp unknown → pass with caveat", func(t *testing.T) { unknownCaveatRun(t, "acp") })

	// A WEAK matching self-report passes as self_reported (never verified), on ordinary CLI review.
	t.Run("cli self-report match → self_reported caveat (not verified)", func(t *testing.T) {
		m := build("selfreport", map[string]model.Adapter{"selfreport": selfReportFake{}})
		ws, _ := makeWorkspace(t)
		out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "u"})
		if err != nil {
			t.Fatalf("CLI self-reported (matching) identity must pass with a weak caveat: %v", err)
		}
		cs := readCallStatus(t, out.RunDir, "c-0001")
		if cs.VerificationStatus != review.VerifSelfReported {
			t.Fatalf("verificationStatus = %q, want self_reported (weak, not verified)", cs.VerificationStatus)
		}
		found := false
		for _, c := range out.IdentityCaveats {
			if c.Role == "reviewer" && c.Status == review.VerifSelfReported && c.ReportedModel == "opus" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a weak self_reported caveat carrying the reported model, got %+v", out.IdentityCaveats)
		}
	})

	// A STRONG-evidence MISMATCH — the strongest identity signal there is, a provider envelope naming a
	// model we did not ask for — is a prominent caveat on EVERY surface, and the findings are kept. What
	// a reviewer FOUND is what decides whether it matters; a wrong label on the author cannot make a real
	// defect false. (This used to be a Class-E halt that discarded the whole review.)
	for _, surface := range []string{"cli", "acp"} {
		t.Run(surface+" strong mismatch → caveat, findings kept", func(t *testing.T) {
			m := build("mismatchid", map[string]model.Adapter{"mismatchid": mismatchFake{}})
			ws, _ := makeWorkspace(t)
			out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: surface, Profile: "u"})
			if err != nil {
				t.Fatalf("a proven model mismatch must not halt on %s: %v", surface, err)
			}
			found := false
			for _, c := range out.IdentityCaveats {
				if c.Role == "reviewer" && c.Status == review.VerifMismatch {
					found = true
				}
			}
			if !found {
				t.Errorf("a mismatch must be RECORDED as a caveat on %s, got %+v", surface, out.IdentityCaveats)
			}
			cs := readCallStatus(t, out.RunDir, "c-0001")
			if cs.VerificationStatus != review.VerifMismatch {
				t.Errorf("verificationStatus = %q, want mismatch", cs.VerificationStatus)
			}
			if cs.HaltClass != nil {
				t.Errorf("identity must not set a halt class, got %v", *cs.HaltClass)
			}
		})
	}

	// A STRONG-evidence adapter that returns output but reports NO model is likewise a caveat. This was
	// the "no-silent-fallback" Class-E halt, and the codex echo test is why it is gone: the declared tier
	// is our own assertion about a CLI, and we were wrong about one. Failing a run on the strength of a
	// tier we cannot audit discards real work for a guarantee we never actually had.
	for _, surface := range []string{"cli", "acp"} {
		t.Run(surface+" strong evidence + no model → caveat, findings kept", func(t *testing.T) {
			m := build("strongunconfirmed", map[string]model.Adapter{"strongunconfirmed": strongUnconfirmedFake{}})
			ws, _ := makeWorkspace(t)
			out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: surface, Profile: "u"})
			if err != nil {
				t.Fatalf("a strong-evidence adapter reporting no model must not halt on %s: %v", surface, err)
			}
			if len(out.IdentityCaveats) == 0 {
				t.Errorf("an unconfirmed STRONG identity must be recorded as a caveat on %s", surface)
			}
			cs := readCallStatus(t, out.RunDir, "c-0001")
			if cs.ReasonCode != "model_identity_unknown" {
				t.Errorf("reasonCode = %q, want model_identity_unknown", cs.ReasonCode)
			}
		})
	}

	// The caveat is committed only for the ACCEPTED attempt: a malformed attempt 1 (which never parses)
	// then a valid attempt 2 must leave exactly ONE caveat (from attempt 2), never a stale duplicate.
	t.Run("unknown: malformed attempt then valid retry → one caveat from the accepted attempt", func(t *testing.T) {
		m := build("unknownid", map[string]model.Adapter{"unknownid": &unknownThenValidFake{}})
		ws, _ := makeWorkspace(t)
		out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "u"})
		if err != nil {
			t.Fatalf("must pass with caveat after the valid retry: %v", err)
		}
		if len(out.IdentityCaveats) != 1 {
			t.Errorf("exactly one caveat (from the accepted attempt), got %+v", out.IdentityCaveats)
		}
	})

	// Parse exhaustion: unknown identity + malformed output on every attempt halts Class G — and NO
	// caveat may be committed, because no attempt returned schema-valid output.
	t.Run("unknown: parse exhaustion → Class-G halt, no committed caveat", func(t *testing.T) {
		m := build("unknownid", map[string]model.Adapter{"unknownid": unknownAlwaysMalformedFake{}})
		ws, _ := makeWorkspace(t)
		out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "u"})
		if err == nil {
			t.Fatal("malformed output on every attempt must halt (Class G), not pass with caveat")
		}
		if len(out.IdentityCaveats) != 0 {
			t.Errorf("no caveat may be committed when the run halts unparsed: %+v", out.IdentityCaveats)
		}
	})

	// Stale-state guard: attempt 1 (unknown + malformed) then attempt 2 (STRONG match, valid) must
	// audit the ACCEPTED attempt as verified — no committed caveat, and no stale unknown reasonCode
	// carried over from the rejected first attempt (a single CallStatus is reused across attempts).
	t.Run("unknown malformed then verified retry → verified, no caveat, no stale reasonCode", func(t *testing.T) {
		m := build("unknownid", map[string]model.Adapter{"unknownid": &unknownThenVerifiedFake{}})
		ws, _ := makeWorkspace(t)
		out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "u"})
		if err != nil {
			t.Fatalf("verified retry must pass: %v", err)
		}
		if len(out.IdentityCaveats) != 0 {
			t.Errorf("a verified accepted attempt must leave NO caveat, got %+v", out.IdentityCaveats)
		}
		cs := readCallStatus(t, out.RunDir, "c-0001")
		if cs.VerificationStatus != review.VerifVerified {
			t.Errorf("verificationStatus = %q, want verified", cs.VerificationStatus)
		}
		if cs.ReasonCode != "" {
			t.Errorf("stale reasonCode from the rejected attempt must be cleared, got %q", cs.ReasonCode)
		}
	})
}

// envelopeFake mimics claude-code: stdout is an envelope, Payload is the unwrapped
// reviewer JSON (as the shell adapter would set it). The Manager must parse Payload.
type envelopeFake struct{}

func (envelopeFake) Name() string              { return "envfake" }
func (envelopeFake) Available() (bool, string) { return true, "envelope fake" }
func (envelopeFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	reviewerJSON := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"request_changes","findings":[{"id":"E1","kind":"fail","severity":"high","title":"envelope finding","file":"main.go","location":"1","source":"reviewer"}]}`, string(c.Role), string(c.Phase))
	env := fmt.Sprintf(`{"type":"result","result":%q,"modelUsage":{%q:{"inputTokens":1}}}`, reviewerJSON, string(c.ModelArg))
	return model.Result{
		ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceEnvelope,
		Stdout: []byte(env), Payload: []byte(reviewerJSON),
	}, nil
}

func TestClaude_EnvelopePayloadParsedByManager(t *testing.T) {
	cfg := config.Default()
	cfg.ModelCatalog["env-model"] = config.CatalogEntry{Provider: "anthropic", CanonicalModel: "opus", Adapters: map[string]config.AdapterModel{"envfake": {ModelArg: "opus"}}}
	cfg.Adapters["envfake"] = config.Adapter{ModelIdentity: "envelope"}
	cfg.Profiles["env"] = config.Profile{AdapterPreference: []string{"envfake"}, Lanes: map[string]config.Lane{
		"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
		"reviewer":          {Execution: "adapter", Adapter: "envfake", Model: "env-model"},
	}}
	m := &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": fake.New(fake.Valid), "envfake": envelopeFake{}},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "env"})
	if err != nil {
		t.Fatalf("envelope review should parse the unwrapped payload: %v", err)
	}
	var found bool
	for _, f := range out.Findings {
		if f.Title == "envelope finding" {
			found = true
		}
	}
	if !found {
		t.Errorf("the unwrapped reviewer finding was not parsed; got %+v", out.Findings)
	}
	// raw envelope kept in stdout.txt; unwrapped JSON in reviewer-result.json
	raw := read(t, filepath.Join(out.RunDir, "calls", "c-0001", "stdout.txt"))
	if !strings.Contains(raw, "modelUsage") {
		t.Error("stdout.txt should preserve the raw envelope")
	}
	rr := read(t, filepath.Join(out.RunDir, "calls", "c-0001", "reviewer-result.json"))
	if strings.Contains(rr, "modelUsage") || !strings.Contains(rr, "envelope finding") {
		t.Errorf("reviewer-result.json should be the unwrapped reviewer JSON, got: %s", rr)
	}
}

func readCallStatus(t *testing.T, runDir, callID string) review.CallStatus {
	t.Helper()
	var cs review.CallStatus
	if err := json.Unmarshal([]byte(read(t, filepath.Join(runDir, "calls", callID, "call-status.json"))), &cs); err != nil {
		t.Fatalf("parse call-status %s: %v", callID, err)
	}
	return cs
}

// TestAdapterRoleMatrix proves every initial adapter can be selected for every review
// role at the ReviewManager layer, deterministically, with NO real CLI invoked.
func TestAdapterRoleMatrix(t *testing.T) {
	adapters := []string{"fake", "ollama", "devin-cli", "claude-code", "codex-cli", "agy-cli", "gemini-cli"}
	type roleCase struct {
		role   review.Role
		callID string
		phase  review.Phase
		prompt string // lane-specific phrase expected in prompt.md (reviewer lanes)
	}
	cases := []roleCase{
		{review.RoleReviewer, "c-0001", review.PhaseIterate, "You are a reviewer"},
		{review.RoleCrossCheck, "c-0001-cc", review.PhaseCrossCheck, "CROSS-CHECK"},
		{review.RoleVerifier, "c-0001-verify", review.PhaseVerify, "VERIFIER"},
		{review.RoleAuthorRemediator, "c-0001-host", review.PhaseAdjudicate, ""},
	}
	for _, ad := range adapters {
		for _, rc := range cases {
			t.Run(ad+"/"+string(rc.role), func(t *testing.T) {
				m := matrixManager(t, rc.role, ad)
				ws, _ := makeWorkspace(t)
				out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "matrix"})
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				// fake host adjudication is deterministic — no model host call is made.
				if rc.role == review.RoleAuthorRemediator && ad == "fake" {
					if _, e := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001-host")); !os.IsNotExist(e) {
						t.Error("fake host adjudication must be deterministic (no host model call dir)")
					}
					if !strings.Contains(read(t, filepath.Join(out.RunDir, "logs", "events.jsonl")), "deterministic adjudication") {
						t.Error("expected a deterministic adjudication event for the fake host")
					}
					return
				}
				cs := readCallStatus(t, out.RunDir, rc.callID)
				if cs.Adapter != ad {
					t.Errorf("adapter = %q, want %q", cs.Adapter, ad)
				}
				// ActualModel is echoed by the adapter only when actually invoked, so this
				// proves the selected adapter genuinely executed (not just configured).
				if want := matrixWantActual(ad); cs.ActualModel != want {
					t.Errorf("actualModel = %q, want %q (adapter did not actually run)", cs.ActualModel, want)
				}
				if cs.Role != rc.role {
					t.Errorf("role = %q, want %q", cs.Role, rc.role)
				}
				if cs.Phase != rc.phase {
					t.Errorf("phase = %q, want %q", cs.Phase, rc.phase)
				}
				if rc.prompt != "" {
					prompt := read(t, filepath.Join(out.RunDir, "calls", rc.callID, "prompt.md"))
					if !strings.Contains(prompt, rc.prompt) {
						t.Errorf("prompt.md for %s should contain %q", rc.role, rc.prompt)
					}
				}
			})
		}
	}
}

func TestMergeAdjResults_RecordsDisagreement(t *testing.T) {
	f := review.Finding{ID: "a", Kind: "fail", File: "x.go", Location: "1", Title: "X"}
	base := adjudication.Result{Findings: []review.Finding{f}, Decisions: []review.Decision{{FindingID: "a", Valid: true, State: review.StateReportedValid}}}
	add := adjudication.Result{Findings: []review.Finding{f}, Decisions: []review.Decision{{FindingID: "a", Valid: false, State: review.StateInvalid}}}
	out := mergeAdjResults(base, add)
	if len(out.Findings) != 2 {
		t.Fatalf("disagreement on same fingerprint should append a finding; got %d", len(out.Findings))
	}
	if !strings.Contains(out.Findings[1].Title, "disagreement") {
		t.Errorf("expected a recorded disagreement, got %q", out.Findings[1].Title)
	}
	if adjudication.Actionable(out.Decisions[1]) {
		t.Error("a disagreement finding must not be actionable")
	}
}

func TestAppendReportOnly_VerifierDisagreementAndNewFinding(t *testing.T) {
	prior := review.Finding{ID: "a", Kind: "fail", File: "x.go", Location: "1", Title: "X"}
	base := adjudication.Result{Findings: []review.Finding{prior}, Decisions: []review.Decision{{FindingID: "a", Valid: true, State: review.StateApplied}}}
	// verifier disputes the prior (now invalid) + raises a new finding
	vDisagree := review.Finding{ID: "a", Kind: "fail", File: "x.go", Location: "1", Title: "X"}
	vNew := review.Finding{ID: "b", Kind: "risk", File: "y.go", Location: "5", Title: "Y"}
	add := adjudication.Result{
		Findings:  []review.Finding{vDisagree, vNew},
		Decisions: []review.Decision{{FindingID: "a", Valid: false, State: review.StateInvalid}, {FindingID: "b", Valid: true, State: review.StateReportedValid}},
	}
	out := appendReportOnly(base, add)
	if len(out.Findings) != 3 {
		t.Fatalf("want prior + disagreement + new = 3, got %d", len(out.Findings))
	}
	for _, d := range out.Decisions[1:] {
		if adjudication.Actionable(d) {
			t.Error("verifier (report-only) findings must never be actionable")
		}
	}
}

func TestRenumberIDs_NoCollision(t *testing.T) {
	adj := adjudication.Result{
		Findings:  []review.Finding{{ID: "F1", Kind: "fail", File: "a.go", Location: "1"}, {ID: "F1", Kind: "fail", File: "b.go", Location: "1"}},
		Decisions: []review.Decision{{FindingID: "F1"}, {FindingID: "F1"}},
	}
	out := renumberIDs(adj)
	if out.Findings[0].ID == out.Findings[1].ID {
		t.Error("merged finding ids must be unique")
	}
	if out.Findings[0].ID != out.Decisions[0].FindingID || out.Findings[1].ID != out.Decisions[1].FindingID {
		t.Error("finding/decision ids must stay aligned")
	}
}

// laneFake is a phase-aware test adapter: it returns a DISTINCT finding per lane
// phase (so reviewer/cross_check/verifier produce different fingerprints), all on the
// shown file main.go. ActualModel echoes the requested model so identity verifies.
type laneFake struct{}

func (laneFake) Name() string              { return "lanefake" }
func (laneFake) Available() (bool, string) { return true, "lane fake (test)" }
func (laneFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	// locations in distinct 10-line buckets so fingerprints (kind|file|bucketedLoc)
	// differ across lanes (NormalizeLocation buckets to the nearest 10).
	id, title, loc := "R1", "reviewer issue", "1"
	switch c.Phase {
	case string(review.PhaseCrossCheck):
		id, title, loc = "C1", "crosscheck issue", "20"
	case string(review.PhaseVerify):
		id, title, loc = "V1", "verifier issue", "30"
	}
	js := fmt.Sprintf(`{"schemaVersion":1,"role":%q,"phase":%q,"summary":"s","verdict":"request_changes","findings":[{"id":%q,"kind":"fail","severity":"high","title":%q,"file":"main.go","location":%q,"source":"reviewer"}]}`,
		string(c.Role), string(c.Phase), id, title, loc)
	return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
}

// unavailFake is registered but reports unavailable (drives preflight halts).
type unavailFake struct{}

func (unavailFake) Name() string              { return "unavail" }
func (unavailFake) Available() (bool, string) { return false, "binary not found (test)" }
func (unavailFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	return model.Result{}, fmt.Errorf("unavail should never be invoked")
}

// multiLaneManager builds a Manager whose "multi" profile has reviewer (+ optional
// cross_check/verifier) on a phase-aware test adapter and a deterministic fake host.
func multiLaneManager(t *testing.T, withCross, withVerify bool, ccAdapter string) *Manager {
	t.Helper()
	cfg := config.Default()
	cfg.ModelCatalog["lanefake-model"] = config.CatalogEntry{
		Provider: "x", CanonicalModel: "lm",
		Adapters: map[string]config.AdapterModel{"lanefake": {ModelArg: "lm"}, "unavail": {ModelArg: "um"}},
	}
	cfg.Adapters["lanefake"] = config.Adapter{ModelIdentity: "self_report"}
	cfg.Adapters["unavail"] = config.Adapter{ModelIdentity: "self_report"}
	lanes := map[string]config.Lane{
		"author_remediator": {Execution: "host", Adapter: "fake", Model: "fake-model"},
		"reviewer":          {Execution: "adapter", Adapter: "lanefake", Model: "lanefake-model"},
	}
	if withCross {
		lanes["cross_check"] = config.Lane{Execution: "adapter", Adapter: ccAdapter, Model: "lanefake-model"}
	}
	if withVerify {
		lanes["verifier"] = config.Lane{Execution: "adapter", Adapter: "lanefake", Model: "lanefake-model"}
	}
	cfg.Profiles["multi"] = config.Profile{Description: "test", AdapterPreference: []string{"lanefake"}, Lanes: lanes}
	return &Manager{
		Cfg: cfg,
		Adapters: map[string]model.Adapter{
			"fake": fake.New(fake.Valid), "lanefake": laneFake{}, "unavail": unavailFake{},
		},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
}

func TestLanes_CrossCheckAndVerifierExecute(t *testing.T) {
	m := multiLaneManager(t, true, true, "lanefake")
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "multi"})
	if err != nil {
		t.Fatalf("multi-lane report: %v", err)
	}
	// reviewer + cross_check + verifier findings all present, none lost
	titles := map[string]bool{}
	for _, f := range out.Findings {
		titles[f.Title] = true
	}
	for _, want := range []string{"reviewer issue", "crosscheck issue", "verifier issue"} {
		if !titles[want] {
			t.Errorf("missing %q finding; got %+v", want, titles)
		}
	}
	// final finding ids are unique across lanes (no collision)
	ids := map[string]bool{}
	for _, f := range out.Findings {
		if ids[f.ID] {
			t.Errorf("duplicate finding id %q across lanes", f.ID)
		}
		ids[f.ID] = true
	}
	// distinct per-lane call artifacts (no ID collision)
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001", "call-status.json"))
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001-cc", "call-status.json"))
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001-verify", "call-status.json"))
	// lane events
	events := read(t, filepath.Join(out.RunDir, "logs", "events.jsonl"))
	for _, ev := range []string{"cross_check_started", "cross_check_completed", "verifier_started", "verifier_completed"} {
		if !strings.Contains(events, ev) {
			t.Errorf("missing event %q", ev)
		}
	}
}

// includeHostReview (report mode): the author_remediator contributes its own findings
// (source=author_self_review), combined + adjudicated alongside reviewer findings, surfaced
// report-only, no workspace mutation.
func TestHostSelfReview_ReportIncludesAuthorSelfReview(t *testing.T) {
	m := multiLaneManager(t, true, true, "lanefake")
	ws, file := makeWorkspace(t)
	before := read(t, file)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "multi", IncludeHostReview: true})
	if err != nil {
		t.Fatalf("self-review report: %v", err)
	}
	selfFound := false
	titles := map[string]bool{}
	for _, f := range out.Findings {
		titles[f.Title] = true
		if f.Source == "author_self_review" {
			selfFound = true
			if f.Title != "Host self-review note" {
				t.Errorf("author_self_review title = %q", f.Title)
			}
		}
	}
	if !selfFound {
		t.Errorf("no author_self_review finding in report; got %+v", out.Findings)
	}
	if !titles["reviewer issue"] {
		t.Error("reviewer finding lost when self-review was added")
	}
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001-selfreview", "call-status.json"))
	events := read(t, filepath.Join(out.RunDir, "logs", "events.jsonl"))
	for _, ev := range []string{"author_self_review_started", "author_self_review_completed"} {
		if !strings.Contains(events, ev) {
			t.Errorf("missing event %q", ev)
		}
	}
	if read(t, file) != before {
		t.Error("report mode must not mutate the workspace")
	}
}

// Default report mode (no flag) is unchanged: no self-review pass, no author_self_review.
func TestHostSelfReview_DefaultReportUnchanged(t *testing.T) {
	m := multiLaneManager(t, true, true, "lanefake")
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "multi"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	for _, f := range out.Findings {
		if f.Source == "author_self_review" {
			t.Errorf("self-review ran without the flag: %+v", f)
		}
	}
	if _, err := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001-selfreview", "call-status.json")); !os.IsNotExist(err) {
		t.Error("self-review call artifact present without the flag")
	}
}

// includeHostReview is report-only: rejected for patch/apply on the effective mode,
// before any audit/model work (no run dir created).
func TestHostSelfReview_RejectedForPatchApply(t *testing.T) {
	for _, mode := range []review.Mode{review.ModeApply, review.ModePatch} {
		m := multiLaneManager(t, false, false, "lanefake")
		ws, _ := makeWorkspace(t)
		out, err := m.Run(Request{Workspace: ws, Mode: mode, Surface: "cli", Profile: "multi", IncludeHostReview: true})
		if err == nil {
			t.Fatalf("mode %s: expected includeHostReview to be rejected", mode)
		}
		if !strings.Contains(err.Error(), "report mode") {
			t.Errorf("mode %s: error = %v, want a report-mode usage error", mode, err)
		}
		if out.RunDir != "" {
			t.Errorf("mode %s: rejection must happen before any run/audit work (got RunDir %q)", mode, out.RunDir)
		}
	}
}

func TestLanes_ApplySafe_VerifierReportOnly(t *testing.T) {
	m := multiLaneManager(t, true, true, "lanefake")
	ws, file := makeWorkspace(t)
	before := read(t, file)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli", Profile: "multi"})
	if err != nil {
		t.Fatalf("multi-lane apply: %v", err)
	}
	// reviewer + cross_check edits were applied (host wrote), so the file changed
	if read(t, file) == before {
		t.Error("expected reviewer/cross_check findings to be applied")
	}
	// verifier finding is still REPORTED (not lost) even though it is never applied
	var sawVerifier bool
	for _, f := range out.Findings {
		if f.Title == "verifier issue" {
			sawVerifier = true
		}
	}
	if !sawVerifier {
		t.Error("verifier finding should be reported (report-only), not dropped")
	}
}

func TestLanes_FakeProfileSkipsExtraLanes(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("fake report: %v", err)
	}
	events := read(t, filepath.Join(out.RunDir, "logs", "events.jsonl"))
	if !strings.Contains(events, "lane_skipped") {
		t.Error("fake profile should emit lane_skipped for cross_check/verifier")
	}
	// no cross_check/verifier call dirs
	if _, err := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001-cc")); !os.IsNotExist(err) {
		t.Error("fake profile must not create a cross_check call dir")
	}
}

func TestLanes_RequiredLaneMissingHaltsPreflight(t *testing.T) {
	m := multiLaneManager(t, true, false, "unavail") // cross_check → unavailable adapter
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "multi"})
	if err == nil {
		t.Fatal("expected preflight halt when a required lane adapter is unavailable")
	}
	if fault.CodeOf(err) != fault.Adapter {
		t.Errorf("exit = %d, want %d (adapter)", fault.CodeOf(err), fault.Adapter)
	}
	// review work never began (no call artifacts)
	if out.RunDir != "" {
		if _, e := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001")); e == nil {
			t.Error("no reviewer call should run when preflight halts")
		}
	}
}

func newManager(t *testing.T, scenario fake.Scenario) *Manager {
	t.Helper()
	// The shipped `default` profile now ships UNCONFIGURED; point the default at the shipped-but-
	// hidden fake profile so these deterministic-fake tests resolve without passing a Profile.
	// The hidden profile resolves only under the internal test-harness gate.
	t.Setenv(fake.EnvVar, "1")
	cfg := config.Default()
	cfg.DefaultProfile = config.FakeProfile
	return &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": fake.New(scenario)},
		ArtifactDir: t.TempDir(),
		TempBase:    t.TempDir(),
	}
}

// A context cancelled at the `patch_written` event — emitted immediately BEFORE the
// apply-mode ctx re-check that gates WorkspaceAccess.Commit (review.go) — must produce NO
// live workspace write. Cancelling exactly here (via the in-process OnEvent sink, which
// runs synchronously) targets that specific guard: baseline TestApply_ModifiesOnlyExpectedFile
// shows the same fake.Valid+apply DOES write the marker when not cancelled, and removing the
// guard would let this commit happen — so the test is non-vacuous. Complements the Devin
// host's manually-verified "no write after cancel".
func TestApply_CancelAtPatchWritten_NoLiveWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)
	req := Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"}
	req.OnEvent = func(ev audit.EventLine) {
		if ev.EventType == "patch_written" { // fired right before the apply-commit guard
			cancel()
		}
	}
	out, err := m.RunContext(ctx, req)
	if strings.Contains(read(t, file), "// reviewmesh[") {
		t.Errorf("a cancel at patch_written must not write to the live workspace; file=%q", read(t, file))
	}
	if err == nil && out.Status == "stable" {
		t.Error("a cancelled apply run must not report a clean stable completion")
	}
}

func makeWorkspace(t *testing.T) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	file = filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("expected artifact %s: %v", p, err)
	}
}

func TestReport_Success_NoMutation_WritesArtifacts(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)
	before := read(t, file)

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("report run failed: %v", err)
	}
	if out.Status != "stable" {
		t.Errorf("status = %q, want stable", out.Status)
	}
	if len(out.Findings) != 1 {
		t.Errorf("findings = %d, want 1", len(out.Findings))
	}
	if read(t, file) != before {
		t.Error("report mode mutated the workspace")
	}
	mustExist(t, filepath.Join(out.RunDir, "run-state.json"))
	mustExist(t, filepath.Join(out.RunDir, "review-summary.md"))
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001", "call-status.json"))
	mustExist(t, filepath.Join(out.RunDir, "decisions", "host-adjudication.json"))
	events := read(t, filepath.Join(out.RunDir, "logs", "events.jsonl"))
	if !strings.Contains(events, "run_started") || !strings.Contains(events, "run_completed") {
		t.Error("events.jsonl missing run_started/run_completed")
	}
}

func TestPatch_CreatesPatchArtifact_NoMutation(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)
	before := read(t, file)

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModePatch, Surface: "cli"})
	if err != nil {
		t.Fatalf("patch run failed: %v", err)
	}
	mustExist(t, filepath.Join(out.RunDir, "patches", "changes.patch"))
	mustExist(t, filepath.Join(out.RunDir, "patches", "patch-summary.json"))
	if read(t, file) != before {
		t.Error("patch mode mutated the live workspace")
	}
}

func TestApply_ModifiesOnlyExpectedFile(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("apply run failed: %v", err)
	}
	got := read(t, file)
	if !strings.Contains(got, "// reviewmesh[F-001]") {
		t.Errorf("apply mode did not insert the marker; file=%q", got)
	}
	mustExist(t, filepath.Join(out.RunDir, "patches", "changes.patch"))
}

// remediationHostFake is a NON-fake host lane: it adjudicates the reviewer's finding as
// apply-worthy and, on the remediation phase, returns an anchored edit that FIXES real code.
type remediationHostFake struct{}

func (remediationHostFake) Name() string              { return "host-cli" }
func (remediationHostFake) Available() (bool, string) { return true, "ok" }
func (remediationHostFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	ok := func(js string) (model.Result, error) {
		return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
	}
	switch c.Phase {
	case string(review.PhaseAdjudicate):
		return ok(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"f1","validity":"valid","decisionState":"applied","reasoning":"apply the fix"}]}`)
	case string(review.PhaseRemediate):
		return ok(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_remediate","edits":[{"file":"main.go","anchor":"func main() {}","replacement":"func main() { _ = 0 }","occurrence":0}]}`)
	default:
		return ok(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_author_review","verdict":"approve","findings":[]}`)
	}
}

// selfCritiqueHostFake REJECTS the finding during natural adjudication, then OVERTURNS that rejection
// during the dismissal-verification pass (detected by the "DISMISSAL VERIFICATION" prompt framing).
type selfCritiqueHostFake struct{}

func (selfCritiqueHostFake) Name() string              { return "sc-host" }
func (selfCritiqueHostFake) Available() (bool, string) { return true, "ok" }
func (selfCritiqueHostFake) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}
func (selfCritiqueHostFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	mk := func(js string) (model.Result, error) {
		return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
	}
	if c.Phase == string(review.PhaseAdjudicate) {
		validity, state := "invalid", "reported_invalid"
		if strings.Contains(c.Prompt, "DISMISSAL VERIFICATION") {
			validity, state = "valid", "reported_valid" // the rejection was wrong — overturn it
		}
		return mk(fmt.Sprintf(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"f1","validity":%q,"decisionState":%q,"reasoning":"r"}]}`, validity, state))
	}
	return mk(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_author_review","verdict":"approve","findings":[]}`)
}

// TestFinalSelfCritique_OverturnsWrongRejection pins M5: a finding the host rejected as invalid is
// re-checked in the dismissal-verification pass and overturned (surfaced report-only), so a wrong
// rejection no longer silently drops a real finding.
func TestFinalSelfCritique_OverturnsWrongRejection(t *testing.T) {
	cfg := config.Default()
	cfg.Adapters["rev-cli"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.Adapters["sc-host"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.ModelCatalog["rev-model"] = config.CatalogEntry{Provider: "x", CanonicalModel: "mm", Adapters: map[string]config.AdapterModel{"rev-cli": {ModelArg: "mm"}}}
	cfg.ModelCatalog["sc-model"] = config.CatalogEntry{Provider: "x", CanonicalModel: "mm", Adapters: map[string]config.AdapterModel{"sc-host": {ModelArg: "mm"}}}
	cfg.Profiles["sc"] = config.Profile{
		Description: "self-critique", AdapterPreference: []string{"sc-host", "rev-cli", "fake"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "sc-host", Model: "sc-model"},
			"reviewer":          {Execution: "adapter", Adapter: "rev-cli", Model: "rev-model"},
		},
	}
	m := &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": fake.New(fake.Valid), "rev-cli": matrixFake{name: "rev-cli"}, "sc-host": selfCritiqueHostFake{}},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli", Profile: "sc"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-final-selfcritique", "prompt.md"))
	decisions := read(t, filepath.Join(out.RunDir, "decisions", "host-adjudication.json"))
	if !strings.Contains(decisions, "overturned on final self-critique") {
		t.Errorf("a wrongly-rejected finding must be overturned by the final self-critique; decisions=%s", decisions)
	}
}

// TestApply_ModelDrivenRemediation_EditsRealCode pins H4: with a real (non-fake) host lane, apply
// mode produces an ACTUAL code fix from the model — not the comment marker.
func TestApply_ModelDrivenRemediation_EditsRealCode(t *testing.T) {
	cfg := config.Default()
	cfg.Adapters["rev-cli"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.Adapters["host-cli"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.ModelCatalog["rev-model"] = config.CatalogEntry{Provider: "x", CanonicalModel: "mm", Adapters: map[string]config.AdapterModel{"rev-cli": {ModelArg: "mm"}}}
	cfg.ModelCatalog["host-model"] = config.CatalogEntry{Provider: "x", CanonicalModel: "mm", Adapters: map[string]config.AdapterModel{"host-cli": {ModelArg: "mm"}}}
	cfg.Profiles["remed"] = config.Profile{
		Description: "model-driven remediation", AdapterPreference: []string{"host-cli", "rev-cli", "fake"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "host-cli", Model: "host-model"},
			"reviewer":          {Execution: "adapter", Adapter: "rev-cli", Model: "rev-model"},
		},
	}
	m := &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": fake.New(fake.Valid), "rev-cli": matrixFake{name: "rev-cli"}, "host-cli": remediationHostFake{}},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
	ws, file := makeWorkspace(t)
	if _, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli", Profile: "remed"}); err != nil {
		t.Fatalf("apply run failed: %v", err)
	}
	got := read(t, file)
	if !strings.Contains(got, "func main() { _ = 0 }") {
		t.Errorf("model-driven remediation must apply a REAL edit; file=%q", got)
	}
	if strings.Contains(got, "// reviewmesh[") {
		t.Errorf("a real edit was expected, not the marker fallback; file=%q", got)
	}
}

func TestMalformed_HaltsClassG_Exit8(t *testing.T) {
	m := newManager(t, fake.Malformed)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err == nil {
		t.Fatal("expected a halt for malformed reviewer output")
	}
	if fault.CodeOf(err) != fault.Internal {
		t.Errorf("exit code = %d, want %d (internal/Class G)", fault.CodeOf(err), fault.Internal)
	}
	if out.Status != "halted" {
		t.Errorf("status = %q, want halted", out.Status)
	}
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001", "halt-record.json"))
}

// An identity mismatch completes the review and is recorded — contrast TestMalformed_HaltsClassG_Exit8
// directly above, which DOES halt. The difference is the point: unusable OUTPUT stops a run; an
// unexpected label on a usable output does not.
func TestIdentityMismatch_CompletesWithCaveat(t *testing.T) {
	m := newManager(t, fake.IdentityMismatch)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("an identity mismatch must not halt the review: %v", err)
	}
	if out.Halt != nil {
		t.Errorf("halt class = %v, want none", out.Halt)
	}
	if out.Status == "halted" {
		t.Error("status must not be halted for an identity mismatch")
	}
	found := false
	for _, c := range out.IdentityCaveats {
		if c.Status == review.VerifMismatch {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a recorded mismatch caveat, got %+v", out.IdentityCaveats)
	}
}

func TestDuplicate_DedupedToOne(t *testing.T) {
	m := newManager(t, fake.Duplicate)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(out.Findings) != 1 {
		t.Errorf("findings after dedup = %d, want 1", len(out.Findings))
	}
	if out.Findings[0].Severity != review.SeverityHigh {
		t.Errorf("dedup kept severity %q, want high", out.Findings[0].Severity)
	}
}

func TestApply_SingleFileTarget(t *testing.T) {
	m := newManager(t, fake.Valid)
	_, file := makeWorkspace(t)

	// the workspace target is a single file, not a directory
	_, err := m.Run(Request{Workspace: file, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("single-file apply failed: %v", err)
	}
	if !strings.Contains(read(t, file), "// reviewmesh[") {
		t.Error("single-file apply did not modify the target file")
	}
}

func TestApply_RepoLike_DoesNotTouchInternalPaths(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkFile(t, filepath.Join(ws, ".git", "config"), "ORIG-GIT")
	mkFile(t, filepath.Join(ws, "tmp", "x"), "ORIG-TMP")

	_, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("apply on repo-like workspace failed: %v", err)
	}
	if got := read(t, filepath.Join(ws, ".git", "config")); got != "ORIG-GIT" {
		t.Errorf(".git/config was modified: %q", got)
	}
	if got := read(t, filepath.Join(ws, "tmp", "x")); got != "ORIG-TMP" {
		t.Errorf("tmp/x was modified: %q", got)
	}
	if !strings.Contains(read(t, filepath.Join(ws, "main.go")), "// reviewmesh[") {
		t.Error("expected the allowed file main.go to be edited")
	}
}

func TestRunContext_PreCancelledHalts(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, _ := makeWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the run starts
	out, err := m.RunContext(ctx, Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err == nil {
		t.Fatal("a cancelled context should halt the run")
	}
	if out.Status != "halted" {
		t.Errorf("status = %q, want halted", out.Status)
	}
}

func TestApply_UnshownFileNotEdited(t *testing.T) {
	m := newManager(t, fake.UnshownFile)
	ws, file := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Status != "stable" {
		t.Errorf("status = %q, want stable", out.Status)
	}
	// the finding targeted a file never shown to the reviewer → no edit to any real file
	if strings.Contains(read(t, file), "// reviewmesh[") {
		t.Error("a finding for an unshown file must not edit a real workspace file")
	}
}

func TestCI_SurfaceCapsToReportNoWrite(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, file := makeWorkspace(t)
	// even if apply is requested, the ci surface caps the effective mode to report
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "ci"})
	if err != nil {
		t.Fatalf("ci review: %v", err)
	}
	if out.Mode != review.ModeReport {
		t.Errorf("ci surface should cap mode to report, got %q", out.Mode)
	}
	if strings.Contains(read(t, file), "// reviewmesh[") {
		t.Error("ci mode must not write to the workspace")
	}
}

func TestInnerLoop_ApproveStopsAfterOnePass(t *testing.T) {
	m := newManager(t, fake.Empty) // verdict approve, no findings
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001-i2")); !os.IsNotExist(statErr) {
		t.Error("approve should stop the inner loop after one pass (no -i2)")
	}
}

func TestInnerLoop_StableStopsBeforeCap(t *testing.T) {
	m := newManager(t, fake.Valid) // same finding every pass → stable after two passes
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001-i2", "prompt.md")) // a 2nd pass ran
	if _, statErr := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001-i3")); !os.IsNotExist(statErr) {
		t.Error("a stable finding set should stop before maxInner (no -i3)")
	}
}

func TestInnerLoop_AddressedContextInLaterPass(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	p2 := read(t, filepath.Join(out.RunDir, "calls", "c-0001-i2", "prompt.md"))
	if !strings.Contains(p2, "PREVIOUSLY REPORTED THIS CYCLE") {
		t.Error("the second reviewer pass prompt should carry the in-cycle reported-context")
	}
}

func TestRetry_FirstMalformedThenValidSucceeds(t *testing.T) {
	m := newManager(t, fake.MalformedThenValid)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("retry should recover: %v", err)
	}
	if out.Status != "stable" {
		t.Errorf("status = %q, want stable", out.Status)
	}
	// a retry attempt directory must exist (proof the second attempt ran)
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001", "retry-1", "prompt.md"))
}

func TestRetry_BothMalformedHaltsClassG(t *testing.T) {
	m := newManager(t, fake.Malformed)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if fault.CodeOf(err) != fault.Internal {
		t.Fatalf("exit code = %d, want %d (Class G)", fault.CodeOf(err), fault.Internal)
	}
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001", "retry-1", "prompt.md")) // retry was attempted
	mustExist(t, filepath.Join(out.RunDir, "calls", "c-0001", "halt-record.json"))
}

// An identity mismatch does not retry — but for a different reason than a timeout does not. The output
// was schema-valid on the first attempt, so there is nothing to re-ask for: a retry would spend a second
// call to receive the same usable answer with the same unexpected label.
func TestNoRetry_IdentityMismatch(t *testing.T) {
	m := newManager(t, fake.IdentityMismatch)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("an identity mismatch must not halt: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001", "retry-1")); !os.IsNotExist(statErr) {
		t.Error("identity mismatch must NOT retry — the first answer was already schema-valid")
	}
}

func TestNoRetry_Timeout(t *testing.T) {
	m := newManager(t, fake.Timeout)
	ws, _ := makeWorkspace(t)
	out, _ := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if _, statErr := os.Stat(filepath.Join(out.RunDir, "calls", "c-0001", "retry-1")); !os.IsNotExist(statErr) {
		t.Error("adapter timeout/failure must NOT retry")
	}
}

func TestConverge_NoFindings_Stable(t *testing.T) {
	m := newManager(t, fake.Empty)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if out.Status != "stable" || len(out.Findings) != 0 {
		t.Errorf("status=%q findings=%d, want stable/0", out.Status, len(out.Findings))
	}
}

func TestApply_SingleCycleConfigSucceeds(t *testing.T) {
	m := newManager(t, fake.Valid)
	one := 1
	m.Cfg.Review.MaxOuterCycles = &one // one-shot apply must succeed, not cap-halt
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err != nil {
		t.Fatalf("a single-cycle apply should succeed: %v", err)
	}
	if out.Status != "stable" {
		t.Errorf("status = %q, want stable", out.Status)
	}
}

func TestConverge_CapHalt_Exit7(t *testing.T) {
	m := newManager(t, fake.Churn)
	two := 2
	m.Cfg.Review.MaxOuterCycles = &two // make the cap quick + deterministic
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeApply, Surface: "cli"})
	if err == nil {
		t.Fatal("expected a cap halt for a non-converging (churn) review")
	}
	if fault.CodeOf(err) != fault.Policy {
		t.Errorf("exit code = %d, want %d (policy/cap)", fault.CodeOf(err), fault.Policy)
	}
	if out.Status != "halted" {
		t.Errorf("status = %q, want halted", out.Status)
	}
}

func mkFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyApprove_NoFindings(t *testing.T) {
	m := newManager(t, fake.Empty)
	ws, _ := makeWorkspace(t)
	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(out.Findings) != 0 {
		t.Errorf("findings = %d, want 0", len(out.Findings))
	}
}
