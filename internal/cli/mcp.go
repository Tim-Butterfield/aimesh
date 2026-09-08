package cli

// `aimesh mcp` — ONE MCP server carrying both domains' tools.
//
// It exists because an MCP server is configured once, in a host's config file, and then serves every
// workspace the operator opens. Two entries (`aimesh review mcp`, `aimesh explore mcp`) means two
// processes, two tool lists to keep straight and two places to state posture; one entry means the
// host launches `aimesh mcp` and gets the whole tool.
//
// Every flag here is LAUNCH-time, and that is not an oversight. The capability grants
// (--allow-remediate) and the trusted-root ceiling CANNOT be per-call:
// the caller on the other end of this wire is a model, and a model that could grant itself write
// access, name the file the server appends to, or widen its own filesystem reach would make the
// grant meaningless. Everything a caller may legitimately vary per run — the workspace, a narrowing
// `roots`, the profile or panel, waitSeconds, maxParallel — is a tool parameter instead, so one
// configured server serves many repos without the operator editing a config and restarting a host.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	explorecli "github.com/Tim-Butterfield/aimesh/internal/explore/surface/cli"
	"github.com/Tim-Butterfield/aimesh/internal/mcpflags"
	"github.com/Tim-Butterfield/aimesh/internal/mcpserve"
	reviewcli "github.com/Tim-Butterfield/aimesh/internal/review/surface/cli"
	reviewmcp "github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/internal/review/version"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// domainFlags names which domain owns each domain-specific flag, so a flag whose domain is not being
// served can be REFUSED rather than silently ignored.
//
// Silently ignoring is the failure that matters here: `--only explore --allow-remediate` reads to an
// operator as "explore, and remediation is on". Accepting it quietly would leave them believing they
// had granted a capability that the served domain does not even have.
var domainFlags = map[string]mcpserve.Domain{
	// NOTE: `framing` is deliberately ABSENT. It is transport posture for the whole process (see
	// internal/mcpflags), so refusing it under `--only explore` told a user their flag "would do
	// nothing" when framing is precisely what it would have done.
	"root":                  mcpserve.DomainReview,
	"no-default-root":       mcpserve.DomainReview,
	"allow-broad-root":      mcpserve.DomainReview,
	"allow-inferred-root":   mcpserve.DomainReview,
	"allow-remediate":       mcpserve.DomainReview,
	"verify-cmd":            mcpserve.DomainReview,
	"verify-timeout":        mcpserve.DomainReview,
	"verify-baseline":       mcpserve.DomainReview,
	"allow-protected-paths": mcpserve.DomainReview,
	"roster":                mcpserve.DomainExplore,
	"no-capture":            mcpserve.DomainExplore,
}

