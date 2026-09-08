package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	store "github.com/Tim-Butterfield/aimesh/meshcore/config"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
)

// docs/schema/config.schema.json is a REFERENCE declaration: nothing loads it at runtime, and the
// typed strict decoder in this package is what a running binary enforces. That is exactly why it
// needs a test. A declaration nothing checks drifts, and this one had: `surfaces` and `defaults`
// were typed only as `object` (so half the write-authority policy was undescribed), `schemaVersion`
// and `lanes` were marked `required` when the decoder requires neither, and the `runtime` enum
// forbade a value the decoder accepts.
//
// The contract these tests hold is two-sided, because a schema can lie in both directions:
//
//   - it must ACCEPT everything the decoder accepts (the shipped seed, the example profiles, a
//     partial layer that names one key), or it tells a reader their valid config is invalid;
//   - it must REFUSE what the decoder refuses (an unknown key, and specifically the two inert keys
//     that were removed), or it invites a config the loader will reject.

// repoRoot walks up from the test's package directory to the directory holding docs/schema.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "docs", "schema")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the repo root (no docs/schema above the package directory)")
	return ""
}

func configSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	s, err := jsonschema.CompileFile(filepath.Join(repoRoot(t), "docs", "schema", "config.schema.json"))
	if err != nil {
		t.Fatalf("compile config.schema.json: %v", err)
	}
	return s
}

// TestConfigSchema_AcceptsTheShippedSeed is the round trip the schema exists for: the seed this
// binary ships must validate against the file that claims to describe it.
func TestConfigSchema_AcceptsTheShippedSeed(t *testing.T) {
	schema := configSchema(t)
	if err := schema.ValidateGo(Default()); err != nil {
		t.Fatalf("the shipped seed config does not validate against docs/schema/config.schema.json: %v", err)
	}
	// …and with the example profiles merged in, since docs point at them as things a user creates.
	if err := schema.ValidateGo(WithExampleProfiles(Default())); err != nil {
		t.Fatalf("seed + example profiles does not validate: %v", err)
	}
}

// TestConfigSchema_AcceptsWhateverTheDecoderAccepts feeds real layers through BOTH the decoder and
// the schema and fails on any disagreement. A layer the decoder takes and the schema rejects is the
// bug the `required` lists were: the schema was stricter than the loader, so it described a
// configuration reviewmesh does not actually demand.
func TestConfigSchema_AcceptsWhateverTheDecoderAccepts(t *testing.T) {
	schema := configSchema(t)
	layers := map[string]string{
		"empty":                          "{}\n",
		"only a default profile":         "defaultProfile: native-three-provider\n",
		"a profile with no lanes at all": "profiles:\n  panelled:\n    reviewers:\n      - execution: adapter\n        adapter: fake\n        model: fake-model\n",
		"one lane, no schemaVersion":     "profiles:\n  p:\n    lanes:\n      verifier:\n        execution: adapter\n        adapter: fake\n        model: fake-model\n",
		"a cloud catalog entry":          "modelCatalog:\n  hosted:\n    provider: anthropic\n    runtime: cloud\n    canonicalModel: claude-sonnet\n    adapters:\n      claude-code:\n        modelArg: sonnet\n",
		"surfaces policy in full":        "surfaces:\n  defaultModeBySurface:\n    mcp: report\n  capabilitiesBySurface:\n    mcp: [allowRemediate]\n  degradeWhenModeUnavailable: false\n",
		"defaults":                       "defaults:\n  adapterPreference: [fake]\n  profileForAdapter:\n    \"*\": default\n",
		"review caps":                    "review:\n  maxInnerIterations: 2\n  maxOuterCycles: 1\n  maxPanelRounds: 3\n",
		"an adapter entry":               "adapters:\n  fake:\n    modelIdentity: self_report\n    enabled: false\n",
	}
	for name, yaml := range layers {
		t.Run(name, func(t *testing.T) {
			decoded, err := parseConfigBytes("layer.yaml", []byte(yaml))
			if err != nil {
				t.Fatalf("the DECODER refused this layer, so the fixture is wrong: %v", err)
			}
			// Validate the layer as written, not just the decoded form: a schema that only ever
			// sees Go's marshalling would never notice a key it fails to describe.
			var asAny any
			raw, cerr := yamlLayerToJSON(t, yaml)
			if cerr != nil {
				t.Fatalf("fixture does not convert: %v", cerr)
			}
			if err := json.Unmarshal(raw, &asAny); err != nil {
				t.Fatalf("fixture does not parse: %v", err)
			}
			if err := schema.Validate(asAny); err != nil {
				t.Fatalf("the SCHEMA refused a layer the decoder accepted — the schema is stricter than what runs: %v", err)
			}
			// The decoded form must validate too: that is the shape merge/marshal round-trips.
			if err := schema.ValidateGo(decoded); err != nil {
				t.Fatalf("the decoded layer does not validate: %v", err)
			}
		})
	}
}

// TestConfigSchema_RefusesWhateverTheDecoderRefuses is the other half. Both must reject, and for
// the same key — otherwise the schema invites a config the loader will fail.
func TestConfigSchema_RefusesWhateverTheDecoderRefuses(t *testing.T) {
	schema := configSchema(t)
	layers := map[string]string{
		"a removed reserved section":      "policy:\n  providerDiversity: true\n",
		"a misspelled top-level key":      "defaultProfiles: default\n",
		"the removed defaults.autoDetect": "defaults:\n  autoDetect: true\n",
		"the removed lane optional flag":  "profiles:\n  p:\n    lanes:\n      verifier:\n        execution: adapter\n        optional: true\n",
		"a stray key inside review":       "review:\n  maxInnerIterations: 2\n  timeoutSeconds: 30\n",
		"a stray key inside a seat":       "profiles:\n  p:\n    reviewers:\n      - adapter: fake\n        model: fake-model\n        effort: deep\n",
	}
	for name, yaml := range layers {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfigBytes("layer.yaml", []byte(yaml)); err == nil {
				t.Fatal("the DECODER accepted this layer, so the fixture is wrong")
			}
			raw, cerr := yamlLayerToJSON(t, yaml)
			if cerr != nil {
				t.Fatalf("fixture does not convert: %v", cerr)
			}
			var asAny any
			if err := json.Unmarshal(raw, &asAny); err != nil {
				t.Fatalf("fixture does not parse: %v", err)
			}
			if err := schema.Validate(asAny); err == nil {
				t.Fatal("the SCHEMA accepted a layer the decoder refuses — a reader would write a config that fails to load")
			}
		})
	}
}

// yamlLayerToJSON converts a fixture through the SAME converter the loader uses, so the schema and
// the decoder are judging the same bytes rather than two readings of the same text.
func yamlLayerToJSON(t *testing.T, yaml string) ([]byte, error) {
	t.Helper()
	return store.YAMLToJSON([]byte(yaml))
}
