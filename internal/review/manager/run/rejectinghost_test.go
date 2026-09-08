package run

// A REVIEWER THAT FINDS SOMETHING AND A HOST THAT REJECTS IT.
//
// The shared fixture for any test needing a run that ends in a settled `reported_invalid` disposition
// rather than an applied one — today the composition and grounding tests.

import (
	"context"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/access/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// rejectingHostFake REJECTS every finding, in the natural adjudication AND in the dismissal-
// verification pass — so the rejection STANDS and the run ends with a settled `reported_invalid`
// disposition rather than one that a later pass reopens.
type rejectingHostFake struct{}

func (rejectingHostFake) Name() string              { return "rej-host" }
func (rejectingHostFake) Available() (bool, string) { return true, "ok" }
func (rejectingHostFake) Evidence() review.IdentityEvidence {
	return review.EvidenceInvocationTag
}
func (rejectingHostFake) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	mk := func(js string) (model.Result, error) {
		return model.Result{ExitCode: 0, ActualModel: string(c.ModelArg), Evidence: review.EvidenceInvocationTag, Stdout: []byte(js)}, nil
	}
	if c.Phase == string(review.PhaseAdjudicate) {
		return mk(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_adjudicate","adjudications":[{"findingId":"f1","validity":"invalid","decisionState":"reported_invalid","reasoning":"the caller declared this out of scope"}]}`)
	}
	return mk(`{"schemaVersion":1,"role":"author_remediator","phase":"semantic_author_review","verdict":"approve","findings":[]}`)
}

// rejectingManager: the deterministic `fake` reviewer (one finding on main.go) with a host lane that
// rejects it.
func rejectingManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv(fake.EnvVar, "1")
	cfg := config.Default()
	cfg.Adapters["rej-host"] = config.Adapter{ModelIdentity: "invocation_tag"}
	cfg.ModelCatalog["rej-model"] = config.CatalogEntry{
		Provider: "x", CanonicalModel: "mm", Adapters: map[string]config.AdapterModel{"rej-host": {ModelArg: "mm"}},
	}
	cfg.Profiles["prior"] = config.Profile{
		Description: "a reviewer that finds, a host that rejects", AdapterPreference: []string{"rej-host", "fake"},
		Lanes: map[string]config.Lane{
			"author_remediator": {Execution: "host", Adapter: "rej-host", Model: "rej-model"},
			"reviewer":          {Execution: "adapter", Adapter: "fake", Model: "fake-model"},
		},
	}
	return &Manager{
		Cfg:         cfg,
		Adapters:    map[string]model.Adapter{"fake": fake.New(fake.Valid), "rej-host": rejectingHostFake{}},
		ArtifactDir: t.TempDir(), TempBase: t.TempDir(),
	}
}
