// Package mcpflags declares the launch flags both MCP servers share.
//
// Declaring them once gives each switch a single description and lets `aimesh mcp` register both
// domains' flags on one flag set, where a duplicate name would panic. Anything a caller may vary per
// run is a tool parameter instead.
package mcpflags

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// DefaultTurnTimeout is the wall-clock budget for one run when the operator names none.
const DefaultTurnTimeout = 10 * time.Minute

// Shared holds the parsed values. The fields are pointers because Register binds them into a
// flag.FlagSet, which writes through them at Parse time.
type Shared struct {
	Protocol     *string
	Framing      *string
	WaitSeconds  *int
	TurnTimeout  *time.Duration
	StrictSchema *bool
}

// Register declares the shared flags on fs. defaultWait is the domain's own DefaultWaitSeconds, so
// this package does not become a third place that number is written down.
func Register(fs *flag.FlagSet, defaultWait int) *Shared {
	return &Shared{
		// Protocol is the era posture. dual serves whichever MCP revision the client opens with;
		// legacy is a pre-2026-07-28 server, a rollback for hosts that misbehave. There is no
		// modern-only pin, because it would strand legacy hosts.
		//
		// SUNSET-PATH (MCP26-SUNSET): the whole flag goes with the era.
		Protocol: fs.String("protocol", string(proto.ProtocolDual),
			"MCP era posture: dual (serve whichever revision the client opens with — the sessionless 2026-07-28 revision or a legacy `initialize` handshake — and commit to it) | legacy (be a pre-2026-07-28 server in every observable respect: `server/discover` is an unknown method and no modern request is served). A legacy-mode process is a documented compatibility fallback, NOT a conformant 2026-07-28 deployment; the `doctor` tool reports which mode is in force"),
		// Framing applies to the whole process, because both domains share one stdio stream.
		Framing: fs.String("framing", "",
			"wire framing: newline (the MCP stdio default) | content-length. It applies to the whole process, so it is accepted whichever domain(s) are being served"),
		WaitSeconds: fs.Int("wait-seconds", defaultWait,
			"default inline wait before a run is handed back as {runId, state:\"running\"} (a caller may override it per call, up to the maximum)"),
		TurnTimeout: fs.Duration("turn-timeout", DefaultTurnTimeout,
			"total wall-clock budget for one run"),
		StrictSchema: fs.Bool("strict-schema", false,
			"validate every result's structuredContent against its tool's declared outputSchema BEFORE sending it, and fail the call loudly on a violation instead of shipping a non-conforming payload. Off by default because it is not free (the payload is re-marshalled and walked on every call); the same check runs in the build for every result shape the tests construct. Also settable with "+proto.StrictSchemaEnv+"=1"),
	}
}

// Era validates --protocol and returns the posture, announcing a legacy pin on errw. An unrecognized
// value is refused rather than defaulted. prefix names the command in messages.
func (s *Shared) Era(errw io.Writer, prefix string) (proto.ProtocolMode, bool) {
	if !proto.ValidProtocolMode(*s.Protocol) {
		fmt.Fprintf(errw, "%s: unknown --protocol %q (want dual or legacy)\n", prefix, *s.Protocol)
		return "", false
	}
	era := proto.ProtocolMode(strings.ToLower(strings.TrimSpace(*s.Protocol)))
	if era == proto.ProtocolLegacy {
		fmt.Fprintf(errw, "%s: --protocol legacy: this process is a pre-2026-07-28 MCP server. `server/discover` answers method-not-found and no modern request is served, so a dual-era host falls back to the `initialize` handshake. This is a compatibility fallback, not a conformant 2026-07-28 deployment; the `doctor` tool reports it.\n", prefix)
	}
	return era, true
}

// Strict combines the flag with its environment form, which reaches hosts that can set a server's
// environment but not its arguments.
func (s *Shared) Strict() bool { return *s.StrictSchema || proto.StrictSchemaFromEnv() }

// AnnounceStrict prints the strict-schema disclosure when it is on.
func (s *Shared) AnnounceStrict(errw io.Writer, prefix string) {
	if s.Strict() {
		fmt.Fprintf(errw, "%s: STRICT SCHEMA VALIDATION is on — every result is validated against its tool's declared outputSchema before it is sent, and a violation fails the call. This costs one schema walk per call.\n", prefix)
	}
}
