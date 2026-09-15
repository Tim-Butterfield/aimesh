package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/Tim-Butterfield/aimesh/internal/agentguide"
	"github.com/Tim-Butterfield/aimesh/meshcore/cliflags"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// runAgentsMD prints the agent guide.
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
	// Stdout carries the document verbatim. An active override is disclosed on stderr, because the
	// guide then need not match this binary.
	if source == agentguide.SourceOverride {
		fmt.Fprintf(errw, "aimesh agents-md: serving the %s override from %s (not the embedded guide)\n", agentguide.EnvVar, path)
	}
	fmt.Fprint(out, guide)
	if len(guide) > 0 && guide[len(guide)-1] != '\n' {
		fmt.Fprintln(out)
	}
	return int(fault.OK)
}
