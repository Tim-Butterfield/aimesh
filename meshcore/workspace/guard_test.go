package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// TestApplyEdit_GuardRefusesProtectedPath pins that the write guard is consulted on the
// LIVE destination (not the temp copy) and refuses a protected path even though the path
// is perfectly writable inside the copy.
func TestApplyEdit_GuardRefusesProtectedPath(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, ".env"), "SECRET=1\n")
	writeFile(t, filepath.Join(live, "ok.txt"), "hello\n")

	res, err := scope.New(live)
	if err != nil {
		t.Fatal(err)
	}
	ws := New(t.TempDir())
	ws.Guard = res

	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	// `.env` was never copied (read-denied), so materialize it in the copy to prove the
	// guard — not the missing file — is what refuses.
	writeFile(t, h.Abs(".env"), "SECRET=1\n")

	err = ws.ApplyEdit(h, core.Edit{File: ".env", Replacement: "X\n"})
	if err == nil {
		t.Fatal("ApplyEdit to .env must be refused")
	}
	if got := scope.ReasonOf(err); got != scope.ReasonWriteDenied {
		t.Errorf("reason = %q, want %q (err: %v)", got, scope.ReasonWriteDenied, err)
	}
	// A normal path is unaffected.
	if err := ws.ApplyEdit(h, core.Edit{File: "ok.txt", Replacement: "X\n"}); err != nil {
		t.Errorf("ApplyEdit to an ordinary path = %v, want allowed", err)
	}
}

// TestCommit_GuardRefusesWholeCommit pins the all-or-nothing rule: when any changed path
// is refused, NOTHING is written — a partially-applied remediation is worse than none.
//
// The refused path is `.env` rather than `.mcp.json`: client-config files are now
// excluded from the copy/commit SET outright (see TestCopy_ExcludesAgentAndIDEConfig), so
// they never reach the guard at all. `.env` is write-denied by the guard while still
// being an ordinary path as far as the workspace layer is concerned, which is what this
// test needs in order to exercise the guard itself.
func TestCommit_GuardRefusesWholeCommit(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "a\n")

	res, err := scope.New(live)
	if err != nil {
		t.Fatal(err)
	}
	ws := New(t.TempDir())
	ws.Guard = res

	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, h.Abs("a.txt"), "changed\n")
	writeFile(t, h.Abs(".env"), "SECRET=1\n") // never copied; materialized to reach the guard

	committed, err := ws.Commit(h)
	if err == nil {
		t.Fatal("Commit touching .env must be refused")
	}
	if len(committed) != 0 {
		t.Errorf("committed = %v, want nothing written", committed)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "a.txt")); string(got) != "a\n" {
		t.Errorf("the allowed file was written despite the refusal: %q", got)
	}
	if _, err := os.Stat(filepath.Join(live, ".env")); !os.IsNotExist(err) {
		t.Error(".env must not have been created in the live tree")
	}
	if got := scope.ReasonOf(err); got != scope.ReasonWriteDenied {
		t.Errorf("reason = %q, want %q", got, scope.ReasonWriteDenied)
	}
}

// TestCommit_NoGuard_Unchanged pins that an Access without a guard behaves exactly as
// before (the guard is opt-in; existing callers are untouched).
func TestCommit_NoGuard_Unchanged(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "a.txt"), "a\n")
	ws := New(t.TempDir())
	h, err := ws.Copy(live, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, h.Abs("a.txt"), "changed\n")
	committed, err := ws.Commit(h)
	if err != nil || len(committed) != 1 {
		t.Fatalf("Commit = %v, %v; want one committed file", committed, err)
	}
}

// TestCopy_SkipsReadDeniedSecrets pins that secrets never enter the isolated copy — the
// copy is what a model is pointed at, so exclusion by construction beats prompt filtering.
func TestCopy_SkipsReadDeniedSecrets(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "main.go"), "package main\n")
	writeFile(t, filepath.Join(live, ".env"), "SECRET=1\n")
	writeFile(t, filepath.Join(live, ".env.production"), "SECRET=2\n")
	writeFile(t, filepath.Join(live, "keys", "id_rsa"), "PRIVATE\n")
	writeFile(t, filepath.Join(live, "certs", "tls.pem"), "PEM\n")
	writeFile(t, filepath.Join(live, "environment.go"), "package main\n") // near-miss: kept

	ws := New(t.TempDir())
	h, err := ws.Copy(live, true, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Cleanup(h)

	for _, rel := range []string{".env", ".env.production", "keys/id_rsa", "certs/tls.pem"} {
		if _, err := os.Stat(h.Abs(filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s was copied into the isolated copy; it must never be", rel)
		}
	}
	for _, rel := range []string{"main.go", "environment.go"} {
		if _, err := os.Stat(h.Abs(rel)); err != nil {
			t.Errorf("%s must still be copied: %v", rel, err)
		}
	}
}

// TestCollectSnippets_SkipsReadDeniedSecrets pins the same rule at the prompt-assembly
// layer, for a caller that collects from a directory it assembled itself.
func TestCollectSnippets_SkipsReadDeniedSecrets(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, ".env"), "SECRET=abc123\n")
	writeFile(t, filepath.Join(root, "deploy", ".env.staging"), "SECRET=def\n")
	writeFile(t, filepath.Join(root, "id_ed25519"), "PRIVATE\n")

	for _, s := range CollectSnippets(root) {
		if strings.Contains(s.Content, "SECRET=") || strings.Contains(s.Content, "PRIVATE") {
			t.Errorf("secret content reached a snippet via %q", s.Path)
		}
		if scope.DeniedRead(s.Path) != "" {
			t.Errorf("read-denied path %q was collected", s.Path)
		}
	}
}
