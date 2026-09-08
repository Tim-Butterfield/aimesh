// Package registry builds exploremesh's adapter registry for a roster plan. Shell adapter support is
// DISCOVERED from meshcore's fixed shell recipes (claude-code, codex-cli, ollama, agy-cli, devin-cli,
// gemini-cli, cursor-cli); ACP adapters are USER-DEFINED instances (from the shared adapters.yaml
// `acpAdapters`) passed in, since ACP is an open protocol with no fixed CLI list. A roster adapter
// whose name matches a shell recipe or an ACP instance is backed by the REAL adapter. The ONE
// explicit `fake` key is the HIDDEN internal test harness: it resolves only when the internal gate
// (meshcore fake.Enabled, set by tests/golden runs — never by users) is on. Any OTHER name — and
// `fake` itself when the gate is off — is FAIL-CLOSED: it is not registered and is returned as
// `unknown` for the caller to surface (see Build) — a typo in a roster must never silently become a
// fake "success".
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

// ResolveACPInstances loads the user-defined ACP adapter instances effective for cwd (the shared
// adapters.yaml `acpAdapters`, user + root-anchored project scope) and converts them to acpagent
// instances for Build. Detect falls back to the binary basename for PATH lookup.
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

// FakeAdapter is the ONE explicit deterministic test-harness adapter key. It is hidden and internal:
// it resolves ONLY when the internal gate (corefake.Enabled — set by tests and the golden runs, never
// by users) is on; otherwise it is an unrecognized name like any other (see Build).
const FakeAdapter = "fake"

// Build resolves every adapter named in the plan (explorers + collator + explicit canonicalizers) to a runnable adapter: the
// real meshcore adapter where the name matches a shell recipe or a user-defined ACP instance, and the
// deterministic fake ONLY for the explicit `fake` key AND only when the internal test-harness gate is
// on. paths supplies optional binary-path overrides for shell recipes; acpInsts is the set of ACP
// adapter instances (nil → none).
//
// FAIL-CLOSED: any other unrecognized adapter name — including `fake` when the internal gate is off —
// is NOT registered and is returned in `unknown` (sorted). It is a configuration error — a typo in a
// roster must never silently become a fake "success". The caller surfaces `unknown` (doctor: a
// failing check; a run: refuse before spending).
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
	// The EXPLICIT canonicalizer identities are governed seats that make real model calls, so their adapters
	// must resolve here too — otherwise an unconfigured canonicalizer adapter is only discovered at the
	// pre-flight, after the caller was told the panel was fine.
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
