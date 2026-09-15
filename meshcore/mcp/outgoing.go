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

// This file implements server-to-client requests, used for `roots/list`. The mechanism is domain-free:
// an id allocator, a pending table, response routing in the read loop, a timeout, and cancellation.
//
//   - Outgoing ids are namespaced strings ("srv:1", "srv:2", ...), so they never collide with client ids.
//   - A client that never answers cannot stall the server: every request has a deadline, and its waiter
//     is removed on every exit path, so a late answer is dropped.

// outgoingIDPrefix namespaces server-issued request ids away from the client's.
const outgoingIDPrefix = "srv:"

// DefaultRequestTimeout bounds one server-to-client request. It is short because the server asks only
// for session-setup facts and proceeds without an answer.
const DefaultRequestTimeout = 10 * time.Second

// ErrNoSession is returned by Request when no session is live (before Serve, or after it returned).
var ErrNoSession = errors.New("mcp: no live session to send a server→client request on")

// ErrModernEra is returned by Request once the process has latched the 2026-07-28 revision, whose stdio
// transport forbids a server from writing JSON-RPC requests. The check is in Request itself, the only
// function that writes a request frame.
//
// SUNSET-PATH (MCP26-SUNSET): this whole file goes at legacy removal; `roots/list` is its only user.
var ErrModernEra = errors.New("mcp: this process is serving MCP 2026-07-28, which forbids a server to write JSON-RPC requests to stdout (server→client interaction moved to Multi Round-Trip Requests)")

// ErrSessionClosed is returned to every request still waiting when the session ends.
var ErrSessionClosed = errors.New("mcp: the session closed before the client answered")

// ErrRequestTimeout is returned when the client did not answer within the request timeout.
var ErrRequestTimeout = errors.New("mcp: the client did not answer within the request timeout")

// Request sends a JSON-RPC request to the client and waits for its response, returning the `result`
// verbatim. An error response becomes a *RequestError, distinct from a timeout.
func (s *Server) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	// Legacy only, checked before anything is allocated or written.
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

	// The waiter is removed on every exit path, so a late answer finds no entry.
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

// deliver routes an inbound response frame to the outgoing request waiting for it and reports whether
// the frame was consumed. A response in the server's id namespace with no waiter is dropped, never
// answered.
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

// Root is one filesystem root a client declares. `uri` is a `file://` URI per the spec.
type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// Path returns the local filesystem path a root's `file://` URI names, and whether it is one. A root
// with any other scheme is not a path.
func (r Root) Path() (string, bool) { return RootPath(r.URI) }

// RootPath converts a `file://` URI to a local filesystem path. A non-file scheme, a remote host or an
// unparseable URI is refused. A Windows drive path (`file:///C:/proj`) loses its leading slash.
func RootPath(uri string) (string, bool) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return "", false
	}
	u, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return "", false
	}
	// An authority other than empty or `localhost` names another machine.
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return "", false
	}
	p := u.Path
	if p == "" {
		return "", false
	}
	// `/C:/proj` → `C:/proj`, decided by the path's shape rather than runtime.GOOS so the input means
	// the same thing on every platform.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' && isDriveLetter(p[1]) {
		p = p[1:]
	}
	return filepath.FromSlash(p), true
}

func isDriveLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ClientDeclaredRoots reports whether the client declared the `roots` capability at initialize. A client
// that declared nothing is never asked, so the server keeps the roots it was launched with.
func (s *Server) ClientDeclaredRoots() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientRoots
}

// FetchRoots issues `roots/list` and returns the client's roots. It is exported so a server can ask on
// its own schedule.
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

// refreshRoots runs one `roots/list` round trip and passes the roots to OnRoots. It runs after
// `notifications/initialized` and on every `notifications/roots/list_changed`. A failure is written to
// Diagnostics and leaves the root set in force unchanged.
func (s *Server) refreshRoots(reason string) {
	if s.OnRoots == nil || !s.ClientDeclaredRoots() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout())
	defer cancel()
	roots, err := s.FetchRoots(ctx)
	if err != nil {
		// Reported on Diagnostics, never on the protocol stream.
		s.Diagnosticf("mcp: roots/list (%s) failed: %v — the root set in force is unchanged", reason, err)
		return
	}
	s.OnRoots(roots)
}
