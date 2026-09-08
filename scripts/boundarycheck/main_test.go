package main

import (
	"path/filepath"
	"testing"
)

// A protocol exemption that fails to match does not report itself — it just re-adds the failures the
// exemption exists to suppress, on one GOOS only. scanMeshcore derives the file from filepath.Rel,
// so the exemption must hold for a NATIVELY joined path: keyed on "mcp/", it silently missed
// `mcp\tasks.go` and turned every exempt identifier in meshcore/mcp into a FAIL on Windows while the
// Unix gate stayed green. filepath.Join is deliberate — a hardcoded backslash would assert nothing
// on Unix, where a backslash is a legal filename character rather than a separator.
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
