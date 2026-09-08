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
	want := Facts{Version: "v0.1.0", Commit: "f62b0bd6974eb462ae127c81ed19d30dfc6053ad", Date: "2026-09-08T01:34:29Z", Dirty: false}
	if f != want {
		t.Errorf("facts = %+v, want %+v", f, want)
	}
}

func TestFromBuildInfo_DirtyPseudoVersion(t *testing.T) {
	bi := &debug.BuildInfo{
		Main:     debug.Module{Version: "v0.1.1-0.20260908013429-f62b0bd69746+dirty"},
		Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}},
	}
	f, ok := fromBuildInfo(bi, true)
	if !ok {
		t.Fatal("a pseudo-version must be usable")
	}
	if f.Version != "v0.1.1-0.20260908013429-f62b0bd69746" {
		t.Errorf("version = %q, want the +dirty suffix stripped (the Dirty flag carries it)", f.Version)
	}
	if !f.Dirty {
		t.Error("vcs.modified=true must set Dirty")
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
