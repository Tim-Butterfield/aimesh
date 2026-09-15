// Package mcp serves explorations over MCP (Model Context Protocol) as a local stdio server. The protocol
// lives in meshcore/mcp; this package owns the tools, their schemas and the run registry.
//
//   - A run-starting call waits up to `waitSeconds` for its result and otherwise returns
//     `{runId, state: "running"}`; explore_run_status and explore_run_result finish the exchange. Client
//     timeouts are shorter than a panel run.
//   - Domain halts are returned as `isError` tool results carrying `{exitCode, haltClass, reasonCode,
//     failure}`, because clients often drop JSON-RPC error data. Protocol errors are reserved for
//     malformed requests.
//   - Every terminal result includes the governance block, identity caveats and panel echo.
//   - Each call composes its panel from the adapters the server was launched with; no call can add an
//     adapter, path or launch argument, and no tool reports paths or environment.
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
	"slices"
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
	"github.com/Tim-Butterfield/aimesh/internal/explore/roster"
	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/runview"
	"github.com/Tim-Butterfield/aimesh/internal/launchflags"
)

// ServerName / ServerVersion identify this server in the MCP handshake.
const (
	ServerName    = "exploremesh"
	ServerVersion = "dev"
)

// maxArgumentBytes bounds one tool call's arguments before they are decoded.
const maxArgumentBytes = 1 << 20 // 1 MiB

// defaultTurnTimeout is the wall-clock budget for one run when none is configured.
const defaultTurnTimeout = 10 * time.Minute

// Explorer runs an exploration. The ACP surface uses the same interface, and tests substitute a fake.
type Explorer interface {
	Run(ctx context.Context, plan roster.Plan, raw schema.RawTask, opts pipeline.Options, onEvent func(audit.EventLine)) (pipeline.Result, error)
}

// Server is the explore MCP server.
type Server struct {
	// Explorer routes a run to the pipeline.
	Explorer Explorer
	// Adapters are the adapters this server was launched with (`--adapter`). A call's panel may name only
	// these; empty means every exploration is refused until the operator names one.
	Adapters launchflags.Set
	// Config supplies the sanitized projections behind `explore_list` and `explore_doctor`.
	Config Config

	// WaitSeconds overrides the default inline wait.
	WaitSeconds int
	// TurnTimeout bounds one run's wall clock; 0 means 10 minutes.
	TurnTimeout time.Duration
	// DisableCapture turns off the on-disk run record, which is on by default.
	DisableCapture bool
	// StrictSchema checks every structuredContent against its tool's outputSchema before sending. See
	// proto.Server.StrictSchema.
	StrictSchema bool

	Framing string
	// Protocol is the launch era posture (`--protocol dual|legacy`; "" means dual). explore_doctor
	// reports it because a host may discard stderr.
	//
	// SUNSET-PATH (MCP26-SUNSET): removed with the legacy era.
	Protocol proto.ProtocolMode
	// Diagnostics is the server's log sink. It must never be the protocol stream.
	Diagnostics io.Writer

	runs *registry
	core *proto.Server

	// attached is set when the server registers into a shared core (the combined `aimesh mcp`), which
	// registers the shared tools such as agents_md itself.
	attached bool

	// artifacts publishes captured run files as MCP resources.
	artifacts proto.RunStore
}

// Serve registers the tools and serves MCP over in and out until EOF. On return every in-flight run is
// cancelled, so a disconnected client leaves no model CLIs running.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	s.build()
	defer s.runs.cancelAll()
	return s.core.Serve(in, out)
}

// Core returns the underlying protocol server, building it if needed.
func (s *Server) Core() *proto.Server {
	s.build()
	return s.core
}

func (s *Server) build() { s.buildInto(nil) }

// Attach registers this domain's tools into a shared protocol core, as the combined `aimesh mcp` server
// does, and returns the resource and task providers the composer must merge.
func (s *Server) Attach(core *proto.Server) (proto.ResourceProvider, proto.TaskProvider) {
	s.buildInto(core)
	return &s.artifacts, taskProvider{s}
}

