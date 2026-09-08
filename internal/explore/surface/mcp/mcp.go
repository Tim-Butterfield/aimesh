// Package mcp is exploremesh's MCP (Model Context Protocol) surface: exploremesh runs as a local stdio
// server so an MCP-speaking agent can drive a governed, blind, multi-model exploration and get back the
// HOST-COMPUTED result. The protocol itself is meshcore/mcp; this package owns the tools, the schemas,
// the job registry and the admission governor — everything that is exploremesh grammar.
//
// Four decisions shape this surface, all of them from the MCP design review:
//
//  1. JOB-SHAPED CALLS. Client request timeouts are commonly ~60 s and a real panel run is minutes. A
//     synchronous call would be killed MID-SPEND with no way to reach the subprocesses it started. So a
//     run-starting tool waits `waitSeconds` (default 25) for the result inline and otherwise returns
//     `{runId, state:"running"}`; `explore_run_status`/`explore_run_result` finish the conversation.
//  2. DOMAIN HALTS RIDE `isError`. Identity mismatch, adapter failure, an admission refusal — all of them
//     come back as a successful JSON-RPC response carrying `CallToolResult{isError: true}` with the
//     `{exitCode, haltClass, reasonCode, failure}` taxonomy. JSON-RPC `error.data` is routinely flattened
//     or dropped by clients, which would lose the taxonomy exactly when the model needs it to react.
//     Protocol errors stay reserved for malformed requests, unknown tools and pre-initialization calls.
//  3. GOVERNANCE IS NOT OPTIONAL. Every terminal result carries the governance block, the identity
//     caveats and the requested-vs-executed panel echo, and the declared outputSchema marks all three
//     `required`. There is no `summary` vs `full` detail parameter to drop them through.
//  4. COMPOSE, NEVER CONFIGURE. A call selects a configured profile or composes a panel from the adapter
//     set bound at STARTUP. It can never introduce an adapter, a binary path or a launch argument, and
//     `explore_list`/`explore_doctor` report logical identifiers only — no paths, no launch args, no environment.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/audit"
	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"

	"github.com/Tim-Butterfield/aimesh/internal/agentguide"
	"github.com/Tim-Butterfield/aimesh/internal/explore/capture"
	"github.com/Tim-Butterfield/aimesh/internal/explore/mode"
	"github.com/Tim-Butterfield/aimesh/internal/explore/pipeline"
	"github.com/Tim-Butterfield/aimesh/internal/explore/profile"
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/runview"
)

// ServerName / ServerVersion identify this server in the MCP handshake.
const (
	ServerName    = "exploremesh"
	ServerVersion = "dev"
)

// maxArgumentBytes bounds one tool call's arguments before anything is decoded — the request-shape half
// of the admission governor (an oversized artifact must be refused before it reaches a prompt builder).
const maxArgumentBytes = 1 << 20 // 1 MiB

// defaultTurnTimeout is the wall-clock budget for one run when none is configured.
const defaultTurnTimeout = 10 * time.Minute

// Explorer is the capability this surface routes an exploration to — the same seam the ACP surface uses,
// so both surfaces run the identical pipeline and a test can substitute a fake.
type Explorer interface {
	Run(ctx context.Context, plan roster.Plan, raw schema.RawTask, opts pipeline.Options, onEvent func(audit.EventLine)) (pipeline.Result, error)
}

// Server is the exploremesh MCP server.
type Server struct {
	// Explorer routes a run to the pipeline; Plan is the DEFAULT panel (what a call that names no panel
	// runs); Profiles is the set a call may select from by name.
	Explorer Explorer
	Plan     roster.Plan
	Profiles profile.Set
	// Adapters is the STARTUP-BOUND adapter set an ad-hoc panel may compose from. Empty means ad-hoc
	// composition is refused — fail-closed, because "no configured set" must not read as "anything goes".
	Adapters []string
	// Config supplies the sanitized projections behind `explore_list` and `explore_doctor`.
	Config Config

	// WaitSeconds overrides the inline wait budget's default. This server has NO admission bounds —
	// neither a lifetime run cap nor an in-flight one; see the note in runs.go for why, and for where
	// the concurrency decision lives now (`maxParallel`, stated per call).
	WaitSeconds int
	// TurnTimeout bounds ONE run's wall clock (0 → 10 minutes).
	TurnTimeout time.Duration
	// DisableCapture turns off the on-disk run record. Capture is ON by default: an MCP run that left no
	// disk record would be the one surface whose governance claims are not checkable after the fact.
	DisableCapture bool
	// StrictSchema turns on the OPT-IN send-time check that every `structuredContent` satisfies the
	// outputSchema its own tool declared. Off by default; see proto.Server.StrictSchema for the cost.
	StrictSchema bool

	Framing string
	// Protocol is the era posture this process was launched with (`--protocol dual|legacy`; ""
	// means dual). It is reported by `explore_doctor` and not only announced on stderr, because a host
	// launches its servers from a config file and MAY discard stderr entirely.
	//
	// SUNSET-PATH (MCP26-SUNSET; migration design §16.2): removed with the legacy era.
	Protocol proto.ProtocolMode
	// Diagnostics is the server's own log sink. It must never be the protocol stream.
	Diagnostics io.Writer

	runs *registry
	core *proto.Server

	// attached is set when this server registers into an EXTERNAL core (the combined `aimesh mcp`).
	// It suppresses the SHARED tools the composer registers once for the whole server — registering
	// agents_md twice would panic, and two copies of one document is the thing internal/agentguide
	// exists to prevent.
	attached bool

	// artifacts publishes a captured run's files as MCP resources. Without it the run record is
	// reachable only from the machine that owns it, and this surface deliberately reports no host
	// path — so a client that wanted the raw explorer envelopes behind a synthesis had no route to
	// them at all.
	artifacts proto.RunStore
}

