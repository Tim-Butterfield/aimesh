package setup

// The shared adapters file (~/.aimesh/adapters.yaml and its project-root sibling) is also written by
// exploremesh, the CLI and hand edits. This file lets setup notice that it changed, refuse a save
// composed against an older version, and report cheaply whether a change broke any configured lane.
//
// Everything here is file reads, hashes and map lookups, and starts no process: checking whether a
// binary runs or is logged in stays in doctor, since such a check can block on an interactive login.

// StaleBaseError refuses a write composed against a version of the shared adapters file that is no
// longer on disk. It is typed so a surface can report a reconcilable conflict.
type StaleBaseError struct{ Message string }

// StaleBaseMessage is the text a refused stale write carries; it states that nothing was written.
const StaleBaseMessage = "the shared adapter configuration changed on disk after this form was loaded, " +
	"so this save was refused rather than overwriting the newer file. Nothing was written. " +
	"Reload the current configuration, then re-apply your change."

// BrokenRef is one configured lane/seat whose adapter no longer resolves.
type BrokenRef struct {
	Profile string `json:"profile"`
	Ref     string `json:"ref"`  // "author_remediator" | "reviewers[1]" …
	Role    string `json:"role"` // the human label
	Adapter string `json:"adapter"`
	Detail  string `json:"detail"`
}

// ConfigIntegrity reports whether any configured lane or reviewer seat names an adapter that no longer
// exists. Computing it runs no binary and spends nothing.
type ConfigIntegrity struct {
	OK     bool        `json:"ok"`
	Broken []BrokenRef `json:"broken,omitempty"`
}
