package scope

import (
	"path/filepath"
	"testing"
)

// TestDeniedRules_ApplyToEveryComponent is the core F1 regression: a secret rule that
// only inspects the BASENAME is bypassed by putting the secret one level down. Every one
// of these paths has an innocuous last component and is still a read of a secret.
func TestDeniedRules_ApplyToEveryComponent(t *testing.T) {
	nested := []string{
		"x/.env/secret.txt",
		".env/production",
		"deploy/.env.local/token",
		"a/b/id_rsa/passphrase.txt",
		"vault/server.pem/notes.txt",
		"home/.ssh/config",
		"home/.aws/credentials",
		"home/.gnupg/private-keys-v1.d/key",
	}
	for _, rel := range nested {
		p := filepath.FromSlash(rel)
		if got := DeniedRead(p); got == "" {
			t.Errorf("DeniedRead(%q) = \"\", want a rule (a file INSIDE a secret is a secret)", rel)
		}
		if got := DeniedWrite(p); got == "" {
			t.Errorf("DeniedWrite(%q) = \"\", want a rule", rel)
		}
	}
}

// TestSecretDenylist_CoversDocumentedFamilies pins the families the package documents as
// ".env*" and "key material": the previous rule required a literal ".env." and only knew
// four key patterns, so `.envrc`, `.env-local`, `id_ecdsa` and `deploy_key` were all read
// into prompts while the doc comment claimed otherwise.
func TestSecretDenylist_CoversDocumentedFamilies(t *testing.T) {
	for _, rel := range []string{
		".envrc",
		".env-local",
		".env_production",
		"cfg/.env.staging",
		"keys/id_ecdsa",
		"keys/id_ecdsa.pub",
		"keys/id_dsa",
		"keys/host_rsa",
		"keys/backup_ed25519",
		"ci/deploy_key",
		"ci/deploy_key.pub",
		"certs/client.pfx",
		"certs/putty.ppk",
		".netrc",
		"_netrc",
		".npmrc",
		".pypirc",
		".pgpass",
		".git-credentials",
	} {
		p := filepath.FromSlash(rel)
		if got := DeniedRead(p); got == "" {
			t.Errorf("DeniedRead(%q) = \"\", want a rule", rel)
		}
		if got := DeniedWrite(p); got == "" {
			t.Errorf("DeniedWrite(%q) = \"\", want a rule", rel)
		}
	}
}

// TestWriteDenylist_CoversClientExecutionConfig pins the IDE/agent configuration families
// that can execute code or repoint a tool at another model/server. They are WRITE-denied
// (reading an editor config is not a secret leak; rewriting one is code execution).
func TestWriteDenylist_CoversClientExecutionConfig(t *testing.T) {
	for _, rel := range []string{
		".vscode/mcp.json",
		".vscode/settings.json",
		".windsurf/rules.md",
		".idea/workspace.xml",
		"nested/.vscode/tasks.json",
		"claude_desktop_config.json",
		"cfg/claude_desktop_config.json",
		".codex/config.toml",
		".gemini/settings.json",
	} {
		p := filepath.FromSlash(rel)
		if got := DeniedWrite(p); got == "" {
			t.Errorf("DeniedWrite(%q) = \"\", want a rule", rel)
		}
	}
}

// TestDenylist_TrailingDotSpaceAliasing pins F4: Windows strips trailing spaces and dots
// when it creates a file, so `.git ` and `.git.` both land on the real `.git`. An exact
// component comparison against the untrimmed name walks straight past the rule.
func TestDenylist_TrailingDotSpaceAliasing(t *testing.T) {
	for _, rel := range []string{
		".git /config",
		".git./config",
		"a/.git  /hooks/pre-commit",
		".claude /settings.json",
		".mcp.json ",
	} {
		if got := DeniedWrite(filepath.FromSlash(rel)); got == "" {
			t.Errorf("DeniedWrite(%q) = \"\", want a rule (trailing dot/space aliasing)", rel)
		}
	}
	for _, rel := range []string{".env /x", "keys/id_rsa "} {
		if got := DeniedRead(filepath.FromSlash(rel)); got == "" {
			t.Errorf("DeniedRead(%q) = \"\", want a rule (trailing dot/space aliasing)", rel)
		}
	}
	// `.` and `..` must survive the trimming unharmed — they are traversal, not names.
	if got := DeniedWrite(filepath.FromSlash("./a/../b/main.go")); got != "" {
		t.Errorf("DeniedWrite(./a/../b/main.go) = %q, want allowed", got)
	}
}

