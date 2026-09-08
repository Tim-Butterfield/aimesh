// Package agentguide holds the one agent-facing guide to aimesh and the rules for serving it.
//
// It lives in its own package, importing nothing app-specific, because MORE THAN ONE SURFACE MUST
// SERVE THE SAME BYTES: the CLI's `aimesh agents-md` and each domain's `agents_md` MCP tool. The CLI
// entry point (internal/cli) imports both domain CLIs, so a domain's mcp package could not reach the
// guide through it without an import cycle. A neutral package is what makes "an agent arriving by
// shell and one arriving over MCP get identical guidance" true by construction rather than by two
// copies that agree today.
//
// The guide is COMPILED IN rather than read from disk or written into the user's project. A file on
// disk drifts: the user edits it, or upgrades the binary and the file goes on describing the previous
// version. Embedding makes "the guide matches the tool that served it" a property of the build.
package agentguide

import (
	_ "embed"
	"fmt"
	"os"
)

//go:embed AGENTS.md
var embedded string

// EnvVar names a file to serve INSTEAD of the embedded guide. It is a deployment knob as much as a
// CLI one: set in an MCP client's server env block, it substitutes an organization's own guidance for
// every agent reaching that server.
const EnvVar = "AGENTS_MD"

// Source says which document Load returned.
type Source string

const (
	// SourceEmbedded is the guide compiled into this binary — it matches the version serving it.
	SourceEmbedded Source = "embedded"
	// SourceOverride is a document named by EnvVar. The version-match guarantee does NOT hold for it.
	SourceOverride Source = "override"
)

// Load returns the guide to serve, which document it is, and the override path when one applied.
//
// An unreadable override is an ERROR, never a quiet fall back to the embedded text: falling back
// would hand the agent a document the operator did not choose while reporting success — the
// silent-substitution failure this tool refuses everywhere else. The message names the remedy,
// because an operator who set the variable in an MCP server config months ago will not otherwise
// connect the error to it.
func Load() (guide string, source Source, path string, err error) {
	p := os.Getenv(EnvVar)
	if p == "" {
		return embedded, SourceEmbedded, "", nil
	}
	b, rerr := os.ReadFile(p)
	if rerr != nil {
		return "", "", p, fmt.Errorf(
			"%s names %s but it could not be read: %w. Unset %s to use the guide embedded in this binary",
			EnvVar, p, rerr, EnvVar)
	}
	return string(b), SourceOverride, p, nil
}
