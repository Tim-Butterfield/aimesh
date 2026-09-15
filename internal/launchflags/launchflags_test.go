package launchflags

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
	"github.com/Tim-Butterfield/aimesh/meshcore/pathexpand"
)

func env(goos string, vars map[string]string) pathexpand.Env {
	return pathexpand.Env{
		GOOS: goos,
		Lookup: func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		},
		Home: func() (string, error) { return "/home/alice", nil },
	}
}

func parse(t *testing.T, args ...string) *Adapters {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a := RegisterAdapters(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return a
}

func TestResolve_Flags(t *testing.T) {
	a := parse(t, "--adapter", "codex-cli", "--adapter", "devin-cli=$TOOLS/devin")
	set, err := a.Resolve(env("linux", map[string]string{"TOOLS": "/opt/tools"}))
	if err != nil {
		t.Fatal(err)
	}
	got := set.Adapters()
	want := []Adapter{
		{Name: "codex-cli", Source: SourceFlag},
		{Name: "devin-cli", Path: "/opt/tools/devin", Source: SourceFlag},
	}
	if len(got) != len(want) {
		t.Fatalf("adapters = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("adapter %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if p := set.Paths(); len(p) != 1 || p["devin-cli"] != "/opt/tools/devin" {
		t.Fatalf("Paths() = %v", p)
	}
}

func TestResolve_SplitsOnFirstEquals(t *testing.T) {
	a := parse(t, "--adapter", "devin-cli=/opt/a=b/devin")
	set, err := a.Resolve(env("linux", nil))
	if err != nil {
		t.Fatal(err)
	}
	if ad, _ := set.Get("devin-cli"); ad.Path != "/opt/a=b/devin" {
		t.Fatalf("path = %q", ad.Path)
	}
}

func TestResolve_EnvForm(t *testing.T) {
	cases := []struct {
		goos, value string
		names       []string
	}{
		{"linux", "codex-cli:devin-cli=/opt/devin", []string{"codex-cli", "devin-cli"}},
		{"windows", `codex-cli;devin-cli=%LOCALAPPDATA%\devin\cli\bin\devin.exe`, []string{"codex-cli", "devin-cli"}},
		// Split before expansion: a separator inside an expanded value is not a separator.
		{"linux", "devin-cli=$WEIRD/devin", []string{"devin-cli"}},
	}
	for _, tc := range cases {
		vars := map[string]string{EnvVar: tc.value, "LOCALAPPDATA": `C:\Users\alice\AppData\Local`, "WEIRD": "/opt/a:b"}
		set, err := parse(t).Resolve(env(tc.goos, vars))
		if err != nil {
			t.Fatalf("%s %q: %v", tc.goos, tc.value, err)
		}
		if got := set.Names(); !equal(got, tc.names) {
			t.Fatalf("%s %q: names = %v, want %v", tc.goos, tc.value, got, tc.names)
		}
		for _, ad := range set.Adapters() {
			if ad.Source != SourceEnv {
				t.Fatalf("source = %q, want env", ad.Source)
			}
		}
	}
	set, _ := parse(t).Resolve(env("linux", map[string]string{EnvVar: "devin-cli=$WEIRD/devin", "WEIRD": "/opt/a:b"}))
	if ad, _ := set.Get("devin-cli"); ad.Path != "/opt/a:b/devin" {
		t.Fatalf("expanded path = %q, want /opt/a:b/devin", ad.Path)
	}
}

func TestResolve_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		vars   map[string]string
		reason string
	}{
		{"unknown name", []string{"--adapter", "nope-cli"}, nil, ReasonUnknownAdapter},
		{"duplicate", []string{"--adapter", "codex-cli", "--adapter", "codex-cli=/opt/codex"}, nil, ReasonDuplicateAdapter},
		{"empty flag value", []string{"--adapter", " "}, nil, ReasonEmptyEntry},
		{"empty path", []string{"--adapter", "codex-cli="}, nil, ReasonEmptyPath},
		{"flag and env", []string{"--adapter", "codex-cli"}, map[string]string{EnvVar: "devin-cli"}, ReasonFlagEnvConflict},
		{"env empty entry", nil, map[string]string{EnvVar: "codex-cli::devin-cli"}, ReasonEmptyEntry},
		{"env trailing separator", nil, map[string]string{EnvVar: "codex-cli:"}, ReasonEmptyEntry},
		{"undefined variable", []string{"--adapter", "devin-cli=$NOPE/devin"}, nil, pathexpand.ReasonUndefinedVariable},
		{"relative path", []string{"--adapter", "devin-cli=bin/devin"}, nil, pathexpand.ReasonNotAbsolute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.args...).Resolve(env("linux", tc.vars))
			if err == nil {
				t.Fatalf("want refusal %q", tc.reason)
			}
			if r := fault.ReasonOf(err); r != tc.reason {
				t.Fatalf("reason = %q, want %q (err %v)", r, tc.reason, err)
			}
			if fault.CodeOf(err) != fault.Config {
				t.Fatalf("code = %v, want config", fault.CodeOf(err))
			}
		})
	}
}

func TestResolve_NothingNamed(t *testing.T) {
	set, err := parse(t).Resolve(env("linux", nil))
	if err != nil || !set.Empty() {
		t.Fatalf("empty resolve = %+v, %v", set, err)
	}
}

func TestResolve_FakeOnlyUnderGate(t *testing.T) {
	t.Setenv(fake.EnvVar, "")
	if _, err := parse(t, "--adapter", FakeAdapter).Resolve(env("linux", nil)); fault.ReasonOf(err) != ReasonUnknownAdapter {
		t.Fatalf("the internal test adapter must be refused without its gate: err = %v", err)
	}
	t.Setenv(fake.EnvVar, "1")
	set, err := parse(t, "--adapter", FakeAdapter).Resolve(env("linux", nil))
	if err != nil || !set.Has(FakeAdapter) {
		t.Fatalf("the internal test adapter must be nameable under its gate: %+v, %v", set, err)
	}
	if ok, _ := set.Available(FakeAdapter); !ok {
		t.Fatal("the internal test adapter is always available")
	}
}

func TestAvailable_IsCheckedAtCallTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit check is Unix-specific")
	}
	bin := filepath.Join(t.TempDir(), "codex")
	set := NewSet(Adapter{Name: "codex-cli", Path: bin, Source: SourceFlag})
	if ok, _ := set.Available("codex-cli"); ok {
		t.Fatal("a missing binary must be unavailable")
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, why := set.Available("codex-cli"); !ok {
		t.Fatalf("a binary created after launch must become available without a restart: %s", why)
	}
	if ok, _ := set.Available("devin-cli"); ok {
		t.Fatal("an adapter that was not named is never available")
	}
}

func TestWrites(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	w := RegisterWrites(fs)
	if w.Allowed() {
		t.Fatal("writes must default to off")
	}
	if err := fs.Parse([]string{"--allow-writes"}); err != nil {
		t.Fatal(err)
	}
	if !w.Allowed() {
		t.Fatal("--allow-writes must grant writes")
	}
	var nilWrites *Writes
	if nilWrites.Allowed() {
		t.Fatal("an absent grant is not a grant")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
