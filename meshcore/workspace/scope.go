package workspace

import (
	"path/filepath"
	"runtime"
	"strings"
)

// defaultExcludedDirs are directory names never copied, diffed, committed, or
// targeted by review. They are internal/generated/local folders that must be safe
// against `aimesh review run --apply .` at a repo root.
//
// The agent/IDE client-config family (`.vscode`, `.idea`, `.windsurf`, `.claude`,
// `.cursor`, `.codex`, `.gemini`, and this tool's own `.aimesh`) is excluded from the COPY
// as well as write-denied by meshcore/scope. Write-denial alone is not enough: a file that
// is copied is a file that can be SHOWN to a model, proposed as an edit, and accepted by a
// human who is reading a diff rather than auditing a path — and `.vscode/mcp.json` or
// `.claude/settings.json` is executable configuration, not source. Excluding it from the
// copy removes the whole chain.
//
// `.reviewmesh` and `.exploremesh` are LEGACY locations — nothing writes them since state
// moved under `.aimesh/{review,explore}` — and they stay listed precisely because of that.
// Anyone who used an earlier build still has those directories on disk with real
// configuration in them; dropping the entries would newly expose exactly the population
// that has something to expose. An entry in a denylist costs two lines and protects a file
// we cannot prove is absent from a user's machine.
// WHAT EARNS A PLACE HERE: a name that is present on machines this tool has never seen. `.git` and
// the build/artifact names are ecosystem conventions; the client-config family ships with widely
// deployed agent/IDE tooling; `.aimesh` (and its legacy spellings) is this tool's own state.
//
// `.aikit` was removed for failing exactly that test. It is one author's local tool, and a
// general-purpose reviewer has no business assuming it exists in anyone else's environment: on every
// other machine the entry matched nothing, and on the one machine it did match it was the STRICTEST
// tier — refusing review targets under a directory whose conventions the tool had simply absorbed
// from where it happened to be developed. A private path in a shipped denylist is a development
// environment leaking into a product.
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

