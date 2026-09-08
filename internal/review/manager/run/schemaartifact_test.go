package run

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/meshcore/jsonschema"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
)

// docs/schema/effective-config.schema.json must describe the artifact that is actually written. A
// schema `require`ing a `_meta` resolution-provenance block (resolvedAt, layerOrder, profileSource,
// …) that nothing in this repo writes would make the `config-effective.json` a run persists FAIL its
// own schema — and an `allOf` + `additionalProperties: false` combination is unsatisfiable
// regardless. This test is what keeps the schema honest, by validating a REAL run's artifact rather
// than a hand-built stand-in.
func TestConfigEffectiveArtifact_ValidatesAgainstItsSchema(t *testing.T) {
	m := newManager(t, fake.Valid)
	ws, _ := makeWorkspace(t)

	out, err := m.Run(Request{Workspace: ws, Mode: review.ModeReport, Surface: "cli"})
	if err != nil {
		t.Fatalf("report run failed: %v", err)
	}
	artifact := filepath.Join(out.RunDir, "config-effective.json")
	b, rerr := os.ReadFile(artifact)
	if rerr != nil {
		t.Fatalf("every run must persist config-effective.json: %v", rerr)
	}

	root := repoRootFromPackage(t)
	schema, cerr := jsonschema.CompileFile(filepath.Join(root, "docs", "schema", "effective-config.schema.json"))
	if cerr != nil {
		t.Fatalf("compile effective-config.schema.json: %v", cerr)
	}
	if err := schema.ValidateJSON(b); err != nil {
		t.Fatalf("the run's config-effective.json does not validate against docs/schema/effective-config.schema.json — the schema describes a file this repo does not write: %v", err)
	}
}

// repoRootFromPackage walks up to the directory holding docs/schema.
func repoRootFromPackage(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, serr := os.Stat(filepath.Join(dir, "docs", "schema")); serr == nil {
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