// buildInto wires the server into core, or into a new core of its own when core is nil.
func (s *Server) buildInto(core *proto.Server) {
	if s.core != nil {
		return
	}
	s.runs = newRegistry()
	s.runs.onEvict = s.artifacts.Forget
	if core != nil {
		s.attached = true
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
			// The tasks extension is a view over the run registry (taskId == runId; see tasks.go).
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

// spendAnnotations returns the annotations for a tool that starts a run: not read-only, not idempotent,
// open-world. Hosts use the read-only hint to decide whether to ask the user first.
func spendAnnotations(title string) *proto.ToolAnnotations {
	return &proto.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: new(false),
		IdempotentHint:  false,
		OpenWorldHint:   new(true),
	}
}

// readOnlyAnnotations returns the annotations for a tool that only reports.
func readOnlyAnnotations(title string) *proto.ToolAnnotations {
	return &proto.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: new(false),
		IdempotentHint:  true,
		OpenWorldHint:   new(false),
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
		Title:        "Report the launched adapters and modes",
		Description:  "Reports what this server can run: the adapters it was launched with and whether each can be started now, the available modes, and the admission limits in force. Read-only. Reports no binary paths, launch arguments or environment detail, and cannot change any configuration. Call this before composing a panel.",
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
		Description:  "Static readiness of the adapters this server was launched with: whether each adapter's CLI can be started. No model call is made, nothing is spent, and no path or environment detail is reported. To check that a panel's agents can do real work, pass verifyReadiness: true on the explore call.",
		InputSchema:  raw(emptyInputSchema),
		OutputSchema: raw(doctorOutputSchema),
		Annotations:  readOnlyAnnotations("Check readiness"),
	}, func(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
		payload := s.doctorPayload(c.Env())
		return proto.Result(renderDoctor(payload), payload), nil
	})

	// The agent guide tool is shared with the review server; a composed server registers it once.
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
				// An unreadable override is an error rather than a silent fallback to the embedded guide.
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

// commonArgs are the parameters shared by every mode.
type commonArgs struct {
	Purpose      string    `json:"purpose"`
	Criteria     []string  `json:"criteria"`
	PriorContext string    `json:"priorContext"`
	Panel        *panelArg `json:"panel"`
	// Canonicalizers names the two canonicalizer identities, like the CLI's `--canonicalizer`.
	// Canonicalization is a separate role, so it sits beside `panel` rather than inside it.
	Canonicalizers []slot `json:"canonicalizers"`
	WaitSeconds    *int   `json:"waitSeconds"`
	// MaxParallel bounds how many explorers run at once; omitted, the whole panel runs in parallel.
	MaxParallel *int `json:"maxParallel"`
	// DryRun returns the run's shape instead of running it, stopping before the first model call.
	DryRun bool `json:"dryRun"`
	// VerifyReadiness makes one bounded call per distinct agent before the run starts.
	VerifyReadiness bool   `json:"verifyReadiness"`
	IdempotencyKey  string `json:"idempotencyKey"`
}

// panelArg is the required `panel`: the explorers and the collator one call composes.
type panelArg struct {
	Explorers []slot `json:"explorers"`
	Collator  *slot  `json:"collator"`
}

type axisArg struct {
	Name      string  `json:"name"`
	Direction string  `json:"direction"`
	Role      string  `json:"role"`
	Weight    float64 `json:"weight"`
}

// exploreArgs is the decode target for the explore tool: the union of every mode's parameters. applyMode
// enforces which ones a given mode accepts.
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

// decodeArgs decodes an explore call's arguments, rejecting unknown fields so a misspelled parameter is an
// error rather than ignored. Parameters belonging to a different mode are refused later by applyMode.
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

