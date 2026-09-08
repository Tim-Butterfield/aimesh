package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// This file is the OTHER direction of the transport: requests the SERVER sends to the CLIENT.
//
// Until it existed, this package could only push notifications, which meant an entire half of the MCP
// lifecycle was unreachable — `roots/list` is a server→client REQUEST, so a server that cannot issue one
// cannot learn the roots its client declared no matter how carefully it implements the intersection rule.
// The transport accepted `notifications/roots/list_changed` and could do nothing with it.
//
// The mechanism is deliberately small and DOMAIN-FREE: an id allocator, a pending table, response routing
// in the read loop, a timeout, and cancellation safety. `roots/list` is the first user; nothing about the
// plumbing knows what a root is.
//
// Two properties are load-bearing:
//
//   - OUTGOING IDS ARE NAMESPACED. They are strings with a fixed prefix ("srv:1", "srv:2", …), so a
//     server-issued id can never collide with a client-issued one, whatever the client's numbering is.
//     Correlating on a bare integer counter would make a well-behaved client's `id: 1` indistinguishable
//     from our own.
//   - A CLIENT THAT NEVER ANSWERS MUST NOT WEDGE THE SERVER. Every outgoing request carries a deadline and
//     is abandoned when it expires; the waiter is removed from the pending table on EVERY exit path
//     (answer, timeout, caller cancellation, session end), so a late answer finds no waiter and is dropped
//     rather than delivered to a channel nobody reads.

// outgoingIDPrefix namespaces server-issued request ids away from the client's.
const outgoingIDPrefix = "srv:"

// DefaultRequestTimeout bounds ONE server→client request. It is short on purpose: everything this server
// asks a client for is a session-setup fact, and a host that cannot answer in this long is a host whose
// answer the server must proceed without rather than block on.
const DefaultRequestTimeout = 10 * time.Second

// ErrNoSession is returned by Request when no session is live (before Serve, or after it returned).
var ErrNoSession = errors.New("mcp: no live session to send a server→client request on")

// ErrModernEra is returned by Request once the process has latched the 2026-07-28 revision.
//
// It is a MUST, quoted from `basic/transports/stdio` §Receiving Messages: "The server MUST NOT write
// JSON-RPC *requests* to `stdout`." Server-to-client interaction moved to Multi Round-Trip Requests —
// the server returns an `InputRequiredResult` and the client retries — and this transport implements
// no MRTR, so on the modern era it simply has no way to ask a client anything.
//
// The guard is here, at the one function that writes a request frame, rather than at `roots/list`'s
// call site: an outgoing-request mechanism that is safe only because today's single caller checks
// first is not safe, it is lucky. SUNSET-PATH inverted (MCP26-SUNSET) — at legacy removal this whole file goes,
// because `roots/list` is its only user and Roots is deprecated in 2026-07-28.
var ErrModernEra = errors.New("mcp: this process is serving MCP 2026-07-28, which forbids a server to write JSON-RPC requests to stdout (server→client interaction moved to Multi Round-Trip Requests)")

// ErrSessionClosed is returned to every request still waiting when the session ends.
var ErrSessionClosed = errors.New("mcp: the session closed before the client answered")

// ErrRequestTimeout is returned when the client did not answer within the request timeout.
var ErrRequestTimeout = errors.New("mcp: the client did not answer within the request timeout")

// Request sends a JSON-RPC request to the CLIENT and waits for its response.
//
// It returns the response's `result` verbatim. A JSON-RPC error response becomes a *RequestError, so a
// caller distinguishes "the client refused" from "the client never answered".
func (s *Server) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	// LEGACY-ONLY, checked before anything is allocated and long before anything is written.
	if s.era() == EraModern {
		return nil, ErrModernEra
	}
	s.mu.Lock()
	f := s.active
	s.mu.Unlock()
	if f == nil {
		return nil, ErrNoSession
	}

	s.omu.Lock()
	if s.closed {
		s.omu.Unlock()
		return nil, ErrSessionClosed
	}
	s.outSeq++
	key := outgoingIDPrefix + strconv.FormatUint(s.outSeq, 10)
	ch := make(chan *rpcResponse, 1)
	if s.pending == nil {
		s.pending = map[string]chan *rpcResponse{}
	}
	s.pending[key] = ch
	s.omu.Unlock()

	// The waiter is removed on EVERY exit path. Without this a timed-out request leaves an entry the
	// read loop would later deliver into, and a client that answers slowly enough would grow the table
	// without bound.
	defer func() {
		s.omu.Lock()
		delete(s.pending, key)
		s.omu.Unlock()
	}()

	idBytes, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: idBytes, Method: method, Params: rawParams(params)})
	if err != nil {
		return nil, err
	}
	s.wmu.Lock()
	werr := f.WriteMessage(b)
	s.wmu.Unlock()
	if werr != nil {
		return nil, werr
	}

	timer := time.NewTimer(s.requestTimeout())
	defer timer.Stop()
	select {
	case resp := <-ch:
		if resp == nil {
			return nil, ErrSessionClosed
		}
		if resp.Error != nil {
			return nil, &RequestError{Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}
		}
		raw, merr := json.Marshal(resp.Result)
		if merr != nil {
			return nil, merr
		}
		return raw, nil
	case <-timer.C:
		return nil, ErrRequestTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) requestTimeout() time.Duration {
	if s.RequestTimeout > 0 {
		return s.RequestTimeout
	}
	return DefaultRequestTimeout
}

