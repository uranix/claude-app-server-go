// Package jsonrpc implements the JSON-RPC 2.0 message shapes used by
// claude-app-server, exchanged as newline-delimited JSON (NDJSON).
package jsonrpc

import "encoding/json"

// ID is either a string, a number, or null, per the JSON-RPC 2.0 spec.
type ID = json.RawMessage

// Request is a client -> server call that expects a Response.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      ID              `json:"id"`
}

// Notification carries no ID and expects no response. Used both directions.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Error is the JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Response is a server -> client reply to a Request.
type Response struct {
	JSONRPC string `json:"jsonrpc"`
	Result  any    `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`
	ID      ID     `json:"id"`
}

// Incoming is the minimal shape needed to tell a Request from a Notification
// (presence of the "id" key) before decoding params.
type Incoming struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      *ID             `json:"id,omitempty"`
}

// Standard JSON-RPC 2.0 error codes, plus this server's own range.
const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603

	ErrNotInitialized    = -32000
	ErrThreadNotFound    = -32001
	ErrTurnBusy          = -32003
	ErrNoActiveTurn      = -32004
	ErrPermissionRefused = -32005
)

// Exception is an error carrying a JSON-RPC error code, for handlers to
// return so the dispatcher can translate it into an Error response.
type Exception struct {
	Code int
	Msg  string
	Data any
}

func (e *Exception) Error() string { return e.Msg }

func NewException(code int, msg string, data any) *Exception {
	return &Exception{Code: code, Msg: msg, Data: data}
}

func Ok(id ID, result any) Response {
	return Response{JSONRPC: "2.0", Result: result, ID: id}
}

func Err(id ID, code int, message string, data any) Response {
	return Response{JSONRPC: "2.0", Error: &Error{Code: code, Message: message, Data: data}, ID: id}
}

func Notif(method string, params any) Notification {
	raw, _ := json.Marshal(params)
	return Notification{JSONRPC: "2.0", Method: method, Params: raw}
}

// ParseLine parses one NDJSON line into an Incoming message. Returns nil, nil
// on a blank line; nil, err on malformed JSON.
func ParseLine(line []byte) (*Incoming, error) {
	trimmed := trimSpace(line)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var msg Incoming
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		return nil, err
	}
	if msg.JSONRPC != "2.0" || msg.Method == "" {
		return nil, errInvalid
	}
	return &msg, nil
}

var errInvalid = NewException(ErrInvalidRequest, "invalid JSON-RPC message", nil)

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
