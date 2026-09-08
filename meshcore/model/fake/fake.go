// Package fake is a deterministic ModelAccess adapter — a HIDDEN, INTERNAL test
// harness, never a user-configurable adapter. Its behavior is selected by a
// scenario string so tests can drive every code path (valid, empty, malformed,
// schema-invalid, duplicate, identity mismatch, timeout) without any network or
// real CLI. Resolution of the `fake` name in the apps is gated on Enabled():
// tests, the golden runs, and internal harnesses (e.g. the ACP-validation
// safe-mode parent) set the env for themselves; users never do, so `fake` fails
// closed as an unknown name everywhere user configuration is resolved.
package fake

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// EnvVar is the internal gate for the fake harness. It is documented ONLY in
// CONTRIBUTING.md — it is not a user-facing knob and must never appear in user
// docs or setup guidance.
const EnvVar = "AIMESH_INTERNAL_FAKE"

// Enabled reports whether the hidden internal fake harness is unlocked
// (AIMESH_INTERNAL_FAKE=1). Every app-side resolution path for the literal
// `fake` adapter (and reviewmesh's hidden `fake-smoke` profile) checks this
// exactly once and fails closed — the same error as any unknown name — when it
// is not set.
func Enabled() bool { return os.Getenv(EnvVar) == "1" }

// Scenario selects the fake's behavior.
type Scenario string

const (
	Valid              Scenario = "valid"
	Empty              Scenario = "empty"
	Malformed          Scenario = "malformed"
	SchemaInvalid      Scenario = "schema_invalid"
	Duplicate          Scenario = "duplicate"
	IdentityMismatch   Scenario = "identity_mismatch"
	Timeout            Scenario = "timeout"
	Churn              Scenario = "churn"                // a different finding each call → never converges (drives the outer-cap halt)
	MalformedThenValid Scenario = "malformed_then_valid" // first call malformed, then valid (drives the schema-failure retry)
	UnshownFile        Scenario = "unshown_file"         // valid finding targeting a file NOT in the workspace (apply-safety gate)
	// ProtectedPath reports TWO findings: one against a real workspace file, and one against
	// `.env` — a path the non-overridable write denylist protects. It exists to drive the
	// partial-refusal contract end to end: the valid finding must be applied, the protected one
	// must be refused and recorded, and the run must not report itself as a clean success. A
	// single-finding scenario would prove the refusal but not the thing D8-C is actually about —
	// that the OTHER findings survive it.
	ProtectedPath Scenario = "protected_path"
	ExitError     Scenario = "exit_error" // exits 1 with a diagnostic stderr (NO Go error) → Class A "adapter exited N" (mirrors a real CLI like codex-cli failing)
)

// Adapter is the fake ModelAccess.
type Adapter struct {
	Scenario Scenario
	n        int // call counter (drives the churn scenario)
}

// New returns a fake adapter for a scenario (defaults to Valid).
func New(s Scenario) *Adapter {
	if s == "" {
		s = Valid
	}
	return &Adapter{Scenario: s}
}

// Name returns the fixed registry key "fake".
func (a *Adapter) Name() string { return "fake" }

// Evidence reports the identity-evidence tier the fake adapter produces (a deterministic local
// invocation tag) so a Client surface can project ACP-validation readiness without invoking it.
func (a *Adapter) Evidence() core.IdentityEvidence { return core.EvidenceInvocationTag }

// Available always reports true: the fake adapter is built in and needs no binary.
func (a *Adapter) Available() (bool, string) { return true, "built-in deterministic fake adapter" }

