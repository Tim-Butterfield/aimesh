// Package launchflags declares the launch flags that give an MCP or ACP server its configuration.
//
// An MCP or ACP server takes its configuration from the arguments and environment its host starts it
// with, and from nothing else. These flags are the operator's grants: which adapters may receive
// content (`--adapter`, or `AIMESH_ADAPTERS` for hosts that can set environment but not arguments),
// and whether aimesh may apply changes to a workspace (`--allow-writes`). A caller on the other end
// of the protocol can use what was granted; it can never extend it.
//
// Each command that owns a flag set registers these once and hands the result to the domain
// builders, so a command serving both domains never registers a name twice.
package launchflags

import (
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/fake"
	"github.com/Tim-Butterfield/aimesh/meshcore/model/shell"
	"github.com/Tim-Butterfield/aimesh/meshcore/pathexpand"
)

// EnvVar is the environment form of `--adapter`: entries separated by the OS path-list separator.
const EnvVar = "AIMESH_ADAPTERS"

// FakeAdapter is the internal test adapter's name. It is nameable only while the internal test gate
// (fake.Enabled) is on; it is never offered to an operator.
const FakeAdapter = "fake"

// Source records where an adapter was named.
type Source string

const (
	// SourceFlag means the adapter was named with --adapter.
	SourceFlag Source = "flag"
	// SourceEnv means the adapter was named in the AIMESH_ADAPTERS environment variable.
	SourceEnv Source = "env"
)

// Reason codes for refused launch input.
const (
	ReasonUnknownAdapter   = "adapter_unknown"
	ReasonDuplicateAdapter = "adapter_duplicate"
	ReasonFlagEnvConflict  = "adapter_flag_env_conflict"
	ReasonEmptyEntry       = "adapter_empty_entry"
	ReasonEmptyPath        = "adapter_empty_path"
)

// Adapters holds the raw `--adapter` values until Resolve.
type Adapters struct {
	values []string
}

// RegisterAdapters declares the repeatable `--adapter` flag on fs.
func RegisterAdapters(fs *flag.FlagSet) *Adapters {
	a := &Adapters{}
	fs.Var((*adapterValues)(&a.values), "adapter",
		"an adapter this server may use: `name` (its CLI is found on PATH) or name=path (the CLI's full path; environment variables and a leading ~ are expanded — %VAR% on Windows, $VAR or ${VAR} elsewhere). Repeatable. Only named adapters are available: they are who may receive the content a call sends. Also settable as "+EnvVar+", with entries separated by the path-list separator (; on Windows, : elsewhere); give one or the other, not both")
	return a
}

type adapterValues []string

func (v *adapterValues) String() string     { return strings.Join(*v, ",") }
func (v *adapterValues) Set(s string) error { *v = append(*v, s); return nil }

// Adapter is one named adapter.
type Adapter struct {
	// Name is the built-in adapter name.
	Name string
	// Path is the expanded binary path, or "" when the CLI is found on PATH.
	Path string
	// Source is where the adapter was named.
	Source Source
}

// Set is the resolved adapter grant: exactly the adapters callers may use.
type Set struct {
	adapters []Adapter
}

// BuiltinNames lists the adapter names `--adapter` accepts: every built-in shell recipe, plus the
// internal test adapter while its gate is on.
func BuiltinNames() []string {
	names := make([]string, 0, len(shell.Recipes())+1)
	for n := range shell.Recipes() {
		names = append(names, n)
	}
	if fake.Enabled() {
		names = append(names, FakeAdapter)
	}
	slices.Sort(names)
	return names
}

