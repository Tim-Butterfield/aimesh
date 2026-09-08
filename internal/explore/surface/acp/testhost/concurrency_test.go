package testhost

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/explore/surface/acp"
)

// A Client owns exactly ONE read loop over its Framer, because a Framer's buffered reader admits
// only one owner — two read loops split frames between them, which is a data race and a corrupted
// stream, not merely an interleaving.
//
// This file pins that with the exact shape the ACP validator uses: a goroutine parked in
// CallCollecting("session/prompt") while the CALLER cancels from another goroutine. Sending that
// cancel as a request is the coupling that made this a race; sending it as a notification is the
// fix, and it is what the test drives.

// streamingAgent is a minimal in-process agent over one Framer, shaped like the real server: an
// inline dispatch loop, plus a goroutine per run that streams `session/update` notifications until
// the run is cancelled and then answers the prompt. The continuous stream is what makes a second
// reader collide with the first rather than merely coexist with it. `started` is closed once the
// first update of a run is on the wire.
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

// pipedClient wires a Client to streamingAgent over two in-memory pipes — the same Framer pair a
// spawned child gives, without the subprocess. The returned channel closes once a run is streaming.
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

// TestClient_CancelDuringInFlightPromptNeverStartsASecondReader is the validator's cancel path in
// miniature. It fails against a Client that lets a second request through: the two read loops race
// on one bufio.Reader (reported under -race) and split the agent's frames between them.
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

	// A second REQUEST must be refused, not served: serving it would mean a second read loop over
	// the reader the prompt above owns.
	if resp, err := c.Call("session/cancel", map[string]any{"sessionId": "s-1"}); !errors.Is(err, ErrRequestInFlight) {
		t.Fatalf("a request issued while one is in flight must be refused with ErrRequestInFlight "+
			"(serving it puts a second read loop on one buffered reader), got resp=%+v err=%v", resp, err)
	}

	// A NOTIFICATION is the way through: it writes and never reads, so it cannot race the reader.
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
