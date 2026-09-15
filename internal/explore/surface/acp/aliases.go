package acp

// The ACP transport (JSON-RPC framing and the session store) lives in meshcore/acp. These aliases let this
// package's handler and its callers refer to it as acp.Framer, acp.SessionStore and so on.

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
