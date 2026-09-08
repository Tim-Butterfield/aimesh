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

// The human form is the tag the commit DESCENDS FROM and nothing else — never the next version
// the toolchain's pseudo-version points at, never the commit, never a dirty marker (those live
// in Commit and Dirty). This is a deliberate presentation decision: the line reads as a version.
func TestDisplayVersion(t *testing.T) {
	cases := []struct {
		module string
		want   string
	}{
		{"v0.1.0", "v0.1.0"},
		{"v0.1.0+dirty", "v0.1.0"},
		{"v0.1.1-0.20260908022103-3d6b10e9b8fa", "v0.1.0"},         // after tag v0.1.0
		{"v0.1.1-0.20260908022103-3d6b10e9b8fa+dirty", "v0.1.0"},   // same, modified tree
		{"v1.0.0-0.20260908022103-3d6b10e9b8fa", "v1.0.0"},         // patch 0 cannot decrement: keep as named
		{"v0.2.0-rc1.0.20260908022103-3d6b10e9b8fa", "v0.2.0-rc1"}, // after a pre-release tag
		{"v0.0.0-20260908022103-3d6b10e9b8fa", "dev"},              // no tag at all
	}
	for _, c := range cases {
		if got := displayVersion(c.module); got != c.want {
			t.Errorf("displayVersion(%q) = %q, want %q", c.module, got, c.want)
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
	if f.Version != "v0.1.0" {
		t.Errorf("version = %q, want the bare nearest tag v0.1.0 (dirtiness is reported by Dirty, not the line)", f.Version)
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
