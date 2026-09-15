package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"

	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/registry"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
)

// launchRegistry resolves the adapters an agent surface was launched with (`--adapter`) into runnable
// adapters, at the paths the launch named. Nothing is read from aimesh configuration: the MCP and ACP
// servers take their adapters from their launch arguments alone, so they work on a fresh install. It
// returns an error naming any adapter the registry cannot build, so the server refuses to start rather
// than advertising an adapter it cannot run.
func launchRegistry(set launchflags.Set) (pipeline.Registry, error) {
	reg, unknown := registry.Build(namesPlan(set.Names()), set.Paths(), nil, 0)
	// The names plan has no collator, which Build reports as a blank name; only a named adapter counts.
	unknown = slices.DeleteFunc(unknown, func(n string) bool { return n == "" })
	if len(unknown) > 0 {
		return nil, fault.New(fault.Config, fmt.Sprintf("adapter(s) %s cannot be built", strings.Join(unknown, ", "))).
			WithReason(launchflags.ReasonUnknownAdapter)
	}
	return reg, nil
}

// noAdapterNotice is the stderr line an agent surface prints when it was launched with no adapter.
func noAdapterNotice(surface string) string {
	return fmt.Sprintf("aimesh explore %s: no adapter was named; every exploration is refused until the launch command adds --adapter <name> (or %s)", surface, launchflags.EnvVar)
}

// launchView is the MCP server's configuration projection for a launch-configured server: the adapters
// it was launched with and whether each can be started now. It carries no path.
type launchView struct {
	set launchflags.Set
}

// Adapters projects the launch set, checking each adapter's availability at the moment it is asked.
func (v launchView) Adapters() []mcp.AdapterFact {
	out := make([]mcp.AdapterFact, 0, len(v.set.Names()))
	for _, a := range v.set.Adapters() {
		ok, why := v.set.Available(a.Name)
		fact := mcp.AdapterFact{Name: a.Name, Kind: "shell", Available: ok, Source: string(a.Source)}
		if !ok {
			fact.Reason = why
		}
		if a.Name == launchflags.FakeAdapter {
			fact.Kind = "fake"
		} else if r, known := shell.Recipes()[a.Name]; known {
			fact.IdentityEvidenceCapability = string(r.Evidence)
		}
		out = append(out, fact)
	}
	return out
}

// Readiness reports, without starting any model call, whether each launched adapter's CLI can be started.
// It is OK when at least one can.
func (v launchView) Readiness() (bool, []mcp.ReadinessCheck) {
	if v.set.Empty() {
		return false, []mcp.ReadinessCheck{{
			Name: "adapters", OK: false,
			Detail: fmt.Sprintf("this server was launched with no adapter; add --adapter <name> (or %s) to the host configuration that starts it", launchflags.EnvVar),
		}}
	}
	ok := false
	checks := make([]mcp.ReadinessCheck, 0, len(v.set.Names()))
	for _, a := range v.set.Adapters() {
		avail, why := v.set.Available(a.Name)
		detail := fmt.Sprintf("%s (named by %s)", why, a.Source)
		checks = append(checks, mcp.ReadinessCheck{Name: "adapter: " + a.Name, OK: avail, Detail: detail})
		ok = ok || avail
	}
	return ok, checks
}
