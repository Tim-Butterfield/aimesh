package model

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitInitIsolatedDir turns an ISOLATED, throwaway directory into a git repository so provider CLIs
// that gate on a trusted / git directory (Codex's `--skip-git-repo-check`, and several CLIs' folder-
// trust prompts) accept it.
//
// It runs in a CLEAN environment — every inherited GIT_*, HOME and XDG_CONFIG_HOME is dropped — so no
// inherited git control variable (GIT_DIR / GIT_WORK_TREE / GIT_CONFIG_COUNT / templates / hooks) can
// redirect the commands at a NON-isolated repository or inject configuration. It is HERMETIC (it reads
// no user git config and copies no user templates or hooks) and BEST-EFFORT with a short timeout: any
// failure — git absent, a poisoned wrapper — is ignored, and the caller's folder-trust diagnostic
// covers a residual rejection.
//
// It is deliberately in meshcore rather than in one app: the reviewmesh web UI's ACP validation and
// meshcore's own deep adapter probe both need EXACTLY this preparation, and two copies of a hermetic
// environment scrub is two chances to get one of them wrong.
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
