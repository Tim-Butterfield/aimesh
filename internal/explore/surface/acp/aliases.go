package acp

// The domain-free ACP transport (JSON-RPC framing + the cross-restart session store) lives in
// meshcore/acp. These re-exports keep the exploremesh ACP handler (acp.go) and its in-repo callers
// (cli, testhost) referencing `acp.Framer` / `acp.NewFramer` / `acp.SessionStore` / etc. — this package
// retains only the exploremesh-specific handler. meshcore stays domain-free (no exploremesh vocabulary).

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
