package workspace

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Snippet is one whole workspace file gathered for a reviewer prompt.
//
// WHOLE, not bounded. This package used to clip a file at 8 KiB and stop collecting at 50 files
// or 64 KiB total, which made a review's COVERAGE an artifact of three constants nobody had
// written a reason for: pointed at this repository, a review saw ten files in path order and
// never reached any source at all. The constants are gone.
//
// The reasoning is the rule ReadUnder already states one file over: a truncated artifact that
// still parses is worse than an error. A clipped review is exactly that — it returns findings, it
// looks complete, and the files it never opened are indistinguishable from files with nothing
// wrong in them. aimesh cannot know a model's context limit (it has no token counter and no
// per-model catalogue; it hands a prompt to a CLI and reads text back), and a limit you cannot
// measure is not one to approximate with a constant. Breadth is the model's to manage; what this
// package owes it is everything the workspace contains, minus only what CONTAINMENT excludes.
type Snippet struct {
	Path    string // workspace-relative (slash-normalized by the caller as needed)
	Content string
}

// CollectSnippets gathers every reviewable file from root (an isolated copy).
//
// Deprecated in favour of CollectSnippetsWithCaveats, which returns the typed refusal and
// the withheld-file list instead of swallowing both. This form CANNOT report why it
// collected nothing, so it fails CLOSED: a refusal (a protected root, a swapped root)
// yields NO snippets at all rather than a partial set that quietly omits the interesting
// file — and a per-file CAVEAT (a hardlinked file, withheld but not fatal) is lost
// entirely. Callers that can carry an error should migrate, so both reach the audit record.
func CollectSnippets(root string) []Snippet {
	out, err := CollectSnippetsChecked(root)
	if err != nil {
		return nil
	}
	return out
}

// CollectSnippetsChecked is CollectSnippetsWithCaveats without the caveat list. Prefer
// CollectSnippetsWithCaveats where the caller has somewhere to record it: a withheld file
// (a hardlinked one) is otherwise indistinguishable from a file that was never there.
func CollectSnippetsChecked(root string) ([]Snippet, error) {
	out, _, err := CollectSnippetsWithCaveats(root)
	return out, err
}

