package model

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitInitIsolatedDir turns an isolated, throwaway directory into a git repository so provider CLIs
// that require a trusted or git directory accept it.
//
// Git runs in a clean environment (every inherited GIT_*, HOME and XDG_CONFIG_HOME dropped, an empty
// global config and no templates), so no inherited variable can redirect it at another repository or
// inject configuration. It is best effort with a short timeout; a failure is ignored, and the caller's
// folder-trust diagnostic covers a remaining rejection.
func GitInitIsolatedDir(ctx context.Context, dir string) {
	tmpl, err := os.MkdirTemp("", "aimesh-gittmpl-")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmpl)
	emptyCfg := filepath.Join(tmpl, "empty.gitconfig")
	if werr := os.WriteFile(emptyCfg, nil, 0o600); werr != nil {
		return
	}
	env := append(CleanGitEnv(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+emptyCfg, // a guaranteed-empty global config (cross-platform; not a null device)
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=aimesh", "GIT_AUTHOR_EMAIL=isolated@aimesh.local",
		"GIT_COMMITTER_NAME=aimesh", "GIT_COMMITTER_EMAIL=isolated@aimesh.local",
	)
	run := func(args ...string) error {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		return cmd.Run()
	}
	if run("init", "-q", "--template="+tmpl) != nil {
		return // git unavailable / poisoned → best-effort; the diagnostic explains a residual reject
	}
	// A best-effort commit gives a valid HEAD for adapters whose gate is stricter than repo existence.
	_ = run("add", "-A")
	_ = run("commit", "-q", "--no-gpg-sign", "-m", "aimesh isolated workspace")
}

// CleanGitEnv returns a minimal environment with every git-control + user-config variable removed
// (all GIT_*, HOME, XDG_CONFIG_HOME), keeping only the basics a subprocess needs so git cannot read
// inherited config or be redirected by an inherited GIT_DIR / GIT_WORK_TREE / GIT_CONFIG_COUNT.
func CleanGitEnv() []string {
	keep := map[string]bool{
		"PATH": true, "PATHEXT": true, // resolve the git binary (+ Windows extensions)
		"SystemRoot": true, "SYSTEMROOT": true, "ComSpec": true, "COMSPEC": true, // Windows basics
		"TEMP": true, "TMP": true, "TMPDIR": true, // temp dirs
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if ok && keep[k] {
			out = append(out, kv)
		}
	}
	return out
}