// Serve builds the tool set and serves MCP over the given streams until EOF. On return every in-flight
// run is cancelled: a disconnected client must not leave a panel of model CLIs running with nobody to
// receive their output.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	s.build()
	defer s.runs.cancelAll()
	return s.core.Serve(in, out)
}

// Core exposes the underlying protocol server (for tests that drive it over an explicit framer).
func (s *Server) Core() *proto.Server {
	s.build()
	return s.core
}

func (s *Server) build() { s.buildInto(nil) }

// Attach registers this domain's tools into an EXTERNAL protocol core — the seam the combined
// `aimesh mcp` server uses — and returns the two providers the composer must fold in, because a
// single proto.Server has one Resources field and one Tasks field and both domains need theirs.
func (s *Server) Attach(core *proto.Server) (proto.ResourceProvider, proto.TaskProvider) {
	s.buildInto(core)
	return &s.artifacts, taskProvider{s}
}

// buildInto wires this server. With core == nil it owns a fresh one (the standalone
// `aimesh explore mcp` case); with a core supplied it registers into that instead.
func (s *Server) buildInto(core *proto.Server) {
	if s.core != nil {
		return
	}
	s.runs = newRegistry()
	s.runs.onEvict = s.artifacts.Forget
	if core != nil {
		s.attached = true
		// Explore sets no core-level semantics of its own — no roots, no per-request confinement —
		// so attaching is purely tool registration plus the two providers returned above.
		s.core = core
	} else {
		s.core = &proto.Server{
			Info:         proto.Implementation{Name: ServerName, Title: "exploremesh", Version: ServerVersion},
			Instructions: instructions,
			Framing:      s.Framing,
			Protocol:     s.Protocol,
			Diagnostics:  s.Diagnostics,
			StrictSchema: s.StrictSchema,
			Resources:    &s.artifacts,
			// The `io.modelcontextprotocol/tasks` extension, as a PROJECTION over the run registry —
			// `taskId == runId` (see tasks.go). Opt-in per client and modern-era only, so a client that
			// declares nothing sees exactly the job shape it saw before.
			Tasks: taskProvider{s},
		}
	}
	s.registerTools()
}

func (s *Server) waitDefault() int {
	if s.WaitSeconds > 0 {
		return s.WaitSeconds
	}
	return DefaultWaitSeconds
}

func (s *Server) modeNames() []string { return mode.Names() }

// spendAnnotations mark a tool that STARTS A RUN: it is not read-only, it reaches an open world of
// external providers, and it is not idempotent. These tools MUST NOT be annotated read-only — that is
// the single most dangerous kind of mistake an annotation can make, because a host uses it to decide
// whether to ask the human first.
func spendAnnotations(title string) *proto.ToolAnnotations {
	return &proto.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: proto.Bool(false), // it spends money; it destroys nothing
		IdempotentHint:  false,
		OpenWorldHint:   proto.Bool(true),
	}
}

// readOnlyAnnotations mark a tool that only reports. Only `explore_list`, `explore_doctor`, `explore_run_status` and `explore_run_result`
// qualify — everything else spawns processes and spends.
func readOnlyAnnotations(title string) *proto.ToolAnnotations {
	return &proto.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: proto.Bool(false),
		IdempotentHint:  true,
		OpenWorldHint:   proto.Bool(false),
	}
}