// applyMode validates the mode, enforces its input rules and copies the arguments onto the raw task. It
// refuses both a missing required input and a parameter that belongs to a different mode, which would
// otherwise be silently ignored. The rules come from modeInputRules, which also generates the schema.
func applyMode(a *exploreArgs, r *schema.RawTask) error {
	r.Mode = strings.TrimSpace(a.Mode)
	if _, ok := mode.Lookup(r.Mode); !ok {
		return proto.InvalidParams("invalid params: unknown mode %q (known: %s)", a.Mode, strings.Join(allModes, ", "))
	}

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
		// A slice keeps the reported missing field deterministic.
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

// ownerOf returns the mode that uses a mode-specific parameter, or "".
func ownerOf(prop string) string {
	for m, rule := range modeInputRules {
		if slices.Contains(append(append([]string{}, rule.requires...), rule.permits...), prop) {
			return m
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
		if err := mode.ValidateTask(task.Mode, task); err != nil {
			return nil, proto.InvalidParams("invalid params: %v", err)
		}
		pick, perr := s.resolvePanel(args.Panel)
		if perr != nil {
			return nil, perr
		}
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
		// An invalid maxParallel is refused rather than clamped.
		maxParallel := 0
		if args.MaxParallel != nil {
			maxParallel = *args.MaxParallel
			if maxParallel < 1 {
				return nil, proto.InvalidParams("invalid params: maxParallel must be at least 1 (got %d) — omit it to run the whole panel in parallel", maxParallel)
			}
		}
		return s.startRun(ctx, c, tool, task, pick, wait, maxParallel, args.DryRun, args.VerifyReadiness, strings.TrimSpace(args.IdempotencyKey))
	}
}

// startRun admits and launches one run, then waits up to wait seconds for it inline.
func (s *Server) startRun(ctx context.Context, c *proto.Call, tool string, task schema.RawTask, pick panelPick, wait, maxParallel int, dryRun, verifyReadiness bool, key string) (*proto.CallToolResult, error) {
	// A retry with the same idempotency key returns the original run instead of starting another.
	if prior := s.runs.existing(key); prior != nil {
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
	// The panel echo lives on the record so status polls can report it while the run is in flight.
	rec.attachPick(pick)

	// The trace context comes from the request that started the run, even if the run outlives it.
	trace := traceOf(c)

	// Progress is reported only during the inline wait, since it is tied to this request's token.
	var sink atomic.Pointer[proto.Call]
	sink.Store(c)
	onEvent := func(ev audit.EventLine) {
		if live := sink.Load(); live != nil {
			if f, ok := phaseFraction[ev.EventType]; ok {
				live.Progress(f, 1, ev.Message)
			}
		}
		// Logging is best-effort; the run record and result carry every governance fact.
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
		out, rerr := s.Explorer.Run(tctx, pick.plan, task, pipeline.Options{MaxParallel: maxParallel, DryRun: dryRun, VerifyReadiness: verifyReadiness}, onEvent)
		// A dry run performs no exploration, so it writes no run record.
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
		// Return the run id rather than hold the request open past the client's timeout. A client that
		// declared the tasks extension receives a CreateTaskResult for the same run instead.
		sink.Store(nil)
		if taskHandOff(c, rec) {
			return nil, nil
		}
		structured, text := runningResult(rec, pick, wait)
		return proto.Result(text, structured), nil
	case <-ctx.Done():
		// The client cancelled: stop the run. The payload carries haltClass and reasonCode so it matches
		// the cancelled branch of the output schema.
		sink.Store(nil)
		cancel()
		structured, text := runningResult(rec, pick, wait)
		structured["state"] = StateCancelled
		structured["haltClass"], structured["reasonCode"] = "cancelled", "run_cancelled"
		return proto.Result(text, structured), nil
	}
}

// newRun creates a run id. With capture on, the id is the audit run directory's name, so the caller's
// handle also names the on-disk record.
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

// traceOf returns the caller's W3C trace context for the run record, or nil if the request carried none
// (as legacy-era requests never do). The values are copied as sent: the MCP specification says servers
// must not make assumptions about reserved `_meta` values.
func traceOf(c *proto.Call) *capture.TraceContext {
	if c == nil {
		return nil
	}
	t := c.Env().Trace
	return capture.NewTraceContext(t.TraceParent, t.TraceState, t.Baggage)
}

// dump writes the run record for a completed or halted run and returns its id, or "" if nothing was
// written.
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

// publishRunArtifacts publishes a captured run's files as MCP resources named
// `aimesh://run/<runId>/<name>`. The file set and digests come from the run's manifest rather than a
// directory walk, and each digest is re-verified on fetch. No path reaches the wire.
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

// artifactResourceName maps a run-relative artifact path to a resource name, for example
// `calls/explorer-1.json` to `calls.explorer-1`. It returns "" for an empty or unsafe path.
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
		// The output schema requires the panel echo on the running branch too.
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

// unknownRun returns the isError result for a run id this server cannot resolve. An expired run is a
// normal outcome of retention, not a malformed request.
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

// phaseFraction maps phase-boundary pipeline events to a progress fraction. Fixed values keep progress
// monotonic; other events are logged but do not move progress.
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
	b.WriteString("Adapters this server was launched with (identifiers only — no paths are reported):\n")
	if len(adapters) == 0 {
		b.WriteString("  none — the operator must add --adapter <name> to the host configuration\n")
	}
	for _, a := range adapters {
		fmt.Fprintf(&b, "  %v [%v] available=%v", a["name"], a["kind"], a["available"])
		if r, ok := a["reason"]; ok {
			fmt.Fprintf(&b, " (%v)", r)
		}
		if ev, ok := a["identityEvidenceCapability"]; ok {
			fmt.Fprintf(&b, " identityEvidence=%v", ev)
		}
		b.WriteString("\n")
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
