// Command boundarycheck enforces the aimesh meshcore boundary: meshcore is the app-neutral engine and
// must not learn either domain. It fails the build when:
//   - meshcore imports a domain package (internal/review or internal/explore), or imports net/http;
//   - the two domains import each other;
//   - a meshcore EXPORTED identifier contains an app-domain denylist term (strict tier).
//
// THE DOMAIN↔DOMAIN CHECK IS NOW THE ONLY THING ENFORCING THAT BOUNDARY. review and explore used to be
// separate Go modules, so the module graph refused a cross-import on its own and this check was a second
// opinion. Collapsing to one module (they are never released independently — a module is a unit of
// distribution, not organization) removed that backstop deliberately, on the understanding that the
// check below replaces it. Deleting or weakening it silently re-opens app↔app coupling with nothing else
// watching.
//
// Denylist tiers: exported identifiers = FAIL (strict); comments in non-test files = WARN
// (baseline, tighten later); test files = report-only (skipped). Denylist terms are matched
// as whole camelCase/underscore words, so `preview`/`overview` do NOT match `review`.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	modPrefix   = "github.com/Tim-Butterfield/aimesh"
	meshcoreMod = modPrefix + "/meshcore"
	// The two domains are now internal packages of the one root module rather than their own modules.
	reviewPkg  = modPrefix + "/internal/review"
	explorePkg = modPrefix + "/internal/explore"
)

// reviewDir / exploreDir are the on-disk roots the import-graph checks are run from.
var (
	reviewDir  = filepath.Join("internal", "review")
	exploreDir = filepath.Join("internal", "explore")
)

var denylist = map[string]bool{
	"review": true, "reviewer": true, "finding": true, "remediation": true,
	"lane": true, "roster": true, "explorer": true, "collator": true,
	"synthesis": true, "task": true, "reviewmesh": true, "exploremesh": true,
}

// protocolExemptions permit a denylisted term inside ONE meshcore package because an EXTERNAL
// STANDARD names the concept there — not because an app domain leaked in. The exemption is keyed
// by (package path prefix, term) and never by term alone, so the same word stays failing
// everywhere else in meshcore.
//
// `task` in `meshcore/mcp`: the MCP `2026-07-28` `io.modelcontextprotocol/tasks` extension defines
// `tasks/get`, `tasks/cancel`, `CreateTaskResult` and `resultType: "task"`. Naming our types
// anything else would leave the package implementing a protocol in vocabulary the protocol does not
// use — and this repo has repeatedly found that verifying code against the spec's own words is what
// catches errors. exploremesh's DOMAIN `task` (`RawTask`, `task.json`) is unrelated and is still
// caught in every other meshcore package.
//
// Adding an entry here is a boundary decision: require an external specification that names the
// term, and scope it to the narrowest package that implements that specification.
var protocolExemptions = map[string]map[string]bool{
	"mcp/": {"task": true},
}

// exemptTerm reports whether term is permitted in this meshcore-relative file by a protocol
// exemption above.
// The keys are slash-separated, so file is normalized rather than trusted: a native-separator path
// would match no prefix and silently drop the exemption, turning a passing check into a failing one
// on Windows only.
func exemptTerm(file, term string) bool {
	file = filepath.ToSlash(file)
	for prefix, terms := range protocolExemptions {
		if strings.HasPrefix(file, prefix) && terms[term] {
			return true
		}
	}
	return false
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "boundarycheck: cannot resolve cwd:", err)
		os.Exit(2)
	}
	var failures, warnings []string

	// (1) Import-graph checks.
	if dirExists(filepath.Join(root, "meshcore")) {
		for _, ip := range listDeps(filepath.Join(root, "meshcore")) {
			if ip == "net/http" || strings.HasPrefix(ip, "net/http/") {
				failures = append(failures, "meshcore imports HTTP ("+ip+"): meshcore must be UI/HTTP-free")
			}
			if strings.HasPrefix(ip, explorePkg) {
				failures = append(failures, "meshcore imports internal/explore ("+ip+"): reverse import")
			} else if strings.HasPrefix(ip, reviewPkg) {
				failures = append(failures, "meshcore imports internal/review ("+ip+"): reverse import")
			}
		}
	}
	// The DOMAIN↔DOMAIN boundary. Both live in the one root module now, so nothing but this refuses a
	// cross-import — see the package comment.
	if dirExists(filepath.Join(root, reviewDir)) {
		for _, ip := range listDeps(filepath.Join(root, reviewDir)) {
			if strings.HasPrefix(ip, explorePkg) {
				failures = append(failures, "internal/review imports internal/explore ("+ip+"): domain↔domain")
			}
		}
	}
	if dirExists(filepath.Join(root, exploreDir)) {
		for _, ip := range listDeps(filepath.Join(root, exploreDir)) {
			if strings.HasPrefix(ip, reviewPkg) {
				failures = append(failures, "internal/explore imports internal/review ("+ip+"): domain↔domain")
			}
		}
	}

	// (2) Denylist source scan of meshcore.
	if dirExists(filepath.Join(root, "meshcore")) {
		f, w := scanMeshcore(filepath.Join(root, "meshcore"))
		failures, warnings = append(failures, f...), append(warnings, w...)
	}

	// (3) Sunset-path markers cite their removal task id.
	failures = append(failures, scanSunsetMarkers(root)...)

	for _, w := range warnings {
		fmt.Println("warn (baseline):", w)
	}
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "FAIL:", f)
		}
		os.Exit(1)
	}
	fmt.Println("boundary-check: OK")
}

