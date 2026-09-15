package review

// Authority documents are the intent a review is judged against: requirements, design documents,
// specifications. They are context, never a target, and are embedded in prompts rather than placed
// in the containment copy. The rules governing them live in internal/review/engine/authority.

// Completeness declares how much of an authority document the caller intends to embed, so partial
// inclusion is always the caller's decision.
type Completeness string

const (
	// CompletenessRequireFull, the default, embeds the whole document.
	CompletenessRequireFull Completeness = "requireFull"
	// CompletenessRanges embeds only the declared byte ranges; the manifest records the inclusion
	// as incomplete and the prompt marks the omitted spans.
	CompletenessRanges Completeness = "ranges"
)

// Authority source values recorded in the inclusion manifest.
const (
	// AuthoritySourcePath is a root-scoped file read, allowed in every mode.
	AuthoritySourcePath = "path"
	// AuthoritySourceInline is caller-supplied content: report mode only, and excluded from the
	// host-adjudication prompt.
	AuthoritySourceInline = "inline"
)

// AuthorityRange is a half-open byte range [Start, End) of an authority document.
type AuthorityRange struct {
	Start int `json:"start"`
	End   int `json:"end"` // exclusive
}

// AuthorityDoc is one declared authority document as a caller supplies it (CLI `--authority`, an
// MCP or ACP `authority` list). Exactly one of Path or Content must be set: Path is read through
// meshcore/scope and its read denylist; Content is report-mode only.
type AuthorityDoc struct {
	// Name labels the document in the prompt, the manifest and `--authority-hash <name>=…`. It is
	// required and unique within a request.
	Name string `json:"name"`
	// Path is a filesystem path to the document.
	Path string `json:"path,omitempty"`
	// Content is the document text supplied inline.
	Content string `json:"content,omitempty"`
	// MediaType is an advisory type (e.g. "text/markdown") recorded in the manifest and prompt
	// header; it does not change how bytes are handled.
	MediaType string `json:"mediaType,omitempty"`
	// ExpectedHash pins the document's full content as "sha256:<hex>" or a bare hex digest. A
	// mismatch halts the run.
	ExpectedHash string `json:"expectedHash,omitempty"`
	// Completeness defaults to CompletenessRequireFull when empty.
	Completeness Completeness `json:"completeness,omitempty"`
	// Ranges are the byte ranges to embed; required with CompletenessRanges.
	Ranges []AuthorityRange `json:"ranges,omitempty"`
}

// AuthorityInclusion is one inclusion-manifest entry: what was embedded for one authority document.
// It is written to authority/inclusion-manifest.json and carried on the result projection.
type AuthorityInclusion struct {
	Name      string `json:"name"`
	Source    string `json:"source"` // AuthoritySourcePath | AuthoritySourceInline
	MediaType string `json:"mediaType,omitempty"`
	// FullHash is sha256 over the document's full bytes, the value ExpectedHash pins.
	FullHash string `json:"fullHash"`
	// EmbeddedHash is sha256 over the bytes that reached the prompt; it equals FullHash exactly
	// when the inclusion is complete.
	EmbeddedHash  string `json:"embeddedHash"`
	BytesEmbedded int    `json:"bytesEmbedded"`
	BytesTotal    int    `json:"bytesTotal"`
	// Complete reports whether the whole document was embedded.
	Complete bool             `json:"complete"`
	Ranges   []AuthorityRange `json:"ranges,omitempty"`
}
