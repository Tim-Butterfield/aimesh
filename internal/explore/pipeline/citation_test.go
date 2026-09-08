package pipeline

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/model/fake"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// TestRun_Citations_ValidatedInPipeline is the end-to-end C1 assertion over the hermetic fake panel: the
// collator is SHOWN the `envelope#k` aliases in its prompt, cites some of them, and the host — inside
// pipeline.Run, so every surface gets the same result — keeps the refs that resolve, drops the ones that
// do not, and labels the finding it could not source. A citation defect must not halt the run.
func TestRun_Citations_ValidatedInPipeline(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.Valid)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("a citation defect must be FAIL-SOFT, not a halt: %v", err)
	}
	// The prompt taught the vocabulary: every primary envelope is labeled with its alias.
	for _, want := range []string{`"envelope":"envelope#0"`, `"envelope":"envelope#1"`, "envelope#0"} {
		if !strings.Contains(res.SynthesizePrompt, want) {
			t.Errorf("collator prompt does not show the citation vocabulary (%q missing):\n%s", want, res.SynthesizePrompt)
		}
	}

	out := mapOutput(t, res)
	if len(out.Findings) != 2 {
		t.Fatalf("expected the fake's 2 findings (one cited, one not), got %d", len(out.Findings))
	}
	cited, uncited := out.Findings[0], out.Findings[1]
	if cited.Uncited || len(cited.Sources) != 2 {
		t.Errorf("the cited finding must keep its resolving refs: %+v", cited)
	}
	if cited.Sources[0] != "envelope#0/claims/0" {
		t.Errorf("the claim-narrowed citation must survive verbatim, got %q", cited.Sources[0])
	}
	if !uncited.Uncited {
		t.Error("the finding whose only source was free text must be labeled uncited by the HOST")
	}
	if len(uncited.Sources) != 0 {
		t.Errorf("an unresolvable ref must be DROPPED, not retained: %v", uncited.Sources)
	}
	// The pass metric: no ref that does not resolve survives anywhere in the collation.
	ix := schema.NewCitationIndex(res.Envelopes)
	for _, f := range out.Findings {
		for _, s := range f.Sources {
			if !ix.Valid(s) {
				t.Errorf("invalid ref %q survived into the terminal output", s)
			}
		}
	}
}

// TestRun_Citations_AllCited_NoUncitedAndNoSelfCertification drives the CollatorCiteAll fake: every
// finding cites a REAL alias, so nothing is uncited — and one of them additionally CLAIMS `uncited: true`,
// which the host must overwrite. A model can make a finding hard to source; it can never decide the answer.
func TestRun_Citations_AllCited_NoUncitedAndNoSelfCertification(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid}, fake.CollatorCiteAll)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out := mapOutput(t, res)
	if len(out.Findings) == 0 {
		t.Fatal("expected findings")
	}
	for _, f := range out.Findings {
		if f.Uncited {
			t.Errorf("an all-cited collation must carry NO uncited finding; %q was marked uncited (a model-supplied claim must be overwritten, not believed)", f.Statement)
		}
		if len(f.Sources) == 0 {
			t.Errorf("finding %q kept no citations", f.Statement)
		}
	}
}

// TestRun_Citations_WeakEnvelopeIsCitable: a weak-identity explorer is a full member of the citable
// vocabulary. The k-membership rule is about which envelopes THIS PANEL produced, not about how well
// each seat could prove its own model — a finding drawn from an unverifiable seat is still a finding
// drawn from a real response, and refusing its citation would leave that finding marked uncited.
func TestRun_Citations_WeakEnvelopeIsCitable(t *testing.T) {
	reg, plan := env(t, []fake.Scenario{fake.Valid, fake.Valid, fake.UnknownIdentity}, fake.Valid)
	res, err := run(t, reg, plan)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Envelopes) != 3 {
		t.Fatalf("expected the weak explorer to stay in the record: %d envelopes", len(res.Envelopes))
	}
	if !strings.Contains(res.SynthesizePrompt, `"envelope":"envelope#2"`) {
		t.Error("the weak explorer's alias must be offered as a citable source")
	}
	if ix := schema.NewCitationIndex(res.Envelopes); !ix.Valid("envelope#2") {
		t.Error("a ref to a weak-identity envelope of this panel must resolve")
	}
}