// IsExcluded reports whether a workspace-relative path is excluded from review: any path
// component is an excluded directory or an excluded file name. Excluded FILE names are
// matched per component too (not only on the basename), so `.mcp.json/x` — a directory
// wearing a protected file's name — cannot slip a child through.
func IsExcluded(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, part := range strings.Split(rel, "/") {
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

// noteworthyExclusion reports whether an exclusion is worth telling the user about.
//
// NOT EVERY OMISSION IS NEWS, and treating them as if they were is how a disclosure becomes noise
// that people learn to skip. `node_modules`, `dist`, `build`, `vendor`, `coverage`, `.cache`, `tmp`
// and `.git` are excluded by universal convention: they exist in nearly every repository, nobody
// asks for findings in them, and reporting them on every single run would bury the one line that
// matters under five that never do.
//
// The CLIENT-CONFIG family is different, and it is the reason this exists. `.claude`, `.cursor`,
// `.codex`, `.gemini`, `.vscode`, `.idea`, `.windsurf` and `.aimesh` now hold rules, prompts and
// hooks that ARE source — a user can reasonably point a review at a repository expecting those to be
// judged, and before this they were dropped in silence. That is the surprise worth spending a line
// on, and `--allow-protected-paths` is what a reader does about it.
func noteworthyExclusion(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
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

// noteworthyExcludedNames is the OTHER TOOLS' config half of defaultExcludedDirs, plus the config
// FILES. It is a separate set rather than a flag on the other one because the two answer different
// questions: what to exclude, and what a user would be surprised to learn was excluded.
//
// `.aimesh` AND ITS LEGACY SPELLINGS ARE NOT HERE, and that was learned by running the tool: a
// review of an ordinary workspace reported
//
//	withheld by containment: .aimesh [workspace_excluded]: rule .aimesh/**
//
// on every run — because this tool puts its own run artifacts there, so the directory is frequently
// created BY the very run that then reports it. Nobody is surprised that aimesh does not review its
// own run records, and a line that appears every time is the noise this set exists to keep out.
var noteworthyExcludedNames = map[string]struct{}{
	".vscode": {}, ".idea": {}, ".windsurf": {},
	".claude": {}, ".cursor": {}, ".codex": {}, ".gemini": {},
	".mcp.json": {}, "claude_desktop_config.json": {},
}

// excludedRule names the COMPONENT that caused an exclusion, so a caveat says which rule fired
// rather than only that one did. `.claude/rules.md` is dropped because of `.claude`, and a reader
// deciding whether they care needs to see that word.
func excludedRule(rel string) string {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
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

// excludedDetail says what the caveat covers. A directory caveat stands for its whole subtree —
// the walk skips it — and a reader must not take one line as one file.
func excludedDetail(isDir bool) string {
	if isDir {
		return "excluded from review: this directory and everything under it was not shown to any reviewer"
	}
	return "excluded from review: this file was not shown to any reviewer"
}

// safeRel reports whether a workspace-relative path stays inside the workspace (not
// absolute/rooted, no ".." escape). The check is **cross-platform**: it rejects a path
// rooted by EITHER platform's conventions even when this platform's filepath.IsAbs would
// not flag it (e.g. a POSIX-rooted "/abs/x" on Windows, or a backslash-rooted "\abs\x").
func safeRel(rel string) bool {
	// Empty is not a usable relative path; a leading '/' (POSIX root) or '\' (Windows
	// drive-relative root / UNC) is rooted on every platform. (The index is guarded by the
	// empty check.)
	if rel == "" || rel[0] == '/' || rel[0] == '\\' {
		return false
	}
	// Reject a Windows drive-letter / drive-relative prefix ("C:\x", "C:rel") on EVERY
	// platform: it is rooted/drive-relative on Windows and is never a legitimate
	// workspace-relative path anywhere. (filepath.VolumeName only detects this on Windows,
	// so check it explicitly to keep the invariant identical cross-platform.)
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

// SafeRel is the exported cross-platform workspace-relative path-safety check (see safeRel).
// It is reused by the ACP inline-workspace materializer so both share one invariant.
func SafeRel(rel string) bool { return safeRel(rel) }

// protectedAncestorDirs are the excluded directory names that are also PROTECTED BY
// CONTENT, so choosing a DESCENDANT of one as the workspace root must be refused rather
// than silently stripping the protection.
//
// This is the fix for the basename-only hole: `IsExcluded` and `excludedTarget` judge
// components RELATIVE to the root, so a root of `/trusted/.vscode` makes `mcp.json` an
// ordinary relative file — and `.vscode/mcp.json` routinely carries MCP server env vars
// (API keys). The `.env`/`.ssh` families are already refused because they are read-DENIED
// (see scope.DeniedRead), but this family is read-ALLOWED and was therefore reachable this
// way. `docs/security.md` promises these are "never copied or accepted as a target"; that
// promise is only true if the ANCESTORS of the root are judged too.
//
// The set is deliberately NARROWER than defaultExcludedDirs. The generic build/artifact
// names (`tmp`, `node_modules`, `vendor`, `dist`, `build`, `coverage`, `.cache`) are
// excluded from it on purpose: nothing inside them is protected by content, a repo
// legitimately lives at `.../build/myrepo`, and refusing every root under a `tmp` ancestor
// would refuse most of `/tmp` — a false refusal with no security value.
//
// Every member is here because its CONTENT is protected: `.git/config` and `.git/hooks/**`
// execute on the next git command, and the client-config family declares MCP servers and run
// configurations.
//
// `.aimesh` AND ITS LEGACY SPELLINGS ARE NOT MEMBERS, for the same reason the build names are
// not. `.aimesh/{review,explore}/runs/**` is where every run artifact lands, and reading a
// previous run is ordinary work — some review and exploration modes exist to do exactly that.
// Blanket-refusing any root beneath `.aimesh` made those runs unreachable as a target, which is
// a false refusal against this tool's own output. The state that IS protected here is protected
// where it matters: `.aimesh` remains WRITE-denied (scope.deniedDirs), so nothing model-authored
// edits the config, and it remains in defaultExcludedDirs, so reviewing a PROJECT still does not
// drag its run artifacts into the payload. Naming a run directory is a different act from
// walking past one, and only the second needed refusing.
//
// `.aikit` was removed earlier — it is one author's local tool directory, it holds no content
// this rule protects, and it was the only member that was never write-denied, which is what an
// entry added by habit rather than by reasoning looks like.
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

// protectedAncestorFiles are the excluded FILE names that must also refuse a root sitting
// underneath them — a directory wearing a protected file's name (`.mcp.json/`) is the same
// bypass one level down. `.ds_store` is deliberately absent: it is noise, not a secret.
var protectedAncestorFiles = map[string]string{
	".mcp.json":                  ".mcp.json",
	"claude_desktop_config.json": "claude_desktop_config.json",
}

// protectedAncestorRule returns the rule refusing p because p ITSELF or one of its
// ancestors is a protected path, or "". p must be ABSOLUTE — the whole point is to judge
// the components a workspace-relative view cannot see.
func protectedAncestorRule(p string) string {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(p)), "/") {
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

// rootForms returns the forms of a workspace ROOT that must be judged: its absolute path
// and, when it differs, its symlink-resolved absolute path. Both are checked because a
// symlink is otherwise a one-step alias past a component rule — `/trusted/link` pointing
// at `/trusted/.vscode` has no protected component in its own spelling.
//
// A path that cannot be made absolute or resolved contributes nothing; the caller still
// judges the forms that ARE available, so a resolution failure can never turn a refusal
// into an acceptance.
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

// excludedTarget reports whether a review *target* must be refused by its own NAME. A
// directory target is judged by its basename (a repo legitimately living under a generic
// excluded-named ancestor like .../build/myrepo is allowed; its excluded subdirs are
// filtered later by IsExcluded). A single-file target is also refused when it sits directly
// inside an excluded dir (e.g. `.git/config`).
//
// Protected ANCESTORS are a separate, stronger rule — see protectedAncestorRule, which the
// root check applies to the target's absolute path.
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

// normalizeName lower-cases a path component and strips the trailing spaces/dots Windows
// drops on create (`.git ` names `.git`), so an exact lookup cannot be aliased past. On
// Windows it also drops an NTFS alternate-data-stream suffix, where `.git::$DATA` resolves
// to the real `.git`; the trim is Windows-only because `:` is an ordinary filename
// character elsewhere (see scope.stripStream for the same rule and the same reasoning).
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
