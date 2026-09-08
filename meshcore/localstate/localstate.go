// Package localstate discovers a project's VCS root and initializes a local, VCS-excluded `.aimesh/`
// state directory (config, run artifacts, containment copies). It is a GIT-FOCUSED SUBSET of aikit's
// `init` semantics: a pure-filesystem walk-up finds the nearest `.git` (directory or file) or `.hg`
// directory; the `.git`-file (worktree/submodule) case skips the exclude step rather than chase the
// real git dir; Mercurial ignore-registration is not yet supported. Domain-free — no app vocabulary.
package localstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HomeDirName is the project-local aimesh state directory; TempSubdir is its scratch/containment area
// (named `temp`, matching aikit's `.aikit/temp`).
const (
	HomeDirName = ".aimesh"
	TempSubdir  = "temp"
)

// ErrRepoNotFound is returned by Init in repo mode when no VCS root is found (mirrors aikit's
// blocked_repo_not_found).
var ErrRepoNotFound = errors.New("not inside a Git or Mercurial repository; use `folder init` for a plain directory")

// ErrInsideRepo is returned by Init in folder mode when a VCS root IS found — folder mode refuses to
// create an un-ignored .aimesh inside a repo; use `repo init`.
var ErrInsideRepo = errors.New("inside a repository; use `repo init` so .aimesh is VCS-excluded")

// gitEntry reports whether dir has a `.git` entry and whether it is a directory (worktrees use a file).
func gitEntry(dir string) (present, isDir bool) {
	fi, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false, false
	}
	return true, fi.IsDir()
}

func hgDir(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".hg"))
	return err == nil && fi.IsDir()
}

// FindRoot walks up from start to the nearest directory containing a `.git` (dir or file) or `.hg` dir
// (git wins ties). ok=false when none is found (a non-repo tree).
func FindRoot(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		if present, _ := gitEntry(dir); present {
			return dir, true
		}
		if hgDir(dir) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // filesystem root
		}
		dir = parent
	}
}

// InitMode selects how Init resolves the target directory.
type InitMode int

const (
	InitAuto   InitMode = iota // repo if a root is found, else folder
	InitRepo                   // require a repo (error if none)
	InitFolder                 // require a non-repo dir (error if inside one)
)

// InitResult reports what Init did.
type InitResult struct {
	Home     string   // the ensured .aimesh directory
	Base     string   // the directory .aimesh lives under (repo root, or start dir for folder mode)
	Repo     bool     // a VCS root was found
	Excluded string   // the ignore file updated with /.aimesh/, or ""
	Actions  []string // human-readable actions taken
}

// Init creates <base>/.aimesh/ + <base>/.aimesh/temp/ and, in a git repo whose `.git` is a DIRECTORY,
// idempotently appends `/.aimesh/` to `.git/info/exclude`. Directories only — no seeded config (adapter
// detection is a separate explicit step).
func Init(start string, mode InitMode) (InitResult, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return InitResult{}, err
	}
	root, found := FindRoot(abs)
	var res InitResult
	switch mode {
	case InitRepo:
		if !found {
			return InitResult{}, ErrRepoNotFound
		}
		res.Base, res.Repo = root, true
	case InitFolder:
		if found {
			return InitResult{}, ErrInsideRepo
		}
		res.Base = abs
	default: // InitAuto
		if found {
			res.Base, res.Repo = root, true
		} else {
			res.Base = abs
		}
	}

	home := filepath.Join(res.Base, HomeDirName)
	if err := os.MkdirAll(filepath.Join(home, TempSubdir), 0o755); err != nil {
		return InitResult{}, fmt.Errorf("create %s: %w", filepath.Join(home, TempSubdir), err)
	}
	res.Home = home
	res.Actions = append(res.Actions, "ensured "+home+"/ and "+filepath.Join(HomeDirName, TempSubdir)+"/")

	if res.Repo {
		if present, isDir := gitEntry(res.Base); present && isDir {
			excl := filepath.Join(res.Base, ".git", "info", "exclude")
			added, err := appendExclude(excl, "/"+HomeDirName+"/")
			if err != nil {
				return res, fmt.Errorf("update %s: %w", excl, err)
			}
			res.Excluded = excl
			if added {
				res.Actions = append(res.Actions, "added /"+HomeDirName+"/ to .git/info/exclude")
			} else {
				res.Actions = append(res.Actions, "/"+HomeDirName+"/ already in .git/info/exclude")
			}
		} else {
			// .git is a FILE (worktree/submodule): the real info/exclude needs gitdir chasing — skip
			// with a notice rather than guess.
			res.Actions = append(res.Actions, "note: .git is not a directory (worktree?) — add /"+HomeDirName+"/ to your ignore rules manually")
		}
	}
	return res, nil
}

// appendExclude idempotently appends line to the git exclude file (creating it + info/ if needed).
// added=false when the exact line is already present.
func appendExclude(path, line string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for l := range strings.SplitSeq(string(b), "\n") {
		if strings.TrimSpace(l) == line {
			return false, nil
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	prefix := ""
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + line + "\n"); err != nil {
		return false, err
	}
	return true, nil
}
