// Package mcpserve composes the two domains into ONE MCP server: `aimesh mcp`. One binary, one
// server, one entry in a client's configuration — matching the CLI, where both domains are reached
// through a single `aimesh`.
//
// TOOLS ARE NOT THE HARD PART. proto.Server dispatches `tools/call` by name, and every tool is
// already domain-prefixed (review_list, explore_list, …), so two registrars sharing one core cannot
// collide. What needs composing is the pair of ID-ADDRESSED protocol methods, which carry no tool
// name and so cannot be routed the same way:
//
//   - `resources/read <uri>`
//   - `tasks/get <id>` and `tasks/cancel <id>`
//
// A proto.Server has ONE Resources field and ONE Tasks field; both domains supply theirs.
//
// HOW THE COMPOSITES ROUTE, AND WHY NOT THE OBVIOUS WAY. The tempting implementation is to parse the
// identifier — review's run ids look like `<timestamp>-<hex>`, explore's like `run-<hex>`. That
// difference is INCIDENTAL, nobody declared it, and a routing rule resting on it would keep working
// right up until one of them changed its id format, at which point `tasks/cancel` would silently
// stop reaching a run that is spending money.
//
// So these route by ASKING, not by parsing. Both provider interfaces already report ownership:
// TaskProvider.Task returns (view, ok) and ReadResource returns a not-found error for a URI it does
// not publish. The composite tries each domain and takes the one that claims the identifier. That is
// exact, survives any id format, and needs no namespacing scheme.
//
// It assumes AT MOST ONE owner per identifier. With 64 bits of randomness per id that is already
// overwhelming, and a test pins that the two formats stay disjoint so the assumption cannot rot
// quietly.
package mcpserve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"

	"github.com/Tim-Butterfield/aimesh/internal/agentguide"
	exploremcp "github.com/Tim-Butterfield/aimesh/internal/explore/surface/mcp"
	reviewmcp "github.com/Tim-Butterfield/aimesh/internal/review/surface/mcp"
)

// ServerName is what the composed server calls itself in the handshake.
const ServerName = "aimesh"

// Domain selects which tool sets are served.
type Domain string

const (
	// DomainAll serves both. The default: one config entry gets you the whole tool.
	DomainAll Domain = ""
	// DomainReview / DomainExplore serve one. Narrowing is worth having because tool-list size costs
	// a model attention on every call, and a review-only user should not carry explore's tools.
	DomainReview  Domain = "review"
	DomainExplore Domain = "explore"
)

// ParseDomain validates an --only value. An unrecognized one is refused rather than defaulted: a
// typo that silently served everything would hand a caller tools they explicitly asked not to have.
func ParseDomain(s string) (Domain, error) {
	switch Domain(strings.TrimSpace(s)) {
	case DomainAll:
		return DomainAll, nil
	case DomainReview:
		return DomainReview, nil
	case DomainExplore:
		return DomainExplore, nil
	default:
		return "", fmt.Errorf("--only %q is not a domain (want review or explore, or omit it to serve both)", s)
	}
}

func (d Domain) servesReview() bool  { return d == DomainAll || d == DomainReview }
func (d Domain) servesExplore() bool { return d == DomainAll || d == DomainExplore }

// Server composes the two domain servers over one protocol core.
type Server struct {
	// Only narrows the served tool set. Zero value serves both.
	Only Domain

	// Review and Explore are the fully-configured domain servers. A nil one is simply not served —
	// which is how the caller expresses --only without this package knowing how either is built.
	Review  *reviewmcp.Server
	Explore *exploremcp.Server

	Framing     string
	Protocol    proto.ProtocolMode
	Diagnostics io.Writer
	Version     string
}

// Serve builds the composed tool set and serves until EOF.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	core, err := s.build()
	if err != nil {
		return err
	}
	return core.Serve(in, out)
}

// Core builds and returns the composed protocol server (for tests driving an explicit framer).
func (s *Server) Core() (*proto.Server, error) { return s.build() }

func (s *Server) build() (*proto.Server, error) {
	core := &proto.Server{
		Info:         proto.Implementation{Name: ServerName, Title: "aimesh", Version: s.Version},
		Instructions: s.instructions(),
		Framing:      s.Framing,
		Protocol:     s.Protocol,
		Diagnostics:  s.Diagnostics,
	}

	var resources []proto.ResourceProvider
	var tasks []proto.TaskProvider

	if s.Only.servesReview() {
		if s.Review == nil {
			return nil, fmt.Errorf("aimesh mcp: the review domain is selected but not configured")
		}
		r, t := s.Review.Attach(core)
		resources, tasks = append(resources, r), append(tasks, t)
	}
	if s.Only.servesExplore() {
		if s.Explore == nil {
			return nil, fmt.Errorf("aimesh mcp: the explore domain is selected but not configured")
		}
		r, t := s.Explore.Attach(core)
		resources, tasks = append(resources, r), append(tasks, t)
	}
	if len(resources) == 0 {
		return nil, fmt.Errorf("aimesh mcp: no domain selected")
	}

	// `agents_md` is registered ONCE for the whole server. It is one document about one tool, and
	// proto.Server PANICS on a duplicate name — so the domain servers skip it whenever they are
	// attached to an external core (see their Attach), and it is registered here instead.
	registerAgentGuide(core)

	core.Resources = composeResources(resources)
	core.Tasks = composeTasks(tasks)
	return core, nil
}

