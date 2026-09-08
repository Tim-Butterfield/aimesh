package acp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The framing layer is the ACP surface's TRUST BOUNDARY: it reads bytes from a local host process
// before any JSON is parsed. docs/acp.md claims both framings are bounded "so a malformed or hostile
// local host cannot force unbounded allocation" — these tests are what make that claim true rather
// than aspirational. They cover the round trip, every declared bound (frame size, header line size,
// header count), and the malformed-input paths.

// infiniteReader yields a single byte forever and NEVER a newline — the shape of a hostile stream
// that tries to make a line-oriented reader allocate without limit.
type infiniteReader struct{ b byte }

func (r infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

func TestNewFramer_DefaultsToNewline(t *testing.T) {
	for _, name := range []string{"", "newline", "bogus-unknown-framing"} {
		if _, ok := NewFramer(name, strings.NewReader(""), io.Discard).(*newlineFramer); !ok {
			t.Errorf("framing %q must default to the newline framer (the framing ACP hosts use)", name)
		}
	}
	if _, ok := NewFramer(FramingContentLength, strings.NewReader(""), io.Discard).(*contentLengthFramer); !ok {
		t.Error("framing \"content-length\" must select the LSP-style header framer")
	}
}

// A message written by one framer must be readable by the same framing, byte-for-byte, for both
// framings — the property the ACP test host depends on to speak the exact wire the server does.
func TestFramer_RoundTrip_BothFramings(t *testing.T) {
	for _, framing := range []string{FramingNewline, FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			msgs := [][]byte{
				[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`),
				[]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"text":"unicode: café ✓"}}`),
			}
			var wire bytes.Buffer
			w := NewFramer(framing, strings.NewReader(""), &wire)
			for _, m := range msgs {
				if err := w.WriteMessage(m); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			r := NewFramer(framing, bytes.NewReader(wire.Bytes()), io.Discard)
			for i, want := range msgs {
				got, err := r.ReadMessage()
				if err != nil {
					t.Fatalf("read %d: %v", i, err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("round trip %d:\n got %s\nwant %s", i, got, want)
				}
			}
			// The stream is exhausted: a further read reports EOF, not a hang or a garbage frame.
			if _, err := r.ReadMessage(); !errors.Is(err, io.EOF) {
				t.Errorf("after the last message want io.EOF, got %v", err)
			}
		})
	}
}

// --- concurrency: one frame per WriteMessage, whoever is writing ---

// yieldingWriter is an underlying writer whose individual Writes are ATOMIC (a shared buffer under a
// lock) but which yields the processor between them — the behavior of every real transport this framer
// runs over (an io.Pipe, an os.Pipe, a child process's stdin). It is the writer that separates the two
// properties that matter: "each Write lands whole" is NOT "each frame lands whole", because a frame is
// composed from more than one Write.
type yieldingWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *yieldingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	runtime.Gosched() // the window a concurrent writer would slip into
	return n, err
}

