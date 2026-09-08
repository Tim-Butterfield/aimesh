package adapterlocations

import (
	"os"
	"path/filepath"
	"testing"
)

func strptr(s string) *string { return &s }

func TestLoad_MissingIsEmpty(t *testing.T) {
	loc, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file must be absent, not an error: %v", err)
	}
	if len(loc.Adapters) != 0 {
		t.Errorf("missing file should yield no adapters, got %v", loc.Adapters)
	}
}

func TestWriteLoad_RoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "adapters.yaml")
	in := Locations{Adapters: map[string]Entry{
		"claude-code": {Path: strptr("/opt/claude")},
		"codex-cli":   {Path: strptr("")}, // explicit clear
	}}
	if err := Write(p, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	loc, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loc.SchemaVersion != SchemaVersion {
		t.Errorf("schemaVersion = %d", loc.SchemaVersion)
	}
	if loc.Adapters["claude-code"].Path == nil || *loc.Adapters["claude-code"].Path != "/opt/claude" {
		t.Errorf("claude path not round-tripped: %+v", loc.Adapters["claude-code"])
	}
	// explicit clear round-trips as a non-nil empty string (distinguishable from omission)
	if e := loc.Adapters["codex-cli"]; e.Path == nil || *e.Path != "" {
		t.Errorf("explicit clear must round-trip as non-nil empty, got %+v", e)
	}
}

func TestLoad_RejectsBadSchemaVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "adapters.yaml")
	_ = os.WriteFile(p, []byte("schemaVersion: 99\nadapters: {}\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("an unsupported schemaVersion must be rejected")
	}
}

func TestUpdate_TransactionalPreserves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "adapters.yaml")
	// Update on a missing file starts from empty and creates it.
	if err := Update(p, func(l *Locations) { l.Adapters["a"] = Entry{Path: strptr("/a")} }); err != nil {
		t.Fatalf("update 1: %v", err)
	}
	// A second, unrelated update must preserve the first (real read-modify-write, not clobber).
	if err := Update(p, func(l *Locations) { l.ACPAdapters["x"] = ACPInstance{Path: "/x", Args: []string{"--acp"}} }); err != nil {
		t.Fatalf("update 2: %v", err)
	}
	loc, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loc.Adapters["a"].Path == nil || *loc.Adapters["a"].Path != "/a" {
		t.Errorf("first update's path was lost: %+v", loc.Adapters["a"])
	}
	if loc.ACPAdapters["x"].Path != "/x" || len(loc.ACPAdapters["x"].Args) != 1 {
		t.Errorf("second update's instance was lost: %+v", loc.ACPAdapters["x"])
	}
}

func TestACPInstances_RoundTripAndMerge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "adapters.yaml")
	user := Locations{ACPAdapters: map[string]ACPInstance{
		"acp-claude": {Title: "ACP: Claude", Path: "/opt/claude", Args: []string{"--acp"}},
		"acp-gemini": {Title: "ACP: Gemini", Args: []string{"acp"}},
	}}
	if err := Write(p, user); err != nil {
		t.Fatalf("write: %v", err)
	}
	loc, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c := loc.ACPAdapters["acp-claude"]; c.Path != "/opt/claude" || c.Title != "ACP: Claude" || len(c.Args) != 1 || c.Args[0] != "--acp" {
		t.Errorf("acp-claude did not round-trip: %+v", c)
	}
	// A project layer's instance replaces a user one of the same name (wholesale).
	project := Locations{ACPAdapters: map[string]ACPInstance{
		"acp-claude": {Title: "ACP: Claude (project)", Path: "/proj/claude", Args: []string{"--acp", "--stdio"}},
	}}
	merged := ACPInstances(user, project) // user first (lower precedence)
	if c := merged["acp-claude"]; c.Path != "/proj/claude" || len(c.Args) != 2 {
		t.Errorf("project instance should override user: %+v", c)
	}
	if _, ok := merged["acp-gemini"]; !ok {
		t.Errorf("a user-only instance must survive the merge: %v", merged)
	}
}

func TestPaths_InheritanceAndClear(t *testing.T) {
	user := Locations{Adapters: map[string]Entry{
		"claude-code": {Path: strptr("/user/claude")},
		"codex-cli":   {Path: strptr("/user/codex")},
		"ollama":      {}, // nil path — no override
	}}
	project := Locations{Adapters: map[string]Entry{
		"claude-code": {Path: strptr("/proj/claude")}, // overrides user
		"codex-cli":   {Path: strptr("")},             // explicit clear — removes the override
	}}
	got := Paths(user, project) // user first (lower precedence), project second
	if got["claude-code"] != "/proj/claude" {
		t.Errorf("project should override user: %v", got)
	}
	if _, ok := got["codex-cli"]; ok {
		t.Errorf("an explicit clear must remove the override (fall back to PATH), got %v", got)
	}
	if _, ok := got["ollama"]; ok {
		t.Errorf("a nil path is no override, got %v", got)
	}
}
