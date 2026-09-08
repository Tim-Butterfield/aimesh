package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// doctor is the readiness substrate BOTH apps compose their `doctor` surface from (and the gate the
// ACP-validation flow runs before spending anything). Its load-bearing rule is the required/opt-in
// asymmetry: an unavailable OPT-IN adapter must not fail a report, while an unavailable REQUIRED one
// must — get that backwards and either every install looks broken or a broken one looks ready.

// stubAdapter is a model.Adapter with controllable availability. probe != nil also makes it a Prober.
type stubAdapter struct {
	name   string
	avail  bool
	detail string
	probe  *model.ProbeResult
}

func (s stubAdapter) Name() string              { return s.name }
func (s stubAdapter) Available() (bool, string) { return s.avail, s.detail }
func (s stubAdapter) Invoke(context.Context, model.Call) (model.Result, error) {
	return model.Result{}, errors.New("stub: not invoked by doctor")
}

// probeAdapter adds the OPTIONAL model.Prober capability.
type probeAdapter struct {
	stubAdapter
	result model.ProbeResult
	gotCtx context.Context
}

func (p *probeAdapter) Probe(ctx context.Context) model.ProbeResult {
	p.gotCtx = ctx
	return p.result
}

func TestReport_FinalizeAndString(t *testing.T) {
	r := Report{Checks: []Check{{Name: "a", OK: true, Detail: "fine"}, {Name: "b", OK: true, Detail: "fine"}}}
	r.Finalize()
	if !r.OK {
		t.Error("all-passing checks must finalize OK")
	}
	if !strings.Contains(r.String(), "all static checks passed") {
		t.Errorf("passing report should say so:\n%s", r.String())
	}

	// A SINGLE failing check must sink the whole report (and must not be reset by later passes).
	r.Checks = append(r.Checks, Check{Name: "c", OK: false, Detail: "broken"}, Check{Name: "d", OK: true})
	r.Finalize()
	if r.OK {
		t.Error("one failing check must fail the report")
	}
	out := r.String()
	if !strings.Contains(out, "[FAIL] c — broken") || !strings.Contains(out, "one or more checks FAILED") {
		t.Errorf("failing report must name the failure:\n%s", out)
	}

	// Finalize is idempotent + recoverable: dropping the failure restores OK (no sticky state).
	r.Checks = r.Checks[:2]
	r.Finalize()
	if !r.OK {
		t.Error("Finalize must recompute from the current checks, not latch")
	}

	// An empty report is vacuously OK (an app appends its own checks; doctor imposes none).
	empty := Report{}
	empty.Finalize()
	if !empty.OK {
		t.Error("an empty report must finalize OK")
	}
}

// The required/opt-in asymmetry, and that results follow the caller's name ORDER (a stable CLI).
func TestAdapterAvailability_RequiredVsOptIn(t *testing.T) {
	adapters := map[string]model.Adapter{
		"present-ok":     stubAdapter{name: "present-ok", avail: true, detail: "found at /bin/x"},
		"present-broken": stubAdapter{name: "present-broken", avail: false, detail: "not on PATH"},
	}
	names := []string{"present-ok", "present-broken", "absent"}
	required := map[string]bool{"present-ok": true, "present-broken": true} // "absent" is opt-in

	got := AdapterAvailability(adapters, names, required)
	if len(got) != 3 {
		t.Fatalf("want one check per name, got %d", len(got))
	}
	for i, want := range names {
		if got[i].Name != "adapter: "+want {
			t.Errorf("check %d = %q, want the caller's order (adapter: %s)", i, got[i].Name, want)
		}
	}
	if !got[0].OK || !strings.Contains(got[0].Detail, "required") || !strings.Contains(got[0].Detail, "found at /bin/x") {
		t.Errorf("an available required adapter must pass and carry its detail: %+v", got[0])
	}
	if got[1].OK {
		t.Errorf("an UNAVAILABLE REQUIRED adapter must FAIL: %+v", got[1])
	}
	if !got[2].OK || !strings.Contains(got[2].Detail, "opt-in") || !strings.Contains(got[2].Detail, "not registered") {
		t.Errorf("an unregistered OPT-IN adapter must PASS, labeled opt-in: %+v", got[2])
	}

	// The same unavailable adapter, now opt-in, must pass — the asymmetry is the whole point.
	relaxed := AdapterAvailability(adapters, []string{"present-broken"}, map[string]bool{})
	if !relaxed[0].OK {
		t.Errorf("an unavailable OPT-IN adapter must not fail the report: %+v", relaxed[0])
	}
}

