package review

// --- Authority / context inputs ---
//
// An AUTHORITY artifact is the intent a reviewed artifact is judged AGAINST: a requirements
// document, a design doc, an ADR, a spec. It is CONTEXT, never a target — it is never
// reviewed, never patched, never applied, and it never enters the containment copy (it is
// prompt-embedded by construction, so the write layer cannot reach it at all).
//
// These are the wire/domain types. The resolution rules that give them their guarantees —
// root-scoped path reads, hash pinning, the no-silent-truncation budget, the provenance
// split, and the quoted-evidence rendering — live in
// reviewmesh/internal/engine/authority, which is the ONE place every surface funnels
// through so the behavior cannot diverge per surface.

// Completeness declares how much of an authority document the caller intends to embed.
//
// It exists so that partial inclusion is always a CALLER DECISION rather than a silent
// host truncation: with CompletenessRequireFull the host must embed the whole document or
// FAIL CLOSED; with CompletenessRanges the caller states exactly which byte ranges it
// wants and the manifest records that the inclusion is incomplete.
type Completeness string

const (
	// CompletenessRequireFull (the default) means: embed the document in full, or refuse.
	// A document that does not fit the embedding budget is a halt naming the document and
	// its sizes — never a quietly shortened prompt.
	CompletenessRequireFull Completeness = "requireFull"
	// CompletenessRanges means the caller declared the byte ranges to embed. The result is
	// marked incomplete in the inclusion manifest, and the omitted spans are marked in the
	// prompt, so "which intent was this judged against" stays a fact.
	CompletenessRanges Completeness = "ranges"
)

// Authority source values recorded in the inclusion manifest.
const (
	// AuthoritySourcePath is a root-scoped file read (allowed in every mode, every phase).
	AuthoritySourcePath = "path"
	// AuthoritySourceInline is caller-supplied content (report mode only; excluded from the
	// host-adjudication prompt — see the provenance split in internal/engine/authority).
	AuthoritySourceInline = "inline"
)

// AuthorityRange is a half-open byte range [Start, End) of an authority document.
type AuthorityRange struct {
	Start int `json:"start"`
	End   int `json:"end"` // exclusive
}

// AuthorityDoc is ONE declared authority artifact, as a caller supplies it on any surface
// (CLI `--authority`, ACP `_meta.reviewmesh.authority[]`, and — later — MCP `authority[]`).
//
// EXACTLY ONE of Path or Content must be set:
//   - Path: read through meshcore/scope (root-scoped, non-overridable read denylist, so a
//     `.env` or key file can never be smuggled in as "authority"). Allowed in every mode.
//   - Content: supplied inline by the caller. REPORT MODE ONLY, and excluded from the
//     host-adjudication prompt — otherwise a client model could supply the intent, its
//     peers could find "deviations" from it, and the host would write the result with no
//     human artifact anywhere in the loop.
type AuthorityDoc struct {
	// Name is the stable label this document is referred to by (in the prompt, the
	// manifest, and `--authority-hash <name>=…`). Required and unique within a request.
	Name string `json:"name"`
	// Path is a filesystem path to the document (exactly one of Path/Content).
	Path string `json:"path,omitempty"`
	// Content is the document text supplied inline (exactly one of Path/Content).
	Content string `json:"content,omitempty"`
	// MediaType is an optional advisory type (e.g. "text/markdown"). It is recorded in the
	// manifest and shown in the prompt header; it never changes how bytes are handled.
	MediaType string `json:"mediaType,omitempty"`
	// ExpectedHash optionally PINS the document's full content: "sha256:<hex>" (a bare hex
	// digest is also accepted). When set and the read bytes hash differently, the run HALTS
	// — this is what stops a path document's content from changing between a report run and
	// the apply run that acts on it.
	ExpectedHash string `json:"expectedHash,omitempty"`
	// Completeness defaults to CompletenessRequireFull when empty.
	Completeness Completeness `json:"completeness,omitempty"`
	// Ranges are the caller-declared byte ranges to embed; required (and only meaningful)
	// with CompletenessRanges.
	Ranges []AuthorityRange `json:"ranges,omitempty"`
}

// AuthorityInclusion is ONE entry of the inclusion manifest: the auditable record of what
// was actually embedded for one authority document. It rides the run record
// (`authority/inclusion-manifest.json`, and the run-state summary) AND the result
// projection, so the audit trail reproduces the exact prompt and a machine consumer can
// answer "which intent was this judged against" without re-reading artifacts.
type AuthorityInclusion struct {
	Name      string `json:"name"`
	Source    string `json:"source"` // AuthoritySourcePath | AuthoritySourceInline
	MediaType string `json:"mediaType,omitempty"`
	// FullHash is sha256 over the document's FULL bytes (what ExpectedHash pins).
	FullHash string `json:"fullHash"`
	// EmbeddedHash is sha256 over EXACTLY the bytes that reached the prompt. It equals
	// FullHash for a complete inclusion and differs for a ranged one — so "complete" is
	// verifiable, not merely asserted.
	EmbeddedHash  string `json:"embeddedHash"`
	BytesEmbedded int    `json:"bytesEmbedded"`
	BytesTotal    int    `json:"bytesTotal"`
	// Complete reports whether the whole document was embedded.
	Complete bool             `json:"complete"`
	Ranges   []AuthorityRange `json:"ranges,omitempty"`
}
