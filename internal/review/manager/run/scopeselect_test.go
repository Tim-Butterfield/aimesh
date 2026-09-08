package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// scopeTree writes a small tree and returns its root.
func scopeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{"a.go", "b.go", "sub/c.go", "sub/deep/d.go", "notes.md"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func resolved(t *testing.T, root string, s Scope) map[string]bool {
	t.Helper()
	got, err := s.Resolve(context.Background(), root)
	if err != nil {
		t.Fatalf("resolve %+v: %v", s, err)
	}
	return got
}

// TestScope_ExplicitPathsAndGlobs — the primitive. Every baseline below produces a file list; this is
// where a file list becomes a scope, so it works in ANY directory by construction.
func TestScope_ExplicitPathsAndGlobs(t *testing.T) {
	root := scopeTree(t)
	cases := map[string]struct {
		patterns []string
		want     []string
	}{
		"one file":                      {[]string{"a.go"}, []string{"a.go"}},
		"two files":                     {[]string{"a.go", "b.go"}, []string{"a.go", "b.go"}},
		"a directory takes its subtree": {[]string{"sub"}, []string{"sub/c.go", "sub/deep/d.go"}},
		"a glob":                        {[]string{"*.go"}, []string{"a.go", "b.go"}},
		"a nested glob":                 {[]string{"sub/*.go"}, []string{"sub/c.go"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := resolved(t, root, Scope{Paths: tc.patterns})
			if len(got) != len(tc.want) {
				t.Fatalf("selected %v, want %v", keysOf(got), tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("%q was not selected; got %v", w, keysOf(got))
				}
			}
		})
	}
}

// TestScope_SelectingNothingIsARefusal is the rule that keeps a mistyped selector from being
// expensive. Falling back to the whole tree would review everything at full cost while the operator
// believed they had narrowed it; proceeding with nothing would report "no findings" about nothing.
func TestScope_SelectingNothingIsARefusal(t *testing.T) {
	_, err := Scope{Paths: []string{"nothing-matches-this-*.xyz"}}.Resolve(context.Background(), scopeTree(t))
	if err == nil {
		t.Fatal("a selector matching no file must be refused")
	}
	if fault.ReasonOf(err) != ReasonScopeEmpty {
		t.Errorf("reason = %q, want %q", fault.ReasonOf(err), ReasonScopeEmpty)
	}
}

// TestScope_EmptySelectorMeansNoNarrowing. nil is "show everything"; an EMPTY map would mean "show
// nothing", and confusing the two would silently review either the whole tree or none of it.
func TestScope_EmptySelectorMeansNoNarrowing(t *testing.T) {
	got, err := Scope{}.Resolve(context.Background(), scopeTree(t))
	if err != nil {
		t.Fatalf("an empty scope is not an error: %v", err)
	}
	if got != nil {
		t.Fatalf("an empty scope must resolve to nil (no narrowing), got %v", keysOf(got))
	}
}

// TestScope_ChangedSinceWorksWithNoVersionControl is the baseline the user's correction is about.
// Not all usage is repo-focused: `folder init` is a peer of `repo init`, and a plain folder needs a
// real "what did I just change" rather than a documented gap.
func TestScope_ChangedSinceWorksWithNoVersionControl(t *testing.T) {
	root := scopeTree(t)
	// Age everything, then touch one file.
	old := time.Now().Add(-24 * time.Hour)
	for _, rel := range []string{"a.go", "b.go", "sub/c.go", "sub/deep/d.go", "notes.md"} {
		if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(rel)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package x // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := resolved(t, root, Scope{ChangedSince: "1h"})
	if len(got) != 1 || !got["b.go"] {
		t.Fatalf("only the recently modified file must be selected, got %v", keysOf(got))
	}
	// And an RFC3339 stamp is accepted as well as a duration.
	if g := resolved(t, root, Scope{ChangedSince: time.Now().Add(-time.Hour).Format(time.RFC3339)}); !g["b.go"] {
		t.Errorf("an RFC3339 window must work too, got %v", keysOf(g))
	}
}

// TestScope_AVCSBaselineInAPlainFolderIsRefused, never ignored. Ignoring it would review the WHOLE
// tree — the opposite of what was asked, at full cost — and the refusal names the baselines that DO
// work there, so it is a redirection rather than a dead end.
func TestScope_AVCSBaselineInAPlainFolderIsRefused(t *testing.T) {
	_, err := Scope{VCSRef: "diff"}.Resolve(context.Background(), scopeTree(t))
	if err == nil {
		t.Fatal("a VCS baseline in a non-repository must be refused")
	}
	if fault.ReasonOf(err) != ReasonScopeUnavailable {
		t.Errorf("reason = %q, want %q", fault.ReasonOf(err), ReasonScopeUnavailable)
	}
	for _, want := range []string{"--changed-since", "--path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must point at a baseline that works here (%s): %v", want, err)
		}
	}
}

// TestScope_VCSBaselineInARepository — the repo half of the same capability.
func TestScope_VCSBaselineInARepository(t *testing.T) {
	root := scopeTree(t)
	gitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package x // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "add", "-A")
	c.Dir, c.Env = root, model.CleanGitEnv()
	_ = c.Run()

	got := resolved(t, root, Scope{VCSRef: "diff"})
	if !got["b.go"] {
		t.Fatalf("the edited file must be selected, got %v", keysOf(got))
	}
	if got["a.go"] {
		t.Errorf("an unchanged file must not be selected, got %v", keysOf(got))
	}
}

// TestScope_BaselinesUnion. Naming two means "review what either selects" — the reading that matches
// how a person says it ("the files I touched, plus this one I am worried about"). The intersection
// reading has no natural phrasing at all.
func TestScope_BaselinesUnion(t *testing.T) {
	root := scopeTree(t)
	old := time.Now().Add(-24 * time.Hour)
	for _, rel := range []string{"a.go", "b.go", "sub/c.go", "sub/deep/d.go", "notes.md"} {
		_ = os.Chtimes(filepath.Join(root, filepath.FromSlash(rel)), old, old)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := resolved(t, root, Scope{Paths: []string{"notes.md"}, ChangedSince: "1h"})
	if !got["notes.md"] || !got["b.go"] {
		t.Fatalf("both baselines must contribute, got %v", keysOf(got))
	}
	if got["a.go"] {
		t.Errorf("nothing else may be selected, got %v", keysOf(got))
	}
}

// TestScopeSummary_SaysWhatItWasSilentAbout. A narrowed review says nothing about the rest of the
// tree, and that silence must not read as approval — which is the same rule the withheld list exists
// to enforce one layer down.
func TestScopeSummary_SaysWhatItWasSilentAbout(t *testing.T) {
	s := Scope{Paths: []string{"a.go"}}
	got := summarizeScope(s, map[string]bool{"a.go": true}, 5)
	if got.Selected != 1 || got.Available != 5 {
		t.Errorf("summary = %+v", got)
	}
	if got.Note != ScopeNote {
		t.Error("the summary must carry the fixed note")
	}
	for _, phrase := range []string{"NARROWED", "nobody looked", "Containment is unchanged"} {
		if !strings.Contains(got.Note, phrase) {
			t.Errorf("the note must state %q:\n%s", phrase, got.Note)
		}
	}
	// An unnarrowed run gets no summary at all — it needs no disclaimer about what it did not see.
	if summarizeScope(Scope{}, nil, 5) != nil {
		t.Error("a full-tree run must produce no scope summary")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
