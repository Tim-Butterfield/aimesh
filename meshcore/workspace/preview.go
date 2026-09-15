package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"

	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Payload describes what a run would send from a workspace into a prompt: which files, their byte
// counts, and what containment withheld. PreviewPayloadWith produces it without copying or retaining
// content.
type Payload struct {
	// Files is every file the run would send, in walk order, with its size. Files are sent whole,
	// so the size is the size on disk.
	Files []PayloadFile
	// Withheld lists files kept out for a containment reason, by the same rules a run applies.
	Withheld []Caveat
	// TotalBytes is the sum of Files' bytes: everything the prompt would carry.
	TotalBytes int
}

// PayloadFile is one file a run would carry, with its content replaced by its length.
type PayloadFile struct {
	Path  string // workspace-relative, slash-separated
	Bytes int
}

// PreviewPayloadWith reports what a run over root would send to its prompts, without making the
// containment copy or retaining content. It reads the live tree through collectUnder, the rules a
// run applies to its copy, so the file set and byte counts match the run's; a test asserts this
// against a real Copy. A hardlinked file is withheld by the collector here and by the copy in a run.
// It writes nothing. Pass the allowProtected value the run uses, or the preview describes a run that
// cannot happen.
func PreviewPayloadWith(root string, allowProtected bool) (Payload, error) {
	const op = "preview"
	var p Payload
	keep := func(rel string, b []byte) {
		p.Files = append(p.Files, PayloadFile{Path: filepath.ToSlash(rel), Bytes: len(b)})
		p.TotalBytes += len(b)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return Payload{}, err
	}
	// A single-file workspace is previewed as that file, matching Copy, which copies only it.
	if !info.IsDir() {
		if err := previewOneFile(op, root, info, allowProtected, keep); err != nil {
			return Payload{}, err
		}
		return p, nil
	}
	caveats, err := collectUnder(op, root, allowProtected, keep)
	if err != nil {
		return Payload{}, err
	}
	p.Withheld = caveats
	return p, nil
}

// previewOneFile applies the collector's per-file rules to one file, read through its parent as an
// identity-bound root. A containment refusal is returned rather than recorded as a caveat, matching
// Copy: when the file is the workspace, the run itself would refuse to start.
func previewOneFile(op, path string, info os.FileInfo, allowProtected bool, keep func(string, []byte)) error {
	if ref := rootRefusal(op, path, allowProtected); ref != nil {
		return ref
	}
	if isReparse(info) {
		return &Refusal{
			Op: op, Path: path, Reason: ReasonReparse,
			Rule: "a workspace that is a symlink/reparse point is never followed",
		}
	}
	if !info.Mode().IsRegular() {
		return nil // devices and sockets are never sent
	}
	base := filepath.Base(path)
	if IsExcluded(base) || scope.DeniedRead(base) != "" {
		return nil
	}
	// The parent may be a linked directory, so its identity comes from a following stat, as in
	// Copy.
	parent := filepath.Dir(path)
	wantParent, err := os.Stat(parent)
	if err != nil {
		return err
	}
	r, err := openRootBound(op, parent, wantParent)
	if err != nil {
		return err
	}
	defer r.Close()
	b, err := readRegular(r, op, base)
	if err != nil {
		return err
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return nil // binary: skipped by the collector, so carried by nothing
	}
	keep(base, b)
	return nil
}

// SortedFiles returns the payload's files ordered by path, for a rendering that should not depend
// on walk order.
func (p Payload) SortedFiles() []PayloadFile {
	out := append([]PayloadFile(nil), p.Files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
