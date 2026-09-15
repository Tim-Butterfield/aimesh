// Package testhost is a test ACP host. It launches `aimesh explore acp` as a subprocess and drives it over
// stdio JSON-RPC 2.0, so the ACP surface can be tested without an editor. It does not replace testing
// against real hosts. The explore ACP surface performs no host file reads, so the host implements no
// filesystem callbacks.
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

// Client drives an explore ACP server over a Framer.
//
// A Framer's reader supports only one read loop, so a Client serves one request at a time: Call and
// CallCollecting refuse with ErrRequestInFlight while another request is running. Notify only writes, so it
// is safe while a request is in flight; send `session/cancel` as a notification to cancel a running prompt.
type Client struct {
	f   acp.Framer
	req sync.Mutex // held for a request's round trip; also guards id
	id  int
}

// ErrRequestInFlight is returned by Call and CallCollecting when the Client is already serving a request.
// They refuse rather than wait, since waiting would deadlock a cancel sent during the request it cancels.
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

// Call sends a request and returns the response with its id, skipping notifications.
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

// CallCollecting sends a request and returns its response along with the notifications received before
// it, in order.
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

// Notify sends a notification. It never reads, so it is safe to call while a request is in flight.
func (c *Client) Notify(method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	return c.f.WriteMessage(b)
}

// Launch starts `<bin> explore acp [--framing <framing>]` and returns a Client on its stdio and a stop
// function that sends `exit` and waits for the process. env is used as given (nil inherits the parent's);
// stderr goes to errSink, or is discarded when errSink is nil.
func Launch(bin, framing string, env []string, errSink io.Writer) (*Client, func() error, error) {
	return LaunchIn("", bin, framing, env, errSink)
}

// LaunchIn is Launch with the child's working directory set to dir ("" inherits the parent's). Pass an
// empty temporary directory to isolate the child from a project's `.aimesh/` configuration.
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
		// The whole shutdown runs under the timeout, since writing `exit` to a stuck child can block.
		done := make(chan error, 1)
		go func() {
			_ = c.Notify("exit", nil)
			_ = stdin.Close()
			done <- cmd.Wait()
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			return fmt.Errorf("acp server did not exit within timeout; killed")
		}
	}
	return c, stop, nil
}