func (w *yieldingWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// TestFramer_WriteMessageIsAtomicUnderConcurrentWriters pins the Framer's stated write contract.
//
// This is the bug behind a real intermittent failure: meshcore/mcp's roots test drove a client that
// answered the server's `roots/list` from one goroutine while the test body sent a `ping` from another.
// Both frames went through the same Framer, whose newline WriteMessage was `Write(body)` then
// `Write("\n")` with nothing in between — so the two frames could interleave into `{ping}{answer}\n\n`,
// which the server read as ONE unparseable line. It answered with a parse error carrying no id, both
// real messages were lost, and the test waited out its full 10s deadline for a `ping` response that
// could never come. Every server in this tree had independently wrapped its own writes in a mutex to
// avoid exactly this; the contract now lives in the framer, so a caller cannot forget it.
//
// The assertion is on the WIRE, not on a timing outcome: N goroutines write N distinct frames, and all
// N must come back out whole and unmixed. Without the framer's write lock this fails immediately.
func TestFramer_WriteMessageIsAtomicUnderConcurrentWriters(t *testing.T) {
	for _, framing := range []string{FramingNewline, FramingContentLength} {
		t.Run(framing, func(t *testing.T) {
			const writers, each = 8, 25
			w := &yieldingWriter{}
			f := newFramer(framing, strings.NewReader(""), w)
			want := map[string]bool{}
			var wg sync.WaitGroup
			for g := 0; g < writers; g++ {
				for i := 0; i < each; i++ {
					want[fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"m%d"}`, g, i)] = true
				}
			}
			for g := 0; g < writers; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < each; i++ {
						if err := f.WriteMessage([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"m%d"}`, g, i))); err != nil {
							t.Errorf("write: %v", err)
							return
						}
					}
				}(g)
			}
			wg.Wait()

			r := newFramer(framing, bytes.NewReader(w.bytes()), io.Discard)
			got := 0
			for {
				msg, err := r.ReadMessage()
				if err != nil {
					break
				}
				got++
				if !want[string(msg)] {
					t.Fatalf("read a frame no writer ever wrote — two frames interleaved on the wire:\n%s", msg)
				}
				delete(want, string(msg))
			}
			if n := writers * each; got != n {
				t.Errorf("read %d frames, want %d — %d never arrived intact", got, n, n-got)
			}
		})
	}
}

// --- bounds: the "cannot force unbounded allocation" guarantee ---

// A newline stream that never sends a newline must be REFUSED at maxFrameBytes rather than
// accumulating forever.
func TestNewlineFramer_UnterminatedStream_BoundedByMaxFrame(t *testing.T) {
	f := newFramer(FramingNewline, infiniteReader{b: 'x'}, io.Discard)
	_, err := f.ReadMessage()
	if err == nil {
		t.Fatal("an endless newline-less stream must error, not accumulate without bound")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(maxFrameBytes)) {
		t.Errorf("error should name the %d-byte frame bound, got %v", maxFrameBytes, err)
	}
}

// The content-length framer must reject an oversized declared length BEFORE allocating the body —
// this is the difference between a bounded refusal and a 1-line OOM.
func TestContentLengthFramer_OversizedDeclaredLength_RejectedBeforeAllocation(t *testing.T) {
	// Declare far more than the cap while supplying no body at all: if the implementation allocated
	// first (or tried to read the body) this would be an OOM / hang instead of a clean error.
	huge := maxFrameBytes + 1
	in := fmt.Sprintf("Content-Length: %d\r\n\r\n", huge)
	f := newFramer(FramingContentLength, strings.NewReader(in), io.Discard)
	_, err := f.ReadMessage()
	if err == nil {
		t.Fatal("a Content-Length above the cap must be rejected")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should report exceeding the cap, got %v", err)
	}
	// A length exactly AT the cap is allowed by the bound check (it then fails on the short body,
	// proving the rejection above was the cap and not merely the missing body).
	atCap := fmt.Sprintf("Content-Length: %d\r\n\r\n", maxFrameBytes)
	f2 := newFramer(FramingContentLength, strings.NewReader(atCap), io.Discard)
	if _, err := f2.ReadMessage(); err == nil || strings.Contains(err.Error(), "exceeds") {
		t.Errorf("a length AT the cap must pass the bound check (then fail as a short body), got %v", err)
	}
}

// A header line that never terminates must be refused at maxHeaderBytes.
func TestContentLengthFramer_UnterminatedHeader_BoundedByMaxHeaderBytes(t *testing.T) {
	f := newFramer(FramingContentLength, infiniteReader{b: 'H'}, io.Discard)
	_, err := f.ReadMessage()
	if err == nil {
		t.Fatal("an endless header line must error, not accumulate without bound")
	}
	if !strings.Contains(err.Error(), "header line exceeds") {
		t.Errorf("error should name the header-line bound, got %v", err)
	}
}

