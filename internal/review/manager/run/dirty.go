package run

// THE DIRTY-TREE PRECONDITION — refusing to entangle our edits with the user's uncommitted ones.
//
// THE HARM, stated exactly, because the mitigation only makes sense against it. An `apply` writes
// into the live tree. The existing defences are strong against a DIFFERENT failure: the base-hash
// pins catch a targeted file changing DURING the run, and they are checked twice, the second time
// inside the commit window. None of them looks at whether the tree ALREADY held uncommitted work.
//
// If it did, then after the apply the user's edits and ours occupy the same files, and the undo they
// would reach for — `git checkout -- .` — discards both. They lose work they authored, silently,
// while every safety mechanism reports success. That is the one failure in this write path that
// destroys something the tool did not create.
//
// WHAT THIS IS NOT. It is not a claim that a clean tree makes an apply safe; it is the removal of one
// specific way for a correct apply to cost the user their own work. And it is not a VCS integration:
// nothing here reads history, branches, or an index beyond "is anything uncommitted".
//
// WHERE THE RULE DOES NOT REACH, and why that is handled elsewhere rather than papered over. A tree
// with no VCS has no "dirty" state — there is no committed baseline to differ from — so this returns
// `known: false` and NOTHING is refused. Refusing every non-repo apply would break `folder init`,
// which is a supported shape, on the strength of a question that cannot be asked there. The undo for
// that tree is the run's own `patches/changes.patch`, and making it durable and visible is the other
// half of the answer (see the apply path's disclosure).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/localstate"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// ReasonWorkspaceDirty is the STABLE MACHINE code for the refusal.
const ReasonWorkspaceDirty = "workspace_dirty"

// dirtyProbeTimeout bounds the status call. A status that hangs must not hang the review: the probe
// is a precondition, and an unanswerable precondition is reported as unknown rather than waited on.
const dirtyProbeTimeout = 20 * time.Second

// maxDirtyPathsReported bounds the file list in the refusal. The point is to let the operator
// recognize the tree, not to reproduce `git status`.
const maxDirtyPathsReported = 10

// WorkspaceDirtiness is what the probe could establish.
type WorkspaceDirtiness struct {
	// Known is false when the question does not apply (no VCS) or could not be answered (no git/hg
	// binary, a status that failed or timed out). NOTHING is refused when it is false — an
	// unanswerable question is not evidence, and treating it as evidence in either direction would
	// be the guessing this codebase refuses everywhere else.
	Known bool
	// VCS names what answered ("git" | "hg"), empty when Known is false.
	VCS string
	// Dirty is meaningful only when Known.
	Dirty bool
	// Paths are up to maxDirtyPathsReported of the uncommitted paths, so the refusal names the tree
	// rather than merely asserting something about it. Truncated reports More.
	Paths []string
	More  int
}

// ProbeWorkspaceDirtiness asks the workspace's VCS whether anything is uncommitted.
//
// It runs with model.CleanGitEnv so an inherited GIT_DIR / GIT_WORK_TREE cannot redirect the question
// at a different repository — the same precaution the isolated-workspace git init takes, and for the
// same reason: a status answered about the wrong tree is worse than no answer.
func ProbeWorkspaceDirtiness(ctx context.Context, workspace string) WorkspaceDirtiness {
	root := strings.TrimSpace(workspace)
	if root == "" {
		return WorkspaceDirtiness{}
	}
	if _, ok := localstate.FindRoot(root); !ok {
		// No VCS above this path at all: the question is not merely unanswered, it does not apply.
		return WorkspaceDirtiness{}
	}
	cctx, cancel := context.WithTimeout(ctx, dirtyProbeTimeout)
	defer cancel()
	// git first, then hg — the same precedence FindRoot uses when both markers exist.
	for _, probe := range []struct {
		vcs  string
		bin  string
		args []string
	}{
		{"git", "git", []string{"status", "--porcelain", "--untracked-files=no"}},
		{"hg", "hg", []string{"status", "--modified", "--added", "--removed", "--deleted"}},
	} {
		c := exec.CommandContext(cctx, probe.bin, probe.args...)
		c.Dir = root
		c.Env = model.CleanGitEnv()
		out, err := c.Output()
		if err != nil {
			continue // not this VCS, or its binary is absent — try the next, then give up
		}
		return summarizeStatus(probe.vcs, string(out))
	}
	return WorkspaceDirtiness{}
}