// CollectSnippetsWithCaveats gathers EVERY reviewable file from root (an isolated copy). It
// refuses outright when the ROOT is itself a protected path or sits inside one, and skips
// excluded/internal paths, read-denied paths, symlinks/reparse points, and binary files. Paths
// are workspace-relative. The result is deterministic (sorted by path).
//
// Every remaining exclusion above is a CONTAINMENT rule — a statement that some file must not
// reach a model. None of them is a size or a count: no file is clipped and the walk never stops
// early, because "we ran out of budget" is not a reason a reviewer could be told about a defect
// it therefore never saw. See Snippet.
//
// The second return value lists files that were WITHHELD for a containment reason rather
// than simply absent (see Caveat) — currently hardlinked regular files. Recording them is
// what keeps a withheld file from looking like a file that never existed.
//
// All file access is root-relative (os.Root): the tree is opened once and every entry is
// read through that handle, so a component swapped to a symlink between the walk and the
// read cannot redirect the read out of the workspace. The root handle is bound by identity
// to the object the checks below ran against, so the BOUNDARY itself cannot be swapped
// either (see openRootBound).
func CollectSnippetsWithCaveats(root string) ([]Snippet, []Caveat, error) {
	var out []Snippet
	caveats, err := collectUnder("collect", root, false, func(rel string, b []byte) {
		out = append(out, Snippet{Path: filepath.ToSlash(rel), Content: string(b)})
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, caveats, nil
}

// collectUnder is the DECISION half of collection, with materialization left to the caller.
//
// It owns every rule about what a workspace may contribute to a prompt — the root judgement, the
// per-entry exclusions, the symlink and hardlink refusals and the binary skip — and hands each
// admitted file's bytes to keep(), which decides what to DO with them. CollectSnippetsWithCaveats
// keeps the content; PreviewPayload keeps only the length.
//
// The split exists so a caller can answer "what would this run carry?" without carrying it. The
// alternative — a second, lighter walk that re-states the same rules — is exactly the shape of
// bug this package is built to avoid: the rules here are subtle (exclusion judged per COMPONENT,
// a hardlinked file withheld but not fatal), and two implementations of them would drift until
// the preview described a run that never happens.
//
// keep is called in WALK order. Callers that want a sorted result sort afterwards.
// allowProtected waives ONLY the protected-ANCESTOR half of the root judgement (see
// rootRefusal). A secret root is refused either way. Collection of an isolated COPY passes
// false and is unaffected: a copy lives under a temp base, so no protected component is in
// its path to begin with. It is the LIVE-tree walk (PreviewPayload) that needs the waiver,
// because there the operator's own `.claude`/`.git` tree is the thing being described.
func collectUnder(op, root string, allowProtected bool, keep func(rel string, b []byte)) ([]Caveat, error) {
	// The ROOT is judged first. A `.env` (or `.ssh`) directory handed in as the root is
	// the secret itself; judging only its children by their own names ("production",
	// "staging") walks the whole thing into a prompt. A root INSIDE a protected directory
	// (`/trusted/.vscode`) is the same bypass with the protection one level up.
	if ref := rootRefusal(op, root, allowProtected); ref != nil {
		return nil, ref
	}
	// The identity the opened boundary must have. Lstat, not Stat: a root that is itself a
	// symlink is refused outright rather than followed — an isolated copy is never one, so
	// a link here is a redirect, not a convenience.
	wantRoot, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if isReparse(wantRoot) {
		return nil, &Refusal{
			Op: op, Path: root, Reason: ReasonReparse,
			Rule: "a collection root that is a symlink/reparse point is never followed",
		}
	}
	r, err := openRootBound(op, root, wantRoot)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var caveats []Caveat
	walkErr := fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return nil
		}
		rel := filepath.FromSlash(p)
		if IsExcluded(rel) {
			// RECORDED when it would surprise someone — see noteworthyExclusion. A DIRECTORY is
			// recorded once and its subtree skipped: one caveat saying `.claude/` was dropped is
			// the useful fact, and a caveat per file inside it would bury the rest of the list.
			if noteworthyExclusion(rel) {
				caveats = append(caveats, Caveat{
					Path: filepath.ToSlash(rel), Reason: ReasonExcluded,
					Rule: excludedRule(rel), Detail: excludedDetail(d.IsDir()),
				})
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// Secrets never reach a prompt: `.env*`, credential dirs and key material are
		// read-denied on EVERY component, so a file under a denied directory is denied
		// with it. The copy layer already skips them, so this is defense in depth for a
		// caller that collects from a directory it assembled itself.
		//
		// DELIBERATELY NOT CAVEATED, unlike the exclusion above. A caveat is reported to the
		// caller — over MCP, that caller is a model — and a list of the paths where this
		// machine keeps its credentials is exactly the inventory a secret rule exists to
		// withhold. The exclusion list has no such problem (nobody is attacked by learning
		// that `dist/` was skipped), and the surprise it prevents is real, which is why the
		// two are treated differently. The secret families are fixed and documented, so a
		// reader can know what is never shown without being handed where it lives.
		if scope.DeniedRead(rel) != "" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// reparse check BEFORE the dir short-circuit so a junction/mount-point dir
		// (which reports IsDir()==true) is never descended into.
		fi, lerr := r.Lstat(toRootPath(rel))
		if lerr != nil {
			return nil
		}
		if isReparse(fi) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil // never follow symlinks/reparse points
		}
		if fi.IsDir() {
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		b, rerr := readRegular(r, op, rel)
		if rerr != nil {
			// A hardlinked regular file is WITHHELD and recorded, not collected and not
			// fatal: its innocuous in-root name may be a second name for `~/.ssh/id_rsa`,
			// so it must never reach a prompt — but a `cp -al` tree or a dedup store is
			// ordinary, and failing the whole collection over one such file is an
			// availability hole. The caveat is what stops the omission being silent.
			if ReasonOf(rerr) == ReasonHardlink {
				caveats = append(caveats, caveatFor(rel, rerr))
				return nil
			}
			// Any other typed refusal is a containment fact the caller must see.
			if _, isRefusal := AsRefusal(rerr); isRefusal {
				return rerr
			}
			return nil
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil // binary file: skip (no metadata-only payload in this build)
		}
		keep(rel, b)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(caveats, func(i, j int) bool { return caveats[i].Path < caveats[j].Path })
	return caveats, nil
}