// ProbeAdapters must probe ONLY required + registered + available adapters (never spend effort on
// one already reported as failing), pass the caller's ctx through, and append a blocker signal.
func TestProbeAdapters_ScopeSignalAndContext(t *testing.T) {
	probed := &probeAdapter{
		stubAdapter: stubAdapter{name: "probed", avail: true, detail: "found"},
		result:      model.ProbeResult{OK: false, Detail: "cannot reach the account", Signal: "login_required"},
	}
	adapters := map[string]model.Adapter{
		"probed":      probed,
		"no-prober":   stubAdapter{name: "no-prober", avail: true, detail: "found"},
		"unavailable": stubAdapter{name: "unavailable", avail: false, detail: "missing"},
		"optional":    stubAdapter{name: "optional", avail: true, detail: "found"},
	}
	names := []string{"probed", "no-prober", "unavailable", "optional", "unregistered"}
	required := map[string]bool{"probed": true, "no-prober": true, "unavailable": true, "unregistered": true}

	type ctxKey string
	ctx := context.WithValue(context.Background(), ctxKey("k"), "v")
	got := ProbeAdapters(ctx, adapters, names, required)

	names_ := map[string]Check{}
	for _, c := range got {
		names_[c.Name] = c
	}
	if len(got) != 2 {
		t.Fatalf("only required+registered+available adapters are probed, got %d: %+v", len(got), got)
	}
	if _, ok := names_["probe: unavailable"]; ok {
		t.Error("an unavailable adapter must be SKIPPED (its failure is already reported by AdapterAvailability)")
	}
	if _, ok := names_["probe: optional"]; ok {
		t.Error("a non-required adapter must not be probed")
	}
	if _, ok := names_["probe: unregistered"]; ok {
		t.Error("an unregistered adapter must not be probed")
	}
	if c := names_["probe: no-prober"]; !c.OK || !strings.Contains(c.Detail, "no probe") {
		t.Errorf("an adapter without the Prober capability must pass as 'no probe': %+v", c)
	}
	if c := names_["probe: probed"]; c.OK || !strings.Contains(c.Detail, "cannot reach the account") || !strings.Contains(c.Detail, "[login_required]") {
		t.Errorf("a failing probe must fail and append its classified signal: %+v", c)
	}
	if probed.gotCtx == nil || probed.gotCtx.Value(ctxKey("k")) != "v" {
		t.Error("the caller's ctx must reach Probe so a cancel/timeout propagates")
	}
}

// A probe with no Signal must not grow an empty "[]" suffix.
func TestProbeAdapters_NoSignal_NoEmptyBrackets(t *testing.T) {
	a := &probeAdapter{
		stubAdapter: stubAdapter{name: "clean", avail: true, detail: "found"},
		result:      model.ProbeResult{OK: true, Detail: "ready"},
	}
	got := ProbeAdapters(context.Background(), map[string]model.Adapter{"clean": a}, []string{"clean"}, map[string]bool{"clean": true})
	if len(got) != 1 || got[0].Detail != "ready" {
		t.Errorf("a signal-less probe detail must pass through verbatim, got %+v", got)
	}
}

// deepAdapter adds the OPTIONAL model.DeepProber capability and records every spec it was handed, so
// a test can prove the once-per-adapter spend bound rather than assume it.
type deepAdapter struct {
	stubAdapter
	result model.ProbeResult
	specs  []model.DeepProbeSpec
	gotCtx context.Context
}

func (d *deepAdapter) ProbeDeep(ctx context.Context, spec model.DeepProbeSpec) model.ProbeResult {
	d.gotCtx = ctx
	d.specs = append(d.specs, spec)
	return d.result
}

