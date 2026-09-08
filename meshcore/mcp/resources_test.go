package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/mcp"
)

// These tests cover `resources/*` — the half of the protocol that makes a governed write COLLECTABLE.
//
// The failure they close is concrete: a remediation completed, wrote a patch, and returned its
// run-record-relative name plus a digest. The run directory is deliberately withheld, stdout is the
// JSON-RPC transport, and `resources/*` did not exist — so nothing the caller held could be turned into
// the bytes it had just paid for.
//
// The invariant that runs through all of them: a URI is TWO OPAQUE IDENTIFIERS. Not a path, not a
// filename, not a hint of a layout.

// artifactFixture writes a file into a fresh directory and returns (dir, rel).
func artifactFixture(t *testing.T, rel, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return dir, rel
}

func TestResources_NotDeclaredWithoutAProvider(t *testing.T) {
	c, stop := serve(t, newServer())
	defer stop()
	init := handshake(t, c)
	caps, _ := init.Result["capabilities"].(map[string]any)
	if _, declared := caps["resources"]; declared {
		t.Fatalf("a server with no provider must NOT declare the resources capability: %+v", caps)
	}
	resp, _ := c.call(t, "resources/list", nil)
	if resp.Error == nil || resp.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("resources/list without a provider = %+v, want -32601", resp)
	}
}

