package mcpserve

import (
	"context"
	"errors"
	"strings"
	"testing"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// --- fakes that report ownership the way the real providers do ---

type fakeResources struct {
	owns map[string]string // uri -> body
	list []proto.Resource
}

func (f *fakeResources) ListResources(context.Context) ([]proto.Resource, error) { return f.list, nil }

func (f *fakeResources) ReadResource(_ context.Context, uri string) ([]proto.ResourceContents, error) {
	if body, ok := f.owns[uri]; ok {
		return []proto.ResourceContents{{URI: uri, Text: body}}, nil
	}
	return nil, &proto.RequestError{Code: proto.CodeResourceNotFound, Message: "no such resource: " + uri}
}

type fakeTasks struct {
	owns      map[string]bool
	cancelled []string
}

// TaskView carries no id — the id is the lookup key, not part of the view — so the fake echoes it in
// StatusMessage to prove which provider answered.
func (f *fakeTasks) Task(id string) (proto.TaskView, bool) {
	if f.owns[id] {
		return proto.TaskView{Status: "working", StatusMessage: id}, true
	}
	return proto.TaskView{}, false
}

func (f *fakeTasks) CancelTask(id string) bool {
	if f.owns[id] {
		f.cancelled = append(f.cancelled, id)
		return true
	}
	return false
}

// --- the routing contract ---

// ListResources lists both domains' resources.
func TestResources_ListConcatenatesBothDomains(t *testing.T) {
	a := &fakeResources{list: []proto.Resource{{URI: "a1"}, {URI: "a2"}}}
	b := &fakeResources{list: []proto.Resource{{URI: "b1"}}}

	got, err := composeResources([]proto.ResourceProvider{a, b}).ListResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("listed %d resources, want 3 (2 + 1)", len(got))
	}
}

// ReadResource routes by asking each provider, not by parsing the URI.
func TestResources_ReadRoutesByOwnershipNotByParsing(t *testing.T) {
	a := &fakeResources{owns: map[string]string{"review://run/1": "review body"}}
	b := &fakeResources{owns: map[string]string{"anything-at-all": "explore body"}}
	c := composeResources([]proto.ResourceProvider{a, b})

	// Owned by the second provider, with a URI shaped nothing like the first's.
	got, err := c.ReadResource(context.Background(), "anything-at-all")
	if err != nil {
		t.Fatalf("a URI owned by the second provider must be reachable: %v", err)
	}
	if len(got) != 1 || got[0].Text != "explore body" {
		t.Errorf("got %+v, want the second provider's body", got)
	}

	// Owned by neither: the refusal survives, and keeps the not-found code a client branches on.
	_, err = c.ReadResource(context.Background(), "owned-by-nobody")
	if err == nil {
		t.Fatal("an unowned URI must be refused")
	}
	var re *proto.RequestError
	if !errors.As(err, &re) || re.Code != proto.CodeResourceNotFound {
		t.Errorf("the refusal must stay a resource-not-found RequestError, got %#v", err)
	}
}

// A cancel must reach the domain that owns the run, or a running run would keep spending.
func TestTasks_CancelReachesTheOwningDomain(t *testing.T) {
	a := &fakeTasks{owns: map[string]bool{"20260809T010101-abc": true}}
	b := &fakeTasks{owns: map[string]bool{"run-def": true}}
	c := composeTasks([]proto.TaskProvider{a, b})

	if !c.CancelTask("run-def") {
		t.Fatal("a cancel for the second domain's run must be acknowledged")
	}
	if len(b.cancelled) != 1 || b.cancelled[0] != "run-def" {
		t.Errorf("the cancel did not reach the owning domain: %v", b.cancelled)
	}
	if len(a.cancelled) != 0 {
		t.Errorf("the cancel reached the WRONG domain: %v", a.cancelled)
	}

	// An unknown id is not acknowledged.
	if c.CancelTask("never-existed") {
		t.Error("an unknown task id must not be acknowledged")
	}
}

func TestTasks_LookupRoutesToTheOwner(t *testing.T) {
	a := &fakeTasks{owns: map[string]bool{"x": true}}
	b := &fakeTasks{owns: map[string]bool{"y": true}}
	c := composeTasks([]proto.TaskProvider{a, b})

	for _, id := range []string{"x", "y"} {
		if v, ok := c.Task(id); !ok || v.StatusMessage != id {
			t.Errorf("Task(%q) = (%+v, %v), want the owner's view", id, v, ok)
		}
	}
	if _, ok := c.Task("z"); ok {
		t.Error("an unknown id must report not-found rather than an empty view")
	}
}

// A single provider is used directly rather than wrapped.
func TestCompose_SingleProviderIsNotWrapped(t *testing.T) {
	r := &fakeResources{}
	if got := composeResources([]proto.ResourceProvider{r}); got != proto.ResourceProvider(r) {
		t.Error("a lone resource provider should be used directly")
	}
	tp := &fakeTasks{}
	if got := composeTasks([]proto.TaskProvider{tp}); got != proto.TaskProvider(tp) {
		t.Error("a lone task provider should be used directly")
	}
}

// --- --only ---

func TestParseDomain(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Domain
		wantErr bool
	}{
		{"", DomainAll, false},
		{"review", DomainReview, false},
		{"explore", DomainExplore, false},
		{" review ", DomainReview, false},
		{"reviews", "", true},
		{"both", "", true},
	} {
		got, err := ParseDomain(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("--only %q must be refused, not defaulted: a typo that silently served everything "+
					"would hand a caller tools they asked not to have", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseDomain(%q) = (%q, %v), want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestDomain_Serves(t *testing.T) {
	for _, tc := range []struct {
		d               Domain
		review, explore bool
	}{
		{DomainAll, true, true},
		{DomainReview, true, false},
		{DomainExplore, false, true},
	} {
		if tc.d.servesReview() != tc.review || tc.d.servesExplore() != tc.explore {
			t.Errorf("%q serves review=%v explore=%v, want %v/%v",
				tc.d, tc.d.servesReview(), tc.d.servesExplore(), tc.review, tc.explore)
		}
	}
}

// A selected domain that is not configured is a build error.
func TestBuild_RefusesASelectedDomainThatIsNotConfigured(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *Server
	}{
		{"review selected, absent", &Server{Only: DomainReview}},
		{"explore selected, absent", &Server{Only: DomainExplore}},
		{"both selected, both absent", &Server{Only: DomainAll}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.s.build(); err == nil {
				t.Error("a selected domain that is not configured must be an error")
			}
		})
	}
}

// The composed instructions describe only the domains served.
func TestInstructions_DescribeOnlyWhatIsServed(t *testing.T) {
	all := (&Server{Only: DomainAll}).instructions()
	if !contains(all, "review_") || !contains(all, "explore") {
		t.Error("the both-domains instructions must mention both")
	}
	rev := (&Server{Only: DomainReview}).instructions()
	if !contains(rev, "review_") {
		t.Error("review-only instructions must mention review")
	}
	if contains(rev, "explore  —") {
		t.Error("review-only instructions must not advertise the explore tool")
	}
	exp := (&Server{Only: DomainExplore}).instructions()
	if contains(exp, "review_report") {
		t.Error("explore-only instructions must not advertise review tools")
	}
	// Every variant points at the agent guide.
	for _, s := range []string{all, rev, exp} {
		if !contains(s, "agents_md") {
			t.Error("every variant must point at agents_md")
		}
	}
}

func contains(hay, needle string) bool { return strings.Contains(hay, needle) }
