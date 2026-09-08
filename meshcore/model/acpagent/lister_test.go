package acpagent

import (
	"context"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// The generic ACP adapter satisfies the OPTIONAL model.Lister capability, so the lane editor can offer
// model discovery for a configured ACP instance (reading the session's advertised availableModels).
func TestAdapter_ImplementsLister(t *testing.T) {
	var _ model.Lister = (*Adapter)(nil)
}

func TestListModels_ReadsAdvertisedModels(t *testing.T) {
	// The fake ACP server advertises availableModels = [{modelId: <model>}] on session/new.
	a := fakeAdapter("gemini-3.5-flash", `{"findings":[]}`, "ok")
	models, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels error: %v", err)
	}
	if len(models) != 1 || models[0].Arg != "gemini-3.5-flash" {
		t.Errorf("ListModels = %+v, want one discovered model gemini-3.5-flash (from availableModels)", models)
	}

	cmd, kind := a.DiscoveryMechanism()
	if cmd == "" || kind != "acp" {
		t.Errorf("DiscoveryMechanism = (%q, %q), want a non-empty command + kind \"acp\"", cmd, kind)
	}
}

func TestListModels_NoModelsAdvertised_IsAnError(t *testing.T) {
	// No currentModelId → the fake advertises an empty availableModels; discovery must report the empty
	// result as an error so the editor guides the user to manual entry rather than an empty list.
	a := fakeAdapter("", `{"findings":[]}`, "ok")
	if _, err := a.ListModels(context.Background()); err == nil {
		t.Error("a session advertising no selectable models must return an error")
	}
}