func TestResources_ListAndReadDeliverThePublishedArtifact(t *testing.T) {
	const patch = "--- a/sample.go\n+++ b/sample.go\n@@\n-old\n+new\n"
	dir, rel := artifactFixture(t, "patches/changes.patch", patch)
	var store mcp.RunStore
	uri := store.Publish("run-abc", mcp.Artifact{
		Name: "patch", Title: "Remediation patch", MimeType: "text/x-diff", Dir: dir, Rel: rel,
	})
	if uri == "" {
		t.Fatal("Publish returned no URI for a real file")
	}

	c, stop := serve(t, newServer(func(s *mcp.Server) { s.Resources = &store }))
	defer stop()
	init := handshake(t, c)
	caps, _ := init.Result["capabilities"].(map[string]any)
	if _, declared := caps["resources"]; !declared {
		t.Fatalf("a server WITH a provider must declare the resources capability: %+v", caps)
	}

	list, _ := c.call(t, "resources/list", map[string]any{})
	if list.Error != nil {
		t.Fatalf("resources/list: %+v", list.Error)
	}
	rows, _ := list.Result["resources"].([]any)
	if len(rows) != 1 {
		t.Fatalf("resources/list returned %d row(s), want 1", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if row["uri"] != uri {
		t.Fatalf("listed uri = %v, want %q", row["uri"], uri)
	}

	read, _ := c.call(t, "resources/read", map[string]any{"uri": uri})
	if read.Error != nil {
		t.Fatalf("resources/read: %+v", read.Error)
	}
	contents, _ := read.Result["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("resources/read returned %d block(s), want 1", len(contents))
	}
	block, _ := contents[0].(map[string]any)
	if block["text"] != patch {
		t.Fatalf("resources/read returned %q, want the artifact verbatim — a link that resolves to something else is worse than no link", block["text"])
	}
}

// The URI is the whole point of the design decision this preserves: no host path on the wire, in EITHER
// direction. The assertion is deliberately over the RAW FRAMES rather than over parsed fields, because a
// path leaking through a description or a name would be just as much a leak.
func TestResources_URIsAndFramesCarryNoHostPath(t *testing.T) {
	dir, rel := artifactFixture(t, "patches/changes.patch", "diff\n")
	var store mcp.RunStore
	uri := store.Publish("run-abc", mcp.Artifact{Name: "patch", Dir: dir, Rel: rel, MimeType: "text/x-diff"})

	if strings.Contains(uri, dir) || strings.Contains(uri, "patches") || strings.Contains(uri, ".patch") {
		t.Fatalf("resource URI %q discloses the artifact's location or file name", uri)
	}
	if got, want := uri, "aimesh://run/run-abc/patch"; got != want {
		t.Fatalf("resource URI = %q, want the opaque run-scoped form %q", got, want)
	}
	runID, name, ok := mcp.ParseResourceURI(uri)
	if !ok || runID != "run-abc" || name != "patch" {
		t.Fatalf("ParseResourceURI(%q) = (%q, %q, %v)", uri, runID, name, ok)
	}

	c, stop := serve(t, newServer(func(s *mcp.Server) { s.Resources = &store }))
	defer stop()
	handshake(t, c)
	for _, call := range []struct {
		method string
		params any
	}{
		{"resources/list", map[string]any{}},
		{"resources/read", map[string]any{"uri": uri}},
	} {
		resp, _ := c.call(t, call.method, call.params)
		b, _ := json.Marshal(resp.Result)
		if strings.Contains(string(b), dir) {
			t.Fatalf("%s leaked the host directory %q into the response: %s", call.method, dir, b)
		}
	}
}

// A URI this server did not publish is unaddressable — there is no request shape that reaches a file the
// store never registered.
func TestResources_ReadRefusesAnythingItDidNotPublish(t *testing.T) {
	dir, rel := artifactFixture(t, "patches/changes.patch", "diff\n")
	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var store mcp.RunStore
	store.Publish("run-abc", mcp.Artifact{Name: "patch", Dir: dir, Rel: rel})

	c, stop := serve(t, newServer(func(s *mcp.Server) { s.Resources = &store }))
	defer stop()
	handshake(t, c)

	for _, uri := range []string{
		"aimesh://run/run-abc/manifest",        // a name this run never published
		"aimesh://run/run-other/patch",         // another run entirely
		"file://" + filepath.ToSlash(secret),   // a bare host path
		"aimesh://run/run-abc/../../etc/hosts", // a traversal attempt
	} {
		resp, _ := c.call(t, "resources/read", map[string]any{"uri": uri})
		if resp.Error == nil {
			t.Fatalf("resources/read(%q) succeeded; only a run's OWN published artifacts are readable: %+v", uri, resp.Result)
		}
		b, _ := json.Marshal(resp)
		if strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatalf("resources/read(%q) returned content it must never reach", uri)
		}
	}
}

// A run's resources must not outlive the run: a link the server can no longer explain is worse than no
// link, because a client will retry it.
func TestResources_ForgetDropsARunsArtifacts(t *testing.T) {
	dir, rel := artifactFixture(t, "patches/changes.patch", "diff\n")
	var store mcp.RunStore
	uri := store.Publish("run-abc", mcp.Artifact{Name: "patch", Dir: dir, Rel: rel})
	store.Publish("run-def", mcp.Artifact{Name: "patch", Dir: dir, Rel: rel})

	store.Forget("run-abc")
	list, err := store.ListResources(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || strings.Contains(list[0].URI, "run-abc") {
		t.Fatalf("after Forget(run-abc) the store lists %+v", list)
	}
	if _, err := store.ReadResource(context.Background(), uri); err == nil {
		t.Fatal("a forgotten run's artifact is still readable")
	}
}

// The receipt's digest is what a caller trusts. An artifact that no longer matches it is refused rather
// than served as though the receipt still described it.
func TestResources_ReadRefusesAnArtifactThatNoLongerMatchesItsRecordedDigest(t *testing.T) {
	dir, rel := artifactFixture(t, "changes.patch", "the original diff\n")
	var store mcp.RunStore
	uri := store.Publish("run-abc", mcp.Artifact{
		Name: "patch", Dir: dir, Rel: rel,
		SHA256: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	})
	if uri == "" {
		t.Fatal("Publish returned no URI")
	}
	if _, err := store.ReadResource(context.Background(), uri); err == nil {
		t.Fatal("an artifact whose bytes do not match the recorded digest was served anyway")
	} else if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("the refusal must name the digest mismatch, got %v", err)
	}
}

// Publishing something that is not there yields no URI at all, rather than a link that 404s later.
func TestResources_PublishRefusesWhatItCannotAddress(t *testing.T) {
	dir, rel := artifactFixture(t, "changes.patch", "diff\n")
	var store mcp.RunStore
	cases := []struct {
		name string
		a    mcp.Artifact
		run  string
	}{
		{"no run id", mcp.Artifact{Name: "patch", Dir: dir, Rel: rel}, ""},
		{"no artifact name", mcp.Artifact{Dir: dir, Rel: rel}, "run-abc"},
		{"a name with a path in it", mcp.Artifact{Name: "patches/changes", Dir: dir, Rel: rel}, "run-abc"},
		{"a rel that climbs out", mcp.Artifact{Name: "patch", Dir: dir, Rel: "../escape"}, "run-abc"},
		{"an absolute rel", mcp.Artifact{Name: "patch", Dir: dir, Rel: filepath.Join(dir, rel)}, "run-abc"},
		{"a file that is not there", mcp.Artifact{Name: "patch", Dir: dir, Rel: "missing.patch"}, "run-abc"},
	}
	for _, tc := range cases {
		if uri := store.Publish(tc.run, tc.a); uri != "" {
			t.Errorf("%s: Publish returned %q; it must publish nothing", tc.name, uri)
		}
	}
}

// --- content-addressed identity (migration design §7.4) ---
//
// Under the modern era a run artifact is ADVERTISED at a URI carrying its content digest, so the digest
// becomes part of the cache key rather than a freshness hint the client is free to ignore. These tests
// cover the READ side, which is era-neutral by design: the digest-bearing form must resolve, the bare
// form must keep resolving (nothing already published is stranded), and a digest this store no longer
// holds must be refused rather than answered with the current bytes under the old identity.

func TestResources_ADigestBearingURIResolvesToTheSameArtifact(t *testing.T) {
	const patch = "--- a/x\n+++ b/x\n"
	dir, rel := artifactFixture(t, "changes.patch", patch)
	sum := sha256.Sum256([]byte(patch))
	digest := "sha256:" + hex.EncodeToString(sum[:])

	var store mcp.RunStore
	uri := store.Publish("run-abc", mcp.Artifact{Name: "patch", Dir: dir, Rel: rel, SHA256: digest})
	if uri == "" {
		t.Fatal("Publish returned no URI")
	}
	addressed := uri + "?sha256=" + strings.TrimPrefix(digest, "sha256:")

	got, err := store.ReadResource(context.Background(), addressed)
	if err != nil {
		t.Fatalf("a digest-bearing URI must resolve: %v", err)
	}
	if len(got) != 1 || got[0].Text != patch {
		t.Fatalf("digest-bearing read returned %+v", got)
	}
	// The echoed URI is the one the CALLER addressed, digest and all: the cache key is the request's
	// `uri` parameter, so answering with the bare form would invite the caller to key its cache on an
	// identity it never asked for.
	if got[0].URI != addressed {
		t.Fatalf("contents URI = %q, want the requested %q", got[0].URI, addressed)
	}

	// The bare form still reads. Legacy URIs are unchanged and nothing already published is stranded.
	if _, err := store.ReadResource(context.Background(), uri); err != nil {
		t.Fatalf("the bare URI must keep resolving: %v", err)
	}
}

func TestResources_AStaleDigestIsRefusedRatherThanAnsweredWithCurrentBytes(t *testing.T) {
	const patch = "--- a/x\n+++ b/x\n"
	dir, rel := artifactFixture(t, "changes.patch", patch)
	sum := sha256.Sum256([]byte(patch))

	var store mcp.RunStore
	uri := store.Publish("run-abc", mcp.Artifact{
		Name: "patch", Dir: dir, Rel: rel, SHA256: "sha256:" + hex.EncodeToString(sum[:]),
	})
	stale := uri + "?sha256=0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := store.ReadResource(context.Background(), stale); err == nil {
		t.Fatal("a URI naming a digest this store does not hold was answered with the current bytes")
	} else if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("the refusal must name the rule, got %v", err)
	}
}