// Resolve validates the flag values and the environment form against env and returns the grant.
func (a *Adapters) Resolve(env pathexpand.Env) (Set, error) {
	envRaw := ""
	if env.Lookup != nil {
		if v, ok := env.Lookup(EnvVar); ok {
			envRaw = strings.TrimSpace(v)
		}
	}
	if len(a.values) > 0 && envRaw != "" {
		return Set{}, refuse(ReasonFlagEnvConflict, fmt.Sprintf("both --adapter and %s are set; name adapters with one of them", EnvVar))
	}

	type entry struct {
		raw    string
		source Source
	}
	var entries []entry
	for _, v := range a.values {
		entries = append(entries, entry{raw: v, source: SourceFlag})
	}
	if envRaw != "" {
		sep := ":"
		if env.GOOS == "windows" {
			sep = ";"
		}
		for part := range strings.SplitSeq(envRaw, sep) {
			entries = append(entries, entry{raw: part, source: SourceEnv})
		}
	}

	known := BuiltinNames()
	var out Set
	seen := map[string]bool{}
	for _, e := range entries {
		label := "--adapter"
		if e.source == SourceEnv {
			label = EnvVar
		}
		raw := strings.TrimSpace(e.raw)
		if raw == "" {
			if e.source == SourceEnv {
				return Set{}, refuse(ReasonEmptyEntry, fmt.Sprintf("%s contains an empty entry; remove the extra separator. A path that itself contains the separator cannot be written in %s — use --adapter flags instead", EnvVar, EnvVar))
			}
			return Set{}, refuse(ReasonEmptyEntry, "--adapter needs a value: name or name=path")
		}
		name, path, hasPath := strings.Cut(raw, "=")
		name = strings.TrimSpace(name)
		if !slices.Contains(known, name) {
			return Set{}, refuse(ReasonUnknownAdapter, fmt.Sprintf("%s names %q, which is not a built-in adapter; known adapters: %s", label, name, strings.Join(known, ", ")))
		}
		if seen[name] {
			return Set{}, refuse(ReasonDuplicateAdapter, fmt.Sprintf("%s names %q more than once", label, name))
		}
		seen[name] = true
		ad := Adapter{Name: name, Source: e.source}
		if hasPath {
			if strings.TrimSpace(path) == "" {
				return Set{}, refuse(ReasonEmptyPath, fmt.Sprintf("%s %s= has an empty path; give the CLI's full path or drop the = to find it on PATH", label, name))
			}
			expanded, err := pathexpand.Expand(path, env)
			if err != nil {
				return Set{}, fault.New(fault.Config, fmt.Sprintf("%s %s: %v", label, name, err)).WithReason(fault.ReasonOf(err))
			}
			ad.Path = expanded
		}
		out.adapters = append(out.adapters, ad)
	}
	slices.SortFunc(out.adapters, byName)
	return out, nil
}

// NewSet builds a grant directly, for tests and for callers that already hold resolved adapters.
func NewSet(adapters ...Adapter) Set {
	s := Set{adapters: slices.Clone(adapters)}
	slices.SortFunc(s.adapters, byName)
	return s
}

func byName(a, b Adapter) int { return strings.Compare(a.Name, b.Name) }

// Empty reports whether no adapter was named.
func (s Set) Empty() bool { return len(s.adapters) == 0 }

// Adapters returns the named adapters, sorted by name.
func (s Set) Adapters() []Adapter { return slices.Clone(s.adapters) }

// Names returns the named adapters' names, sorted.
func (s Set) Names() []string {
	out := make([]string, 0, len(s.adapters))
	for _, a := range s.adapters {
		out = append(out, a.Name)
	}
	return out
}

// Has reports whether name was granted.
func (s Set) Has(name string) bool {
	_, ok := s.lookup(name)
	return ok
}

// Get returns the named adapter.
func (s Set) Get(name string) (Adapter, bool) { return s.lookup(name) }

// Paths maps each named adapter with an explicit path to that path, in the shape shell.Registry takes.
func (s Set) Paths() map[string]string {
	out := map[string]string{}
	for _, a := range s.adapters {
		if a.Path != "" {
			out[a.Name] = a.Path
		}
	}
	return out
}

// Available checks, at the moment it is called, whether a named adapter's CLI can be started: its
// explicit path exists and is executable, or its binary is on PATH. It starts no process. Checking on
// every call is what lets a CLI installed after launch become usable without a restart.
func (s Set) Available(name string) (bool, string) {
	a, ok := s.lookup(name)
	if !ok {
		return false, fmt.Sprintf("adapter %q is not named by --adapter", name)
	}
	if a.Name == FakeAdapter {
		return true, "internal test adapter"
	}
	r, ok := shell.Recipes()[a.Name]
	if !ok {
		return false, fmt.Sprintf("adapter %q has no built-in recipe", a.Name)
	}
	return shell.New(r, a.Path, 0).Available()
}

func (s Set) lookup(name string) (Adapter, bool) {
	for _, a := range s.adapters {
		if a.Name == name {
			return a, true
		}
	}
	return Adapter{}, false
}

// Writes holds the `--allow-writes` grant.
type Writes struct {
	allow *bool
}

// RegisterWrites declares `--allow-writes` on fs.
func RegisterWrites(fs *flag.FlagSet) *Writes {
	return &Writes{allow: fs.Bool("allow-writes", false,
		"let aimesh apply accepted review findings to a workspace. Without it aimesh never changes project content; a caller can still ask for the diff and apply it itself. With it, every apply is still confirmed per call and applies only a completed review's accepted findings to the tree that review read")}
}

// Allowed reports whether the operator granted writes.
func (w *Writes) Allowed() bool { return w != nil && w.allow != nil && *w.allow }

func refuse(reason, msg string) error {
	return fault.New(fault.Config, msg).WithReason(reason)
}
