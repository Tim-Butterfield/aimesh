package acp

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/internal/review"
	"github.com/Tim-Butterfield/aimesh/internal/review/manager/run"
)

// The TURN BUDGET. Until this shipped, an ACP prompt could run until the host gave up — and on a
// stdio agent that means a panel of model CLIs still spending with nobody waiting for them, which
// is precisely the surface-parity gap the MCP design recorded (every other aimesh agent surface has
// bounded a turn since it shipped).
//
// The budget is enforced by cancelling the run's context, so a timeout takes the SAME path a host
// cancellation takes: the manager sees ctx.Done at its own boundaries, and the no-write-after-cancel
// guarantee is the one that already exists rather than a second, parallel one.

// blockingReviewer runs until its context is cancelled, then reports why.
type blockingReviewer struct {
	noRemediation // the turn under test is a report turn
	entered       chan struct{}
	err           chan error
}

func (b *blockingReviewer) RunContext(ctx context.Context, _ run.Request) (review.RunOutcome, error) {
	close(b.entered)
	<-ctx.Done()
	b.err <- ctx.Err()
	return review.RunOutcome{}, ctx.Err()
}

func TestTurnTimeout_CancelsARunThatOutlivesIt(t *testing.T) {
	rv := &blockingReviewer{entered: make(chan struct{}), err: make(chan error, 1)}
	srv := &Server{
		Manager: rv, Caps: review.SurfaceCaps{FileRead: true},
		Roots: harnessRoots(t), TurnTimeout: 40 * time.Millisecond,
	}
	// A real pipe rather than the shared string-reader harness: the harness's input EOFs
	// immediately, and EOF cancels every in-flight run by design — which would mask the very thing
	// under test. Here the connection stays OPEN, exactly as a live host's would, so the only thing
	// that can end this run is the turn budget.
	in, inw := io.Pipe()
	served := make(chan struct{})
	var out lockedBuffer
	go func() {
		_ = srv.Serve(in, &out)
		close(served)
	}()
	t.Cleanup(func() {
		_ = inw.Close()
		<-served
	})
	frames := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"review","params":{"workspace":` + strconv.Quote(t.TempDir()) + `,"mode":"report"}}` + "\n"
	if _, err := io.WriteString(inw, frames); err != nil {
		t.Fatalf("write frames: %v", err)
	}
	select {
	case <-rv.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the manager was never called")
	}
	select {
	case err := <-rv.err:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the run ended with %v, want the turn budget's DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn budget did not cancel the run")
	}
	// A timed-out turn is answered with the ordinary halt taxonomy, never left hanging and never
	// presented as a partial result.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(out.String(), `"error"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no error response after the turn budget expired; got: %s", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lockedBuffer is a concurrency-safe sink for the server's frames.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A zero TurnTimeout means the default, not "unbounded": an operator who never passed the flag must
// still get a bounded turn.
func TestTurnBudget_DefaultsRatherThanDisabling(t *testing.T) {
	if got := (&Server{}).turnBudget(); got != DefaultTurnTimeout {
		t.Fatalf("turnBudget() = %v with no configuration, want the %v default", got, DefaultTurnTimeout)
	}
	if got := (&Server{TurnTimeout: time.Minute}).turnBudget(); got != time.Minute {
		t.Fatalf("turnBudget() = %v, want the configured minute", got)
	}
}

// The flag is documented where an operator looks for it. The review guide moved to docs/review.md when
// the app modules collapsed into one: it is no longer a module README, because there is no module.
func TestTurnTimeout_IsDocumentedInTheReadme(t *testing.T) {
	b, err := os.ReadFile("../../../../docs/review.md")
	if err != nil {
		t.Fatalf("read the review guide: %v", err)
	}
	if !strings.Contains(string(b), "--turn-timeout") {
		t.Error("docs/review.md does not document --turn-timeout")
	}
}
