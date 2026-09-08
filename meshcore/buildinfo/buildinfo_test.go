package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo_StampedTagWithVCSFacts(t *testing.T) {
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.1.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "f62b0bd6974eb462ae127c81ed19d30dfc6053ad"},
			{Key: "vcs.time", Value: "2026-09-08T01:34:29Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	}
	f, ok := fromBuildInfo(bi, true)
	if !ok {
		t.Fatal("a stamped tag must be usable")
	}
	want := Facts{Version: "v0.1.0", ModuleVersion: "v0.1.0", Commit: "f62b0bd6974eb462ae127c81ed19d30dfc6053ad", Date: "2026-09-08T01:34:29Z", Dirty: false}
	if f != want {
		t.Errorf("facts = %+v, want %+v", f, want)
	}
}

// The human form names the tag the commit DESCENDS FROM plus the commit, never the next
// version the toolchain's pseudo-version points at.
func TestDisplayVersion(t *testing.T) {
	cases := []struct {
		module string
		dirty  bool
		want   string
	}{
		{"v0.1.0", false, "v0.1.0"},
		{"v0.1.0", true, "v0.1.0+dirty"},
		{"v0.1.1-0.20260908022103-3d6b10e9b8fa", false, "v0.1.0+3d6b10e"},             // after tag v0.1.0
		{"v0.1.1-0.20260908022103-3d6b10e9b8fa", true, "v0.1.0+3d6b10e.dirty"},        // same, modified tree
		{"v0.1.1-0.20260908022103-3d6b10e9b8fa+dirty", false, "v0.1.0+3d6b10e.dirty"}, // toolchain's own suffix
		{"v1.0.0-0.20260908022103-3d6b10e9b8fa", false, "v1.0.0+3d6b10e"},             // patch 0 cannot decrement: keep as named
		{"v0.2.0-rc1.0.20260908022103-3d6b10e9b8fa", false, "v0.2.0-rc1+3d6b10e"},     // after a pre-release tag
		{"v0.0.0-20260908022103-3d6b10e9b8fa", false, "dev+3d6b10e"},                  // no tag at all
		{"v0.0.0-20260908022103-3d6b10e9b8fa", true, "dev+3d6b10e.dirty"},
	}
	for _, c := range cases {
		if got := displayVersion(c.module, c.dirty); got != c.want {
			t.Errorf("displayVersion(%q, dirty=%v) = %q, want %q", c.module, c.dirty, got, c.want)
		}
	}
}

func TestFromBuildInfo_DirtyPseudoVersionSetsDirty(t *testing.T) {
	bi := &debug.BuildInfo{
		Main:     debug.Module{Version: "v0.1.1-0.20260908013429-f62b0bd69746+dirty"},
		Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}},
	}
	f, ok := fromBuildInfo(bi, true)
	if !ok {
		t.Fatal("a pseudo-version must be usable")
	}
	if !f.Dirty {
		t.Error("vcs.modified=true must set Dirty")
	}
	if f.Version != "v0.1.0+f62b0bd.dirty" {
		t.Errorf("version = %q, want v0.1.0+f62b0bd.dirty", f.Version)
	}
	if f.ModuleVersion != "v0.1.1-0.20260908013429-f62b0bd69746+dirty" {
		t.Errorf("the toolchain's own string must be kept untouched, got %q", f.ModuleVersion)
	}
}

// The placeholder the toolchain writes when it could not determine a version must NOT be reported
// as one: "(devel)" is what a `go test` binary and an out-of-VCS build carry, and a caller's own
// "dev" default is the honest answer there.
func TestFromBuildInfo_PlaceholdersAreNotVersions(t *testing.T) {
	for _, v := range []string{"", "(devel)", "  "} {
		if _, ok := fromBuildInfo(&debug.BuildInfo{Main: debug.Module{Version: v}}, true); ok {
			t.Errorf("version %q must not be reported as usable", v)
		}
	}
	if _, ok := fromBuildInfo(nil, false); ok {
		t.Error("absent build info must not be usable")
	}
}
