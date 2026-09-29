// Package claudecli manages a persistent `claude` CLI subprocess per thread,
// fed over --input-format stream-json, so a thread's process survives across
// turns instead of being respawned for each one.
package claudecli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
)

// TurnStatus mirrors the TS server's Turn.status values.
type TurnStatus string

const (
	StatusCompleted   TurnStatus = "completed"
	StatusInterrupted TurnStatus = "interrupted"
	StatusError       TurnStatus = "error"
)

// Item mirrors the finalized content items reported via item/created.
type Item struct {
	Type      string          `json:"type"` // text | thinking | tool_call | tool_result
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Event is the union of things a Session reports as it processes turns.
type Event struct {
	Kind string // "init" | "progress" | "item" | "result" | "consumed" | "exit"

	// init
	SessionID      string
	PermissionMode string
	Model          string
	Cwd            string

	// progress
	DeltaType string // text | thinking | tool_input
	Text      string

	// item
	Item Item

	// result
	Status            TurnStatus
	PermissionDenials []PermissionDenial
	ResultText        string

	// consumed: the CLI took the user message with this ID off its queue
	MessageID string

	// permission_request / permission_cancel
	RequestID   string
	ToolName    string
	ToolUseID   string
	Description string
	Input       json.RawMessage
	Suggestions json.RawMessage

	// exit
	Err error
}

// StartOptions configures how the claude process is spawned.
type StartOptions struct {
	ClaudePath     string
	Cwd            string
	PermissionMode string
	Model          string

	// PermissionPrompts routes "may I use this tool?" decisions to the host
	// (--permission-prompt-tool stdio) instead of denying them silently. The CLI
	// then blocks on each decision until the host answers.
	PermissionPrompts bool

	// AppendSystemPrompt is added to Claude's default system prompt
	// (--append-system-prompt). It must be passed on every spawn, resumes included.
	AppendSystemPrompt string

	// Exactly one of these describes session identity.
	NewSessionID string // first turn of a thread: --session-id <uuid>
	ResumeID     string // subsequent spawns: --resume <id>
	ForkSession  bool   // combined with ResumeID: --resume <id> --fork-session
}

// Session wraps one running `claude` child process.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan Event

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan controlResult

	sentMu sync.Mutex
	sent   map[string]bool // message IDs given to SendUserMessage and not yet consumed

	waitOnce sync.Once
	waitErr  error
}

