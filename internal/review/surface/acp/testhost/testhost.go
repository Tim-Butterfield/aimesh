// Package testhost is a deterministic ACP test host. It launches the reviewmesh ACP agent as a
// subprocess and drives it over JSON-RPC 2.0 stdio, so the ACP surface can be tested without an
// editor. It sends workspace paths and does not implement host filesystem callbacks.
package testhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
)

// Client drives a reviewmesh ACP server over a Framer.
//
// A Framer's reader admits one owner, so a Client serves one request at a time: Call and
// CallCollecting read until their response arrives, and a second request meanwhile fails with
// ErrRequestInFlight. Notify writes without reading, so it is safe while a request is in flight;
// cancel an in-flight session/prompt by sending session/cancel as a notification.
type Client struct {
	f   acp.Framer
	req sync.Mutex // held for a whole request→response round trip; also guards id
	id  int
}

// ErrRequestInFlight is returned by Call and CallCollecting when the Client is already serving a
// request. Waiting would deadlock a cancel of that request, so use Notify instead.
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

// NewClient returns a Client reading server output from fromServer and writing to toServer.
func NewClient(framing string, fromServer io.Reader, toServer io.Writer) *Client {
	return &Client{f: acp.NewFramer(framing, fromServer, toServer)}
}

// Call sends a request and returns the first response with a matching id, skipping notifications.
// It returns ErrRequestInFlight if another request is in flight.
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

// Notification is a decoded JSON-RPC notification, such as session/update.
type Notification struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// CallCollecting sends a request and returns the matching response and, in order, every
// notification that arrived before it. It returns ErrRequestInFlight if another request is in
// flight.
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

// Notify sends a notification. It never reads, so it is safe to call from another goroutine while
// a request is in flight.
func (c *Client) Notify(method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	return c.f.WriteMessage(b)
}

// Options configures a launched child agent.
type Options struct {
	// Framing is the wire framing; "" uses the agent's default.
	Framing string
	// Dir is the child's working directory; "" inherits the parent's. The agent infers nothing from it.
	Dir string
	// Adapters are passed to the child as repeated --adapter flags.
	Adapters []string
	// Roots are passed as repeated --root flags, the ceiling every declared workspace must lie inside.
	Roots []string
	// AllowWrites launches the child with --allow-writes.
	AllowWrites bool
	// Env is the child's environment; nil inherits the parent's.
	Env []string
	// ErrSink receives the child's stderr; nil discards it.
	ErrSink io.Writer
}

// Launch starts `<bin> acp` with the given framing and returns a Client wired to its stdio and a
// stop function that sends exit and waits for the child. env and errSink are as in Options. The
// child has no adapter; use LaunchWith for adapters, roots or --allow-writes.
func Launch(bin, framing string, env []string, errSink io.Writer) (*Client, func() error, error) {
	return LaunchWith(bin, Options{Framing: framing, Env: env, ErrSink: errSink})
}

// LaunchIn is Launch with the child's working directory set to dir.
func LaunchIn(dir, bin, framing string, env []string, errSink io.Writer) (*Client, func() error, error) {
	return LaunchWith(bin, Options{Framing: framing, Dir: dir, Env: env, ErrSink: errSink})
}

// LaunchWith starts the child agent configured by o.
func LaunchWith(bin string, o Options) (*Client, func() error, error) {
	framing, env, errSink := o.Framing, o.Env, o.ErrSink
	args := []string{"review", "acp"}
	if framing != "" {
		args = append(args, "--framing", framing)
	}
	for _, a := range o.Adapters {
		args = append(args, "--adapter", a)
	}
	for _, r := range o.Roots {
		args = append(args, "--root", r)
	}
	if o.AllowWrites {
		args = append(args, "--allow-writes")
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = o.Dir
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
		// Run the whole shutdown under the timeout: writing exit to a child that stopped reading stdin
		// would block.
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
