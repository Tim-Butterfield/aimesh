package evidence

// Tests for the export DESTINATION GUARD (dest.go). Every one of them describes a way the previous,
// unguarded `os.Remove(dbPath) + rewrite` destroyed a file it was never asked to touch:
//
//	a protected path (~/.aimesh/**, .git/**, .env*, key material) was deleted and replaced
//	an existing, unrelated file was replaced with no confirmation at all
//	a FAILED export had already removed the previous database before it failed
//	a symlink was followed, and an empty directory was simply removed
//
// They run on deterministic in-process fakes, and every path is under t.TempDir(): the denylist matches
// on path COMPONENTS, so a `.aimesh` inside a temp directory is refused exactly as the real one is, and
// no test needs to go anywhere near the developer's home.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// sentinel writes a recognizable file and returns its path — the "whatever was there before" every one of
// these tests checks is still there afterwards.
func sentinel(t *testing.T, path string) []byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := []byte("original contents of " + filepath.Base(path) + " — must survive\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return body
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestExport_ProtectedDestinationIsRefusedWithTheRule is the rule this whole file exists for.
// `aimesh explore export --sqlite ~/.aimesh/adapters.yaml` must never DELETE the shared adapter configuration —
// the exact file the non-overridable write denylist protects everywhere else in the repo — and replace it
// with a SQLite database. The destination goes through meshcore/scope, the denylist applies
// INSIDE the root the human's own consent established, and the refusal is a typed containment halt
// (exit 6, class M6) carrying the resolver's machine reason and naming the rule that refused.
func TestExport_ProtectedDestinationIsRefusedWithTheRule(t *testing.T) {
	dir := capturedRun(t, shortlistTask())
	base := t.TempDir()

	for _, tc := range []struct {
		name string
		rel  string
		rule string // a fragment of the denylist rule the message must name
	}{
		{"shared adapter config", filepath.Join(".aimesh", "adapters.yaml"), ".aimesh"},
		{"per-app state dir", filepath.Join(".exploremesh", "profiles.yaml"), ".exploremesh"},
		{"git config", filepath.Join(".git", "config"), ".git"},
		{"agent client config", ".mcp.json", ".mcp.json"},
		{"dotenv", ".env.local", ".env*"},
		{"key material", filepath.Join("keys", "id_ed25519"), "key material"},
		{"ide config", filepath.Join(".vscode", "mcp.json"), ".vscode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(base, tc.name, tc.rel)
			want := sentinel(t, dest)

			// --force is passed deliberately: the point is that the strongest opt-in the CLI offers does
			// NOT lift the denylist.
			_, err := Export(dir, dest, Options{Force: true})
			if err == nil {
				t.Fatalf("export to the protected path %s must be REFUSED", dest)
			}
			if got := fault.CodeOf(err); got != fault.Containment {
				t.Errorf("exit code = %d, want %d (containment)", got, int(fault.Containment))
			}
			if got := fault.ReasonOf(err); got != string(scope.ReasonWriteDenied) {
				t.Errorf("machine reason = %q, want %q", got, scope.ReasonWriteDenied)
			}
			if !strings.Contains(err.Error(), tc.rule) {
				t.Errorf("the refusal must NAME the rule %q, got: %v", tc.rule, err)
			}
			if got := mustReadFile(t, dest); string(got) != string(want) {
				t.Fatalf("the protected file was MODIFIED by a refused export:\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// TestExport_ExistingDestinationNeedsForce pins the no-clobber rule in both directions: without the opt-in
// the original bytes are still there and the message names the flag; with it, the export really does
// replace the file. The evidence database is derived and a rebuild is a rebuild — but the FILE is the
// user's, and a mistyped path must not be how they find that out.
func TestExport_ExistingDestinationNeedsForce(t *testing.T) {
	dir := capturedRun(t, shortlistTask())
	dest := filepath.Join(t.TempDir(), "evidence.db")
	want := sentinel(t, dest)

	_, err := Export(dir, dest, Options{})
	if err == nil {
		t.Fatal("an existing destination must be refused without the opt-in")
	}
	if got := fault.CodeOf(err); got != fault.Policy {
		t.Errorf("exit code = %d, want %d (policy)", got, int(fault.Policy))
	}
	if got := fault.ReasonOf(err); got != ReasonDestinationExists {
		t.Errorf("machine reason = %q, want %q", got, ReasonDestinationExists)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal must TEACH the flag that allows it, got: %v", err)
	}
	if got := mustReadFile(t, dest); string(got) != string(want) {
		t.Fatalf("a refused export modified the destination:\n got: %q\nwant: %q", got, want)
	}

	// With the opt-in the same call replaces the file with a real, readable evidence database.
	sum, ferr := Export(dir, dest, Options{Force: true})
	if ferr != nil {
		t.Fatalf("export --force: %v", ferr)
	}
	if sum.DBPath != dest {
		t.Errorf("the summary must report the path the caller named, got %q", sum.DBPath)
	}
	db := openDB(t, dest)
	if got := scalar[int](t, db, "PRAGMA user_version"); got != ExportSchemaVersion {
		t.Errorf("--force did not produce a valid evidence database (user_version %d)", got)
	}
}

// TestExport_SidecarCollisionAlsoNeedsForce covers the half of the destination a user never thinks about:
// SQLite's `-journal`/`-wal`/`-shm` files. The old code removed the whole set unconditionally, so a stale
// journal from an unrelated database vanished silently; they are now part of the destination for the
// no-clobber check too, and the refusal names the file actually in the way.
func TestExport_SidecarCollisionAlsoNeedsForce(t *testing.T) {
	dir := capturedRun(t, shortlistTask())
	dest := filepath.Join(t.TempDir(), "evidence.db")
	want := sentinel(t, dest+"-wal")

	if _, err := Export(dir, dest, Options{}); err == nil {
		t.Fatal("a sidecar collision must be refused without the opt-in")
	} else if !strings.Contains(err.Error(), "evidence.db-wal") {
		t.Errorf("the refusal must name the file in the way, got: %v", err)
	}
	if got := mustReadFile(t, dest+"-wal"); string(got) != string(want) {
		t.Fatal("a refused export removed a sidecar it had no consent to touch")
	}
}

// TestExport_FailedExportLeavesThePreviousDatabaseIntact is the ATOMICITY contract. The old code removed
// the destination FIRST and only then started building, so any failure after that point — an interrupted
// process, a full disk, or, as here, the governance invariant refusing a doctored run — left the user with
// no database at all where a perfectly good one had been. The build now happens in a temporary file beside
// the destination and the destination changes only at the rename.
func TestExport_FailedExportLeavesThePreviousDatabaseIntact(t *testing.T) {
	good := capturedRun(t, shortlistTask())
	dest := filepath.Join(t.TempDir(), "evidence.db")
	if _, err := Export(good, dest, Options{}); err != nil {
		t.Fatalf("baseline export: %v", err)
	}
	before := mustReadFile(t, dest)
	digestBefore, err := FileDigest(dest)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	// The injected failure is a REAL one, not a hook: a run directory whose ballot names a canonical ID
	// absent from the confirmed revision is refused by the foreign keys — which happens after the run has
	// been read and after the temporary database has been created and partly written. That is precisely
	// the window the old code left the destination deleted in.
	broken := doctoredRun(t)
	if _, eerr := Export(broken, dest, Options{Force: true}); eerr == nil {
		t.Fatal("the doctored run must fail the export")
	}

	after, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("the previous database is GONE after a failed export: %v", rerr)
	}
	if string(after) != string(before) {
		t.Error("a failed export replaced the previous database with something else")
	}
	digestAfter, err := FileDigest(dest)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if digestAfter != digestBefore {
		t.Errorf("the previous database changed: %s → %s", digestBefore[:12], digestAfter[:12])
	}
	// And no debris: the temporary file is removed on every failure path.
	entries, derr := os.ReadDir(filepath.Dir(dest))
	if derr != nil {
		t.Fatalf("read dir: %v", derr)
	}
	for _, e := range entries {
		if e.Name() != "evidence.db" {
			t.Errorf("a failed export left %q behind", e.Name())
		}
	}
}

// TestExport_NonRegularDestinationIsRefused: a directory, a symlink or any other non-plain-file
// destination is refused rather than followed. The symlink case is the one the scope check alone cannot
// catch — canonicalization RESOLVES a final symlink, so the resolver would happily approve a link pointing
// at a file the user never named — and the directory case is the one the old code silently DELETED, since
// `os.Remove` succeeds on an empty directory.
func TestExport_NonRegularDestinationIsRefused(t *testing.T) {
	dir := capturedRun(t, shortlistTask())

	t.Run("directory", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "evidence.db")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, err := Export(dir, dest, Options{Force: true})
		if err == nil {
			t.Fatal("a directory destination must be refused")
		}
		if got := fault.ReasonOf(err); got != ReasonDestinationNotRegular {
			t.Errorf("machine reason = %q, want %q", got, ReasonDestinationNotRegular)
		}
		if got := fault.CodeOf(err); got != fault.Containment {
			t.Errorf("exit code = %d, want %d (containment)", got, int(fault.Containment))
		}
		if fi, serr := os.Stat(dest); serr != nil || !fi.IsDir() {
			t.Fatal("the refused export removed the directory")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "someone-elses-file.txt")
		want := sentinel(t, target)
		dest := filepath.Join(base, "evidence.db")
		if err := os.Symlink(target, dest); err != nil {
			t.Skipf("this platform cannot create symlinks: %v", err)
		}
		_, err := Export(dir, dest, Options{Force: true})
		if err == nil {
			t.Fatal("a symlink destination must be refused, not followed")
		}
		if got := fault.ReasonOf(err); got != ReasonDestinationNotRegular {
			t.Errorf("machine reason = %q, want %q", got, ReasonDestinationNotRegular)
		}
		if !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("the refusal must say what the destination is, got: %v", err)
		}
		if got := mustReadFile(t, target); string(got) != string(want) {
			t.Fatal("the export followed the symlink and overwrote its target")
		}
	})
}

// TestExport_FreshDestinationStillJustWorks is the guard's other half: none of the above may turn the
// ordinary case — a path that does not exist yet, in a directory that does not exist yet — into a chore.
func TestExport_FreshDestinationStillJustWorks(t *testing.T) {
	dir := capturedRun(t, shortlistTask())
	dest := filepath.Join(t.TempDir(), "nested", "deeper", "evidence.db")
	if _, err := Export(dir, dest, Options{}); err != nil {
		t.Fatalf("export to a fresh nested path: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the export wrote no database: %v", err)
	}
}

// doctoredRun captures a shortlist run and then tampers with its decision record so a ballot names a
// canonical ID that is absent from the confirmed revision — the §9 invariant, and a failure that lands
// AFTER the export has started writing.
func doctoredRun(t *testing.T) string {
	t.Helper()
	dir := capturedRun(t, shortlistTask())
	path := filepath.Join(dir, "decision.json")
	b := mustReadFile(t, path)
	var decision map[string]any
	if err := json.Unmarshal(b, &decision); err != nil {
		t.Fatalf("parse decision.json: %v", err)
	}
	ballots, _ := decision["ballots"].([]any)
	if len(ballots) == 0 {
		t.Fatal("the shortlist run recorded no ballot to doctor")
	}
	first, _ := ballots[0].(map[string]any)
	ranking, _ := first["ranking"].([]any)
	first["ranking"] = append(ranking, "canon-this-candidate-was-never-confirmed")
	doctored, err := json.MarshalIndent(decision, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, doctored, 0o644); err != nil {
		t.Fatalf("write doctored decision.json: %v", err)
	}
	return dir
}
