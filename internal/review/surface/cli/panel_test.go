package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// The --reviewer grammar is key=value because a model tag may contain a colon.
func TestParseSeatSpec_Grammar(t *testing.T) {
	seat, err := parseSeatSpec("adapter=ollama,model=llama3:8b,effort=high")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if seat.Adapter != "ollama" || seat.Model != "llama3:8b" || seat.Effort != "high" {
		t.Errorf("parsed %+v; a model tag's colon must survive verbatim", seat)
	}
	if s, err := parseSeatSpec("adapter=a,model=m,"); err != nil || s.Model != "m" {
		t.Errorf("a trailing comma must be tolerated, got %+v / %v", s, err)
	}
	for _, tc := range []struct{ spec, want string }{
		{"adapter=a", "both adapter and model are required"},
		{"model=m", "both adapter and model are required"},
		{"adapter=a,model=m,adapter=b", "duplicate key"},
		{"adapter=a,model=m,role=reviewer", "unknown key"},
		{"adapter", "invalid field"},
	} {
		_, err := parseSeatSpec(tc.spec)
		if err == nil {
			t.Errorf("%q must be rejected", tc.spec)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q → %v, want it to mention %q", tc.spec, err, tc.want)
		}
		if fault.CodeOf(err) != fault.Usage {
			t.Errorf("%q → exit code %v, want a usage error", tc.spec, fault.CodeOf(err))
		}
	}
}

// Flag order is panel order, and an oversized panel is refused rather than trimmed.
func TestParseReviewerPanel_OrderAndCap(t *testing.T) {
	seats, err := parseReviewerPanel([]string{"adapter=a,model=m1", "adapter=b,model=m2", "adapter=c,model=m3"})
	if err != nil {
		t.Fatalf("parse panel: %v", err)
	}
	if len(seats) != 3 || seats[0].Adapter != "a" || seats[2].Adapter != "c" {
		t.Fatalf("panel order must follow flag order, got %+v", seats)
	}
	many := make([]string, 0, 17)
	for range 17 {
		many = append(many, "adapter=a,model=m")
	}
	if _, err := parseReviewerPanel(many); err == nil || !strings.Contains(err.Error(), "panel cap") {
		t.Errorf("17 seats must be refused with the cap named, got %v", err)
	}
}

// Composing a panel and selecting a profile answer the same question, so naming both is a usage
// error.
func TestReview_ReviewerAndProfileAreMutuallyExclusive(t *testing.T) {
	var out, errw bytes.Buffer
	code := runReview([]string{"--reviewer", "adapter=fake,model=fake-model", "--profile", "default", t.TempDir()}, &out, &errw)
	if code != int(fault.Usage) {
		t.Fatalf("exit code = %d, want %d (usage)", code, int(fault.Usage))
	}
	if !strings.Contains(errw.String(), "cannot be combined with --profile") {
		t.Errorf("stderr = %q, want the mutual-exclusion message", errw.String())
	}
}

// A per-role override cannot address one of several seats, so combining it with an ad-hoc panel is
// refused.
func TestReview_ReviewerAndSetReviewerConflict(t *testing.T) {
	var out, errw bytes.Buffer
	code := runReview([]string{"--reviewer", "adapter=fake,model=fake-model", "--set", "reviewer.adapter=fake", t.TempDir()}, &out, &errw)
	if code != int(fault.Usage) {
		t.Fatalf("exit code = %d, want %d (usage)", code, int(fault.Usage))
	}
	if !strings.Contains(errw.String(), "--set reviewer.*") {
		t.Errorf("stderr = %q, want the --set/--reviewer conflict message", errw.String())
	}
}