func dirExists(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// sunsetRemovalTaskID is the backlog task a sunset-path marker must cite. It is one id today because
// there is one sunset in flight; make it a set when there is a second.
const sunsetRemovalTaskID = "MCP26-SUNSET"

// scanSunsetMarkers enforces CONTRIBUTING.md's rule that code written to serve only a protocol
// revision we intend to delete is marked "at the moment you write it … a comment naming it
// sunset-path AND CITING THE REMOVAL TASK ID".
//
// WHY THIS IS A BUILD FAILURE RATHER THAN A CONVENTION. The rule was followed in spirit and broken in
// letter at every one of the 28 sites the migration produced: each said "legacy removal task" and none
// said `MCP26-SUNSET`. That is invisible until the day it matters, and on that day the removal is a
// grep — so a marker that the grep does not find is a marker that does not exist. The whole value of
// marking-at-the-point is that the removal is a checklist rather than a search; a citation-free marker
// silently converts it back into a search.
//
// It scans the whole repo rather than meshcore alone, because sunset-path code lives in both apps'
// surfaces and their CLIs. Test files are included: a legacy-only test is legacy-only code.
func scanSunsetMarkers(root string) (failures []string) {
	skipDir := map[string]bool{
		".git": true, ".aikit": true, "node_modules": true, "testdata": true, "web": true,
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		// This checker's own source is skipped: it necessarily contains the marker token as a string
		// literal (and in this comment), and a checker that fails on its own implementation is a
		// checker nobody can run. It is the only exclusion, and it is a path, not a pattern.
		if strings.HasPrefix(filepath.ToSlash(rel), "scripts/boundarycheck/") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "SUNSET-PATH") {
				continue
			}
			if strings.Contains(line, sunsetRemovalTaskID) {
				continue
			}
			failures = append(failures, fmt.Sprintf(
				"%s:%d: a SUNSET-PATH marker does not cite its removal task id (%s). "+
					"CONTRIBUTING.md requires the id, because the removal is done by grepping for it: %s",
				rel, i+1, sunsetRemovalTaskID, strings.TrimSpace(line)))
		}
		return nil
	})
	return failures
}

// listDeps returns the transitive import paths of every package in moduleDir.
func listDeps(moduleDir string) []string {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./...")
	cmd.Dir = moduleDir
	out, err := cmd.Output()
	if err != nil {
		return nil // no packages yet (empty module) → no imports to check
	}
	var res []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			res = append(res, line)
		}
	}
	return res
}

func scanMeshcore(dir string) (failures, warnings []string) {
	fset := token.NewFileSet()
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		// Slash-normalized: rel feeds both the protocolExemptions prefix lookup (keyed "mcp/") and
		// the reported path. On Windows filepath.Rel yields `mcp\result.go`, which matches no
		// prefix — so every exempt identifier in meshcore/mcp failed there while passing on Unix.
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(path, "_test.go") {
			return nil // test files: report-only tier (skipped)
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return nil
		}
		// Strict tier: exported identifiers.
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeSpec:
				checkIdent(x.Name, rel, &failures)
			case *ast.FuncDecl:
				checkIdent(x.Name, rel, &failures)
			case *ast.ValueSpec:
				for _, nm := range x.Names {
					checkIdent(nm, rel, &failures)
				}
			case *ast.Field:
				for _, nm := range x.Names {
					checkIdent(nm, rel, &failures)
				}
			}
			return true
		})
		// Baseline tier: comments in non-test files → warn only. Protocol exemptions apply here
		// too: a package documenting the specification it implements must be able to name that
		// specification's concepts, and warning on every mention would bury the app-vocabulary
		// leaks this tier exists to surface.
		for _, cg := range file.Comments {
			for _, w := range wordsIn(cg.Text()) {
				if denylist[w] && !exemptTerm(rel, w) {
					warnings = append(warnings, fmt.Sprintf("comment in meshcore/%s uses app term %q", rel, w))
					break
				}
			}
		}
		return nil
	})
	return failures, warnings
}

func checkIdent(id *ast.Ident, file string, failures *[]string) {
	if id == nil || !id.IsExported() {
		return
	}
	for _, w := range splitWords(id.Name) {
		if denylist[w] && !exemptTerm(file, w) {
			*failures = append(*failures, fmt.Sprintf("meshcore exported identifier %q (meshcore/%s) contains denylisted app term %q", id.Name, file, w))
		}
	}
}

// splitWords breaks an identifier into lowercase words at camelCase (lower→upper)
// boundaries and underscores, so `PreviewArgs` → [preview args] (not matching `review`).
func splitWords(s string) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if r == '_' {
			flush()
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' && !(runes[i-1] >= 'A' && runes[i-1] <= 'Z') {
			flush()
		}
		cur.WriteRune(r)
	}
	flush()
	return words
}

// wordsIn lowercases free text and splits on non-letters, for comment scanning.
func wordsIn(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r < 'a' || r > 'z'
	})
}
