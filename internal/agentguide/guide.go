// Package agentguide holds the agent-facing guide to aimesh and serves it.
//
// The CLI's `aimesh agents-md` and every `agents_md` MCP tool serve the guide from this package. It
// imports nothing app-specific, so each surface can reach it without an import cycle. The guide is
// compiled in, so the document served always matches the binary serving it.
package agentguide

import (
	_ "embed"
	"fmt"
	"os"
)

//go:embed AGENTS.md
var embedded string

// EnvVar names a file to serve instead of the embedded guide. Set in an MCP server's env block, it
// substitutes an organization's own guidance for every agent using that server.
const EnvVar = "AGENTS_MD"

// Source says which document Load returned.
type Source string

const (
	// SourceEmbedded is the guide compiled into this binary.
	SourceEmbedded Source = "embedded"
	// SourceOverride is a document named by EnvVar; it need not match this binary.
	SourceOverride Source = "override"
)

// Load returns the guide to serve, its source, and the override path when one applied.
//
// An unreadable override is an error rather than a fallback to the embedded guide, so an agent never
// receives a document the operator did not choose. The error names EnvVar as the remedy.
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