// A stream of endless well-formed-but-useless headers must be refused at maxHeaderLines.
func TestContentLengthFramer_TooManyHeaderLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= maxHeaderLines+5; i++ {
		fmt.Fprintf(&b, "X-Filler-%d: v\r\n", i)
	}
	b.WriteString("\r\n")
	f := newFramer(FramingContentLength, strings.NewReader(b.String()), io.Discard)
	_, err := f.ReadMessage()
	if err == nil || !strings.Contains(err.Error(), "too many header lines") {
		t.Fatalf("want a too-many-header-lines refusal, got %v", err)
	}
}

// --- malformed input ---

func TestContentLengthFramer_MalformedHeaders(t *testing.T) {
	cases := []struct {
		name, in, wantErr string
	}{
		{"no colon", "NotAHeader\r\n\r\n", "malformed header line"},
		{"missing content-length", "X-Other: 1\r\n\r\n{}", "missing Content-Length"},
		{"non-numeric length", "Content-Length: abc\r\n\r\n", "invalid Content-Length"},
		{"negative length", "Content-Length: -5\r\n\r\n", "invalid Content-Length"},
		{"short body", "Content-Length: 50\r\n\r\n{\"a\":1}", "short body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFramer(FramingContentLength, strings.NewReader(c.in), io.Discard)
			_, err := f.ReadMessage()
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("want an error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

// Header parsing must be case-insensitive and whitespace-tolerant (real LSP hosts vary), and a
// clean EOF between messages must surface as io.EOF rather than a synthetic error.
func TestContentLengthFramer_HeaderToleranceAndCleanEOF(t *testing.T) {
	body := `{"ok":true}`
	in := fmt.Sprintf("content-length:   %d  \r\nX-Extra: ignored\r\n\r\n%s", len(body), body)
	f := newFramer(FramingContentLength, strings.NewReader(in), io.Discard)
	got, err := f.ReadMessage()
	if err != nil {
		t.Fatalf("lowercase/padded Content-Length must parse: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if _, err := f.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Errorf("a clean end between messages must be io.EOF, got %v", err)
	}
}

// The newline framer skips blank lines, tolerates a final message with no trailing newline, and
// then reports EOF — the shapes a real host actually produces.
func TestNewlineFramer_BlankLinesAndUnterminatedFinalMessage(t *testing.T) {
	f := newFramer(FramingNewline, strings.NewReader("\n\n  \n{\"a\":1}\n\n{\"b\":2}"), io.Discard)
	for _, want := range []string{`{"a":1}`, `{"b":2}`} {
		got, err := f.ReadMessage()
		if err != nil {
			t.Fatalf("read %s: %v", want, err)
		}
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if _, err := f.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Errorf("want io.EOF after the last message, got %v", err)
	}
}

// A message longer than bufio's internal buffer must still be read whole (the ErrBufferFull
// accumulation path) — a large-but-legal frame is not an error.
func TestNewlineFramer_LargeButLegalMessage(t *testing.T) {
	big := `{"pad":"` + strings.Repeat("z", 512*1024) + `"}`
	var wire bytes.Buffer
	w := newFramer(FramingNewline, strings.NewReader(""), &wire)
	if err := w.WriteMessage([]byte(big)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := newFramer(FramingNewline, bytes.NewReader(wire.Bytes()), io.Discard).ReadMessage()
	if err != nil {
		t.Fatalf("a 512 KiB message is legal (well under the %d-byte cap): %v", maxFrameBytes, err)
	}
	if string(got) != big {
		t.Errorf("large message corrupted: got %d bytes, want %d", len(got), len(big))
	}
}

// WriteMessage must surface a failing writer rather than silently dropping the frame.
func TestFramer_WriteError_Surfaced(t *testing.T) {
	for _, framing := range []string{FramingNewline, FramingContentLength} {
		if err := newFramer(framing, strings.NewReader(""), failWriter{}).WriteMessage([]byte(`{}`)); err == nil {
			t.Errorf("%s: a failing writer must surface an error", framing)
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
