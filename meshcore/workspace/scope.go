package workspace

import (
	"path/filepath"
	"runtime"
	"strings"
)

// defaultExcludedDirs are directory names never copied, diffed, committed or targeted. They cover
// VCS, build and dependency output; agent and IDE client configuration, which is excluded from the
// copy as well as write-denied, because a copied file can be shown to a model and proposed as an
// edit; and this tool's state directories, including older per-application names. A name belongs
// here only if it exists on machines this tool has never seen.
var defaultExcludedDirs = map[string]struct{}{
	".git":         {},
	"tmp":          {},
	"node_modules": {},
	"vendor":       {},
	"dist":         {},
	"build":        {},
	"coverage":     {},
	".cache":       {},
	".vscode":      {},
	".idea":        {},
	".windsurf":    {},
	".claude":      {},
	".cursor":      {},
	".codex":       {},
	".gemini":      {},
	".aimesh":      {},
	".reviewmesh":  {},
	".exploremesh": {},
}

// defaultExcludedFiles are component names never copied/targeted (keys are lower-cased;
// lookups case-fold the candidate, so this matches any casing).
var defaultExcludedFiles = map[string]struct{}{
	".ds_store":                  {},
	".mcp.json":                  {},
	"claude_desktop_config.json": {},
}

// IsExcluded reports whether any component of a workspace-relative path is an excluded directory or
// file name. File names are matched per component too, so a directory named `.mcp.json` cannot
// expose its children.
func IsExcluded(rel string) bool {
	rel = filepath.ToSlash(rel)
	for part := range strings.SplitSeq(rel, "/") {
		if part == "" || part == "." || part == ".." {
			continue
		}
		// Trailing dots/spaces are dropped by Windows when a file is created, so `.git `
		// names the real `.git`; compare the trimmed, case-folded component.
		part = normalizeName(part)
		if _, ok := defaultExcludedDirs[part]; ok { // case-fold for case-insensitive FS
			return true
		}
		if _, ok := defaultExcludedFiles[part]; ok {
			return true
		}
	}
	return false
}

// noteworthyExclusion reports whether an exclusion is worth reporting. Conventional VCS, build and
// dependency directories are excluded silently. Client configuration is reported, because it holds
// rules, prompts and hooks a user may expect to be read, and --allow-protected-paths includes it.
func noteworthyExclusion(rel string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." || part == ".." {
			continue
		}
		n := normalizeName(part)
		if _, ok := noteworthyExcludedNames[n]; ok {
			return true
		}
	}
	return false
}

// noteworthyExcludedNames are the client-configuration directory and file names among the
// exclusions. This tool's own state directories are not included: runs create them in the
// workspace, so reporting them would add the same line to every run.
var noteworthyExcludedNames = map[string]struct{}{
	".vscode": {}, ".idea": {}, ".windsurf": {},
	".claude": {}, ".cursor": {}, ".codex": {}, ".gemini": {},
	".mcp.json": {}, "claude_desktop_config.json": {},
}

// excludedRule names the component that caused an exclusion, so a caveat shows which rule fired.
func excludedRule(rel string) string {
	for part := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." || part == ".." {
			continue
		}
		n := normalizeName(part)
		if _, ok := defaultExcludedDirs[n]; ok {
			return n + "/**"
		}
		if _, ok := defaultExcludedFiles[n]; ok {
			return n
		}
	}
	return ""
}

// excludedDetail says what a caveat covers; a directory caveat covers its whole subtree.
func excludedDetail(isDir bool) string {
	if isDir {
		return "excluded from review: this directory and everything under it was not shown to any reviewer"
	}
	return "excluded from review: this file was not shown to any reviewer"
}