// Start spawns the claude CLI with the given options and begins reading its
// stdout in a background goroutine. Events are delivered on Session.Events()
// until the process exits, at which point a final Kind:"exit" event is sent
// and the channel is closed.
func Start(opts StartOptions) (*Session, error) {
	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--replay-user-messages",
		"--permission-mode", opts.PermissionMode,
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.PermissionPrompts {
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	switch {
	case opts.ResumeID != "":
		args = append(args, "--resume", opts.ResumeID)
		if opts.ForkSession {
			args = append(args, "--fork-session")
		}
	case opts.NewSessionID != "":
		args = append(args, "--session-id", opts.NewSessionID)
	default:
		return nil, errors.New("claudecli: must set NewSessionID or ResumeID")
	}

	cmd := exec.Command(opts.ClaudePath, args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &Session{
		cmd:     cmd,
		stdin:   stdin,
		events:  make(chan Event, 64),
		pending: make(map[string]chan controlResult),
	}
	go s.readLoop(stdout)
	return s, nil
}

// Events returns the channel of parsed events for this session.
func (s *Session) Events() <-chan Event { return s.events }

// SendUserMessage feeds one user turn (initial content, or a steer message
// sent while a turn is in flight) to the CLI's stdin. The CLI queues and
// runs it after any turn currently in progress -- this is the entirety of
// the "steering" mechanism; no separate control message is needed.
// A non-empty messageID is reported back as a "consumed" event when the CLI
// actually starts processing the message.
func (s *Session) SendUserMessage(content, messageID string) error {
	if messageID != "" {
		s.sentMu.Lock()
		if s.sent == nil {
			s.sent = map[string]bool{}
		}
		s.sent[messageID] = true
		s.sentMu.Unlock()
	}
	msg := UserInputMessage{
		Type: "user",
		UUID: messageID,
		Message: UserInputPayload{
			Role:    "user",
			Content: content,
		},
	}
	return s.writeJSONLine(msg)
}

// Interrupt sends a control_request{subtype:"interrupt"} and waits for the
// corresponding control_response. The in-flight turn ends with a
// result{subtype:"error_during_execution", terminal_reason:"aborted_streaming"}
// event on the Events channel; the process itself survives.
func (s *Session) Interrupt(ctx context.Context) error {
	_, err := s.controlRequest(ctx, map[string]any{"subtype": "interrupt"})
	return err
}

// SetPermissionMode issues a runtime control_request to change the mode of
// the running session, returning the mode the CLI confirms.
func (s *Session) SetPermissionMode(ctx context.Context, mode string) (string, error) {
	resp, err := s.controlRequest(ctx, map[string]any{"subtype": "set_permission_mode", "mode": mode})
	if err != nil {
		return "", err
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(resp, &body); err != nil {
		return "", err
	}
	return body.Mode, nil
}

// RespondPermission answers a can_use_tool request. payload is the decision:
// {"behavior":"allow","updatedInput":...[,"updatedPermissions":...]} or
// {"behavior":"deny","message":...}.
func (s *Session) RespondPermission(requestID string, payload any) error {
	return s.writeJSONLine(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype": "success", "request_id": requestID, "response": payload,
		},
	})
}

// Close terminates the process (used for idle reaping) and releases
// resources. It does not block waiting for exit; the read loop's exit event
// and Wait() handle that asynchronously.
func (s *Session) Close() error {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	go func() { _ = s.Wait() }() // reap the child so it does not linger as a zombie
	return nil
}

// Wait blocks until the process has exited and returns its error, if any.
// Safe to call from multiple goroutines; the underlying cmd.Wait runs once.
func (s *Session) Wait() error {
	s.waitOnce.Do(func() {
		s.waitErr = s.cmd.Wait()
	})
	return s.waitErr
}

// controlResult is what a pending control_request resolves to: either the
// CLI's response payload, or an error (a wire-level failure, or the CLI's
// own control_response{subtype:"error"}).
type controlResult struct {
	response json.RawMessage
	err      error
}

func (s *Session) controlRequest(ctx context.Context, request any) (json.RawMessage, error) {
	id := randomHex(8)
	ch := make(chan controlResult, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}()

	out := ControlRequestOut{Type: "control_request", RequestID: id, Request: request}
	if err := s.writeJSONLine(out); err != nil {
		return nil, err
	}

	select {
	case res := <-ch:
		return res.response, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Session) writeJSONLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(b)
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0xF]
	}
	return string(out)
}

// blockState accumulates a single content block's streamed deltas until its
// content_block_stop, at which point it is finalized into an Item.
type blockState struct {
	blockType   string // text | thinking | tool_use
	toolUseID   string
	toolName    string
	text        string
	thinking    string
	partialJSON string
}

func (s *Session) readLoop(stdout io.Reader) {
	defer close(s.events)

	br := bufio.NewReaderSize(stdout, 64*1024)
	blocks := map[int]*blockState{}

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			s.handleLine(line, blocks)
		}
		if err != nil {
			if err != io.EOF {
				s.events <- Event{Kind: "exit", Err: err}
			} else {
				s.events <- Event{Kind: "exit", Err: nil}
			}
			return
		}
	}
}

