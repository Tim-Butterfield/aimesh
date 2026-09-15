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

// This file implements MCP `resources/*`, through which a caller collects the artifacts a run wrote.
//
// Two rules shape it:
//
//   - A resource URI carries no host path. A URI is `aimesh://run/<runId>/<artifact>`: two opaque
//     identifiers the server resolves against a table it built.
//   - A caller names a registration, never a path. `resources/read` accepts only a URI already in the
//     store, which holds the (directory, run-relative name) pair recorded when the artifact was written,
//     so an unpublished file is unaddressable rather than refused by a check.

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

	// Digest is the artifact's recorded content digest in "sha256:<hex>" form, or "" when it has none.
	// It is not a wire field: on the modern era it is folded into the URI (see digestedResourceURI),
	// making it part of the cache key.
	Digest string `json:"-"`
}

// resourceDigestParam is the query parameter that content-addresses a resource URI on the modern era.
// The digest goes in the URI rather than relying on `ttlMs: 0`, because clients may serve stale
// responses when a re-fetch fails but must not serve a cached response for different parameters, so a
// cached read is always the correct bytes for its digest. Separately, the server refuses to serve bytes
// that do not match the recorded digest.
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

// ResourceContents is one block of a `resources/read` result. Only text contents are produced, since
// published artifacts are diffs, JSON records and logs.
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// ResourceProvider is the application seam. A nil provider means the `resources` capability is not
// declared and `resources/*` returns -32601.
type ResourceProvider interface {
	// ListResources returns every resource currently published. It is called per request, so a provider
	// whose set changes (runs finishing, runs evicted) needs no invalidation protocol.
	ListResources(ctx context.Context) ([]Resource, error)
	// ReadResource returns the contents of one URI, or a *RequestError for a URI it does not publish.
	ReadResource(ctx context.Context, uri string) ([]ResourceContents, error)
}

// CodeResourceNotFound is the code `resources/read` returns for a URI this server does not publish. It
// shares -32002 with CodeNotInitialized, as the MCP specification assigns that value to "Resource not
// found"; the two cannot be confused, since one occurs only before the handshake and the other only
// after. The modern revision forbids emitting -32002 and names -32602 instead, so on that era this code
// is a declared deviation.
//
// This live code is not part of the MCP26-SUNSET removal, which deletes only CodeNotInitialized.
const CodeResourceNotFound = -32002

// --- the run-scoped artifact store ---

// maxResourceBytes bounds one artifact read. A larger artifact is refused rather than truncated, because
// a valid partial diff is worse than an error.
const maxResourceBytes int64 = 4 << 20 // 4 MiB

// RunStore publishes a run's artifacts as MCP resources, addressed by an opaque run id plus a logical
// artifact name. It holds each artifact's directory and directory-relative name, and reads through
// workspace.ReadUnder, the identity-bound, no-follow, bounded read of the containment layer, so an
// artifact swapped for a symlink after registration is refused.
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
	dir         string // the run directory, never published
	rel         string // the run-relative artifact name
	size        int64
	sha256      string
	published   time.Time
}

// Artifact declares one artifact to publish. Dir + Rel are the server's own private resolution; nothing
// about either reaches the wire.
type Artifact struct {
	// Name is the logical artifact name a URI addresses ("patch", "manifest"). It must be a single
	// path-free token: it is the last segment of the URI, not a file name.
	Name string
	// Title / Description are for a human reading a client's resource picker.
	Title       string
	Description string
	MimeType    string
	// Dir is the directory the artifact lives in; Rel is its directory-relative name.
	Dir string
	Rel string
	// SHA256 is the digest recorded when the artifact was written, in "sha256:<hex>" form. When
	// present it is re-verified on every read, and an artifact that no longer matches is refused.
	SHA256 string
}

// ResourceURI is the opaque address of one run artifact. It contains two identifiers the caller already
// has (or was given) and nothing else — no directory, no file name, no host.
func ResourceURI(runID, name string) string {
	return ResourceURIScheme + "://run/" + url.PathEscape(runID) + "/" + url.PathEscape(name)
}

// Publish registers an artifact and returns its URI. A name that is not a single path-free token is
// refused with an empty URI, since the URI's last segment is an identifier.
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

// ReadResource implements ResourceProvider. Only a published URI resolves. A URI may carry the
// artifact's digest (the modern form, see resourceDigestParam) or not (the legacy form); both run the
// same server-side digest check, and a digest this store no longer holds is refused.
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
		// The refusal names the rule, never the path.
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
	// The URI is echoed as the caller addressed it, digest included, since the cache key is the
	// request's `uri`.
	return []ResourceContents{{URI: strings.TrimSpace(uri), MimeType: a.mimeType, Text: string(b)}}, nil
}

// ruleOf renders a workspace refusal's rule, falling back to the error text.
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
	// No paging: the set is a handful of artifacts per retained run.
	return okResp(id, s.result(env, map[string]any{"resources": advertisedResources(env, list)}))
}

// advertisedResources returns the addresses resources are published under for the request's era. On the
// modern era each URI carries its content digest (see resourceDigestParam); legacy addresses are
// unchanged. The bare URI still reads on both eras.
//
// SUNSET-PATH (MCP26-SUNSET): the dual-URI handling collapses to the digest form.
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

// readResourceURI decodes and validates a `resources/read` request; admission step 5 applies the same
// check before the latch.
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
		if re, ok := errors.AsType[*RequestError](err); ok {
			return errResp(id, re.Code, re.Message, re.Data)
		}
		return errResp(id, CodeInternal, "resources/read failed: "+err.Error())
	}
	if contents == nil {
		contents = []ResourceContents{}
	}
	return okResp(id, s.result(env, map[string]any{"contents": contents}))
}
