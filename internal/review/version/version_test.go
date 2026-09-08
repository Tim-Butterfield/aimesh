package version

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGet_HasRuntimeFacts(t *testing.T) {
	i := Get()
	if i.GoVersion == "" || i.OS == "" || i.Arch == "" {
		t.Errorf("missing runtime facts: %+v", i)
	}
}

func TestJSON_RetainsFullMetadata(t *testing.T) {
	// The --json form keeps the full build metadata for audit/debug/release stamping,
	// even though the default --version line does not show it.
	s, err := Get().JSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("version JSON invalid: %v", err)
	}
	for _, k := range []string{"version", "commit", "date", "dirty", "goVersion", "os", "arch"} {
		if _, ok := m[k]; !ok {
			t.Errorf("version JSON missing %q (metadata must stay internally for audit/release)", k)
		}
	}
}

// The default --version line is exactly "reviewmesh <version>" — a single user-facing
// line with no commit/date/dirty/go/os-arch.
func TestString_SingleUserFacingLine(t *testing.T) {
	s := Get().String()
	if s != "reviewmesh "+Version {
		t.Errorf("String() = %q, want %q", s, "reviewmesh "+Version)
	}
	if strings.Contains(s, "\n") {
		t.Errorf("version line must be a single line, got %q", s)
	}
	for _, leaked := range []string{"commit", "date", "go:", "os/arch", "dirty", "(dirty)"} {
		if strings.Contains(s, leaked) {
			t.Errorf("version line must not expose %q: %q", leaked, s)
		}
	}
}

// An unstamped local build prints "reviewmesh dev".
func TestString_UnstampedDev(t *testing.T) {
	if got := (Info{Version: "dev"}).String(); got != "reviewmesh dev" {
		t.Errorf("unstamped String() = %q, want \"reviewmesh dev\"", got)
	}
}
