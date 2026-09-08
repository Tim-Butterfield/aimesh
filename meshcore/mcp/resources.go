package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/workspace"
)

// This file is MCP `resources/*`: the read side of the protocol, and the reason a governed write can
// actually be collected.
//
// It exists because of a concrete, host-verified failure in an application built on this transport. A
// governed write completed, produced a diff, and returned the artifact's run-record-relative NAME plus
// its sha256 — and there was no mechanism by which the caller could obtain the bytes. The run directory
// is deliberately dropped (host paths do not belong on a wire that ends in a third party's inference
// log), stdout is the JSON-RPC transport, and `resources/*` was unimplemented. The workflow finished and
// delivered nothing.
//
// Two rules shape the design:
//
//   - A RESOURCE URI CARRIES NO HOST PATH. That was a deliberate decision and it is kept. A URI is
//     `aimesh://run/<runId>/<artifact>`: two opaque identifiers the SERVER resolves internally against a
//     table it built itself. Nothing on the wire says where anything lives.
//   - A CALLER NAMES A REGISTRATION, NEVER A PATH. `resources/read` takes a URI, not a filename; the URI
//     must already be in the store, and the store holds the (directory, run-relative name) pair that was
//     recorded when the artifact was written. There is no request shape that reaches a file the server did
//     not itself publish, so "a resource outside the run's own artifacts" is not refused by a check — it
//     is unaddressable.

// ResourceURIScheme is the scheme of every URI this package mints.
const ResourceURIScheme = "aimesh"

// Resource is one readable artifact a server publishes.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        int64  `json:"size,omitempty"`

	// Digest is the artifact's recorded content digest, in the receipt's "sha256:<hex>" form, or "" for
	// an artifact published without one. It is NOT a wire field: under the modern era it is folded into
	// the URI instead (see digestedResourceURI), which is what makes it part of the cache key rather
	// than a hint the client is free to ignore.
	Digest string `json:"-"`
}

// resourceDigestParam is the query parameter that content-addresses a resource URI under the modern era.
//
// WHY THE DIGEST IS IN THE URI AND NOT IN A TTL. `ttlMs: 0` is a hint, not a mechanism —
// server/utilities/caching says "If `ttlMs` is `0`, the response **SHOULD** be considered immediately
// stale", and then says "Clients **MAY** serve stale responses if errors occur during re-fetching". A
// zero TTL asks nicely and explicitly permits serving stale bytes when a re-fetch fails. The cache key,
// by contrast, is enforced: "Clients **MUST NOT** serve a cached response for a request whose method or
// parameters differ from the request that produced it." Putting the digest in the `uri` parameter makes
// a cached read serviceable ONLY for the exact digest it was fetched under, so a stale hit is by
// construction the correct bytes for that identity.
//
// The guarantee stated honestly: this is a SERVER-SIDE READ guarantee — we never serve bytes that do not
// match the digest the receipt recorded, and a mismatch is a loud refusal. It was never a client-side
// freshness guarantee and cannot be made one.
const resourceDigestParam = "sha256"

// digestedResourceURI returns uri with the artifact's digest attached as a query parameter, or uri
// unchanged when the artifact was published without a digest (nothing to address it by).
func digestedResourceURI(uri, digest string) string {
	hex := strings.TrimPrefix(strings.TrimSpace(digest), "sha256:")
	if uri == "" || hex == "" || strings.Contains(uri, "?") {
		return uri
	}
	return uri + "?" + resourceDigestParam + "=" + url.QueryEscape(hex)
}

// splitResourceDigest separates a resource URI from the digest a caller addressed it by. A URI with no
// digest returns itself and "" — the legacy form, which stays readable under both eras.
func splitResourceDigest(uri string) (base, digest string) {
	uri = strings.TrimSpace(uri)
	i := strings.IndexByte(uri, '?')
	if i < 0 {
		return uri, ""
	}
	base = uri[:i]
	q, err := url.ParseQuery(uri[i+1:])
	if err != nil {
		return base, ""
	}
	return base, q.Get(resourceDigestParam)
}

