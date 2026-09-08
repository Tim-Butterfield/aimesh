// Package testhost is a deterministic ACP test host: it launches `exploremesh acp` as a subprocess and
// drives it over JSON-RPC 2.0 stdio (newline or Content-Length framing), so the ACP surface — and the
// web-UI ACP validator that spawns a child `exploremesh acp` — can be exercised WITHOUT a real IDE/editor.
//
// It is NOT a real ACP host and NOT a replacement for Zed / JetBrains / Devin interop (which stays
// verify-on-provision). exploremesh's ACP surface takes only a TASK over the wire (criteria-via-_meta),
// never host-mediated file reads, so this host implements no filesystem callbacks.
package testhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
)

// Client drives an exploremesh ACP server over a Framer (the same framing the server uses).
//
// CONCURRENCY. A Client OWNS its Framer's reader, and a Framer's reader can only ever have one
// owner: `ReadMessage` is a buffered read over a stream, so two read loops split frames between
// them (see the Framer contract in meshcore/acp). A Client therefore serves ONE request at a
// time — Call and CallCollecting each run a read loop until their own response arrives, and a
// second request entered while one is in flight is refused with ErrRequestInFlight rather than
// allowed to start a competing reader.
//
// Notify is the only method safe to use WHILE a request is in flight: it writes and never reads,
// and Framer.WriteMessage is atomic. That is why a driver which must reach the agent mid-request —
// cancelling an in-flight `session/prompt` from another goroutine — sends `session/cancel` as a
// JSON-RPC NOTIFICATION. The server cancels on the notification exactly as it does on a request
// (the ack is the only thing a notification gives up), and the cancelling goroutine never reads.
type Client struct {
	f   acp.Framer
	req sync.Mutex // held for a whole request→response round trip; also guards id
	id  int
}

// ErrRequestInFlight is returned by Call/CallCollecting when this Client is already serving a
// request. It is a REFUSAL, not a wait: blocking would deadlock the common case (a cancel issued
// while the request it cancels is still in flight), and proceeding would put a second read loop on
// a reader that admits only one. Use Notify for anything that must reach the agent mid-request.
var ErrRequestInFlight = errors.New("testhost: a request is already in flight on this client (only Notify is safe concurrently)")

// Response is a decoded JSON-RPC response.
type Response struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *RPCError       `json:"error"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

// NewClient wraps a reader/writer pair (server stdout / server stdin) in a Framer.
func NewClient(framing string, fromServer io.Reader, toServer io.Writer) *Client {
	return &Client{f: acp.NewFramer(framing, fromServer, toServer)}
}

// Call sends a request and returns the first response whose id matches, skipping any notifications that
// arrive first. It owns the reader for the round trip; a concurrent Call/CallCollecting gets
// ErrRequestInFlight (see the Client concurrency contract).
func (c *Client) Call(method string, params any) (*Response, error) {
	if !c.req.TryLock() {
		return nil, ErrRequestInFlight
	}
	defer c.req.Unlock()
	c.id++
	id := c.id
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if err := c.f.WriteMessage(b); err != nil {
		return nil, err
	}
	for {
		raw, err := c.f.ReadMessage()
		if err != nil {
			return nil, err
		}
		var resp Response
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		if len(resp.ID) == 0 {
			continue // a notification — keep reading for our response
		}
		var got int
		if json.Unmarshal(resp.ID, &got) == nil && got == id {
			return &resp, nil
		}
	}
}

// Notification is a decoded JSON-RPC notification (no id) — e.g. session/update.
type Notification struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// CallCollecting sends a request and returns the matching response plus every notification frame that
// arrived before it, in order — so a caller can assert interleaved progress. It owns the reader for the
// round trip; a concurrent Call/CallCollecting gets ErrRequestInFlight.
func (c *Client) CallCollecting(method string, params any) (*Response, []Notification, error) {
	if !c.req.TryLock() {
		return nil, nil, ErrRequestInFlight
	}
	defer c.req.Unlock()
	c.id++
	id := c.id
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if err := c.f.WriteMessage(b); err != nil {
		return nil, nil, err
	}
	var notes []Notification
	for {
		raw, err := c.f.ReadMessage()
		if err != nil {
			return nil, notes, err
		}
		var resp Response
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		if len(resp.ID) == 0 {
			var n Notification
			if json.Unmarshal(raw, &n) == nil && n.Method != "" {
				notes = append(notes, n)
			}
			continue
		}
		var got int
		if json.Unmarshal(resp.ID, &got) == nil && got == id {
			return &resp, notes, nil
		}
	}
}

// Notify sends a notification (no id, so the server sends no response). It writes and never reads, so —
// unlike Call — it is safe to use from another goroutine while a request is in flight, which is how an
// in-flight prompt is cancelled.
func (c *Client) Notify(method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	return c.f.WriteMessage(b)
}

// Launch starts `<bin> acp [--framing <framing>]` as a subprocess and returns a Client wired to its stdio
// plus a stop func that sends `exit` and waits for clean termination. env is passed verbatim; nil inherits
// the parent environment. stderr is sent to errSink (nil → discarded).
func Launch(bin, framing string, env []string, errSink io.Writer) (*Client, func() error, error) {
	return LaunchIn("", bin, framing, env, errSink)
}

// LaunchIn is Launch with an explicit child working directory (dir; "" inherits the parent cwd). A caller
// that must isolate the child from a live project config (`<cwd>/.exploremesh/…`, `<cwd>/.aimesh/…`) — e.g.
// the web-UI ACP validator — passes a throwaway dir that contains no `.exploremesh` / `.aimesh`.
func LaunchIn(dir, bin, framing string, env []string, errSink io.Writer) (*Client, func() error, error) {
	args := []string{"explore", "acp"}
	if framing != "" {
		args = append(args, "--framing", framing)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	if errSink == nil {
		errSink = io.Discard
	}
	cmd.Stderr = errSink
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start %s acp: %w", bin, err)
	}
	c := NewClient(framing, stdout, stdin)
	stop := func() error {
		// The ENTIRE termination sequence runs in the goroutine: writing `exit` to a wedged child that has
		// stopped reading stdin would block forever, so even the graceful attempt must be under the timeout.
		done := make(chan error, 1)
		go func() {
			_ = c.Notify("exit", nil) // graceful: server returns on `exit`
			_ = stdin.Close()         // EOF also unblocks the server's read loop
			done <- cmd.Wait()
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill() // bounded cleanup: never hang on a wedged child
			<-done
			return fmt.Errorf("acp server did not exit within timeout; killed")
		}
	}
	return c, stop, nil
}
