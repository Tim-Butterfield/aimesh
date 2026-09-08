// Package mcpflags declares the LAUNCH flags both MCP servers share, once.
//
// Why a package rather than two copies: these flags state POSTURE, not preference. `--protocol
// legacy` decides whether this process is a conformant 2026-07-28 server or a compatibility
// fallback; `--strict-schema` decides whether a non-conforming payload is shipped or refused;
// `--probe-deep` decides whether launching the server spends real tokens. An operator reads the
// help text to learn what they are agreeing to, and two hand-maintained descriptions of the same
// switch drift — they already had, differing in whether a run was called "a run" or "an
// exploration" — with nothing in the build to catch it.
//
// It is also what makes `aimesh mcp` possible. A composed server registers both domains' flags on
// ONE flag set, and a duplicate name panics; declaring the shared ones here is what leaves each
// name registered exactly once.
//
// Everything here is launch-only by nature. Anything a caller may vary per run — the workspace, the
// panel, waitSeconds, maxParallel — is a tool parameter instead, so a single configured server can
// serve many workspaces without the operator editing a host config and restarting.
package mcpflags

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// DefaultTurnTimeout is the wall-clock budget for ONE run when the operator names none.
const DefaultTurnTimeout = 10 * time.Minute

// Shared holds the parsed values. The fields are pointers because Register binds them into a
// flag.FlagSet, which writes through them at Parse time.
type Shared struct {
	Protocol     *string
	Framing      *string
	WaitSeconds  *int
	TurnTimeout  *time.Duration
	StrictSchema *bool
	ProbeDeep    *bool
}

// Register declares the shared flags on fs. defaultWait is the domain's own DefaultWaitSeconds, so
// this package does not become a third place that number is written down.
func Register(fs *flag.FlagSet, defaultWait int) *Shared {
	return &Shared{
		// THE ERA POSTURE. `dual` (the default) serves whichever MCP revision the client opens with
		// and then commits to it for the life of the process. `legacy` makes this process a
		// pre-2026-07-28 server in every observable respect.
		//
		// `legacy` exists so an operator whose host misbehaves has a documented rollback that is not
		// "downgrade the binary". There is deliberately no `modern`: a modern-only pin would strand
		// legacy hosts, which have no fall-forward mechanism, and buys nothing an operator cannot get
		// by simply not sending `initialize`.
		//
		// SUNSET-PATH (MCP26-SUNSET; migration design §16.2): the whole flag goes with the era.
		Protocol: fs.String("protocol", string(proto.ProtocolDual),
			"MCP era posture: dual (serve whichever revision the client opens with — the sessionless 2026-07-28 revision or a legacy `initialize` handshake — and commit to it) | legacy (be a pre-2026-07-28 server in every observable respect: `server/discover` is an unknown method and no modern request is served). A legacy-mode process is a documented compatibility fallback, NOT a conformant 2026-07-28 deployment; the `doctor` tool reports which mode is in force"),
		// WIRE FRAMING IS A PROPERTY OF THE PROCESS, not of a domain — it decides how bytes are
		// delimited on the one stdio stream both domains share, so it belongs beside --protocol.
		//
		// It used to be declared by the REVIEW domain, which had two consequences a user could hit.
		// `aimesh mcp --only explore --framing content-length` was REFUSED as a foreign flag with
		// "--framing would do nothing" — untrue, since framing is exactly what would have done
		// something. And `aimesh explore mcp` had no --framing at all, so an explore-only deployment
		// could not choose content-length even though its server has always honoured the setting.
		Framing: fs.String("framing", "",
			"wire framing: newline (the MCP stdio default) | content-length. It applies to the whole process, so it is accepted whichever domain(s) are being served"),
		WaitSeconds: fs.Int("wait-seconds", defaultWait,
			"default inline wait before a run is handed back as {runId, state:\"running\"} (a caller may override it per call, up to the maximum)"),
		TurnTimeout: fs.Duration("turn-timeout", DefaultTurnTimeout,
			"total wall-clock budget for one run"),
		StrictSchema: fs.Bool("strict-schema", false,
			"validate every result's structuredContent against its tool's declared outputSchema BEFORE sending it, and fail the call loudly on a violation instead of shipping a non-conforming payload. Off by default because it is not free (the payload is re-marshalled and walked on every call); the same check runs in the build for every result shape the tests construct. Also settable with "+proto.StrictSchemaEnv+"=1"),
		// The DEEP probe is a LAUNCH-TIME flag, not a tool parameter, and that is the whole design.
		// It spends real tokens, and the `doctor` tool carries readOnlyHint with a description
		// promising that no process is started and nothing is spent. A peer that could trigger a
		// spend by calling a read-only tool would make that annotation a lie.
		ProbeDeep: fs.Bool("probe-deep", false,
			"at launch (once), run the DEEP readiness probe of the default panel's adapters — one REAL, bounded model invocation each, in a throwaway isolated directory — and include the result in this server's readiness projection (the `doctor` tool). SPENDS REAL TOKENS at startup. Without it, readiness stays static. No interactive trust/login prompt is ever answered on your behalf"),
	}
}

// Era validates --protocol and returns the posture, announcing a legacy pin on errw.
//
// An unrecognized value is a CONFIG ERROR refused before serving, never a fallback to the default:
// an operator who typed `--protocol modern` asked for a pin and would otherwise silently get the
// opposite. prefix names the command in the message ("aimesh mcp", "reviewmesh mcp", …).
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

// Strict combines the flag with the environment form.
//
// The ENV form exists because an MCP server is usually launched by a HOST's config file, and many
// hosts let an operator set `env` for a server but not extra argv (Claude Desktop's
// `claude_desktop_config.json` is the common case). A diagnostic-only switch that the people who
// most need it cannot reach would be a switch in name only.
func (s *Shared) Strict() bool { return *s.StrictSchema || proto.StrictSchemaFromEnv() }

// AnnounceStrict prints the strict-schema disclosure when it is on.
func (s *Shared) AnnounceStrict(errw io.Writer, prefix string) {
	if s.Strict() {
		fmt.Fprintf(errw, "%s: STRICT SCHEMA VALIDATION is on — every result is validated against its tool's declared outputSchema before it is sent, and a violation fails the call. This costs one schema walk per call.\n", prefix)
	}
}