// The deep probe SPENDS, so the scope rules are spend rules: one call per adapter no matter how many
// seats name it, nothing for an unavailable or unregistered adapter, and the caller's model argument
// carried through (a recipe builds it into the argv, so probing another model answers another
// question).
func TestProbeAdaptersDeep_OncePerAdapterAndCarriesTheModelArgument(t *testing.T) {
	deep := &deepAdapter{
		stubAdapter: stubAdapter{name: "deep", avail: true, detail: "found"},
		result:      model.ProbeResult{OK: false, Detail: "refused the throwaway directory", Signal: "folder_trust"},
	}
	adapters := map[string]model.Adapter{
		"deep":        deep,
		"no-deep":     stubAdapter{name: "no-deep", avail: true, detail: "found"},
		"unavailable": stubAdapter{name: "unavailable", avail: false, detail: "missing"},
	}
	type ctxKey string
	ctx := context.WithValue(context.Background(), ctxKey("k"), "v")
	got := ProbeAdaptersDeep(ctx, adapters, []DeepSeat{
		{Adapter: "deep", ModelArg: "m-first", Effort: "high"},
		{Adapter: "deep", ModelArg: "m-second"}, // the SAME adapter again: must not spend twice
		{Adapter: "no-deep", ModelArg: "x"},
		{Adapter: "unavailable", ModelArg: "x"},
		{Adapter: "unregistered", ModelArg: "x"},
	})

	byName := map[string]Check{}
	for _, c := range got {
		byName[c.Name] = c
	}
	if len(got) != 2 {
		t.Fatalf("want one row for `deep` and one for `no-deep`, got %d: %+v", len(got), got)
	}
	if len(deep.specs) != 1 {
		t.Fatalf("a second seat on the same adapter must NOT spend again: %d invocation(s) %+v", len(deep.specs), deep.specs)
	}
	if deep.specs[0].ModelArg != "m-first" || deep.specs[0].Effort != "high" {
		t.Errorf("the first seat's model/effort must reach the adapter, got %+v", deep.specs[0])
	}
	if deep.gotCtx == nil || deep.gotCtx.Value(ctxKey("k")) != "v" {
		t.Error("the caller's ctx must reach ProbeDeep so a cancel/timeout propagates")
	}
	if c := byName["probe-deep: deep"]; c.OK || !strings.Contains(c.Detail, "[folder_trust]") {
		t.Errorf("a failing deep probe must fail and append its classified signal: %+v", c)
	}
	if c := byName["probe-deep: no-deep"]; !c.OK || !strings.Contains(c.Detail, "no deep probe") {
		t.Errorf("an adapter without the capability must pass as 'no deep probe': %+v", c)
	}
	if _, ok := byName["probe-deep: unavailable"]; ok {
		t.Error("an unavailable adapter must be SKIPPED (its failure is already reported by AdapterAvailability)")
	}
	if _, ok := byName["probe-deep: unregistered"]; ok {
		t.Error("an unregistered adapter must not be deep-probed")
	}
}

// The projection must let a consumer tell a FREE check from one that SPENT. A deep row is both
// `probe` (it executed something) and `deep` (it cost money); a cheap row is only the former.
func TestProject_MarksDeepProbeRowsDistinctlyFromCheapOnes(t *testing.T) {
	v := Project(Report{OK: false, Checks: []Check{
		{Name: "adapter: x", OK: true},
		{Name: "probe: x", OK: true},
		{Name: "probe-deep: x", OK: false},
	}})
	if v.Checks[0].Probe || v.Checks[0].Deep {
		t.Errorf("a static check must be neither probe nor deep: %+v", v.Checks[0])
	}
	if !v.Checks[1].Probe || v.Checks[1].Deep {
		t.Errorf("a cheap probe row must be probe-but-not-deep: %+v", v.Checks[1])
	}
	if !v.Checks[2].Probe || !v.Checks[2].Deep {
		t.Errorf("a deep probe row must be BOTH probe and deep — a consumer that treats readiness as free needs to see it spent: %+v", v.Checks[2])
	}
}

func TestWritableDir(t *testing.T) {
	base := t.TempDir()

	// Creates nested dirs as needed, and leaves NO probe file behind.
	target := filepath.Join(base, "a", "b")
	if !WritableDir(target) {
		t.Fatal("a creatable dir must report writable")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the write probe must clean up after itself, found %d entrie(s)", len(entries))
	}

	if WritableDir("") {
		t.Error("an empty dir must not report writable")
	}

	// A path whose PARENT is a regular file cannot be created → not writable.
	file := filepath.Join(base, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if WritableDir(filepath.Join(file, "sub")) {
		t.Error("a dir under a regular file must not report writable")
	}
}

// Guard the evidence-tier import surface doctor shares with the apps (a compile-time canary that
// core's tiers stay where doctor's consumers expect them).
var _ core.IdentityEvidence = core.EvidenceNone
