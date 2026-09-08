package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/mcp"
)

// `resources/*` on the exploration server, driven by the OFFICIAL SDK client.
//
// exploremesh writes a run record for every MCP run precisely so its governance claims stay checkable
// after the fact — and until now that record was reachable only from the machine that owns it. This
// surface reports no host path (deliberately), so a client that wanted the manifest behind a synthesis,
// or the raw envelopes behind a count, had no route to either.
//
// The set published is taken from the run's OWN MANIFEST, so what a client can fetch is exactly what the
// capture layer recorded — including each artifact's digest, which is re-verified on every read.

// capturingServer builds a server with capture ON, writing into a temp artifact directory so no test
// leaves a run record in the repo.
func capturingServer(t *testing.T, exp mcp.Explorer) *mcp.Server {
	t.Helper()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", t.TempDir())
	return newServer(t, exp, func(s *mcp.Server) { s.DisableCapture = false })
}

func TestResources_ACapturedRunPublishesItsArtifacts(t *testing.T) {
	session := connect(t, capturingServer(t, &fakeExplorer{}))
	ctx := context.Background()

	res := call(t, session, "explore", exploreArgs(nil))
	if res.IsError {
		t.Fatalf("explore: %s", textOf(res))
	}
	runID, _ := structured(t, res)["runId"].(string)
	if runID == "" {
		t.Fatalf("explore returned no runId: %+v", structured(t, res))
	}

	list, err := session.ListResources(ctx, &sdk.ListResourcesParams{})
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	if len(list.Resources) == 0 {
		t.Fatal("a captured run published no resources — the run record is then reachable only from the machine that owns it")
	}
	var manifestURI string
	for _, r := range list.Resources {
		if strings.HasSuffix(r.URI, "/manifest") {
			manifestURI = r.URI
		}
		if !strings.HasPrefix(r.URI, "aimesh://run/") {
			t.Errorf("resource URI %q is not the opaque run-scoped form", r.URI)
		}
	}
	if manifestURI == "" {
		t.Fatalf("no manifest resource among %+v — it is the entry point to every other artifact of the run", list.Resources)
	}

	read, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: manifestURI})
	if err != nil {
		t.Fatalf("resources/read(%s): %v", manifestURI, err)
	}
	if len(read.Contents) != 1 {
		t.Fatalf("resources/read returned %d block(s), want 1", len(read.Contents))
	}
	var m map[string]any
	if jerr := json.Unmarshal([]byte(read.Contents[0].Text), &m); jerr != nil {
		t.Fatalf("the fetched manifest is not JSON: %v", jerr)
	}
	if m["runId"] != runID {
		t.Fatalf("the fetched manifest names run %v, want the run the caller was handed (%s)", m["runId"], runID)
	}
}

// The same rule as everywhere else on this surface: nothing on the wire says where anything lives.
func TestResources_ExploremeshURIsCarryNoHostPath(t *testing.T) {
	artifactDir := t.TempDir()
	t.Setenv("EXPLOREMESH_ARTIFACT_DIR", artifactDir)
	s := newServer(t, &fakeExplorer{}, func(s *mcp.Server) { s.DisableCapture = false })
	session := connect(t, s)
	ctx := context.Background()

	if res := call(t, session, "explore", exploreArgs(nil)); res.IsError {
		t.Fatalf("explore: %s", textOf(res))
	}
	list, err := session.ListResources(ctx, &sdk.ListResourcesParams{})
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), artifactDir) {
		t.Fatalf("resources/list leaked the artifact directory %q: %s", artifactDir, b)
	}
	for _, r := range list.Resources {
		if strings.Contains(r.URI, "/") && strings.Count(strings.TrimPrefix(r.URI, "aimesh://run/"), "/") != 1 {
			t.Errorf("resource URI %q spells out a layout; a URI segment is an identifier", r.URI)
		}
	}
}

// A URI this server never published is unaddressable, whatever it is spelt as.
func TestResources_ExploremeshRefusesAnUnpublishedURI(t *testing.T) {
	session := connect(t, capturingServer(t, &fakeExplorer{}))
	ctx := context.Background()
	if res := call(t, session, "explore", exploreArgs(nil)); res.IsError {
		t.Fatalf("explore: %s", textOf(res))
	}
	for _, uri := range []string{
		"aimesh://run/not-a-run/manifest",
		"file:///etc/hosts",
		"aimesh://run/../../etc/hosts",
	} {
		if _, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri}); err == nil {
			t.Errorf("resources/read(%q) succeeded; only a run's own published artifacts are readable", uri)
		}
	}
}

// With capture OFF there is nothing on disk to publish, and the server says so honestly rather than
// advertising links to a record it never wrote.
func TestResources_NothingIsPublishedWhenCaptureIsOff(t *testing.T) {
	session := connect(t, newServer(t, &fakeExplorer{})) // capture is off in newServer
	ctx := context.Background()
	if res := call(t, session, "explore", exploreArgs(nil)); res.IsError {
		t.Fatalf("explore: %s", textOf(res))
	}
	list, err := session.ListResources(ctx, &sdk.ListResourcesParams{})
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	if len(list.Resources) != 0 {
		t.Fatalf("capture is off, yet %d resource(s) were published: %+v", len(list.Resources), list.Resources)
	}
}
