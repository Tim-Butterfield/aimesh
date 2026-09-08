package review

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRootPackageHygiene locks the public package boundary: every non-test root-level
// .go file must belong to package reviewmesh (no root-level package main) and must NOT
// import any internal implementation package, and the package must carry a doc comment
// for pkg.go.dev. It tests the boundary, not incidental filenames.
func TestRootPackageHygiene(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}
	fset := token.NewFileSet()
	sawDoc := false
	sawSource := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		sawSource = true
		f, perr := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		if f.Name.Name != "review" {
			t.Errorf("%s: package %q, want review (no package main here / mismatched package)", name, f.Name.Name)
		}
		if f.Doc != nil && strings.TrimSpace(f.Doc.Text()) != "" {
			sawDoc = true
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(p, "Tim-Butterfield/aimesh/internal/review") {
				t.Errorf("%s imports %q — the public root package must not depend on implementation packages", name, p)
			}
		}
	}
	if !sawSource {
		t.Fatal("no root-level .go source files found")
	}
	if !sawDoc {
		t.Error("root package is missing a package doc comment (doc.go) for pkg.go.dev")
	}
}

// TestDocsNoStaleProductPhrases guards user-facing docs against wording that contradicts the current
// product decisions (settled after the SPA redesign + visible-key cleanup): the normal UI shows no
// raw keys as secondary `key:` detail, the normal ACP flow is not "pick a profile + framing", and
// `fake` is an adapter (not the default profile). These EXACT stale phrases must not reappear in
// README.md or docs/*.md. It does NOT ban legitimate technical identifiers in config/schema examples.
func TestDocsNoStaleProductPhrases(t *testing.T) {
	banned := []string{
		"pick any saved profile + framing", // ACP framing is an Advanced diagnostic, not a normal control
		"saved profile + framing",          // "
		"raw key only as small",            // no raw keys as secondary `key:` detail in normal UI
		"fake adapter is the default",      // the built-in Default profile USES the Fake adapter
		"fake is the default profile",      // "
	}
	// README.md and docs/ live at the repo root, one level above this reviewmesh module.
	files := []string{"../README.md"}
	_ = filepath.WalkDir("../docs", func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue // a missing doc is not this test's concern
		}
		lower := strings.ToLower(string(data))
		for _, phrase := range banned {
			if strings.Contains(lower, phrase) {
				t.Errorf("%s contains stale product phrase %q — use current wording (readable names; ACP framing is an Advanced diagnostic; the built-in Default profile uses the Fake adapter)", f, phrase)
			}
		}
	}
}