// summarizeStatus turns a porcelain status into the answer.
//
// UNTRACKED FILES ARE DELIBERATELY EXCLUDED (`--untracked-files=no`, and hg's status is asked only
// for tracked states). An untracked file cannot be entangled with an edit to a tracked one: nothing
// this tool writes touches it, and `git checkout -- .` does not remove it either. Counting it would
// refuse an apply in every repository with a stray build artifact or a scratch note, which is most of
// them — and a precondition that fires constantly is one an operator learns to override by reflex.
func summarizeStatus(vcs, out string) WorkspaceDirtiness {
	d := WorkspaceDirtiness{Known: true, VCS: vcs}
	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		d.Dirty = true
		// Both porcelain formats put status flags first and the path last; the path is what an
		// operator needs to recognize the tree.
		if fields := strings.Fields(s); len(fields) > 1 {
			s = fields[len(fields)-1]
		}
		if len(d.Paths) < maxDirtyPathsReported {
			d.Paths = append(d.Paths, s)
			continue
		}
		d.More++
	}
	return d
}

// Summary renders the dirtiness for a message: the VCS that answered and what it found.
func (d WorkspaceDirtiness) Summary() string {
	if !d.Known {
		return "no version control answered for this tree, so uncommitted work is not a question that can be asked here"
	}
	if !d.Dirty {
		return d.VCS + " reports no uncommitted changes"
	}
	s := d.VCS + " reports uncommitted changes in " + strings.Join(d.Paths, ", ")
	if d.More > 0 {
		s += " (and " + itoa(d.More) + " more)"
	}
	return s
}

// EnsureDurableRunDir creates the project state directory for a workspace that is about to be
// WRITTEN and has no version control, so the run record — and with it the only undo that tree will
// ever have — does not land in the OS temp directory.
//
// THE ORDERING THAT MAKES THIS NECESSARY. `localstate.RunDir` falls back to OS temp when no
// `.aimesh/` exists, and it exists only if someone ran `init`. So the tree with no VCS AND no init is
// simultaneously the one with no other way back and the one whose recovery artifact is stored
// somewhere self-cleaning. That is the wrong way round, and it is a defect independent of the
// dirty-tree question.
//
// CREATING A DIRECTORY UNASKED is normally refused here — `localstate.ProjectHome` says so
// explicitly, and it is right: inventing state in someone's tree is a surprise. This is the one case
// where the reasoning inverts. We are ALREADY about to write into this tree, by explicit instruction;
// declining to also write the means of undoing that would be restraint aimed at the wrong thing.
//
// It is a no-op for every other tree: a repository has a VCS, and an initialized folder already has
// the directory. A failure is not fatal — the run proceeds and keeps its temp-directory artifacts,
// which is what it did before this existed.
func EnsureDurableRunDir(ctx context.Context, workspace, mode, artifactDir string) (string, bool) {
	if mode != "apply" || strings.TrimSpace(workspace) == "" {
		return "", false
	}
	// ONLY the fallback location is claimed, never a configured one. `localstate.RunDir` returns this
	// exact path when nothing else was resolved; anything else is a directory an operator or a surface
	// chose, and relocating THAT would be this feature overruling a deliberate decision in order to
	// protect against a hazard the operator may already have handled.
	if artifactDir != localstate.RunDir(reviewComponent, "") {
		return "", false
	}
	if d := ProbeWorkspaceDirtiness(ctx, workspace); d.Known {
		return "", false // a VCS is the way back; nothing to arrange
	}
	if _, ok := localstate.ComponentDir(reviewComponent); ok {
		return "", false // already initialized: the run directory is durable
	}
	home := filepath.Join(workspace, localstate.HomeDirName, reviewComponent, localstate.RunsSubdir)
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", false
	}
	return home, true
}

// reviewComponent is review's subdirectory of the shared `.aimesh/` state root.
const reviewComponent = "review"

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