func (s *Server) registerTools() {
	s.core.Register(proto.Tool{
		Name:         "explore",
		Title:        "Run a blind multi-model exploration",
		Description:  exploreToolDescription,
		InputSchema:  raw(exploreInputSchema),
		OutputSchema: raw(runResultSchema),
		Annotations:  spendAnnotations("Explore"),
	}, s.exploreHandler("explore", applyMode))

	s.core.Register(proto.Tool{
		Name:         "explore_list",
		Title:        "Report the configured adapters, profiles and modes",
		Description:  "Reports what this server can run: adapter IDENTIFIERS and their readiness, the configured profiles (ordered explorers + collator + canonicalizers), the available modes, and the admission limits in force. Read-only. Reports no binary paths, launch arguments or environment detail, and cannot change any configuration. Call this before composing a panel.",
		InputSchema:  raw(emptyInputSchema),
		OutputSchema: raw(listOutputSchema),
		Annotations:  readOnlyAnnotations("List configuration"),
	}, func(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
		payload := s.listPayload()
		return proto.Result(renderList(payload), payload), nil
	})

	s.core.Register(proto.Tool{
		Name:         "explore_doctor",
		Title:        "Report adapter readiness",
		Description:  "Static readiness of the explorers and collator this server would run: no process is started, nothing is spent, and no path or environment detail is reported. An unrecognized adapter is a failing check — it is never silently substituted.",
		InputSchema:  raw(emptyInputSchema),
		OutputSchema: raw(doctorOutputSchema),
		Annotations:  readOnlyAnnotations("Check readiness"),
	}, func(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
		payload := s.doctorPayload(c.Env())
		return proto.Result(renderDoctor(payload), payload), nil
	})

	// The agent guide, byte-identical to `aimesh agents-md` and to the review server's tool. Defined
	// in internal/agentguide so the two servers cannot drift into serving different documents.
	// Skipped when attached: the composed server registers it once for both domains.
	if !s.attached {
		s.core.Register(proto.Tool{
			Name:         agentguide.ToolName,
			Title:        agentguide.ToolTitle,
			Description:  agentguide.ToolDescription,
			InputSchema:  raw(agentguide.InputSchema),
			OutputSchema: raw(agentguide.OutputSchema),
			Annotations:  readOnlyAnnotations(agentguide.ToolAnnotationTitle),
		}, func(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
			text, payload, err := agentguide.ToolPayload()
			if err != nil {
				// An unreadable AGENTS_MD override is an ERROR, never a quiet fall back to the
				// embedded guide: falling back would hand the agent a document the operator did not
				// choose while reporting success.
				return proto.ErrorResult(err.Error(), nil), nil
			}
			return proto.Result(text, payload), nil
		})
	}

	s.core.Register(proto.Tool{
		Name:         "explore_run_status",
		Title:        "Poll a run",
		Description:  "Reports whether a run started by one of the explore tools is still running, and its panel. Read-only; it never starts or re-starts anything. When the state is no longer `running`, fetch the result with explore_run_result.",
		InputSchema:  raw(runIDInputSchema),
		OutputSchema: raw(runStatusSchema),
		Annotations:  readOnlyAnnotations("Run status"),
	}, s.runStatusHandler)

	s.core.Register(proto.Tool{
		Name:         "explore_run_result",
		Title:        "Fetch a run's result",
		Description:  "Returns the full result of a run started by one of the explore tools — the same payload the starting call would have returned had it finished in time, governance block and identity caveats included. Read-only and free: re-reading a run costs nothing and never re-runs it.",
		InputSchema:  raw(runIDInputSchema),
		OutputSchema: raw(runResultSchema),
		Annotations:  readOnlyAnnotations("Run result"),
	}, s.runResultHandler)
}

// --- arguments ---

