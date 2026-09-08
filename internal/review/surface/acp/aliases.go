package acp

// The domain-free ACP transport (JSON-RPC framing + the cross-restart session store) now lives in
// meshcore/acp. These re-exports keep the reviewmesh ACP handler (acp.go) and its external callers
// (cli, webui, testhost) referencing `acp.Framer` / `acp.NewFramer` / `acp.SessionStore` / etc.
// unchanged — this package retains only the review-specific handler + validation.

import macp "github.com/Tim-Butterfield/aimesh/meshcore/acp"

type (
	Framer        = macp.Framer
	SessionStore  = macp.SessionStore
	SessionRecord = macp.SessionRecord
)

const (
	FramingNewline       = macp.FramingNewline
	FramingContentLength = macp.FramingContentLength
	SessionRecordSchema  = macp.SessionRecordSchema
)

var (
	NewFramer           = macp.NewFramer
	NewFileSessionStore = macp.NewFileSessionStore
	ErrSessionNotFound  = macp.ErrSessionNotFound
	ErrSessionMalformed = macp.ErrSessionMalformed
	ErrSessionExpired   = macp.ErrSessionExpired
)
