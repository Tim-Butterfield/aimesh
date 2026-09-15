package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// --reviewer model=… may only name a modelCatalog key, so list reports the catalog beside adapters
// and profiles.
func TestList_ShowsTheVocabularyItRequires(t *testing.T) {
	view := listView{
		Catalog: []listCatalogEntry{{
			Key: "claude-code-opus-medium", Provider: "anthropic", CanonicalModel: "opus",
			Adapters: []listCatalogBind{{Adapter: "claude-code", ModelArg: "opus", Effort: "medium"}},
		}},
	}
	var b strings.Builder
	printList(&b, view)
	out := b.String()

	if !strings.Contains(out, "claude-code-opus-medium") {
		t.Errorf("the catalog KEY is missing — it is the exact token --reviewer requires:\n%s", out)
	}
	// The model argument often differs from the key, and effort is embedded in the key by convention.
	if !strings.Contains(out, "via claude-code as opus") {
		t.Errorf("the adapter binding and its model argument are missing:\n%s", out)
	}
	if !strings.Contains(out, "effort medium") {
		t.Errorf("the resolved effort is missing — it is embedded in the key and must not be inferred:\n%s", out)
	}
	// The catalog is shown beside the flag that consumes it.
	if !strings.Contains(out, "--reviewer") {
		t.Errorf("the catalog heading does not say what the keys are FOR:\n%s", out)
	}
}

// The JSON projection carries the catalog too.
func TestListJSON_CarriesTheCatalog(t *testing.T) {
	payload, err := json.Marshal(listView{Catalog: []listCatalogEntry{{
		Key: "k", Adapters: []listCatalogBind{{Adapter: "a", ModelArg: "m"}},
	}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["modelCatalog"]; !ok {
		t.Fatalf("list --json has no modelCatalog key: %s", payload)
	}
}