// Invoke returns deterministic output for the configured scenario. It honors ctx
// cancellation so cancellation tests are deterministic.
func (a *Adapter) Invoke(ctx context.Context, c model.Call) (model.Result, error) {
	if err := ctx.Err(); err != nil {
		return model.Result{ExitCode: 125, Stderr: []byte("fake: canceled")}, err
	}
	a.n++
	want := string(c.ModelArg)
	target := firstFile(c.CopyRoot)

	// Report-only host self-review pass: a distinct deterministic finding (a different
	// kind+location bucket than the reviewer's valid() finding, so fingerprints don't
	// collide). The Manager attributes it as source=author_self_review.
	if c.Phase == "semantic_author_review" { // opaque phase label (author self-review)
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag, Stdout: selfReview(target)}, nil
	}

	switch a.Scenario {
	case Churn:
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag, Stdout: churn(target, a.n)}, nil
	case MalformedThenValid:
		// first call: malformed (drives the schema-failure retry); then valid
		if a.n == 1 {
			return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag,
				Stdout: []byte(`{"schemaVersion":1,"role":"reviewer","verdict":"request_changes","findings":[{"id":"F-001"`)}, nil
		}
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag, Stdout: valid(target)}, nil
	case Timeout:
		return model.Result{ExitCode: 124, Stderr: []byte("fake: simulated timeout")}, fmt.Errorf("fake: timeout")
	case ExitError:
		// A non-zero exit with NO Go error → the Class A "adapter %q exited %d" halt (mirrors a
		// real CLI like codex-cli failing). The stderr deliberately contains a home path + an
		// API-key-shaped token so tests prove the surface sanitizer redacts both.
		return model.Result{ExitCode: 1, Stderr: []byte(
			"codex-cli: error: request failed (HTTP 401 Unauthorized)\n" +
				"  config: /Users/example/.codex/auth.json\n" +
				"  API_KEY=sk-fake-abcdef0123456789abcdef\n" +
				"  hint: run `codex login` and retry")}, nil
	case Malformed:
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag,
			Stdout: []byte(`{"schemaVersion":1,"role":"reviewer","verdict":"request_changes","findings":[{"id":"F-001"`)}, nil
	case SchemaInvalid:
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag,
			Stdout: []byte(`{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","verdict":"request_changes","findings":[{"id":"F-001","kind":"bug","title":"bad enum"}]}`)}, nil
	case IdentityMismatch:
		return model.Result{ExitCode: 0, ActualModel: "fake-9-wrong", Evidence: core.EvidenceInvocationTag,
			Stdout: approve()}, nil
	case Empty:
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag, Stdout: approve()}, nil
	case Duplicate:
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag, Stdout: duplicate(target)}, nil
	case ProtectedPath:
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag,
			Stdout: protectedPath(target)}, nil
	case UnshownFile:
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag,
			Stdout: []byte(`{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","summary":"phantom","verdict":"request_changes","findings":[{"id":"F-1","kind":"fail","severity":"high","title":"Phantom","file":"ghost-not-in-ws.go","location":"1","source":"reviewer"}]}`)}, nil
	default: // Valid
		return model.Result{ExitCode: 0, ActualModel: want, Evidence: core.EvidenceInvocationTag, Stdout: valid(target)}, nil
	}
}

func approve() []byte {
	return []byte(`{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","summary":"No actionable issues.","verdict":"approve","findings":[]}`)
}

func valid(target string) []byte {
	return fmt.Appendf(nil, `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","summary":"One issue found.","verdict":"request_changes","findings":[{"id":"F-001","kind":"fail","severity":"high","title":"Example finding","detail":"A deterministic finding from the fake adapter.","suggestion":"Address it.","file":%q,"location":"1","source":"reviewer","primitive":"P1"}]}`, target)
}

func selfReview(target string) []byte {
	return fmt.Appendf(nil, `{"schemaVersion":1,"role":"author_remediator","phase":"semantic_author_review","summary":"Host self-review.","verdict":"request_changes","findings":[{"id":"H-001","kind":"risk","severity":"medium","title":"Host self-review note","detail":"A deterministic self-review finding from the fake host.","file":%q,"location":"90","source":"reviewer","primitive":"P1"}]}`, target)
}

func churn(target string, n int) []byte {
	// a new line location each call → a new fingerprint → never already_addressed
	loc := fmt.Sprintf("%d", n*100)
	return fmt.Appendf(nil, `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","summary":"churning","verdict":"request_changes","findings":[{"id":"F-%d","kind":"fail","severity":"high","title":"Churn %d","file":%q,"location":%q,"source":"reviewer"}]}`, n, n, target, loc)
}

// protectedPath is one ordinary finding plus one aimed at `.env`. The two carry different kinds
// and locations so they cannot dedup into each other, and the ordinary one comes FIRST so the
// refusal is genuinely mid-set rather than the only thing the write path sees.
func protectedPath(target string) []byte {
	return fmt.Appendf(nil, `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","summary":"One ordinary issue and one in a protected file.","verdict":"request_changes","findings":[{"id":"F-001","kind":"fail","severity":"high","title":"Example finding","detail":"A deterministic finding from the fake adapter.","suggestion":"Address it.","file":%q,"location":"1","source":"reviewer","primitive":"P1"},{"id":"F-002","kind":"risk","severity":"high","title":"Secret in env file","detail":"A deterministic finding aimed at a protected path.","suggestion":"Rotate it.","file":".env","location":"3","source":"reviewer","primitive":"P1"}]}`, target)
}

func duplicate(target string) []byte {
	return fmt.Appendf(nil, `{"schemaVersion":1,"role":"reviewer","phase":"semantic_iterate","summary":"Same issue twice.","verdict":"request_changes","findings":[{"id":"F-001","kind":"fail","severity":"medium","title":"Dup A","file":%q,"location":"1","source":"reviewer"},{"id":"F-002","kind":"fail","severity":"high","title":"Dup B","file":%q,"location":"1","source":"reviewer"}]}`, target, target)
}

// firstFile returns the workspace-relative path of the first regular file under
// root (sorted), so synthesized findings/edits target a real file. "" if none.
func firstFile(root string) string {
	if root == "" {
		return "main.go"
	}
	var found []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr == nil {
			found = append(found, rel)
		}
		return nil
	})
	if len(found) == 0 {
		return "main.go"
	}
	sort.Strings(found)
	return found[0]
}
