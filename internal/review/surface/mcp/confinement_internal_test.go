package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

func mkdirUnder(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir
}

// envWithRoots builds the protocol context a request would carry with the given trusted roots.
func envWithRoots(t *testing.T, roots ...string) *proto.RequestEnv {
	t.Helper()
	r, err := scope.New(roots...)
	if err != nil || r == nil {
		t.Fatalf("scope.New(%v): %v", roots, err)
	}
	return &proto.RequestEnv{Method: "tools/call", Era: proto.EraLegacy, Roots: r.Roots(), Trust: r, RootSource: proto.RootsExplicit}
}

// The modern era's fail-closed rule, tested from INSIDE the package because the branch it guards is
// not reachable from the wire yet: no modern protocol version is accepted, so envFor hard-codes the
// legacy era. The confinement fix must land BEFORE the widening it prevents becomes reachable, which
// means it has to be provable before there is a wire to prove it on.
//
// AGAINST THE OLD CODE these do not compile: trustFor took no era, Server had no RootSource and no
// AllowInferredRoot, and there was no waiver to assert. That is stated plainly rather than dressed up
// as a behavioral failure — a test that cannot compile has not observed old behavior.

// TestAnInferredCwdBehavesTheSameOnBothEras.
//
// This replaces an era-conditional refusal: a modern-era inferred cwd used to be discarded here,
// while a legacy one stood. The rule was keyed on the wrong variable. Its stated risk was that the
// newest revision removes the client's ability to narrow the server — but a LEGACY client that
// simply declines the roots capability does not narrow it either, and that case sailed through
// unrefused. It missed the exposure it named, and it fired on a default launch that a new user had
// no way to diagnose.
//
// The question is now asked where it can actually be answered — at the point of inference, which
// adopts a cwd only when it carries a PROJECT MARKER (acp.hasProjectMarker). By the time a root
// reaches this function it has already been judged, so BOTH eras honour it identically.
func TestAnInferredCwdBehavesTheSameOnBothEras(t *testing.T) {
	root := t.TempDir()
	s := &Server{Roots: []string{root}, RootSource: acp.RootsInferredCwd}
	s.rebuildTrust()

	for _, era := range []struct {
		name string
		era  proto.Era
	}{{"legacy", proto.EraLegacy}, {"modern", proto.EraModern}} {
		tc := s.trustFor(era.era)
		if len(tc.Roots) != 1 {
			t.Errorf("%s roots = %v, want the inferred root honoured — the eras must not disagree about a root that already passed the marker test", era.name, tc.Roots)
		}
		if _, err := tc.Resolver.ResolveRead(root); err != nil {
			t.Errorf("%s must resolve inside the inferred root: %v", era.name, err)
		}
		// The PROVENANCE is still reported, on both eras: an operator reading `doctor` should be able
		// to see that nobody typed this root, even though it was accepted.
		if tc.Source != proto.RootsInferredCwd {
			t.Errorf("%s provenance = %q, want it still reported as inferred", era.name, tc.Source)
		}
	}
}

func TestModernEra_AnExplicitRootNeedsNoWaiver(t *testing.T) {
	root := t.TempDir()
	s := &Server{Roots: []string{root}, RootSource: acp.RootsExplicit}
	s.rebuildTrust()

	// The rule is about PROVENANCE, not about the era: authority a human typed on a command line is
	// the same authority on every revision, and fail-closing it would be a self-inflicted outage in
	// the name of a narrowing the operator never asked for.
	modern := s.trustFor(proto.EraModern)
	if len(modern.Roots) != 1 {
		t.Fatalf("an EXPLICIT --root must stand on every era; got %v", modern.Roots)
	}
	if modern.Source != proto.RootsExplicit {
		t.Fatalf("provenance = %q, want explicit", modern.Source)
	}
}

// --- the narrowing-only `roots` argument ---

func TestNarrowRoots_CanOnlyEverShrink(t *testing.T) {
	parent := t.TempDir()
	child := mkdirUnder(t, parent, "project")
	outside := t.TempDir()

	base := envWithRoots(t, parent)

	// A DEEPER directory narrows.
	got, res, err := narrowRoots(base, []string{child})
	if err != nil || res != nil {
		t.Fatalf("narrowing to a subdirectory must succeed: %v %v", err, res)
	}
	// Compared by BEHAVIOUR rather than by string: the effective roots are the resolver's canonical
	// forms, and on a machine whose temp directory is reached through a symlink (macOS) the canonical
	// spelling is not the one the test typed. Asserting the string would be asserting the platform.
	if len(got.Roots) != 1 {
		t.Fatalf("effective roots = %v, want exactly the deeper directory", got.Roots)
	}
	if _, derr := got.Trust.ResolveRead(child); derr != nil {
		t.Fatalf("the narrowed call must still reach the directory it named: %v", derr)
	}
	if !got.Narrowed {
		t.Fatal("a narrowed request must say so — the doctor projection reports it")
	}
	if _, derr := got.Trust.ResolveRead(parent); derr == nil {
		t.Fatal("after narrowing to the child, the parent must no longer resolve")
	}

	// A WIDER directory grants nothing: the intersection keeps the server's own narrower root.
	deep := envWithRoots(t, child)
	got, res, err = narrowRoots(deep, []string{parent})
	if err != nil || res != nil {
		t.Fatalf("naming a wider root is not an error, it is simply not a widening: %v %v", err, res)
	}
	if _, derr := got.Trust.ResolveRead(parent); derr == nil {
		t.Fatal("naming a WIDER root widened the call — intersectRoots must never be a union")
	}

	// A DISJOINT directory is refused, not ignored and not granted.
	_, res, err = narrowRoots(base, []string{outside})
	if err != nil {
		t.Fatalf("a disjoint narrowing is a refusal about the world, not a malformed request: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("a `roots` argument disjoint from the server's roots must REFUSE the call, not fall back to the roots it had")
	}
}

func TestNarrowRoots_RefusesARelativePath(t *testing.T) {
	base := envWithRoots(t, t.TempDir())
	if _, _, err := narrowRoots(base, []string{"project"}); err == nil {
		t.Fatal("a relative `roots` entry must be a teaching error: it has no meaning to a server that does not share the caller's working directory")
	}
}

func TestNarrowRoots_OmittedChangesNothing(t *testing.T) {
	base := envWithRoots(t, t.TempDir())
	got, res, err := narrowRoots(base, nil)
	if err != nil || res != nil {
		t.Fatalf("an omitted `roots` must be a no-op: %v %v", err, res)
	}
	if got != base {
		t.Fatal("an omitted `roots` must return the request's own env untouched, not a copy that could drift from it")
	}
}
