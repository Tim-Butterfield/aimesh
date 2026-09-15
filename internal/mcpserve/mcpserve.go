// Package mcpserve composes the review and explore MCP servers into one server, `aimesh mcp`.
//
// Tool names are domain-prefixed, so tools need no routing. The ID-addressed methods
// (resources/read, tasks/get and tasks/cancel) carry no tool name, so the composites route them by
// asking each domain whether it owns the identifier instead of parsing the identifier's format. This
// relies on at most one domain owning any identifier.
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
	// DomainAll serves both domains. It is the default.
	DomainAll Domain = ""
	// DomainReview serves only the review tools.
	DomainReview Domain = "review"
	// DomainExplore serves only the explore tools.
	DomainExplore Domain = "explore"
)

// ParseDomain validates an --only value. An unrecognized value is refused rather than defaulted.
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

	// Review and Explore are the configured domain servers. A selected domain must be non-nil.
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

// build assembles the protocol core with the selected domains attached.
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

	// agents_md is registered once here. Domain servers attached to a shared core skip it, because
	// proto.Server panics on a duplicate tool name.
	registerAgentGuide(core)

	core.Resources = composeResources(resources)
	core.Tasks = composeTasks(tasks)
	return core, nil
}

// instructions returns the composed server's instructions, describing only the domains served.
func (s *Server) instructions() string {
	var b strings.Builder
	b.WriteString("aimesh runs provider-diverse, governed AI reviews and explorations.\n\n")
	b.WriteString("Tools are named by domain. Call agents_md FIRST for the full contract — the rules that are\n")
	b.WriteString("expensive to get wrong (identity is recorded but never acted on, the containment copy, the\n")
	b.WriteString("write denylist, apply semantics) are stated there and nowhere shorter.\n\n")
	if s.Only.servesReview() {
		b.WriteString("review_* — review a workspace. review_report changes no project content. review_remediate\n")
		b.WriteString("supplies the diff (output=patch) on every server, and applies it (output=apply) only when the\n")
		b.WriteString("operator launched with --allow-writes; review_doctor reports which.\n")
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
			DestructiveHint: new(false),
			IdempotentHint:  true,
			OpenWorldHint:   new(false),
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

// ReadResource returns the contents from the first domain that owns the URI. A not-found error from
// one provider only means the URI belongs to another.
func (c resourceComposite) ReadResource(ctx context.Context, uri string) ([]proto.ResourceContents, error) {
	var lastErr error
	for _, p := range c {
		got, err := p.ReadResource(ctx, uri)
		if err == nil {
			return got, nil
		}
		lastErr = err
	}
	// No provider owns the URI; return the last not-found error unchanged.
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

// CancelTask delivers the cancel to the domain that owns the id and reports whether one did.
func (c taskComposite) CancelTask(taskID string) bool {
	for _, p := range c {
		if p.CancelTask(taskID) {
			return true
		}
	}
	return false
}
