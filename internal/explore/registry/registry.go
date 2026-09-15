// Package registry builds the adapter registry for a roster plan. A name matching one of meshcore's shell
// recipes or a user-defined ACP instance resolves to that adapter. The `fake` test adapter resolves only
// when the internal test gate is on. Any other name is reported as unknown rather than registered.
package registry

import (
	"maps"
	"path/filepath"
	"sort"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/explore/model/fake"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/meshcore/config/adapterlocations"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/acpagent"
	corefake "github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
)

// ResolveACPInstances loads the user-defined ACP adapters effective for cwd, from the user and project
// shared adapters.yaml, as acpagent instances for Build. Detect is the binary's basename when a path is set.
func ResolveACPInstances(cwd string) (map[string]acpagent.Instance, error) {
	locs, err := adapterlocations.ResolveACPInstances(cwd)
	if err != nil {
		return nil, err
	}
	out := make(map[string]acpagent.Instance, len(locs))
	for name, in := range locs {
		detect := name
		if in.Path != "" {
			detect = filepath.Base(in.Path)
		}
		out[name] = acpagent.Instance{Name: name, Detect: detect, Path: in.Path, Args: in.Args}
	}
	return out, nil
}

// FakeAdapter is the deterministic test adapter's key. It resolves only when corefake.Enabled, which
// tests and golden runs set.
const FakeAdapter = "fake"

// Build resolves every adapter the plan names (explorers, collator and explicit canonicalizers). paths
// overrides shell recipe binaries and acpInsts supplies ACP instances. Names that resolve to nothing are
// returned, sorted, as unknown, so a typo is a configuration error the caller reports before any model call.
func Build(plan roster.Plan, paths map[string]string, acpInsts map[string]acpagent.Instance, timeout time.Duration) (pipeline.Registry, []string) {
	real := shell.Registry(paths, timeout)
	maps.Copy(real, acpagent.Registry(acpInsts, timeout)) // user-defined ACP-client adapters
	reg := pipeline.Registry{}
	unknown := map[string]bool{}
	tags := []string{"A", "B", "C", "D", "E", "F"}
	resolve := func(name string, i int) {
		if _, done := reg[name]; done {
			return
		}
		if a, ok := real[name]; ok {
			reg[name] = a
		} else if name == FakeAdapter && corefake.Enabled() {
			reg[name] = fake.New(name, tags[i%len(tags)], fake.Valid)
		} else {
			unknown[name] = true
		}
	}
	for i, e := range plan.Explorers {
		resolve(e.Adapter, i)
	}
	resolve(plan.Collator.Adapter, len(plan.Explorers))
	// Explicit canonicalizers make model calls too, so an unconfigured one is reported here rather than at
	// pre-flight.
	for i, c := range plan.Canonicalizers {
		resolve(c.Adapter, len(plan.Explorers)+1+i)
	}

	names := make([]string, 0, len(unknown))
	for n := range unknown {
		names = append(names, n)
	}
	sort.Strings(names)
	return reg, names
}
