package testhost

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review/surface/acp"
)

// A Client owns exactly one read loop over its Framer: two read loops would split frames between
// them. These tests cancel from another goroutine while a session/prompt call is in flight, sending
// the cancel as a notification so nothing else reads.

// streamingAgent is a minimal in-process agent shaped like the real server: an inline dispatch loop
// and a goroutine per run that streams session/update notifications until cancelled, then answers
// the prompt. started is closed once a run's first update is written.
func streamingAgent(f acp.Framer, started chan<- struct{}) {
	cancelled := make(chan struct{})
	var cancelOnce, startedOnce sync.Once
	cancel := func() { cancelOnce.Do(func() { close(cancelled) }) }
	var runs sync.WaitGroup
	defer runs.Wait()
	defer cancel() // a torn-down transport must not leave a run streaming forever
	for {
		raw, err := f.ReadMessage()
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Method {
		case "session/prompt":
			runs.Add(1)
			go func(id json.RawMessage) {
				defer runs.Done()
				note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{"sessionId": "s-1"}})
				for {
					select {
					case <-cancelled:
						resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
							"result": map[string]any{"stopReason": "cancelled"}})
						_ = f.WriteMessage(resp)
						return
					default:
					}
					if werr := f.WriteMessage(note); werr != nil {
						return
					}
					startedOnce.Do(func() { close(started) })
					time.Sleep(200 * time.Microsecond)
				}
			}(m.ID)
		case "session/cancel":
			cancel()
			if len(m.ID) > 0 { // the request form still gets its ack
				resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"result": map[string]any{"cancelled": true}})
				_ = f.WriteMessage(resp)
			}
		case "exit":
			return
		}
	}
}

// pipedClient wires a Client to streamingAgent over in-memory pipes. The returned channel closes
// once a run is streaming.
func pipedClient(t *testing.T) (*Client, <-chan struct{}) {
	t.Helper()
	cr, sw := io.Pipe() // agent → client
	sr, cw := io.Pipe() // client → agent
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamingAgent(acp.NewFramer(acp.FramingNewline, sr, sw), started)
	}()
	t.Cleanup(func() {
		_ = cw.Close()
		_ = sw.Close()
		_ = sr.Close()
		_ = cr.Close()
		<-done
	})
	return NewClient(acp.FramingNewline, cr, cw), started
}

// A second request during an in-flight prompt must be refused; otherwise two read loops race on one
// bufio.Reader and split the agent's frames.
func TestClient_CancelDuringInFlightPromptNeverStartsASecondReader(t *testing.T) {
	c, started := pipedClient(t)

	type out struct {
		resp  *Response
		notes []Notification
		err   error
	}
	done := make(chan out, 1)
	go func() {
		resp, notes, err := c.CallCollecting("session/prompt", map[string]any{"sessionId": "s-1"})
		done <- out{resp, notes, err}
	}()
	<-started // the prompt owns the reader and updates are flowing

	// A second request would need a second read loop, so it is refused.
	if resp, err := c.Call("session/cancel", map[string]any{"sessionId": "s-1"}); !errors.Is(err, ErrRequestInFlight) {
		t.Fatalf("a request issued while one is in flight must be refused with ErrRequestInFlight "+
			"(serving it puts a second read loop on one buffered reader), got resp=%+v err=%v", resp, err)
	}

	// A notification only writes, so it cannot race the reader.
	if err := c.Notify("session/cancel", map[string]any{"sessionId": "s-1"}); err != nil {
		t.Fatalf("notify session/cancel: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the in-flight prompt failed: %v", got.err)
		}
		if reason, _ := got.resp.Result["stopReason"].(string); reason != "cancelled" {
			t.Errorf("stopReason = %q, want cancelled — the notification must reach the agent", reason)
		}
		if len(got.notes) == 0 {
			t.Error("the prompt collected no notifications, so it did not own the stream it was reading")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled prompt never answered")
	}
}
