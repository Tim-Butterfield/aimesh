package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/Tim-Butterfield/aimesh/internal/agentguide"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

func runAgentsMD(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("agents-md", flag.ContinueOnError)
	fs.SetOutput(errw)
	cliflags.Style(fs, "aimesh agents-md")
	if err := fs.Parse(args); err != nil {
		return int(fault.Usage)
	}
	guide, source, path, err := agentguide.Load()
	if err != nil {
		fmt.Fprintln(errw, "aimesh agents-md:", err)
		return int(fault.Config)
	}
	// STDOUT carries the document VERBATIM, so `aimesh agents-md > FILE` and a model reading the
	// text both get exactly what was authored. Provenance goes to stderr — because when an override
	// is active the "matches the binary" guarantee no longer holds, and a reader who cannot tell
	// which document they received has been quietly misled. (The MCP tool carries the same fact in
	// structuredContent, where a caller can read it as data.)
	if source == agentguide.SourceOverride {
		fmt.Fprintf(errw, "aimesh agents-md: serving the %s override from %s (not the embedded guide)\n", agentguide.EnvVar, path)
	}
	fmt.Fprint(out, guide)
	if len(guide) > 0 && guide[len(guide)-1] != '\n' {
		fmt.Fprintln(out)
	}
	return int(fault.OK)
}