func runMCP(args []string, stdout, errw io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh mcp")
	only := fs.String("only", "", "serve ONE domain's tools instead of both: review | explore. Narrowing is worth having because a tool list costs a model attention on every call, and a review-only user should not carry explore's tools")
	// The shared flags are registered ONCE — which is the whole reason internal/mcpflags exists. Both
	// domains declare them, and registering the same name twice on one flag set panics.
	sh := mcpflags.Register(fs, reviewmcp.DefaultWaitSeconds)
	buildReview := reviewcli.RegisterMCPFlags(fs, sh)
	buildExplore := explorecli.RegisterMCPFlags(fs, sh)
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}

	domain, derr := mcpserve.ParseDomain(*only)
	if derr != nil {
		fmt.Fprintln(errw, "aimesh mcp:", derr)
		return int(fault.Usage)
	}
	if code := refuseForeignFlags(fs, domain, errw); code != int(fault.OK) {
		return code
	}

	// ONE UNCONFIGURED DOMAIN MUST NOT TAKE THE OTHER DOWN WITH IT.
	//
	// Each domain refuses at launch when it has no usable configuration — the deliberate
	// fresh-install posture, and it stays. What was wrong was the COMPOSITION: `aimesh mcp` builds
	// both, and a single `return code` from either one killed the process. On a fresh install that
	// is every install, and `{"command": "aimesh", "args": ["mcp"]}` is the config docs/mcp.md hands
	// people to paste — so the documented first run reported "server disconnected" with the reason
	// on a stderr channel MCP hosts are permitted to discard.
	//
	// So: when both were asked for, a domain that cannot be built is SKIPPED and said out loud, and
	// the other one serves. An explicit `--only <domain>` still fails hard, because there the user
	// named the thing that cannot start and has nothing else to fall back to.
	// Framing comes from the SHARED flag, so it holds whichever domain(s) actually build — an
	// explore-only server can be pinned to content-length exactly as a review-only one can.
	srv := &mcpserve.Server{Only: domain, Diagnostics: errw, Version: version.Get().Version, Framing: *sh.Framing}
	both := domain == mcpserve.DomainAll
	var lastCode int
	if domain != mcpserve.DomainExplore {
		s, code := buildReview(errw)
		if s == nil {
			if !both {
				return code
			}
			lastCode = code
			fmt.Fprintln(errw, "aimesh mcp: serving EXPLORE only — the review domain is not configured (its refusal is above). Configure it with `aimesh review setup`, or pass --only explore to silence this.")
		} else {
			// The composed server owns the transport; framing is already set from the shared flag
			// above, so only the era posture travels up here.
			srv.Review, srv.Protocol = s, s.Protocol
		}
	}
	if domain != mcpserve.DomainReview {
		s, code := buildExplore(errw)
		if s == nil {
			if !both {
				return code
			}
			lastCode = code
			fmt.Fprintln(errw, "aimesh mcp: serving REVIEW only — the explore domain is not configured (its refusal is above). Configure it with `aimesh explore setup`, or pass --only review to silence this.")
		} else {
			srv.Explore = s
			if srv.Review == nil {
				srv.Protocol = s.Protocol
			}
		}
	}
	// Both failed: there is no server to run, so this is the same hard failure as before.
	if srv.Review == nil && srv.Explore == nil {
		fmt.Fprintln(errw, "aimesh mcp: neither domain is configured, so there are no tools to serve. Run `aimesh review setup` and/or `aimesh explore setup` first.")
		return lastCode
	}
	// Narrow what is ADVERTISED to what actually built, so the tool list never promises a domain
	// this process cannot serve.
	if srv.Review == nil {
		srv.Only = mcpserve.DomainExplore
	} else if srv.Explore == nil {
		srv.Only = mcpserve.DomainReview
	}

	// STDOUT PURITY, for the same reason each domain's own `mcp` does it: the protocol stream is the
	// writer handed in from main, and repointing the os.Stdout package variable at stderr sends every
	// stray print — from this process, a library, or a spawned provider CLI that inherited the
	// descriptor — to stderr instead of into a JSON-RPC frame.
	restore := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = restore }()
	if err := srv.Serve(os.Stdin, stdout); err != nil {
		fmt.Fprintln(errw, "aimesh mcp:", err)
		return int(fault.Internal)
	}
	return int(fault.OK)
}

// refuseForeignFlags rejects a domain-specific flag the caller SET whose domain is not being served.
// fs.Visit walks only flags actually set, so a default is never mistaken for an instruction.
func refuseForeignFlags(fs *flag.FlagSet, domain mcpserve.Domain, errw io.Writer) int {
	var bad []string
	fs.Visit(func(f *flag.Flag) {
		owner, ok := domainFlags[f.Name]
		if !ok || owner == domain || domain == mcpserve.DomainAll {
			return
		}
		bad = append(bad, "--"+f.Name)
	})
	if len(bad) == 0 {
		return int(fault.OK)
	}
	fmt.Fprintf(errw, "aimesh mcp: --only %s does not serve %s, so %s would do nothing. Drop the flag, or drop --only to serve both.\n",
		domain, otherDomain(domain), strings.Join(bad, ", "))
	return int(fault.Usage)
}

func otherDomain(d mcpserve.Domain) mcpserve.Domain {
	if d == mcpserve.DomainReview {
		return mcpserve.DomainExplore
	}
	return mcpserve.DomainReview
}