func (s *Session) handleLine(line []byte, blocks map[int]*blockState) {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}

	switch env.Type {
	case "system":
		var init SystemInit
		if err := json.Unmarshal(line, &init); err != nil || init.Subtype != "init" {
			return
		}
		s.events <- Event{
			Kind:           "init",
			SessionID:      init.SessionID,
			PermissionMode: init.PermissionMode,
			Model:          init.Model,
			Cwd:            init.Cwd,
		}

	case "stream_event":
		var se StreamEvent
		if err := json.Unmarshal(line, &se); err != nil {
			return
		}
		s.handleStreamEvent(se.Event, blocks)

	case "user":
		// A replayed prompt has a plain string as content, which UserMessage
		// (a list of tool results) cannot decode, so read the echo header first.
		var echo struct {
			UUID     string `json:"uuid"`
			IsReplay bool   `json:"isReplay"`
		}
		if json.Unmarshal(line, &echo) == nil && echo.IsReplay && echo.UUID != "" {
			s.sentMu.Lock()
			ours := s.sent[echo.UUID]
			delete(s.sent, echo.UUID)
			s.sentMu.Unlock()
			if ours {
				s.events <- Event{Kind: "consumed", MessageID: echo.UUID}
			}
			return
		}
		var um UserMessage
		if err := json.Unmarshal(line, &um); err != nil {
			return
		}
		for _, c := range um.Message.Content {
			if c.Type != "tool_result" {
				continue
			}
			s.events <- Event{Kind: "item", Item: Item{
				Type:      "tool_result",
				ToolUseID: c.ToolUseID,
				Content:   contentToString(c.Content),
				IsError:   c.IsError,
			}}
		}

	case "result":
		var res Result
		if err := json.Unmarshal(line, &res); err != nil {
			return
		}
		status := StatusCompleted
		switch {
		case res.Subtype == "error_during_execution" &&
			(res.TerminalReason == "aborted_streaming" || res.TerminalReason == "aborted_tools"):
			// aborted_tools: interrupted while a tool (or its permission prompt) was pending
			status = StatusInterrupted
		case res.IsError || res.Subtype != "success":
			status = StatusError
		}
		s.events <- Event{
			Kind:              "result",
			Status:            status,
			PermissionDenials: res.PermissionDenials,
			ResultText:        res.Result,
		}

	case "control_response":
		var cr ControlResponse
		if err := json.Unmarshal(line, &cr); err != nil {
			return
		}
		s.pendingMu.Lock()
		ch, ok := s.pending[cr.Response.RequestID]
		s.pendingMu.Unlock()
		if !ok {
			return
		}
		if cr.Response.Subtype == "error" {
			ch <- controlResult{err: errors.New(cr.Response.Error)}
		} else {
			ch <- controlResult{response: cr.Response.Response}
		}

	case "control_request":
		// Only present when started with PermissionPrompts. The CLI waits for our
		// answer with no timeout of its own, so the caller must always respond.
		var cr ControlRequest
		if err := json.Unmarshal(line, &cr); err != nil {
			return
		}
		var body ControlRequestBody
		if err := json.Unmarshal(cr.Request, &body); err != nil || body.Subtype != "can_use_tool" {
			return
		}
		s.events <- Event{
			Kind: "permission_request", RequestID: cr.RequestID,
			ToolName: body.ToolName, ToolUseID: body.ToolUseID, Description: body.Description,
			Input: body.Input, Suggestions: body.PermissionSuggestions,
		}

	case "control_cancel_request":
		// The CLI withdrew a pending prompt (the turn was interrupted).
		var c struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(line, &c); err != nil || c.RequestID == "" {
			return
		}
		s.events <- Event{Kind: "permission_cancel", RequestID: c.RequestID}
	}
}

func (s *Session) handleStreamEvent(ev RawEvent, blocks map[int]*blockState) {
	switch ev.Type {
	case "content_block_start":
		if ev.ContentBlock == nil {
			return
		}
		blocks[ev.Index] = &blockState{
			blockType: ev.ContentBlock.Type,
			toolUseID: ev.ContentBlock.ID,
			toolName:  ev.ContentBlock.Name,
		}

	case "content_block_delta":
		if ev.Delta == nil {
			return
		}
		bs := blocks[ev.Index]
		if bs == nil {
			bs = &blockState{}
			blocks[ev.Index] = bs
		}
		switch ev.Delta.Type {
		case "text_delta":
			bs.text += ev.Delta.Text
			s.events <- Event{Kind: "progress", DeltaType: "text", Text: ev.Delta.Text}
		case "thinking_delta":
			bs.thinking += ev.Delta.Thinking
			s.events <- Event{Kind: "progress", DeltaType: "thinking", Text: ev.Delta.Thinking}
		case "input_json_delta":
			bs.partialJSON += ev.Delta.PartialJSON
			s.events <- Event{Kind: "progress", DeltaType: "tool_input", Text: ev.Delta.PartialJSON}
		}

	case "content_block_stop":
		bs := blocks[ev.Index]
		if bs == nil {
			return
		}
		delete(blocks, ev.Index)
		switch bs.blockType {
		case "text":
			s.events <- Event{Kind: "item", Item: Item{Type: "text", Text: bs.text}}
		case "thinking":
			s.events <- Event{Kind: "item", Item: Item{Type: "thinking", Thinking: bs.thinking}}
		case "tool_use":
			input := json.RawMessage(bs.partialJSON)
			if !json.Valid(input) {
				input = json.RawMessage("{}")
			}
			s.events <- Event{Kind: "item", Item: Item{
				Type:      "tool_call",
				ToolUseID: bs.toolUseID,
				Name:      bs.toolName,
				Input:     input,
			}}
		}
	}
}

func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// tool_result content can also be a content-block array; fall back to
	// its raw JSON rather than losing the data.
	return string(raw)
}
