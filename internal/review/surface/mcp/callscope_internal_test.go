package mcp

import (
	"os"
	"path/filepath"
	"testing"

	proto "github.com/Tim-Butterfield/aimesh/meshcore/mcp"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

func mkdir(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func legacyEnv() *proto.RequestEnv {
	return &proto.RequestEnv{Method: "tools/call", Era: proto.EraLegacy}
}

// Each call's scope is built from the paths THAT call declares, so two calls never share one.
func TestCallScope_IsBuiltFromTheCallsOwnPaths(t *testing.T) {
	parent := t.TempDir()
	a, b := mkdir(t, parent, "a"), mkdir(t, parent, "b")
	s := &Server{}

	envA, res, err := s.callScope(legacyEnv(), a, nil)
	if err != nil || res != nil {
		t.Fatalf("callScope(a): %v %+v", err, res)
	}
	if _, derr := envA.Trust.ResolveRead(a); derr != nil {
		t.Fatalf("a call must read its own workspace: %v", derr)
	}
	if _, derr := envA.Trust.ResolveRead(b); derr == nil {
		t.Fatal("a call scoped to a must not read b")
	}
	envB, _, _ := s.callScope(legacyEnv(), b, []string{a})
	if len(envB.Roots) != 2 {
		t.Fatalf("roots = %v, want the workspace plus the extra root", envB.Roots)
	}
}

func TestCallScope_ARelativePathIsInvalidParams(t *testing.T) {
	s := &Server{}
	if _, _, err := s.callScope(legacyEnv(), "project", nil); err == nil {
		t.Fatal("a relative workspace must be refused as invalid params")
	}
	if _, _, err := s.callScope(legacyEnv(), t.TempDir(), []string{" "}); err == nil {
		t.Fatal("a blank root must be refused as invalid params")
	}
}

func TestCallScope_TheCeilingBoundsEveryPath(t *testing.T) {
	ceiling, outside := t.TempDir(), t.TempDir()
	inside := mkdir(t, ceiling, "project")
	s := &Server{Ceiling: []string{ceiling}}

	if _, res, err := s.callScope(legacyEnv(), inside, nil); err != nil || res != nil {
		t.Fatalf("a path inside the ceiling must be admitted: %v %+v", err, res)
	}
	if _, res, err := s.callScope(legacyEnv(), inside, []string{outside}); err != nil || res == nil || !res.IsError {
		t.Fatalf("an extra root outside the ceiling must be a domain refusal, got err=%v res=%+v", err, res)
	}
}

func TestCallScope_ClientRootsOnlyNarrow(t *testing.T) {
	parent := t.TempDir()
	sub, other := mkdir(t, parent, "sub"), mkdir(t, parent, "other")
	s := &Server{ClientRoots: []string{sub}}

	env, res, err := s.callScope(legacyEnv(), sub, nil)
	if err != nil || res != nil {
		t.Fatalf("a path inside the client's roots must be admitted: %v %+v", err, res)
	}
	if !env.Narrowed {
		t.Error("a call judged against client roots must record that it was narrowed")
	}
	if _, res, _ := s.callScope(legacyEnv(), other, nil); res == nil || !res.IsError {
		t.Fatal("a path outside the client's roots must be refused")
	}
	if _, res, _ := s.callScope(legacyEnv(), parent, nil); res == nil || !res.IsError {
		t.Fatal("a client root can never widen a call to its parent")
	}
}

// Inline content declares no path, so its scope admits no filesystem path at all.
func TestCallScope_NoPathAdmitsNothing(t *testing.T) {
	s := &Server{}
	env, res, err := s.callScope(legacyEnv(), "", nil)
	if err != nil || res != nil {
		t.Fatalf("callScope with no path: %v %+v", err, res)
	}
	if _, derr := env.Trust.ResolveRead(t.TempDir()); scope.ReasonOf(derr) != scope.ReasonNoRoots {
		t.Fatalf("a call that declares no path must read nothing, got %v", derr)
	}
}