// TestDenylist_NTFSStreamAliasing pins H7: on NTFS every file has a default data stream, and
// `.git::$DATA` (or `.git:x:$DATA`) resolves to the very same `.git` directory. Trailing
// dot/space trimming does not touch that shape, so an exact component lookup of the
// untrimmed name matches no rule while the write lands on the real target.
//
// The trim is deliberately Windows-only, so this test states BOTH platforms' expectations
// by toggling the gate: on unix `:` is an ordinary filename character, and stripping the
// suffix there would LOOSEN the denylist (a real file named `report:.pem` would stop
// matching the `*.pem` key-material rule).
func TestDenylist_NTFSStreamAliasing(t *testing.T) {
	aliased := []string{
		`.git::$DATA/config`,
		`.git:hidden:$DATA/hooks/pre-commit`,
		`a/.claude::$DATA/settings.json`,
		`.vscode::$DATA/mcp.json`,
		`.mcp.json::$DATA`,
	}
	aliasedSecrets := []string{`.env::$DATA/x`, `keys/id_rsa::$DATA`}

	t.Run("windows semantics", func(t *testing.T) {
		defer restoreStreamAliasing(streamAliasing)
		streamAliasing = true
		for _, rel := range aliased {
			if got := DeniedWrite(filepath.FromSlash(rel)); got == "" {
				t.Errorf("DeniedWrite(%q) = \"\", want a rule (NTFS stream aliasing)", rel)
			}
		}
		for _, rel := range aliasedSecrets {
			if got := DeniedRead(filepath.FromSlash(rel)); got == "" {
				t.Errorf("DeniedRead(%q) = \"\", want a rule (NTFS stream aliasing)", rel)
			}
		}
		// A drive specifier normalizes to the empty component and is simply skipped.
		if got := DeniedWrite(`C:\src\main.go`); got != "" {
			t.Errorf("DeniedWrite(C:\\src\\main.go) = %q, want allowed", got)
		}
	})

	t.Run("unix semantics", func(t *testing.T) {
		defer restoreStreamAliasing(streamAliasing)
		streamAliasing = false
		// `:` is an ordinary character here: the literal name is NOT the denied name, and
		// pretending otherwise would only widen what the rules let through elsewhere.
		if got := DeniedWrite(filepath.FromSlash(`.git::$DATA/config`)); got != "" {
			t.Errorf("DeniedWrite = %q on unix semantics, want allowed (a literal colon name)", got)
		}
		// The loosening this gate prevents: with the suffix stripped, this stops matching.
		if got := DeniedRead("report:.pem"); got == "" {
			t.Error(`DeniedRead("report:.pem") = "", want the key-material rule on unix semantics`)
		}
	})
}

func restoreStreamAliasing(v bool) { streamAliasing = v }

// TestDenylist_NearMissesStillAllowed guards the other direction: component matching must
// not swallow ordinary source trees. A denylist that refuses everything is not a
// confinement layer, it is an outage.
func TestDenylist_NearMissesStillAllowed(t *testing.T) {
	for _, rel := range []string{
		"internal/environment/config.go",
		"env/config.go",
		"pkg/keys/registry.go",
		"docs/keys.md",
		"cmd/ssh/main.go",
		"internal/credentials/provider.go", // a `credentials` PACKAGE is not `~/.aws/credentials`
		"testdata/known_hosts",
		"a.pemx",
		"gitconfig",
		"keyboard.go",
		"web/dist/app.js",
	} {
		p := filepath.FromSlash(rel)
		if got := DeniedRead(p); got != "" {
			t.Errorf("DeniedRead(%q) = %q, want allowed", rel, got)
		}
		if got := DeniedWrite(p); got != "" {
			t.Errorf("DeniedWrite(%q) = %q, want allowed", rel, got)
		}
	}
}

// TestResolve_NestedSecretDirRefused exercises the same hole through the RESOLVER, on a
// real filesystem: a file inside a `.env` directory sits happily inside the allowed root
// and must still be refused for read and write.
func TestResolve_NestedSecretDirRefused(t *testing.T) {
	root := mkRoot(t)
	write(t, filepath.Join(root, "x", ".env", "secret.txt"), "SECRET=1\n")
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "x", ".env", "secret.txt")
	if _, err := r.ResolveRead(p); ReasonOf(err) != ReasonReadDenied {
		t.Errorf("ResolveRead reason = %q, want %q", ReasonOf(err), ReasonReadDenied)
	}
	if _, err := r.ResolveWrite(p); ReasonOf(err) != ReasonWriteDenied {
		t.Errorf("ResolveWrite reason = %q, want %q", ReasonOf(err), ReasonWriteDenied)
	}
}

// TestResolve_DeniedRootDeniesItsChildren pins F2 at the resolver: when the ROOT a caller
// consented to is itself a protected directory, its children are not innocent because
// their own names are innocent — the whole subtree is the secret.
func TestResolve_DeniedRootDeniesItsChildren(t *testing.T) {
	parent := mkRoot(t)
	root := filepath.Join(parent, ".env")
	write(t, filepath.Join(root, "production"), "SECRET=1\n")
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "production")} {
		if _, err := r.ResolveRead(p); ReasonOf(err) != ReasonReadDenied {
			t.Errorf("ResolveRead(%q) reason = %q, want %q", p, ReasonOf(err), ReasonReadDenied)
		}
	}
}
