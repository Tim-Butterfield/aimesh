package cli

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// --- aimesh mcp ---------------------------------------------------------------------------------

// The composed server is the entry a host config points at, so the root must dispatch it at all.
// Before this it answered `unknown command "mcp"` while both domains had their own.
func TestMCP_IsAShippedRootCommand(t *testing.T) {
	_, out, _ := run(t, "--help")
	if !strings.Contains(out, "  mcp ") && !strings.Contains(out, "  mcp\n") {
		t.Error("usage does not list the mcp command")
	}
	if code, _, errb := run(t, "mcp", "--only", "sideways"); code == int(fault.OK) {
		t.Errorf("an unknown --only must fail; stderr=%q", errb)
	}
}

// --only names a domain or it is refused. A typo that silently served BOTH would hand a caller the
// tools they explicitly asked not to have — the tool list is the enforcement.
func TestMCP_OnlyIsValidatedNotDefaulted(t *testing.T) {
	for _, bad := range []string{"sideways", "reviews", "Review ", "both"} {
		code, _, errb := run(t, "mcp", "--only", bad)
		if code != int(fault.Usage) {
			t.Errorf("--only %q: exit = %d, want %d", bad, code, fault.Usage)
		}
		if !strings.Contains(errb, "not a domain") {
			t.Errorf("--only %q must say why: %q", bad, errb)
		}
	}
}

// A domain-specific flag whose domain is NOT served is refused, never ignored.
//
// This is the case that matters most: --allow-remediate is a capability grant. Accepting it quietly
// under `--only explore` would leave an operator believing they had enabled a write on a server that
// does not even carry the tool.
func TestMCP_AFlagWhoseDomainIsNotServedIsRefused(t *testing.T) {
	cases := []struct{ only, flag string }{
		{"explore", "--allow-remediate"},
		// A bool, so it needs no value — the table drives flags positionally and a value-taking flag
		// would fail on the argument rather than on the domain rule this test is about.
		{"explore", "--verify-baseline"},
		{"explore", "--no-default-root"},
		{"review", "--no-capture"},
	}
	for _, c := range cases {
		code, _, errb := run(t, "mcp", "--only", c.only, c.flag)
		if code != int(fault.Usage) {
			t.Errorf("--only %s %s: exit = %d, want %d — a flag that would do nothing must be refused", c.only, c.flag, code, fault.Usage)
		}
		if !strings.Contains(errb, c.flag) {
			t.Errorf("the refusal must NAME the flag it is refusing: %q", errb)
		}
		if !strings.Contains(errb, "would do nothing") {
			t.Errorf("the refusal must say why: %q", errb)
		}
	}
}

// The mirror of the above: a flag IS accepted when its domain is served. Without this, a refusal
// rule that simply rejected every domain flag would pass the test above and break every real launch.
func TestMCP_AFlagIsAcceptedWhenItsDomainIsServed(t *testing.T) {
	// --only review with a review flag must get PAST flag validation. It then fails or serves on its
	// own merits, which this test does not drive; what it must not be is a usage error about the flag.
	_, _, errb := run(t, "mcp", "--only", "review", "--allow-remediate", "--help")
	if strings.Contains(errb, "would do nothing") {
		t.Errorf("--allow-remediate is review's own flag and --only review serves review: %q", errb)
	}
}

// Every domain-specific flag this package refuses must actually exist on the composed command, or
// the refusal table is guarding names nothing declares — which would silently stop refusing if a
// flag were renamed.
func TestMCP_TheRefusalTableNamesRealFlags(t *testing.T) {
	_, out, _ := run(t, "mcp", "--help")
	help := out
	if help == "" {
		// --help on a FlagSet writes to the error writer; take whichever carried it.
		_, _, errb := run(t, "mcp", "--help")
		help = errb
	}
	for name := range domainFlags {
		if !strings.Contains(help, "-"+name) {
			t.Errorf("the refusal table names %q, which `aimesh mcp --help` does not declare", name)
		}
	}
}
