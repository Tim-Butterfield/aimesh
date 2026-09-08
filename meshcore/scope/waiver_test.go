package scope

import (
	"path/filepath"
	"testing"
)

// The waiver's whole value is the LINE it draws. These tests pin both sides of it: what the
// operator can now reach, and what no operator can reach. The second set matters more — a
// waiver that quietly widened to secrets would be indistinguishable from this one in every
// test that only checked the happy path.

func TestAllowProtectedWrites_TheOperatorReachesTheirOwnConfigTrees(t *testing.T) {
	// These are ordinary directories people own. `~/.claude/hooks` is somebody's source tree,
	// and refusing to review it was the tool overriding an explicit instruction.
	for _, rel := range []string{
		".claude/hooks/pre.py",
		".git/hooks/pre-commit",
		".vscode/mcp.json",
		".aimesh/config.yaml",
		".cursor/rules.md",
		"nested/deep/.idea/workspace.xml",
		".mcp.json",
	} {
		if rule := DeniedWriteWith(rel, true); rule != "" {
			t.Errorf("write to %q still refused under the waiver (rule %q)", rel, rule)
		}
		// ...and without the waiver it is still refused, or the flag would be meaningless.
		if rule := DeniedWriteWith(rel, false); rule == "" {
			t.Errorf("write to %q is allowed WITHOUT the waiver — then the waiver grants nothing", rel)
		}
	}
}

func TestAllowProtectedWrites_NeverReachesASecret(t *testing.T) {
	// The waiver is about protecting a WRITE. A secret's danger is the READ: the bytes land in
	// a prompt and the prompt goes to a vendor, and no operator intent recalls them afterwards.
	for _, rel := range []string{
		".env",
		".envrc",
		".env.production",
		".ssh/id_ed25519",
		".ssh/config",
		".aws/credentials",
		".gnupg/secring.gpg",
		".netrc",
		".npmrc",
		".git-credentials",
		"deploy_key",
		"certs/server.pem",
		"secrets/private.key",
		"nested/.env/anything.txt",
	} {
		if rule := DeniedWriteWith(rel, true); rule == "" {
			t.Errorf("SECRET %q became writable under the protected-paths waiver — the waiver must not reach this family", rel)
		}
		if rule := DeniedRead(rel); rule == "" {
			t.Errorf("SECRET %q is readable — a read is what puts it in a prompt", rel)
		}
	}
}

func TestAllowProtectedWrites_DoesNotWidenRootConfinement(t *testing.T) {
	// The waiver changes WHICH NAMES are allowed inside a root. It must not change which roots
	// exist: a resolver is still the only thing that says where a write may land.
	root := t.TempDir()
	r, err := NewWith(Options{AllowProtectedWrites: true}, root)
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")
	if _, err := r.ResolveWrite(outside); err == nil {
		t.Fatal("a path outside every root was admitted — the waiver widened confinement, which it must never do")
	} else if d, ok := AsDenial(err); !ok || d.Reason != ReasonOutsideRoot {
		t.Fatalf("refusal reason = %v, want %v", d, ReasonOutsideRoot)
	}
	// Inside the root, the protected name is now reachable.
	if _, err := r.ResolveWrite(filepath.Join(root, ".claude", "hooks", "x.py")); err != nil {
		t.Fatalf("protected path inside the root still refused under the waiver: %v", err)
	}
	// And a secret inside the very same root is not.
	if _, err := r.ResolveWrite(filepath.Join(root, ".ssh", "id_rsa")); err == nil {
		t.Fatal("a secret inside the root was admitted under the waiver")
	}
}

func TestNew_IsStrict_TheZeroOptionsAreTheSafeOnes(t *testing.T) {
	// A caller that never heard of Options must get the old behavior. This is the property that
	// makes the waiver opt-in rather than a default that happens to be off today.
	root := t.TempDir()
	r, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.ResolveWrite(filepath.Join(root, ".git", "config")); err == nil {
		t.Fatal("scope.New admitted a protected write — the strict constructor is not strict")
	}
}