// instructions is the composed cross-tool contract. Each domain's own instructions are written for a
// server serving only that domain, so the composed server states the shape rather than concatenating
// two documents that both open by describing "this server".
func (s *Server) instructions() string {
	var b strings.Builder
	b.WriteString("aimesh runs provider-diverse, governed AI reviews and explorations.\n\n")
	b.WriteString("Tools are named by domain. Call agents_md FIRST for the full contract — the rules that are\n")
	b.WriteString("expensive to get wrong (identity is recorded but never acted on, the containment copy, the\n")
	b.WriteString("write denylist, apply semantics) are stated there and nowhere shorter.\n\n")
	if s.Only.servesReview() {
		b.WriteString("review_* — review a workspace. review_report writes NOTHING. review_remediate WRITES and is\n")
		b.WriteString("listed only when the operator launched with --allow-remediate.\n")
	}
	if s.Only.servesExplore() {
		b.WriteString("explore  — run a blind multi-model exploration. `mode` is required and decides which other\n")
		b.WriteString("parameters are required; a parameter belonging to another mode is refused, not ignored.\n")
	}
	b.WriteString("\nRuns are JOB-SHAPED: a run that outlives waitSeconds returns {runId, state:\"running\"} — poll\n")
	b.WriteString("the domain's *_run_status, then fetch its *_run_result. Do not re-issue the call; pass the same\n")
	b.WriteString("idempotencyKey and you get the existing run back rather than paying twice.\n")
	b.WriteString("\nCOUNTS, RANKINGS AND POOLED NUMBERS ARE COMPUTED BY THE HOST. Report them as given; never\n")
	b.WriteString("recompute or restate them. Always surface the governance block and the identity caveats.\n")
	return b.String()
}

func registerAgentGuide(core *proto.Server) {
	core.Register(proto.Tool{
		Name:         agentguide.ToolName,
		Title:        agentguide.ToolTitle,
		Description:  agentguide.ToolDescription,
		InputSchema:  json.RawMessage(agentguide.InputSchema),
		OutputSchema: json.RawMessage(agentguide.OutputSchema),
		Annotations: &proto.ToolAnnotations{
			Title:           agentguide.ToolAnnotationTitle,
			ReadOnlyHint:    true,
			DestructiveHint: proto.Bool(false),
			IdempotentHint:  true,
			OpenWorldHint:   proto.Bool(false),
		},
	}, func(ctx context.Context, c *proto.Call) (*proto.CallToolResult, error) {
		text, payload, err := agentguide.ToolPayload()
		if err != nil {
			return proto.ErrorResult(err.Error(), nil), nil
		}
		return proto.Result(text, payload), nil
	})
}

// --- the composites ---

type resourceComposite []proto.ResourceProvider

func composeResources(ps []proto.ResourceProvider) proto.ResourceProvider {
	if len(ps) == 1 {
		return ps[0]
	}
	return resourceComposite(ps)
}

func (c resourceComposite) ListResources(ctx context.Context) ([]proto.Resource, error) {
	var out []proto.Resource
	for _, p := range c {
		got, err := p.ListResources(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// ReadResource asks each domain in turn and returns the first that CLAIMS the URI. A not-found from
// one provider is not the answer — it only means "not mine".
func (c resourceComposite) ReadResource(ctx context.Context, uri string) ([]proto.ResourceContents, error) {
	var lastErr error
	for _, p := range c {
		got, err := p.ReadResource(ctx, uri)
		if err == nil {
			return got, nil
		}
		lastErr = err
	}
	// Every provider disclaimed it: the last refusal is the honest answer, and it is already the
	// right shape (a *RequestError carrying CodeResourceNotFound).
	return nil, lastErr
}

type taskComposite []proto.TaskProvider

func composeTasks(ps []proto.TaskProvider) proto.TaskProvider {
	if len(ps) == 1 {
		return ps[0]
	}
	return taskComposite(ps)
}

func (c taskComposite) Task(taskID string) (proto.TaskView, bool) {
	for _, p := range c {
		if v, ok := p.Task(taskID); ok {
			return v, true
		}
	}
	return proto.TaskView{}, false
}

// CancelTask delivers the cancel to whichever domain owns the id. Ordering is irrelevant because at
// most one can own it — and that is the assumption worth stating, since a cancel delivered to the
// wrong registry would be reported as acknowledged while the real run kept spending.
func (c taskComposite) CancelTask(taskID string) bool {
	for _, p := range c {
		if p.CancelTask(taskID) {
			return true
		}
	}
	return false
}
