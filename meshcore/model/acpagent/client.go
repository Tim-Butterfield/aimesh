package acpagent

import (
	"encoding/json"
	"fmt"

	"github.com/Tim-Butterfield/aimesh/meshcore/acp"
)

// rpcError is a JSON-RPC 2.0 error object. It doubles as a Go error so a failed
// call (e.g. an account-gated session/new) surfaces as an adapter failure.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("acp rpc error %d: %s", e.Code, e.Message) }

// rpcMessage is the union JSON-RPC frame (request / response / notification). Only the
// fields present on a given kind are populated; omitempty keeps the wire form valid.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// sessionUpdate is one streamed `session/update` payload. We only consume the
// assistant's message text (agent_message_chunk); thought chunks and other update
// kinds are ignored for the review payload.
type sessionUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	Content       struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// client is a minimal synchronous JSON-RPC 2.0 client over meshcore's ACP framer. It
// issues one outstanding request at a time (the review flow is strictly sequential),
// pumping interleaved notifications and auto-answering agent→client requests so the
// agent never blocks waiting on a client it drives read-only.
type client struct {
	fr   acp.Framer
	next int
}

func newClient(fr acp.Framer) *client { return &client{fr: fr} }

func (c *client) write(m rpcMessage) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.fr.WriteMessage(b)
}

// notify sends a fire-and-forget JSON-RPC notification (no id, no response read). Used for a
// best-effort shutdown on cleanup, where blocking on a response could hang.
func (c *client) notify(method string, params any) error {
	var praw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		praw = b
	}
	return c.write(rpcMessage{Method: method, Params: praw})
}

// call sends a request and reads until the matching response arrives. Notifications are
// delivered to onUpdate (may be nil); agent→client requests are auto-answered. It returns
// the response `result` bytes, or the JSON-RPC error, or a transport error (e.g. EOF when
// the process is killed on timeout).
func (c *client) call(method string, params any, onUpdate func(sessionUpdate)) (json.RawMessage, error) {
	c.next++
	idRaw, _ := json.Marshal(c.next)
	var praw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		praw = b
	}
	if err := c.write(rpcMessage{ID: idRaw, Method: method, Params: praw}); err != nil {
		return nil, err
	}
	for {
		raw, err := c.fr.ReadMessage()
		if err != nil {
			return nil, err
		}
		var m rpcMessage
		if json.Unmarshal(raw, &m) != nil {
			continue // ignore an unparseable frame rather than aborting the turn
		}
		switch {
		case m.Method == "" && len(m.ID) > 0: // a response
			if string(m.ID) == string(idRaw) {
				if m.Error != nil {
					return nil, m.Error
				}
				return m.Result, nil
			}
			// stale response to a prior id — ignore (sequential calls)
		case m.Method != "" && len(m.ID) > 0: // an agent→client request
			c.answerRequest(m)
		case m.Method != "": // a notification
			if m.Method == "session/update" && onUpdate != nil {
				var p struct {
					Update sessionUpdate `json:"update"`
				}
				if json.Unmarshal(m.Params, &p) == nil {
					onUpdate(p.Update)
				}
			}
		}
	}
}

// answerRequest replies to an agent→client request so the agent proceeds. The client is a
// read-only reviewer: it advertised no filesystem/terminal capability and grants no tool
// permission, so it cancels permission prompts and refuses any other client-side method.
func (c *client) answerRequest(m rpcMessage) {
	switch m.Method {
	case "session/request_permission":
		res, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		_ = c.write(rpcMessage{ID: m.ID, Result: res})
	default:
		_ = c.write(rpcMessage{ID: m.ID, Error: &rpcError{Code: -32601, Message: "method not supported by this client"}})
	}
}
