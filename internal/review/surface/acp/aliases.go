package acp

// The domain-free ACP transport (framing and session store) lives in meshcore/acp. These aliases
// let this package and its callers refer to acp.Framer, acp.NewFramer, acp.SessionStore and so on.

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