// rawParams marshals params to json.RawMessage, or nil when there are none.
func rawParams(params any) json.RawMessage {
	if params == nil {
		return nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	return b
}

// deliver routes an inbound RESPONSE frame to whichever outgoing request is waiting for it. It reports
// whether the frame was consumed here — a response for an id nobody is waiting on (a duplicate, or an
// answer that arrived after its deadline) is dropped, because the alternative is to treat it as a request
// and reply to it, which is a protocol violation.
func (s *Server) deliver(raw []byte) bool {
	var resp rpcResponse
	if json.Unmarshal(raw, &resp) != nil {
		return false
	}
	var id string
	if json.Unmarshal(resp.ID, &id) != nil || !strings.HasPrefix(id, outgoingIDPrefix) {
		return false
	}
	s.omu.Lock()
	ch := s.pending[id]
	delete(s.pending, id)
	s.omu.Unlock()
	if ch == nil {
		return true // ours by namespace, but nobody is waiting: drop it rather than answer it
	}
	ch <- &resp
	return true
}

// closeOutgoing fails every request still waiting when the session ends. A caller blocked on a client
// that went away must return, not hang until its deadline.
func (s *Server) closeOutgoing() {
	s.omu.Lock()
	s.closed = true
	pend := s.pending
	s.pending = nil
	s.omu.Unlock()
	for _, ch := range pend {
		ch <- nil
	}
}

// --- roots ---

// Root is one filesystem root a CLIENT declares. `uri` is a `file://` URI per the spec.
type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// Path returns the local filesystem path a root's `file://` URI names, and whether it is one. A root with
// any other scheme is NOT a path, and reporting a plausible-looking one for it would hand a caller a
// boundary derived from something that never was a directory.
func (r Root) Path() (string, bool) { return RootPath(r.URI) }

// RootPath converts a `file://` URI to a local filesystem path.
//
// It is deliberately strict: a non-file scheme, a URI naming a remote host, or one that does not parse is
// refused rather than coerced. Windows drive paths (`file:///C:/proj`) lose the leading slash, which is
// the only platform-shaped part of the conversion.
func RootPath(uri string) (string, bool) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return "", false
	}
	u, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return "", false
	}
	// An authority component other than an empty one or `localhost` names ANOTHER machine. There is no
	// local path for it, and inventing one would silently reinterpret a remote root as a local directory.
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return "", false
	}
	p := u.Path
	if p == "" {
		return "", false
	}
	// `/C:/proj` → `C:/proj`. The check is on the SHAPE (a drive letter followed by a colon), not on
	// runtime.GOOS: a Windows-shaped URI means the same thing wherever it is parsed, and keying it off
	// the host OS would make the same input mean two things.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' && isDriveLetter(p[1]) {
		p = p[1:]
	}
	return filepath.FromSlash(p), true
}

func isDriveLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ClientDeclaredRoots reports whether the client declared the `roots` capability at initialize. It is the
// gate on the whole round trip: a client that declared nothing is never asked, and — the rule that matters
// — a server that is never told anything keeps exactly the roots its operator launched it with.
func (s *Server) ClientDeclaredRoots() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientRoots
}

// FetchRoots issues `roots/list` and hands the result to OnRoots. It is called automatically after the
// client's `notifications/initialized` (when the client declared the capability) and again on every
// `notifications/roots/list_changed`; it is exported so a server can re-ask on its own schedule.
//
// A failure is DIAGNOSED, never fatal, and never silently widening: OnRoots is not called at all, so
// whatever root set was already in force stays in force.
func (s *Server) FetchRoots(ctx context.Context) ([]Root, error) {
	raw, err := s.Request(ctx, "roots/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Roots []Root `json:"roots"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return nil, fmt.Errorf("mcp: roots/list returned a result this transport cannot read: %w", uerr)
	}
	return out.Roots, nil
}

// refreshRoots runs one roots/list round trip in the background and reports it to the application.
func (s *Server) refreshRoots(reason string) {
	if s.OnRoots == nil || !s.ClientDeclaredRoots() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout())
	defer cancel()
	roots, err := s.FetchRoots(ctx)
	if err != nil {
		// Stated, not swallowed — and stated on the DIAGNOSTICS sink, never on the protocol stream.
		s.Diagnosticf("mcp: roots/list (%s) failed: %v — the root set in force is unchanged", reason, err)
		return
	}
	s.OnRoots(roots)
}