// ResourceContents is one block of a `resources/read` result. Only text contents are produced here: every
// artifact this repo publishes is a diff, a JSON record or a log, and base64-wrapping text would make it
// harder to read for no gain.
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// ResourceProvider is the application seam. A nil provider means the `resources` capability is NOT
// declared and `resources/*` stays a -32601 — the honest answer for a server with nothing to publish.
type ResourceProvider interface {
	// ListResources returns every resource currently published. It is called per request, so a provider
	// whose set changes (runs finishing, runs evicted) needs no invalidation protocol.
	ListResources(ctx context.Context) ([]Resource, error)
	// ReadResource returns the contents of one URI, or a *RequestError for a URI it does not publish.
	ReadResource(ctx context.Context, uri string) ([]ResourceContents, error)
}

// CodeResourceNotFound is the code `resources/read` returns for a URI this server does not publish.
//
// It is the same numeric value as CodeNotInitialized, because the MCP specification assigns -32002 to
// "Resource not found" and this transport had already taken it for the pre-initialization refusal. The two
// are not ambiguous in practice — one can only occur BEFORE the handshake and the other only AFTER it, and
// each message names its own case — but the sharing is stated here rather than left for a reader to notice.
//
// NOT SUNSET-PATH — and this counter-marker is the point. MCP26-SUNSET's checklist has a row
// "the pre-init `-32002` refusal … deleted, not renumbered". That row means CodeNotInitialized. It
// does NOT mean this constant, which is a live `resources/read` code and is reachable on both eras.
// A remover who greps `-32002` and deletes what it finds would take the wrong one.
//
// Two things are nonetheless owed here at removal, and are recorded so they are not rediscovered:
// `2026-07-28` forbids emitting `-32002` at all ("Implementations of this protocol version MUST NOT
// emit these codes: `-32002` …"), so on the modern era this code is a declared deviation, and the
// replacement the same page names is `-32602`. Changing it is a wire-visible change to a live code
// and is deliberately not bundled into the sunset deletion.
const CodeResourceNotFound = -32002

// --- the run-scoped artifact store ---

// maxResourceBytes bounds ONE artifact read. A patch large enough to exceed it is refused rather than
// truncated: a syntactically valid partial diff is worse than an error, which is the same reason the
// receipt carries a link and a hash instead of inline content.
const maxResourceBytes int64 = 4 << 20 // 4 MiB

// RunStore publishes a run's artifacts as MCP resources, addressed by an opaque run id plus a logical
// artifact name. It is domain-free: it knows a run has artifacts, not what any of them mean.
//
// The store holds the artifact's DIRECTORY and its directory-relative name. A read goes through
// workspace.ReadUnder — the same identity-bound, no-follow, regular-file-only, bounded read the containment
// layer uses — so an artifact whose path was swapped for a symlink between registration and collection is
// refused rather than followed.
type RunStore struct {
	mu   sync.Mutex
	byID map[string]*storedArtifact
	seq  []string // publication order, so `resources/list` is stable
}

type storedArtifact struct {
	uri         string
	runID       string
	name        string
	title       string
	description string
	mimeType    string
	dir         string // the run directory — NEVER published
	rel         string // the run-relative artifact name
	size        int64
	sha256      string
	published   time.Time
}

// Artifact declares one artifact to publish. Dir + Rel are the server's own private resolution; nothing
// about either reaches the wire.
type Artifact struct {
	// Name is the LOGICAL artifact name a URI addresses ("patch", "manifest"). It must be a single
	// path-free token: it is the last segment of the URI, not a file name.
	Name string
	// Title / Description are for a human reading a client's resource picker.
	Title       string
	Description string
	MimeType    string
	// Dir is the directory the artifact lives in; Rel is its directory-relative name.
	Dir string
	Rel string
	// SHA256 is the digest recorded when the artifact was WRITTEN, in the receipt's own
	// "sha256:<hex>" form. When present it is re-verified on every read: an artifact that no longer
	// digests to what its receipt says is refused, because the receipt is the thing a caller trusts.
	SHA256 string
}

// ResourceURI is the opaque address of one run artifact. It contains two identifiers the caller already
// has (or was given) and nothing else — no directory, no file name, no host.
func ResourceURI(runID, name string) string {
	return ResourceURIScheme + "://run/" + url.PathEscape(runID) + "/" + url.PathEscape(name)
}

