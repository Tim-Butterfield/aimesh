package main

import (
	"path/filepath"
	"testing"
)

// scanMeshcore derives paths with filepath.Rel, so the "mcp/" exemption must also match a natively
// joined path such as mcp\tasks.go on Windows. filepath.Join is used because a literal backslash is an
// ordinary filename character on Unix.
func TestExemptTerm_MatchesANativelyJoinedPath(t *testing.T) {
	if file := filepath.Join("mcp", "tasks.go"); !exemptTerm(file, "task") {
		t.Errorf("exemptTerm(%q, \"task\") = false, want true (the mcp/ protocol exemption must apply)", file)
	}
}

// The exemption stays keyed by (package, term): tolerating the native separator must not widen it.
func TestExemptTerm_DoesNotLeakOutsideItsPackageOrTerm(t *testing.T) {
	cases := []struct{ file, term string }{
		{filepath.Join("workspace", "workspace.go"), "task"}, // right term, wrong package
		{filepath.Join("mcp", "tasks.go"), "review"},         // right package, wrong term
	}
	for _, c := range cases {
		if exemptTerm(c.file, c.term) {
			t.Errorf("exemptTerm(%q, %q) = true, want false", c.file, c.term)
		}
	}
}
