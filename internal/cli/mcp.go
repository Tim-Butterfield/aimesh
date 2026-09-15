package cli

// `aimesh mcp` serves both domains' tools from one MCP server, so a host needs one config entry.
//
// Every flag is launch-time. The grants (--adapter, --allow-writes, --verify-cmd,
// --allow-protected-paths) and the --root ceiling cannot be tool parameters, because the caller is a
// model and must not extend its own access. What a caller may vary per run (workspace, roots, panel,
// waitSeconds, maxParallel) is a tool parameter.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	explorecli "github.com/Tim-Butterfield/aimesh/internal/explore/surface/cli"
	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
	"github.com/Tim-Butterfield/aimesh/internal/mcpflags"
	"github.com/Tim-Butterfield/aimesh/internal/mcpserve"
	reviewcli "github.com/Tim-Butterfield/aimesh/internal/review/surface/cli"
	reviewmcp "github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
	"github.com/Tim-Butterfield/aimesh/internal/review/version"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// domainFlags maps each domain-specific flag to its domain, so a flag set for a domain that is not
// served is refused rather than ignored: `--only explore --allow-writes` must not look like a grant.
// Flags both domains use, such as --adapter and --framing, are absent.
var domainFlags = map[string]mcpserve.Domain{
	"root":                  mcpserve.DomainReview,
	"allow-broad-root":      mcpserve.DomainReview,
	"allow-writes":          mcpserve.DomainReview,
	"verify-cmd":            mcpserve.DomainReview,
	"verify-timeout":        mcpserve.DomainReview,
	"verify-baseline":       mcpserve.DomainReview,
	"allow-protected-paths": mcpserve.DomainReview,
	"no-capture":            mcpserve.DomainExplore,
}

// runMCP runs the composed MCP server over stdio.
func runMCP(args []string, stdout, errw io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh mcp")
	only := fs.String("only", "", "serve ONE domain's tools instead of both: review | explore. Narrowing is worth having because a tool list costs a model attention on every call, and a review-only user should not carry explore's tools")
	// Shared flags and launch grants are registered once on this flag set and handed to each domain;
	// registering a name twice panics.
	sh := mcpflags.Register(fs, reviewmcp.DefaultWaitSeconds)
	adapters := launchflags.RegisterAdapters(fs)
	writes := launchflags.RegisterWrites(fs)
	buildReview := reviewcli.RegisterMCPFlags(fs, sh, adapters, writes)
	buildExplore := explorecli.RegisterMCPFlags(fs, sh, adapters)
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

	// When both domains are requested, one whose launch arguments are invalid is skipped with a
	// notice and the other serves. With --only, a build failure is fatal because the user named that
	// domain. Framing comes from the shared flag, so it applies whichever domains build.
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
			fmt.Fprintln(errw, "aimesh mcp: serving EXPLORE only — the review domain could not start (its refusal is above). Fix its launch arguments, or pass --only explore to silence this.")
		} else {
			// The composed server owns the transport; only the era posture comes from the domain.
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
			fmt.Fprintln(errw, "aimesh mcp: serving REVIEW only — the explore domain could not start (its refusal is above). Fix its launch arguments, or pass --only review to silence this.")
		} else {
			srv.Explore = s
			if srv.Review == nil {
				srv.Protocol = s.Protocol
			}
		}
	}
	// Neither domain built, so there is nothing to serve.
	if srv.Review == nil && srv.Explore == nil {
		fmt.Fprintln(errw, "aimesh mcp: neither domain could start, so there are no tools to serve. Fix the launch arguments named above.")
		return lastCode
	}
	// Advertise only the domains that built.
	if srv.Review == nil {
		srv.Only = mcpserve.DomainExplore
	} else if srv.Explore == nil {
		srv.Only = mcpserve.DomainReview
	}

	// The protocol stream is the writer passed in from main. Pointing os.Stdout at stderr keeps stray
	// prints from this process or its libraries out of the JSON-RPC stream.
	restore := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = restore }()
	if err := srv.Serve(os.Stdin, stdout); err != nil {
		fmt.Fprintln(errw, "aimesh mcp:", err)
		return int(fault.Internal)
	}
	return int(fault.OK)
}

// refuseForeignFlags rejects any set flag whose domain is not being served. fs.Visit walks only
// flags that were set, so defaults are never refused.
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