// ParseResourceURI splits a resource URI back into its run id and artifact name.
func ParseResourceURI(uri string) (runID, name string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || !strings.EqualFold(u.Scheme, ResourceURIScheme) || u.Host != "run" {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	runID, rerr := url.PathUnescape(parts[0])
	name, nerr := url.PathUnescape(parts[1])
	if rerr != nil || nerr != nil || runID == "" || name == "" {
		return "", "", false
	}
	return runID, name, true
}

// Publish registers an artifact and returns its URI. A name that is not a single path-free token is
// refused (empty URI): the URI's last segment is an IDENTIFIER, and letting a path shape through would
// make the address describe a layout.
func (st *RunStore) Publish(runID string, a Artifact) string {
	runID, a.Name = strings.TrimSpace(runID), strings.TrimSpace(a.Name)
	if runID == "" || a.Name == "" || strings.ContainsAny(a.Name, `/\`) || strings.TrimSpace(a.Dir) == "" {
		return ""
	}
	if !workspace.SafeRel(a.Rel) {
		return ""
	}
	size, err := workspace.StatUnder(a.Dir, a.Rel)
	if err != nil {
		return "" // an artifact that cannot be stat-ed now is not published at all, rather than as a broken link
	}
	uri := ResourceURI(runID, a.Name)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.byID == nil {
		st.byID = map[string]*storedArtifact{}
	}
	if _, dup := st.byID[uri]; !dup {
		st.seq = append(st.seq, uri)
	}
	st.byID[uri] = &storedArtifact{
		uri: uri, runID: runID, name: a.Name, title: a.Title, description: a.Description,
		mimeType: a.MimeType, dir: a.Dir, rel: a.Rel, size: size, sha256: strings.TrimSpace(a.SHA256),
		published: time.Now(),
	}
	return uri
}

// Forget drops every artifact of a run. A registry that evicts a run must call it, or the store keeps
// publishing links to a run the server can no longer explain.
func (st *RunStore) Forget(runID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	keep := st.seq[:0]
	for _, uri := range st.seq {
		if a := st.byID[uri]; a != nil && a.runID == runID {
			delete(st.byID, uri)
			continue
		}
		keep = append(keep, uri)
	}
	st.seq = keep
}

// ListResources implements ResourceProvider.
func (st *RunStore) ListResources(context.Context) ([]Resource, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Resource, 0, len(st.seq))
	for _, uri := range st.seq {
		a := st.byID[uri]
		if a == nil {
			continue
		}
		out = append(out, Resource{
			URI: a.uri, Name: a.runID + "/" + a.name, Title: a.title,
			Description: a.description, MimeType: a.mimeType, Size: a.size,
			Digest: a.sha256,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReadResource implements ResourceProvider. The URI must already be published; there is no request shape
// that reaches a file this store did not itself register.
// A URI may carry the artifact's digest (the modern era's content-addressed form, see
// resourceDigestParam) or not (the legacy form). BOTH resolve to the same artifact and both run the same
// server-side digest check; a digest that names a version this store no longer holds is refused rather
// than answered with the current bytes under the old identity.
func (st *RunStore) ReadResource(_ context.Context, uri string) ([]ResourceContents, error) {
	base, asked := splitResourceDigest(uri)
	st.mu.Lock()
	a := st.byID[base]
	st.mu.Unlock()
	if a != nil && asked != "" && !strings.EqualFold(asked, strings.TrimPrefix(a.sha256, "sha256:")) {
		return nil, &RequestError{
			Code: CodeResourceNotFound,
			Message: fmt.Sprintf("resource not found: %q — this server no longer publishes that content digest for this artifact. "+
				"Call resources/list for the current addresses.", uri),
			Data: map[string]any{"uri": uri, "reasonCode": "artifact_digest_stale"},
		}
	}
	if a == nil {
		return nil, &RequestError{
			Code:    CodeResourceNotFound,
			Message: fmt.Sprintf("resource not found: %q — this server publishes only its own runs' artifacts, and a run's resources are dropped when the run is evicted. Call resources/list for what is available now.", uri),
			Data:    map[string]any{"uri": uri},
		}
	}
	b, err := workspace.ReadUnder(a.dir, a.rel, maxResourceBytes)
	if err != nil {
		// The refusal names the RULE, never the path: the path is precisely what this surface withholds.
		return nil, &RequestError{
			Code: CodeInternal,
			Message: fmt.Sprintf("resource %q could not be read: %s (reasonCode %s)",
				uri, ruleOf(err), workspace.ReasonOf(err)),
			Data: map[string]any{"uri": uri, "reasonCode": string(workspace.ReasonOf(err))},
		}
	}
	if a.sha256 != "" {
		sum := sha256.Sum256(b)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != a.sha256 {
			return nil, &RequestError{
				Code:    CodeInternal,
				Message: fmt.Sprintf("resource %q no longer matches the digest its receipt recorded — it is refused rather than served, because the receipt is what a caller trusts", uri),
				Data:    map[string]any{"uri": uri, "reasonCode": "artifact_digest_mismatch"},
			}
		}
	}
	// The URI is echoed AS THE CALLER ADDRESSED IT, digest and all: the cache key is the request's `uri`
	// parameter, so a response that answered a digest-bearing request with the bare URI would invite the
	// caller to key its cache on an identity it never asked for.
	return []ResourceContents{{URI: strings.TrimSpace(uri), MimeType: a.mimeType, Text: string(b)}}, nil
}

// ruleOf renders a workspace refusal's RULE (what was disallowed), falling back to the error text.
func ruleOf(err error) string {
	if r, ok := workspace.AsRefusal(err); ok && r.Rule != "" {
		return r.Rule
	}
	return err.Error()
}

// --- protocol handlers ---

type readResourceParams struct {
	URI string `json:"uri"`
}

func (s *Server) listResources(ctx context.Context, env *RequestEnv, id json.RawMessage, params json.RawMessage) *rpcResponse {
	if env == nil {
		var refusal *rpcResponse
		if env, refusal = s.envFor("resources/list", params); refusal != nil {
			refusal.ID = id
			return refusal
		}
	}
	list, err := s.Resources.ListResources(ctx)
	if err != nil {
		return errResp(id, CodeInternal, "resources/list failed: "+err.Error())
	}
	if list == nil {
		list = []Resource{}
	}
	// No paging: the set is a handful of artifacts per retained run, and a cursor nothing can page
	// through would be a declaration this server does not honor.
	return okResp(id, s.result(env, map[string]any{"resources": advertisedResources(env, list)}))
}

// advertisedResources is the ADDRESS a resource is published under, per era.
//
// Modern: the URI carries the artifact's content digest, so the digest is part of the cache key (see
// resourceDigestParam). Legacy: unchanged. The bare URI still READS under both eras — we simply stop
// ADVERTISING it under modern — so no client is stranded by the change, and the legacy golden is
// untouched because legacy emits no caching hints at all and nothing is invited to cache.
//
// SUNSET-PATH (MCP26-SUNSET; migration design §16.2): the dual-URI handling collapses to the
// digest form and this function becomes unconditional.
func advertisedResources(env *RequestEnv, list []Resource) []Resource {
	if env == nil || env.Era != EraModern {
		return list
	}
	out := make([]Resource, len(list))
	copy(out, list)
	for i := range out {
		out[i].URI = digestedResourceURI(out[i].URI, out[i].Digest)
	}
	return out
}

// readResourceURI decodes and validates a `resources/read` request. Split out so admission STEP 5 can
// apply the identical check before the era latch.
func (s *Server) readResourceURI(id json.RawMessage, params json.RawMessage) (string, *rpcResponse) {
	var p readResourceParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return "", errResp(id, CodeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if strings.TrimSpace(p.URI) == "" {
		return "", errResp(id, CodeInvalidParams, "invalid params: resources/read requires a `uri` (call resources/list for the published set)")
	}
	return p.URI, nil
}

func (s *Server) readResource(ctx context.Context, env *RequestEnv, id json.RawMessage, params json.RawMessage) *rpcResponse {
	uri, refusal := s.readResourceURI(id, params)
	if refusal != nil {
		return refusal
	}
	if env == nil {
		if env, refusal = s.envFor("resources/read", params); refusal != nil {
			refusal.ID = id
			return refusal
		}
	}
	contents, err := s.Resources.ReadResource(ctx, uri)
	if err != nil {
		var re *RequestError
		if errors.As(err, &re) {
			return errResp(id, re.Code, re.Message, re.Data)
		}
		return errResp(id, CodeInternal, "resources/read failed: "+err.Error())
	}
	if contents == nil {
		contents = []ResourceContents{}
	}
	return okResp(id, s.result(env, map[string]any{"contents": contents}))
}
