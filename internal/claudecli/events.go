package claudecli

import "encoding/json"

// Envelope is the outer shape of every line the claude CLI writes to stdout
// under --output-format stream-json. Type discriminates the payload.
type Envelope struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

// SystemInit is the "system" / subtype "init" message, re-emitted at the
// start of every turn (not once per session, despite the name).
type SystemInit struct {
	Type           string   `json:"type"`
	Subtype        string   `json:"subtype"`
	SessionID      string   `json:"session_id"`
	Cwd            string   `json:"cwd"`
	PermissionMode string   `json:"permissionMode"`
	Model          string   `json:"model"`
	Tools          []string `json:"tools"`
}

// StreamEvent wraps a raw Anthropic Messages API streaming event, as emitted
// with --include-partial-messages.
type StreamEvent struct {
	Type  string   `json:"type"`
	Event RawEvent `json:"event"`
}

type RawEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
	Delta        *Delta        `json:"delta,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Delta covers text_delta, thinking_delta, and input_json_delta payloads;
// only the field relevant to the delta's Type is populated.
type Delta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

// AssistantMessage is the "assistant" envelope carrying a finalized message
// (used to recover tool_use blocks with their full, parsed input once
// streaming for that block completes).
type AssistantMessage struct {
	Type    string `json:"type"`
	Message struct {
		Content []ContentItem `json:"content"`
	} `json:"message"`
}

type ContentItem struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// UserMessage carries tool_result blocks back from the CLI's perspective
// (it echoes the tool results it fed to the model).
type UserMessage struct {
	Type    string `json:"type"`
	Message struct {
		Content []ContentItem `json:"content"`
	} `json:"message"`
}

// Result is the terminal message for a turn.
type Result struct {
	Type              string             `json:"type"`
	Subtype           string             `json:"subtype"`
	TerminalReason    string             `json:"terminal_reason,omitempty"`
	IsError           bool               `json:"is_error"`
	Result            string             `json:"result,omitempty"`
	SessionID         string             `json:"session_id"`
	PermissionDenials []PermissionDenial `json:"permission_denials,omitempty"`
}

type PermissionDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// ControlRequest / ControlResponse implement the bidirectional control
// channel multiplexed over the same stdout/stdin streams as turn content.
type ControlRequest struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

type ControlRequestBody struct {
	Subtype string `json:"subtype"`
	Mode    string `json:"mode,omitempty"`
	// can_use_tool fields (Phase 2; parsed for forward-compat, unused today).
	ToolName  string          `json:"tool_name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

// ControlResponse is the CLI's ack for a control_request. Unlike the request
// we send (which has request_id at the top level), the CLI nests everything
// -- subtype, request_id, the actual response payload, and any error -- one
// level deeper inside "response".
type ControlResponse struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error,omitempty"`
	} `json:"response"`
}

// ControlRequestOut is what we send TO the CLI on stdin to drive it
// (interrupt, set_permission_mode).
type ControlRequestOut struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   any    `json:"request"`
}

// UserInputMessage is what we send TO the CLI on stdin to start/steer a turn.
type UserInputMessage struct {
	Type    string           `json:"type"`
	Message UserInputPayload `json:"message"`
}

type UserInputPayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
