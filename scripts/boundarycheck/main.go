// Command boundarycheck enforces the meshcore boundary. It fails when:
//   - meshcore imports internal/review, internal/explore or net/http;
//   - internal/review and internal/explore import each other;
//   - a meshcore exported identifier contains an app-domain denylist term;
//   - a sunset-path marker does not cite its removal task id.
//
// Both domains live in one Go module, so this check is the only thing that prevents a cross-domain
// import.
//
// Denylist terms match whole camelCase or underscore-separated words, so `preview` does not match
// `review`. Exported identifiers fail, comments in non-test files warn, and test files are skipped.
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
	// The domains are internal packages of the root module.
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

// protocolExemptions permit a denylist term in one meshcore package because an external standard
// names the concept there. Entries are keyed by package path prefix and term, so the term still
// fails everywhere else in meshcore.
//
// `task` in meshcore/mcp: the MCP 2026-07-28 tasks extension defines tasks/get, tasks/cancel and
// CreateTaskResult, so the package uses the specification's vocabulary.
//
// Add an entry only for a term an external specification names, scoped to the narrowest package
// that implements it.
var protocolExemptions = map[string]map[string]bool{
	"mcp/": {"task": true},
}

// exemptTerm reports whether a protocol exemption permits term in the meshcore-relative file. The
// path is slash-normalized because the keys use forward slashes and Windows paths do not.
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
	// The domain boundary; see the package comment.
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

// sunsetRemovalTaskID is the task id every sunset-path marker must cite.
const sunsetRemovalTaskID = "MCP26-SUNSET"

// scanSunsetMarkers reports every sunset-path marker that does not cite sunsetRemovalTaskID, as
// CONTRIBUTING.md requires. The removal is found by searching for the id, so a marker without it
// would be missed. It scans the whole repository, test files included.
func scanSunsetMarkers(root string) (failures []string) {
	skipDir := map[string]bool{
		".git": true, ".aikit": true, "node_modules": true, "testdata": true,
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
		// Skip this checker's own source, which contains the marker token as a literal.
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
		// Slash-normalize rel, which feeds the protocolExemptions prefix lookup and the report.
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(path, "_test.go") {
			return nil // test files are not scanned
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
		// Comments only warn. Protocol exemptions apply here too, so a package can name the concepts
		// of the specification it implements.
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