// commonArgs are the parameters every run-starting tool takes. They are embedded rather than repeated so
// the strict decode below (which rejects unknown fields) sees exactly the union each tool declares.
type commonArgs struct {
	Purpose      string    `json:"purpose"`
	Criteria     []string  `json:"criteria"`
	PriorContext string    `json:"priorContext"`
	Panel        *panelArg `json:"panel"`
	// Canonicalizers names the two identities that propose the canonicalization (design §4) — the MCP
	// analogue of the CLI's repeatable `--canonicalizer` and ACP's `_meta.exploremesh.canonicalizers`. It is
	// a SIBLING of `panel` rather than a member of it because it applies to every panel shape (default,
	// profile, ad-hoc) and because canonicalization is a distinct role, not an explorer seat.
	Canonicalizers []slot `json:"canonicalizers"`
	WaitSeconds    *int   `json:"waitSeconds"`
	// MaxParallel bounds how many of this run's explorers invoke their model CLI AT ONCE. Omitted,
	// the whole panel runs in parallel. It is a PER-CALL parameter and not a launch flag because the
	// right number is a fact about the machine the CLIs run on — resident memory for a cloud CLI,
	// loaded weights for a local model — and about the caller's provider rate limits, none of which
	// this server can see. It bounds parallelism only: every explorer still answers.
	MaxParallel *int `json:"maxParallel"`
	// DryRun resolves everything and spends nothing, answering with `dryRun: true` and a `shape` block
	// instead of a result. It is the same pre-spend disclosure the CLI's --dry-run prints, and it stops at
	// the same place: BEFORE the identity pre-flight, which is an exploration's first model call.
	DryRun         bool   `json:"dryRun"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type panelArg struct {
	Profile   string `json:"profile"`
	Count     *int   `json:"count"`
	Explorers []slot `json:"explorers"`
	Collator  *slot  `json:"collator"`
}

type axisArg struct {
	Name      string  `json:"name"`
	Direction string  `json:"direction"`
	Role      string  `json:"role"`
	Weight    float64 `json:"weight"`
}

// exploreArgs is the decode target for the single run-starting tool: the union of every mode's
// parameters. Which of them a given call may actually carry is decided by its `mode` and enforced in
// applyMode — the union here is a decoding convenience, never a statement that any combination is
// legal.
type exploreArgs struct {
	commonArgs
	Mode              string    `json:"mode"`
	Artifact          string    `json:"artifact"`
	Options           []string  `json:"options"`
	ComparisonAxes    []axisArg `json:"comparisonAxes"`
	Target            string    `json:"target"`
	Unit              string    `json:"unit"`
	Horizon           string    `json:"horizon"`
	ConditioningEvent string    `json:"conditioningEvent"`
}

// decodeArgs strictly decodes one run-starting call. Strict decoding is what makes
// `additionalProperties: false` a real boundary: a schema is advisory to a client, so a server that
// trusted it would accept a misspelled field and run something the caller did not ask for.
//
// There used to be four per-tool shapes here, one per run-starting tool, so a field belonging to a
// DIFFERENT tool failed to decode. With one tool the decode target is the union, and that protection
// has to be restated as an explicit per-mode check — applyMode does it, and refuses rather than
// ignores. Losing it silently is the failure this comment exists to prevent.
func decodeArgs(tool string, args json.RawMessage) (*exploreArgs, error) {
	if tool != "explore" {
		return nil, proto.InvalidParams("invalid params: %s does not take exploration arguments", tool)
	}
	if len(args) > maxArgumentBytes {
		return nil, proto.InvalidParams("invalid params: arguments are %d bytes, exceeding the %d-byte limit for one call", len(args), maxArgumentBytes)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	var out exploreArgs
	if err := dec.Decode(&out); err != nil {
		return nil, proto.InvalidParams("invalid params: %v", err)
	}
	return &out, nil
}

// applyMode is the single run-starting tool's mode dispatch: it validates the mode, enforces the
// per-mode input contract, and maps the arguments onto the raw task.
//
// It enforces BOTH directions, because only one of them is obvious. A missing required input is the
// expected error. A parameter belonging to a DIFFERENT mode is the dangerous one: accepted-and-
// ignored, a caller who passes `artifact` to a `map` run gets a successful result and believes their
// artifact was reviewed. The four separate tools used to get this for free — each decoded into its
// own struct, so a foreign field was a decode error — and collapsing them means saying it out loud.
//
// The rules come from modeInputRules, the same table the schema is generated from.
func applyMode(a *exploreArgs, r *schema.RawTask) error {
	r.Mode = strings.TrimSpace(a.Mode)
	if _, ok := mode.Lookup(r.Mode); !ok {
		return proto.InvalidParams("invalid params: unknown mode %q (known: %s)", a.Mode, strings.Join(allModes, ", "))
	}

	// Refuse any mode-specific parameter this mode does not take, naming the mode that does.
	allowed := allowedFor(r.Mode)
	supplied := map[string]bool{
		"artifact":          strings.TrimSpace(a.Artifact) != "",
		"options":           len(a.Options) > 0,
		"comparisonAxes":    len(a.ComparisonAxes) > 0,
		"target":            strings.TrimSpace(a.Target) != "",
		"unit":              strings.TrimSpace(a.Unit) != "",
		"horizon":           strings.TrimSpace(a.Horizon) != "",
		"conditioningEvent": strings.TrimSpace(a.ConditioningEvent) != "",
	}
	for _, p := range modeSpecificProps {
		if supplied[p] && !allowed[p] {
			return proto.InvalidParams(
				"invalid params: `%s` does not belong to mode %q and was refused rather than ignored (it is used by mode %q). "+
					"A parameter that is silently dropped reads as accepted: the run would have succeeded without ever using it",
				p, r.Mode, ownerOf(p))
		}
	}

	switch r.Mode {
	case mode.Challenge:
		if strings.TrimSpace(a.Artifact) == "" {
			return proto.InvalidParams("invalid params: `artifact` is required by mode %q and must not be blank — challenge reviews the artifact you supply, and there is nothing to review", mode.Challenge)
		}
		r.Artifact = a.Artifact

	case mode.Compare:
		if len(a.Options) < 2 {
			return proto.InvalidParams("invalid params: `options` is required by mode %q and needs at least 2 entries (got %d) — a comparison of one option is not a comparison", mode.Compare, len(a.Options))
		}
		seen := map[string]bool{}
		for _, o := range a.Options {
			key := strings.TrimSpace(o)
			if key == "" {
				return proto.InvalidParams("invalid params: `options` contains a blank entry — every declared option must name something")
			}
			if seen[key] {
				return proto.InvalidParams("invalid params: `options` contains the duplicate %q — the declared option set must be unique", key)
			}
			seen[key] = true
		}
		if len(a.ComparisonAxes) == 0 {
			return proto.InvalidParams("invalid params: `comparisonAxes` is required by mode %q — it is what the options are measured on (note: `criteria` are the constraints the exploration must satisfy, which is a different thing)", mode.Compare)
		}
		r.Options = a.Options
		for i, ax := range a.ComparisonAxes {
			c := schema.CompareCriterion{Name: strings.TrimSpace(ax.Name), Direction: schema.Direction(ax.Direction), Role: schema.CriterionRole(ax.Role), Weight: ax.Weight}
			if err := c.Validate(); err != nil {
				return proto.InvalidParams("invalid params: comparisonAxes[%d]: %v", i, err)
			}
			r.CompareCriteria = append(r.CompareCriteria, c)
		}

	case mode.Forecast:
		r.Target, r.Unit, r.Horizon = strings.TrimSpace(a.Target), strings.TrimSpace(a.Unit), strings.TrimSpace(a.Horizon)
		r.ConditioningEvent = strings.TrimSpace(a.ConditioningEvent)
		// Ordered, not a map range: a map would report a random one of several missing fields, so two
		// identical calls could blame different parameters.
		for _, f := range []struct {
			name, val string
		}{{"target", r.Target}, {"unit", r.Unit}, {"horizon", r.Horizon}} {
			if f.val == "" {
				return proto.InvalidParams("invalid params: `%s` is required by mode %q and must not be blank — a forecast whose target, unit or horizon is unstated cannot be pooled", f.name, mode.Forecast)
			}
		}
	}
	return nil
}

// ownerOf names the mode a mode-specific parameter belongs to, so a refusal can point somewhere
// rather than only say no.
func ownerOf(prop string) string {
	for m, rule := range modeInputRules {
		for _, p := range append(append([]string{}, rule.requires...), rule.permits...) {
			if p == prop {
				return m
			}
		}
	}
	return ""
}

// --- the run-starting handler ---

func (s *Server) exploreHandler(tool string, apply func(*exploreArgs, *schema.RawTask) error) proto.Handler {
	return func(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
		args, err := decodeArgs(tool, c.Arguments)
		if err != nil {
			return nil, err
		}
		task := schema.RawTask{
			Purpose:      strings.TrimSpace(args.Purpose),
			Criteria:     args.Criteria,
			PriorContext: strings.TrimSpace(args.PriorContext),
		}
		if err := apply(args, &task); err != nil {
			return nil, err
		}
		if strings.TrimSpace(task.Purpose) == "" {
			return nil, proto.InvalidParams("invalid params: `purpose` is required and must not be blank — it is what the panel is asked to explore")
		}
		if countNonBlank(task.Criteria) == 0 {
			return nil, proto.InvalidParams("invalid params: `criteria` must contain at least one non-blank entry — criteria are load-bearing intent and are never invented on your behalf")
		}
		if err := task.Validate(); err != nil {
			return nil, proto.InvalidParams("invalid params: %v", err)
		}
		// The MODE's own task requirement, checked before the panel is touched so a missing declaration
		// costs nothing.
		if err := mode.ValidateTask(task.Mode, task); err != nil {
			return nil, proto.InvalidParams("invalid params: %v", err)
		}
		pick, perr := s.resolvePanel(args.Panel)
		if perr != nil {
			return nil, perr
		}
		// The CANONICALIZER identities, resolved with the panel and BEFORE admission: same fail-closed
		// posture, same compose-not-configure rule, and no spend on either side of the refusal.
		if len(args.Canonicalizers) > 0 {
			cs, cerr := s.resolveCanonicalizers(args.Canonicalizers)
			if cerr != nil {
				return nil, cerr
			}
			pick.plan.Canonicalizers = cs
			pick.reqCanon = args.Canonicalizers
		}
		wait := s.waitDefault()
		if args.WaitSeconds != nil {
			wait = *args.WaitSeconds
			if wait < 1 || wait > MaxWaitSeconds {
				return nil, proto.InvalidParams("invalid params: waitSeconds must be between 1 and %d (got %d) — it is the INLINE budget, not the run's own timeout; a longer run is fetched with explore_run_result", MaxWaitSeconds, wait)
			}
		}
		// maxParallel is refused rather than clamped: a caller who asked to run two explorers at a
		// time because that is what their machine can host must not silently get the whole panel.
		maxParallel := 0
		if args.MaxParallel != nil {
			maxParallel = *args.MaxParallel
			if maxParallel < 1 {
				return nil, proto.InvalidParams("invalid params: maxParallel must be at least 1 (got %d) — omit it to run the whole panel in parallel", maxParallel)
			}
		}
		return s.startRun(ctx, c, tool, task, pick, wait, maxParallel, args.DryRun, strings.TrimSpace(args.IdempotencyKey))
	}
}

// startRun admits, launches and inline-waits for one run. dryRun stops it before the first model call and
// answers with the shape instead of a result.
func (s *Server) startRun(ctx context.Context, c *proto.Call, tool string, task schema.RawTask, pick panelPick, wait, maxParallel int, dryRun bool, key string) (*proto.CallToolResult, error) {
	// IDEMPOTENCY first, before any admission accounting: a retry after a dropped connection must return
	// the ORIGINAL run, not a second panel billed to the same person for the same question.
	if prior := s.runs.existing(key); prior != nil {
		// A still-running prior run is handed back as a TASK to a client that declared the tasks
		// extension, and as the job shape to everyone else — the retry gets whatever the original
		// call would now get.
		if taskHandOff(c, prior) {
			return nil, nil
		}
		return s.payloadFor(prior), nil
	}
	runID, auditRun := s.newRun()
	runCtx, cancel := context.WithCancel(context.Background())
	rec, aerr := s.runs.admit(runID, tool, runview.EffectiveMode(task.Mode), key, cancel)
	if aerr != nil {
		cancel()
		structured, text := refusalResult("", aerr)
		return proto.ErrorResult(text, structured), nil
	}
	// The panel echo is attached to the RECORD, not just to this response, so a later explore_run_status /
	// explore_run_result can answer "which panel is this?" while the run is still in flight.
	rec.attachPick(pick)

	// The caller's trace context, taken ONCE here, from the request that started the run. It is not
	// read later from the live Call: a run that outlives its inline budget still belongs to the trace
	// of the request that paid for it, and re-reading would attach whichever request happened to be in
	// scope when the goroutine got there.
	trace := traceOf(c)

	// The progress sink is live only for the INLINE wait: a progress notification is correlated to the
	// request that supplied the token, and this call is the only request that did.
	var sink atomic.Pointer[proto.Call]
	sink.Store(c)
	onEvent := func(ev audit.EventLine) {
		if live := sink.Load(); live != nil {
			if f, ok := phaseFraction[ev.EventType]; ok {
				live.Progress(f, 1, ev.Message)
			}
		}
		// Logging is session-level and BEST-EFFORT BY CONTRACT: it is dropped below the client's level
		// and nothing here is the only carrier of a governance fact — the run record and the result are.
		c.Log(logLevelFor(ev.Level), "exploremesh", map[string]any{
			"runId": runID, "eventType": ev.EventType, "message": ev.Message,
		})
	}

	go func() {
		defer cancel()
		budget := s.TurnTimeout
		if budget <= 0 {
			budget = defaultTurnTimeout
		}
		tctx, tcancel := context.WithTimeout(runCtx, budget)
		defer tcancel()
		out, rerr := s.Explorer.Run(tctx, pick.plan, task, pipeline.Options{MaxParallel: maxParallel, DryRun: dryRun}, onEvent)
		// A dry run is NOT captured. A run directory records an exploration — envelopes, raw model outputs,
		// prompts, a manifest — and a dry run produced none of them; writing one would put a record of an
		// exploration nobody performed next to the records of ones that happened. The CLI refuses
		// `--dry-run --dump-run` for the same reason.
		capturedID := ""
		if !dryRun {
			capturedID = s.dump(auditRun, pick.plan, task, out, rerr, trace)
		}
		cancelled := errors.Is(rerr, context.Canceled) || runCtx.Err() != nil
		switch {
		case rerr != nil:
			structured, text := haltResult(rec, pick, task, out, rerr, capturedID, cancelled)
			state := StateHalted
			if cancelled {
				state = StateCancelled
			}
			rec.finish(state, structured, text, true)
		default:
			structured, text := completeResult(rec, task, pick, out, capturedID)
			rec.finish(StateComplete, structured, text, false)
		}
		s.runs.release(runID)
	}()

	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()
	select {
	case <-rec.done:
		sink.Store(nil)
		if c.HasProgressToken() {
			c.Progress(1, 1, "run complete")
		}
		return s.payloadFor(rec), nil
	case <-timer.C:
		// JOB SHAPE: hand back the run id rather than holding a request open past the client's timeout,
		// where it would be killed mid-spend with the panel still running.
		sink.Store(nil)
		// THE ONE PLACE THE TASKS EXTENSION CHANGES A `tools/call` ANSWER: a client that declared
		// `io.modelcontextprotocol/tasks` receives a `CreateTaskResult` for the SAME run id; a
		// client that did not receives the identical bytes it always did.
		if taskHandOff(c, rec) {
			return nil, nil
		}
		structured, text := runningResult(rec, pick, wait)
		return proto.Result(text, structured), nil
	case <-ctx.Done():
		// The CLIENT cancelled (or the session went away). Kill the run: a cancelled call must not keep
		// spending, and it must not commit anything afterwards. The receipt is the run record on disk —
		// the response a cancelled call never gets is not the only place the outcome exists.
		//
		// The payload is the CANCELLED shape, not the running shape with a different `state`: it carries
		// the taxonomy (`haltClass`/`reasonCode`) a client branches on, and it satisfies the `cancelled`
		// branch of the declared outputSchema. Overwriting `state` on the running shape produced a
		// payload that matched no branch at all.
		sink.Store(nil)
		cancel()
		structured, text := runningResult(rec, pick, wait)
		structured["state"] = StateCancelled
		structured["haltClass"], structured["reasonCode"] = "cancelled", "run_cancelled"
		return proto.Result(text, structured), nil
	}
}

// newRun mints the run id. When capture is on, the id IS the audit run directory's name, so the wire id
// and the on-disk record are the same identifier — a run id that did not name its own audit record would
// make the record unfindable from the only handle the caller has.
func (s *Server) newRun() (string, *audit.Run) {
	if !s.DisableCapture {
		if run, err := audit.NewRun(capture.ArtifactDir(), "", time.Now()); err == nil {
			return run.ID, run
		} else if s.core != nil {
			s.core.Diagnosticf("aimesh explore mcp: run capture unavailable (%v); continuing without a disk record", err)
		}
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano()), nil
	}
	return "run-" + hex.EncodeToString(b[:]), nil
}

// traceOf lifts the caller's W3C trace context off a request's protocol context into the run record's
// own shape. It returns nil when the caller sent none, which is every legacy-era request: the three
// keys are a `2026-07-28` `_meta` convention and the legacy env never fills them.
//
// The values are carried VERBATIM and never validated. `basic/index` §`_meta` says of every reserved
// key that "implementations MUST NOT make assumptions about values at these keys"; the W3C-format MUST
// on the same page binds the sender, so refusing a malformed `traceparent` here would be this server
// asserting a meaning for a field it does not own.
func traceOf(c *proto.Call) *capture.TraceContext {
	if c == nil {
		return nil
	}
	t := c.Env().Trace
	return capture.NewTraceContext(t.TraceParent, t.TraceState, t.Baggage)
}

// dump writes the run record (on BOTH the success and the halt path — a halted run is the fixture most
// worth keeping) and returns the captured run id, or "" when nothing was written.
func (s *Server) dump(run *audit.Run, plan roster.Plan, task schema.RawTask, out pipeline.Result, rerr error, trace *capture.TraceContext) string {
	if run == nil {
		return ""
	}
	status, faultMsg := "complete", ""
	if rerr != nil {
		status, faultMsg = "halted", rerr.Error()
	}
	if err := capture.Dump(capture.Input{Run: run, Plan: plan, Result: out, Status: status, Fault: faultMsg, Task: task, Trace: trace}); err != nil {
		if s.core != nil {
			s.core.Diagnosticf("aimesh explore mcp: run capture failed for %s: %v", run.ID, err)
		}
		return ""
	}
	s.publishRunArtifacts(run)
	return run.ID
}

// publishRunArtifacts exposes a captured run's files as MCP resources.
//
// The set is taken from the run's OWN MANIFEST rather than by walking the directory: the manifest is the
// index the capture layer wrote, with each artifact's run-relative path, size and digest, so what is
// published is exactly what was recorded — and the recorded digest is re-verified on every fetch. Walking
// instead would publish whatever happened to be in the directory, which is a different (and looser) claim.
//
// Nothing here reaches the wire but the manifest's own logical names. The run directory is held privately
// by the store; a resource URI is `aimesh://run/<runId>/<name>`.
func (s *Server) publishRunArtifacts(run *audit.Run) {
	b, err := workspace.ReadUnder(run.Dir, manifestFile, 0)
	if err != nil {
		if s.core != nil {
			s.core.Diagnosticf("aimesh explore mcp: run %s captured, but its manifest could not be read (%v); no resources published", run.ID, err)
		}
		return
	}
	var m capture.ManifestV1
	if jerr := json.Unmarshal(b, &m); jerr != nil {
		if s.core != nil {
			s.core.Diagnosticf("aimesh explore mcp: run %s has an unreadable manifest (%v); no resources published", run.ID, jerr)
		}
		return
	}
	s.artifacts.Publish(run.ID, proto.Artifact{
		Name: "manifest", Title: "Run manifest (" + run.ID + ")",
		Description: "The typed index of this run: the explorer/collator identities, the envelope aliases, every dropped seat with its audited reason, and the governance counters. It is the entry point to every other resource of this run.",
		MimeType:    "application/json", Dir: run.Dir, Rel: manifestFile,
	})
	for _, a := range m.Artifacts {
		name := artifactResourceName(a.Path)
		if name == "" {
			continue
		}
		s.artifacts.Publish(run.ID, proto.Artifact{
			Name: name, Title: name + " (run " + run.ID + ")",
			Description: "A captured artifact of this run, exactly as the run record holds it.",
			MimeType:    mimeForArtifact(a.Path),
			Dir:         run.Dir, Rel: a.Path, SHA256: a.SHA256,
		})
	}
}

// manifestFile is the capture layer's index file, run-relative.
const manifestFile = "manifest.json"

// artifactResourceName turns a run-relative artifact PATH into the single path-free token a resource URI
// addresses: `calls/explorer-1.json` → `calls.explorer-1`. A URI segment is an identifier, so the layout
// is folded away rather than published; the mapping is deterministic, which is what lets a caller read a
// name in `resources/list` and use it unchanged.
func artifactResourceName(rel string) string {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, `\`, "/"))
	rel = strings.TrimSuffix(rel, filepath.Ext(rel))
	rel = strings.Trim(strings.ReplaceAll(rel, "/", "."), ".")
	if rel == "" || strings.Contains(rel, "..") {
		return ""
	}
	return rel
}

func mimeForArtifact(rel string) string {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".json":
		return "application/json"
	case ".jsonl":
		return "application/x-ndjson"
	case ".md":
		return "text/markdown"
	default:
		return "text/plain"
	}
}

// payloadFor renders a record's current state as a tool result.
func (s *Server) payloadFor(rec *record) *proto.CallToolResult {
	state, structured, text, isErr := rec.snapshot()
	if state == StateRunning {
		out := map[string]any{"runId": rec.ID, "state": StateRunning, "tool": rec.Tool, "mode": rec.Mode}
		// The panel echo is REQUIRED on the running branch, and it is knowable from admission — the
		// requested half always, the executed half from the frozen plan. Omitting it here made
		// `explore_run_result` on a still-running exploration emit a payload its own declared schema rejected.
		out["panel"] = rec.pickSnapshot().echo(nil)
		return proto.Result(fmt.Sprintf("Run %s is still running. Poll explore_run_status, then fetch explore_run_result.", rec.ID), out)
	}
	if isErr {
		return proto.ErrorResult(text, structured)
	}
	return proto.Result(text, structured)
}

// --- explore_run_status / explore_run_result ---

type runIDArgs struct {
	RunID string `json:"runId"`
}

func decodeRunID(args json.RawMessage) (string, error) {
	if len(args) == 0 {
		return "", proto.InvalidParams("invalid params: `runId` is required")
	}
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	var a runIDArgs
	if err := dec.Decode(&a); err != nil {
		return "", proto.InvalidParams("invalid params: %v", err)
	}
	if strings.TrimSpace(a.RunID) == "" {
		return "", proto.InvalidParams("invalid params: `runId` is required and must not be blank")
	}
	return strings.TrimSpace(a.RunID), nil
}

// unknownRun is the refusal for a run id this server cannot resolve. It rides `isError` rather than a
// protocol error because it is something the CALLING MODEL has to react to (re-run, or stop asking), and
// because an expired run is a legitimate outcome of the retention bound rather than a malformed request.
func unknownRun(id string) *proto.CallToolResult {
	err := fault.New(fault.Usage, fmt.Sprintf(
		"unknown or expired runId %q — finished runs are retained for a bounded time and count. If you still need the answer, start a new run.", id)).
		WithHalt("usage").WithReason("unknown_run_id")
	structured, text := refusalResult(id, err)
	return proto.ErrorResult(text, structured)
}

func (s *Server) runStatusHandler(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
	id, err := decodeRunID(c.Arguments)
	if err != nil {
		return nil, err
	}
	rec := s.runs.get(id)
	if rec == nil {
		return unknownRun(id), nil
	}
	state, structured, _, _ := rec.snapshot()
	out := map[string]any{
		"runId":          rec.ID,
		"state":          state,
		"tool":           rec.Tool,
		"mode":           rec.Mode,
		"elapsedSeconds": elapsed(rec),
	}
	if structured != nil {
		if pan, ok := structured["panel"]; ok {
			out["panel"] = pan
		}
		if cap, ok := structured["captured"]; ok {
			out["captured"] = cap
		}
	}
	text := fmt.Sprintf("Run %s (%s): %s after %.1fs.", rec.ID, rec.Tool, state, elapsed(rec))
	if state == StateRunning {
		text += " Keep polling; do not start another run for the same question."
	} else {
		text += " Fetch the full result with explore_run_result."
	}
	return proto.Result(text, out), nil
}

func (s *Server) runResultHandler(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
	id, err := decodeRunID(c.Arguments)
	if err != nil {
		return nil, err
	}
	rec := s.runs.get(id)
	if rec == nil {
		return unknownRun(id), nil
	}
	return s.payloadFor(rec), nil
}

func elapsed(rec *record) float64 {
	end := rec.End
	if state, _, _, _ := rec.snapshot(); state == StateRunning {
		end = time.Now()
	}
	return end.Sub(rec.Start).Seconds()
}

// --- progress + logging mapping ---

// phaseFraction maps a pipeline event to the FRACTION OF PHASES complete. It is deliberately a fixed
// ladder over named phases rather than a per-item counter: a counter that resets when a phase is retried
// goes backwards, and a client that sees progress go backwards cannot tell a retry from a bug. Events
// that are not phase boundaries (a citation tally, a drop) move nothing — they are logged, not counted.
var phaseFraction = map[string]float64{
	"panel_frozen":        0.10,
	"formulate_start":     0.15,
	"formulate_done":      0.25,
	"synthesize_start":    0.70,
	"citations_validated": 0.90,
	"synthesize_done":     0.95,
}

// logLevelFor maps a pipeline event level onto the MCP logging levels.
func logLevelFor(level string) proto.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return proto.LevelDebug
	case "warn", "warning":
		return proto.LevelWarning
	case "error":
		return proto.LevelError
	default:
		return proto.LevelInfo
	}
}

func countNonBlank(ss []string) int {
	n := 0
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			n++
		}
	}
	return n
}

// --- human renderings for the read-only tools ---

func renderList(payload map[string]any) string {
	var b strings.Builder
	adapters, _ := payload["adapters"].([]map[string]any)
	b.WriteString("Configured adapters (identifiers only — no paths are reported):\n")
	for _, a := range adapters {
		fmt.Fprintf(&b, "  %v [%v] configured=%v", a["name"], a["kind"], a["configured"])
		if ev, ok := a["identityEvidenceCapability"]; ok {
			fmt.Fprintf(&b, " identityEvidence=%v", ev)
		}
		b.WriteString("\n")
	}
	profiles, _ := payload["profiles"].(map[string]any)
	rows, _ := profiles["profiles"].([]map[string]any)
	fmt.Fprintf(&b, "Profiles (default: %v):\n", profiles["defaultProfile"])
	for _, p := range rows {
		seats, _ := p["explorers"].([]map[string]any)
		fmt.Fprintf(&b, "  %v — %d explorer(s) in preference order + 1 collator\n", p["name"], len(seats))
	}
	modes, _ := payload["modes"].([]string)
	fmt.Fprintf(&b, "Modes: %s\n", strings.Join(modes, ", "))
	b.WriteString("Full detail, including the admission limits, is in structuredContent.")
	return b.String()
}

func renderDoctor(payload map[string]any) string {
	var b strings.Builder
	ok, _ := payload["ok"].(bool)
	if ok {
		b.WriteString("Readiness: OK (static checks only — no process was started and nothing was spent).\n")
	} else {
		b.WriteString("Readiness: FAILING (static checks only — no process was started and nothing was spent).\n")
	}
	checks, _ := payload["checks"].([]map[string]any)
	for _, c := range checks {
		state := "ok  "
		if v, _ := c["ok"].(bool); !v {
			state = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %v", state, c["name"])
		if d, has := c["detail"]; has {
			fmt.Fprintf(&b, " — %v", d)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
