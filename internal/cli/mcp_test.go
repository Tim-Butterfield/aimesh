package cli

import (
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// --- aimesh mcp ---------------------------------------------------------------------------------

// The composed server is the entry a host config points at, so the root must dispatch it.
func TestMCP_IsAShippedRootCommand(t *testing.T) {
	_, out, _ := run(t, "--help")
	if !strings.Contains(out, "  mcp ") && !strings.Contains(out, "  mcp\n") {
		t.Error("usage does not list the mcp command")
	}
	if code, _, errb := run(t, "mcp", "--only", "sideways"); code == int(fault.OK) {
		t.Errorf("an unknown --only must fail; stderr=%q", errb)
	}
}

// An --only value that names no domain is refused rather than defaulted to both.
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

// A domain-specific flag whose domain is not served is refused rather than ignored.
func TestMCP_AFlagWhoseDomainIsNotServedIsRefused(t *testing.T) {
	cases := []struct{ only, flag string }{
		{"explore", "--allow-writes"},
		// Boolean flags, so a missing value cannot fail the parse before the domain rule applies.
		{"explore", "--verify-baseline"},
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

// A flag is accepted when its domain is served.
func TestMCP_AFlagIsAcceptedWhenItsDomainIsServed(t *testing.T) {
	// Only the flag-domain refusal is checked; the launch itself is not driven.
	_, _, errb := run(t, "mcp", "--only", "review", "--allow-writes", "--help")
	if strings.Contains(errb, "would do nothing") {
		t.Errorf("--allow-writes is review's own flag and --only review serves review: %q", errb)
	}
}

// Every flag in the refusal table must exist on the composed command, so a renamed flag cannot
// silently escape the rule.
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
