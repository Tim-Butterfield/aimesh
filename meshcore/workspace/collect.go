package workspace

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Snippet is one whole workspace file gathered for a prompt. Files are never clipped and collection
// has no file-count cap, because a truncated set still looks complete; only containment rules
// exclude files.
type Snippet struct {
	Path    string // workspace-relative (slash-normalized by the caller as needed)
	Content string
}

// CollectSnippetsWithCaveats gathers every file from root (an isolated copy), sorted by
// workspace-relative path. It refuses a root that is or sits inside a protected path, and skips
// excluded and read-denied paths, symlinks, reparse points and binary files. No file is clipped.
// The caveats list files withheld for a containment reason (currently hardlinked files).
//
// Every read goes through an os.Root bound by identity to the checked root, so neither a swapped
// component nor a swapped root can redirect a read (see openRootBound).
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

// collectUnder applies every rule about what a workspace may contribute to a prompt and passes each
// admitted file's bytes to keep, in walk order. CollectSnippetsWithCaveats keeps the content and
// PreviewPayloadWith keeps only the length, so both share one implementation of the rules.
//
// allowProtected waives only the protected-ancestor rule for the root (see rootRefusal).
// Collection from a copy passes false; PreviewPayloadWith passes the operator's setting because it
// walks the live tree.
func collectUnder(op, root string, allowProtected bool, keep func(rel string, b []byte)) ([]Caveat, error) {
	// The root is judged first: a secret directory given as the root is itself the secret, and a
	// root inside a protected directory would bypass the component rules.
	if ref := rootRefusal(op, root, allowProtected); ref != nil {
		return nil, ref
	}
	// Lstat, not Stat: a collection root that is a symlink is refused rather than followed.
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
			// A noteworthy excluded directory is recorded once and its subtree skipped.
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
		// Read-denied files never reach a prompt. The copy already skips them; this also covers a
		// caller collecting from a directory it assembled. They are not recorded as caveats,
		// because the caller may be a model and the list would reveal where credentials live.
		if scope.DeniedRead(rel) != "" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// The reparse check precedes the directory check so a junction, which reports
		// IsDir, is never descended into.
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
			// A hardlinked file may be a second name for a secret, so it is withheld and
			// recorded rather than collected; failing the whole collection would be an
			// availability hole.
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
			return nil // binary files are skipped
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
