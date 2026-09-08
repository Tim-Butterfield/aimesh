package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"

	"github.com/Tim-Butterfield/aimesh/meshcore/scope"
)

// Payload is WHAT A RUN WOULD CARRY out of a workspace and into a prompt: which files, how many
// bytes of each, and what containment withheld. It is the answer to the half of "what is this
// about to cost me" that a call count cannot express.
//
// It is produced WITHOUT making the containment copy and without keeping a single byte of
// content — see PreviewPayload for why that matters and what it costs.
type Payload struct {
	// Files is every file the run would show a reviewer, in the order the walk met them, each
	// with the byte count that would be sent. Files are sent WHOLE, so the count is the size on
	// disk; nothing here is a post-budget figure, because there is no budget (see Snippet).
	Files []PayloadFile
	// Withheld lists files kept out for a containment reason rather than simply absent — the
	// same Caveats the real run records, from the same rules. See Caveat.
	Withheld []Caveat
	// TotalBytes is the sum of Files' bytes: everything the prompt would carry.
	TotalBytes int
}

// PayloadFile is one file a run would carry, with its content replaced by its length.
type PayloadFile struct {
	Path  string // workspace-relative, slash-separated
	Bytes int
}

// PreviewPayload reports what a review of `root` would carry into its reviewer prompts, WITHOUT
// making the containment copy and without retaining any content.
//
// It exists for a dry run. Pricing a run in model calls answers half the question; the other half
// is what those calls would be shown, and the two differ enormously between `--dry-run .` on a
// monorepo and `--dry-run internal/review` — a difference a call count alone cannot express, and
// which (measured 2026-08-11) made those two disclosures byte-identical.
//
// EQUIVALENCE IS THE POINT, AND IT IS TESTED. A real run copies the workspace and then collects
// from the copy; this walks the live tree once and collects nothing. Both go through collectUnder,
// which owns every rule, so the file set and the byte counts are the same set the run would send
// — see TestPreviewPayload_MatchesWhatTheRunWouldCarry, which asserts it against an actual
// Copy + CollectSnippetsWithCaveats rather than against an expectation.
// The one deliberate difference is bookkeeping: a hardlinked file is withheld by the COPY in a
// real run and by the COLLECTOR here, so the real run's withheld list is the union of two stages
// and this one is a single list with the same contents.
//
// IT READS THE LIVE TREE. That is what makes it exact, and it is the same read the copy performs
// moments later under the same rules — no path is opened here that a real run would not open. It
// creates nothing, writes nothing and copies nothing, so it costs a walk and no spend, which is
// what puts it on the free side of the dry-run stop.
func PreviewPayload(root string) (Payload, error) { return PreviewPayloadWith(root, false) }

// PreviewPayloadWith is PreviewPayload with the operator's protected-ancestor waiver applied,
// so a dry run can describe the same tree the run itself would be allowed to copy. Passing a
// value that disagrees with the run's own Access.AllowProtectedRoots is what would make the
// preview describe a run that cannot happen — the equivalence this function exists to hold.
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
	// A SINGLE-FILE workspace is previewed as that one file, never as its directory. Copy treats
	// a file target as the whole subject of the run (the copy holds exactly it), so walking the
	// parent here would report a payload of files no reviewer would ever be shown — the precise
	// over-statement this function exists to remove.
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

// previewOneFile is PreviewPayload's single-file branch: the collector's per-file rules applied to
// one named file, read through its PARENT as an identity-bound root so the read is confined the
// same way every other containment read is.
//
// A containment refusal here is FATAL rather than a caveat, matching Copy: when the file IS the
// workspace, withholding it leaves nothing to review, and reporting an empty payload would say
// "this run carries nothing" where the run itself will refuse to start.
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
		return nil // a device or socket is not review material: nothing would be carried
	}
	base := filepath.Base(path)
	if IsExcluded(base) || scope.DeniedRead(base) != "" {
		return nil
	}
	// The parent may legitimately BE a symlink (a file reached through a linked directory), so
	// its identity is captured with a following stat — the same distinction Copy draws.
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
