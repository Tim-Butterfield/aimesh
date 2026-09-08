package acp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Framer reads and writes raw JSON-RPC message bytes per a wire framing. The
// message dispatch layer is framing-agnostic — both framings share it.
//
// WriteMessage is ATOMIC and safe for concurrent use: a frame is composed from more
// than one Write on the underlying writer (body + "\n"; header + body), and an
// underlying writer whose individual Writes are themselves atomic — an os.Pipe, an
// io.Pipe, a process's stdin — does NOT make that COMPOSITION atomic. Two goroutines
// writing at once could otherwise emit `{a}{b}\n\n`, which the peer reads as one
// unparseable line and answers with a parse error, silently losing BOTH messages. The
// serialization lives here rather than in each caller because it is a property of the
// framing, not of any one server: every writer in this tree independently reinvented a
// mutex to compensate for its absence, and the one that did not corrupted its stream.
//
// ReadMessage is NOT concurrent-safe and never can be: it owns a buffered reader over a
// stream, so two concurrent readers would split frames between them. One read loop per
// Framer.
type Framer interface {
	ReadMessage() ([]byte, error)
	WriteMessage([]byte) error
}

// Framing names.
const (
	FramingNewline       = "newline"        // one JSON object per line (ACP / Zed style)
	FramingContentLength = "content-length" // LSP-style Content-Length headers
)

// maxFrameBytes bounds a single message so a malformed/hostile local host cannot
// force unbounded allocation (huge line or huge Content-Length).
const maxFrameBytes = 16 << 20 // 16 MiB

// header bounds prevent a headers-phase OOM (a stream with no newline / no blank line).
const (
	maxHeaderBytes = 8 << 10 // per header line
	maxHeaderLines = 64
)

// NewFramer exposes the framing constructor for the ACP test host (and any other
// in-repo client) so it speaks the exact wire framing the server uses.
func NewFramer(name string, in io.Reader, out io.Writer) Framer {
	return newFramer(name, in, out)
}

// newFramer selects a Framer. Unknown/empty names default to newline-delimited
// (the framing ACP hosts use); "content-length" gives LSP-style header framing.
func newFramer(name string, in io.Reader, out io.Writer) Framer {
	if name == FramingContentLength {
		return &contentLengthFramer{r: bufio.NewReader(in), w: out}
	}
	return &newlineFramer{r: bufio.NewReaderSize(in, 64*1024), w: out}
}

// newlineFramer: one JSON object per line.
type newlineFramer struct {
	r  *bufio.Reader
	w  io.Writer
	wm sync.Mutex // makes the body + "\n" pair ONE frame; see the Framer contract
}

func (f *newlineFramer) ReadMessage() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := f.r.ReadSlice('\n') // references the bufio buffer; we copy via append
		buf = append(buf, chunk...)
		if len(buf) > maxFrameBytes {
			return nil, fmt.Errorf("acp: message exceeds %d bytes", maxFrameBytes)
		}
		if err == bufio.ErrBufferFull {
			continue // no newline yet; keep accumulating (bounded above)
		}
		t := bytes.TrimSpace(buf)
		if len(t) > 0 {
			return t, nil // content found (even if EOF arrived with it; next call returns EOF)
		}
		if err != nil {
			return nil, err
		}
		buf = buf[:0] // blank line; keep reading
	}
}

func (f *newlineFramer) WriteMessage(b []byte) error {
	f.wm.Lock()
	defer f.wm.Unlock()
	if _, err := f.w.Write(b); err != nil {
		return err
	}
	_, err := f.w.Write([]byte("\n"))
	return err
}

// contentLengthFramer: LSP-style `Content-Length: N\r\n\r\n<body>`.
type contentLengthFramer struct {
	r  *bufio.Reader
	w  io.Writer
	wm sync.Mutex // makes the header + body pair ONE frame; see the Framer contract
}

// readHeaderLine reads one header line bounded by maxHeaderBytes so a stream that
// never sends a newline cannot force unbounded allocation.
func readHeaderLine(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxHeaderBytes {
			return "", fmt.Errorf("acp: header line exceeds %d bytes", maxHeaderBytes)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return string(buf), err
	}
}

func (f *contentLengthFramer) ReadMessage() ([]byte, error) {
	length := -1
	for n := 0; ; n++ {
		if n > maxHeaderLines {
			return nil, fmt.Errorf("acp: too many header lines (> %d)", maxHeaderLines)
		}
		line, err := readHeaderLine(f.r)
		if err != nil {
			if line == "" {
				return nil, err // clean EOF between messages
			}
			return nil, fmt.Errorf("acp: incomplete header: %w", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break // blank line terminates headers
		}
		i := strings.IndexByte(trimmed, ':')
		if i < 0 {
			return nil, fmt.Errorf("acp: malformed header line %q", trimmed)
		}
		if strings.EqualFold(strings.TrimSpace(trimmed[:i]), "Content-Length") {
			n, perr := strconv.Atoi(strings.TrimSpace(trimmed[i+1:]))
			if perr != nil || n < 0 {
				return nil, fmt.Errorf("acp: invalid Content-Length %q", trimmed[i+1:])
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("acp: missing Content-Length header")
	}
	if length > maxFrameBytes {
		return nil, fmt.Errorf("acp: Content-Length %d exceeds %d", length, maxFrameBytes)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(f.r, buf); err != nil { // handles partial reads
		return nil, fmt.Errorf("acp: short body: %w", err)
	}
	return buf, nil
}

func (f *contentLengthFramer) WriteMessage(b []byte) error {
	f.wm.Lock()
	defer f.wm.Unlock()
	if _, err := fmt.Fprintf(f.w, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err := f.w.Write(b)
	return err
}