// safeRel reports whether a workspace-relative path stays inside the workspace (not
// absolute or rooted, no ".." escape). It rejects a path rooted by either platform's conventions,
// even where this platform's filepath.IsAbs would not (such as "/abs/x" on Windows or "\abs\x").
func safeRel(rel string) bool {
	// Empty is not a usable relative path; a leading '/' (POSIX root) or '\' (Windows
	// drive-relative root / UNC) is rooted on every platform. (The index is guarded by the
	// empty check.)
	if rel == "" || rel[0] == '/' || rel[0] == '\\' {
		return false
	}
	// Reject a drive-letter prefix ("C:\x", "C:rel") on every platform. filepath.VolumeName
	// detects it only on Windows, so it is checked explicitly.
	if len(rel) >= 2 && rel[1] == ':' &&
		((rel[0] >= 'A' && rel[0] <= 'Z') || (rel[0] >= 'a' && rel[0] <= 'z')) {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	// Reject any volume name (Windows drive-letter / UNC): `C:..\x` is not "absolute"
	// per filepath.IsAbs but escapes when joined onto a same-drive workspace root.
	if filepath.VolumeName(rel) != "" {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(rel))
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

// SafeRel reports whether a workspace-relative path stays inside the workspace; see safeRel.
func SafeRel(rel string) bool { return safeRel(rel) }

// protectedAncestorDirs are excluded directory names whose content is protected, so a workspace
// root inside one is refused. Exclusion is otherwise judged relative to the root, so a root of
// `/trusted/.vscode` would make `mcp.json` an ordinary file.
//
// Generic build names are not members: nothing in them is protected, and repositories legitimately
// live under directories such as `build` or `tmp`. This tool's state directories are not members
// either: previous runs stored there are legitimate targets, and the directories stay write-denied
// and excluded from workspace copies.
var protectedAncestorDirs = map[string]string{
	".git":      ".git/** (includes .git/config and .git/hooks/**)",
	".vscode":   ".vscode/** (includes .vscode/mcp.json)",
	".idea":     ".idea/**",
	".windsurf": ".windsurf/**",
	".claude":   ".claude/**",
	".cursor":   ".cursor/**",
	".codex":    ".codex/**",
	".gemini":   ".gemini/**",
}

// protectedAncestorFiles are excluded file names that also refuse a root beneath a directory of
// that name. `.ds_store` is not listed because it protects nothing.
var protectedAncestorFiles = map[string]string{
	".mcp.json":                  ".mcp.json",
	"claude_desktop_config.json": "claude_desktop_config.json",
}

// protectedAncestorRule returns the rule refusing p because p or one of its ancestors is a
// protected path, or "". p must be absolute.
func protectedAncestorRule(p string) string {
	for part := range strings.SplitSeq(filepath.ToSlash(filepath.Clean(p)), "/") {
		if part == "" || part == "." || part == ".." {
			continue
		}
		part = normalizeName(part)
		if rule, ok := protectedAncestorDirs[part]; ok {
			return rule
		}
		if rule, ok := protectedAncestorFiles[part]; ok {
			return rule
		}
	}
	return ""
}

// rootForms returns the forms of a workspace root to judge: as given, absolute and
// symlink-resolved, without duplicates. The resolved form catches a link such as `/trusted/link` to
// `/trusted/.vscode`. A form that cannot be computed is omitted; the others are still judged.
func rootForms(p string) []string {
	forms := []string{filepath.Clean(p)}
	abs, err := filepath.Abs(p)
	if err == nil && !containsForm(forms, abs) {
		forms = append(forms, abs)
	}
	if resolved, rerr := filepath.EvalSymlinks(forms[len(forms)-1]); rerr == nil && !containsForm(forms, resolved) {
		forms = append(forms, resolved)
	}
	return forms
}

func containsForm(list []string, p string) bool {
	for _, e := range list {
		if samePath(e, p) {
			return true
		}
	}
	return false
}

// excludedTarget reports whether a copy target must be refused by its own name. A directory target
// is judged by its basename, so a repository under `.../build/myrepo` is allowed. A single-file
// target is also refused when it sits directly inside an excluded directory (such as
// `.git/config`). Protected ancestors are judged separately by protectedAncestorRule.
func excludedTarget(p string, isDir bool) bool {
	p = filepath.Clean(p)
	base := normalizeName(filepath.Base(p))
	if _, ok := defaultExcludedDirs[base]; ok {
		return true
	}
	if _, ok := defaultExcludedFiles[base]; ok {
		return true
	}
	if isDir {
		return false
	}
	_, ok := defaultExcludedDirs[normalizeName(filepath.Base(filepath.Dir(p)))]
	return ok
}

// normalizeName lower-cases a path component and strips the trailing spaces and dots Windows drops
// on create, plus, on Windows only, an NTFS stream suffix. It matches scope's normalization.
func normalizeName(s string) string {
	if streamAliasing {
		if i := strings.IndexByte(s, ':'); i >= 0 {
			s = s[:i]
		}
	}
	return strings.ToLower(strings.TrimRight(s, " ."))
}

// streamAliasing mirrors scope's gate: NTFS stream suffixes are stripped only on Windows.
var streamAliasing = runtime.GOOS == "windows"
