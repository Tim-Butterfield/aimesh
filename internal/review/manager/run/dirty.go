package run

// Dirty-tree probing reports whether an apply's workspace already held uncommitted work. After an
// apply the user's edits and aimesh's can share files, and a blunt undo such as `git checkout -- .`
// discards both; patches/changes.patch reverse-applies only aimesh's edits. The write path records the
// probe result as an event and never refuses.
//
// A tree without version control has no dirty state, so the probe reports Known false. That tree's
// only undo is the run's patches/changes.patch, which EnsureDurableRunDir keeps out of the OS temp
// directory.

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

// ReasonWorkspaceDirty is the reason code for a workspace with uncommitted work.
const ReasonWorkspaceDirty = "workspace_dirty"

// dirtyProbeTimeout bounds the status call; a probe that times out reports unknown.
const dirtyProbeTimeout = 20 * time.Second

// maxDirtyPathsReported bounds the reported paths to enough to recognize the tree.
const maxDirtyPathsReported = 10

// WorkspaceDirtiness is what the probe could establish.
type WorkspaceDirtiness struct {
	// Known is false when there is no version control or the status could not be obtained. An
	// unknown result is not treated as evidence either way.
	Known bool
	// VCS names what answered ("git" | "hg"), empty when Known is false.
	VCS string
	// Dirty is meaningful only when Known.
	Dirty bool
	// Paths holds up to maxDirtyPathsReported uncommitted paths; More counts the rest.
	Paths []string
	More  int
}

// ProbeWorkspaceDirtiness asks the workspace's version control whether tracked files have uncommitted
// changes. It uses model.CleanGitEnv so an inherited GIT_DIR or GIT_WORK_TREE cannot point the query at
// another repository.
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

// summarizeStatus parses status output. Untracked files are excluded by the status queries: nothing
// aimesh writes touches them and a checkout does not remove them, so counting them would flag almost
// every repository.
func summarizeStatus(vcs, out string) WorkspaceDirtiness {
	d := WorkspaceDirtiness{Known: true, VCS: vcs}
	for line := range strings.SplitSeq(out, "\n") {
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

// EnsureDurableRunDir returns a durable run directory for an apply into a tree without version
// control, so the run record, which holds that tree's only undo, is not kept in the OS temp directory.
// aimesh does not normally create state directories unasked, but this run is already writing to the
// tree. It reports false, changing nothing, for any other tree or when creation fails.
func EnsureDurableRunDir(ctx context.Context, workspace, mode, artifactDir string) (string, bool) {
	if mode != "apply" || strings.TrimSpace(workspace) == "" {
		return "", false
	}
	// Only the temp fallback is replaced; an artifact directory chosen by an operator or surface stays.
	if artifactDir != localstate.TempRunDir(reviewComponent) {
		return "", false
	}
	if d := ProbeWorkspaceDirtiness(ctx, workspace); d.Known {
		return "", false // a VCS is the way back; nothing to arrange
	}
	if cdir, ok := localstate.ComponentDirFor(workspace, reviewComponent); ok {
		return filepath.Join(cdir, localstate.RunsSubdir), true // already initialized: use it
	}
	home := filepath.Join(workspace, localstate.HomeDirName, reviewComponent, localstate.RunsSubdir)
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", false
	}
	return home, true
}

// reviewComponent is review's subdirectory of the shared `.aimesh/` state root.
const reviewComponent = "review"

// itoa formats a non-negative integer in decimal.
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
