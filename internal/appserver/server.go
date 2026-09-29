package appserver

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"

	"github.com/uranix/claude-app-server-go/internal/idgen"
	"github.com/uranix/claude-app-server-go/internal/jsonrpc"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Config are the server-wide settings a Conn is created with.
type Config struct {
	ClaudePath             string
	AllowBypassPermissions bool
	IdleTimeout            time.Duration
	MaxThreads             int
	MaxItemsPerTurn        int
}

func DefaultConfig(claudePath string) Config {
	return Config{
		ClaudePath:      claudePath,
		IdleTimeout:     10 * time.Minute,
		MaxThreads:      64,
		MaxItemsPerTurn: 2000,
	}
}

// Conn holds all state for one client connection: its threads, whether it
// has completed the initialize handshake, and how to deliver notifications.
// One Conn per transport connection (stdio process lifetime, or one per
// WebSocket) so that closing the connection can evict exactly its threads.
type Conn struct {
	cfg  Config
	send Sender

	mu          sync.Mutex
	initialized bool
	threads     map[string]*Thread
}

func NewConn(cfg Config, send Sender) *Conn {
	return &Conn{cfg: cfg, send: send, threads: make(map[string]*Thread)}
}

// Close terminates every thread's claude process. Call when the underlying
// transport connection closes.
func (c *Conn) Close() {
	c.mu.Lock()
	threads := make([]*Thread, 0, len(c.threads))
	for _, t := range c.threads {
		threads = append(threads, t)
	}
	c.threads = make(map[string]*Thread)
	c.mu.Unlock()

	for _, t := range threads {
		closeThread(t)
	}
}

// HandleRequest dispatches one JSON-RPC method call and returns its result
// or an error (a *jsonrpc.Exception for well-known error codes).
func (c *Conn) HandleRequest(method string, params json.RawMessage) (any, error) {
	if method != "initialize" {
		c.mu.Lock()
		ready := c.initialized
		c.mu.Unlock()
		if !ready {
			return nil, jsonrpc.NewException(jsonrpc.ErrNotInitialized, "call initialize first", nil)
		}
	}

	switch method {
	case "initialize":
		return c.handleInitialize(params)
	case "thread/start":
		return c.handleThreadStart(params)
	case "thread/resume":
		return c.handleThreadResume(params)
	case "thread/fork":
		return c.handleThreadFork(params)
	case "thread/attach":
		return c.handleThreadAttach(params)
	case "thread/close":
		return c.handleThreadClose(params)
	case "turn/start":
		return c.handleTurnStart(params)
	case "turn/steer":
		return c.handleTurnSteer(params)
	case "turn/interrupt":
		return c.handleTurnInterrupt(params)
	case "approval/respond":
		return c.handleApprovalRespond(params)
	case "model/list":
		return handleModelList()
	case "skills/list":
		return handleSkillsList()
	case "app/list":
		return map[string]any{"apps": []any{}}, nil
	default:
		return nil, jsonrpc.NewException(jsonrpc.ErrMethodNotFound, "unknown method: "+method, nil)
	}
}

// ─── initialize ─────────────────────────────────────────────────────────────

type initializeParams struct {
	Client json.RawMessage `json:"client"`
	Cwd    string          `json:"cwd"`
}

func (c *Conn) handleInitialize(raw json.RawMessage) (any, error) {
	var p initializeParams
	_ = json.Unmarshal(raw, &p)

	c.mu.Lock()
	c.initialized = true
	c.mu.Unlock()

	c.send("initialized", map[string]any{"server": "claude-app-server"})

	return map[string]any{
		"server": map[string]any{
			"name":    "claude-app-server-go",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{},
	}, nil
}

// ─── thread/* ───────────────────────────────────────────────────────────────

type threadStartParams struct {
	Cwd            string  `json:"cwd"`
	PermissionMode *string `json:"permission_mode"`
}

func (c *Conn) handleThreadStart(raw json.RawMessage) (any, error) {
	var p threadStartParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid params", nil)
		}
	}
	mode, err := c.checkPermissionMode(p.PermissionMode)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if len(c.threads) >= c.cfg.MaxThreads {
		c.mu.Unlock()
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "thread limit reached for this connection", nil)
	}
	t := &Thread{
		ID:             idgen.UUIDv4(),
		CreatedAt:      nowMillis(),
		Cwd:            p.Cwd,
		PermissionMode: mode,
	}
	c.threads[t.ID] = t
	c.mu.Unlock()

	// A fresh thread's CLI session id is its thread id (see ensureSession),
	// so clients can persist it and thread/attach to it after a restart.
	return map[string]any{"thread_id": t.ID, "created_at": t.CreatedAt, "cli_session_id": t.ID}, nil
}

// handleThreadClose kills the thread's claude process (aborting any running
// turn) and forgets the thread, freeing its slot against MaxThreads. The
// conversation itself is not deleted: it lives in the CLI's session file and
// can be continued later with thread/attach and the returned cli_session_id.
func (c *Conn) handleThreadClose(raw json.RawMessage) (any, error) {
	var p threadIDParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid params", nil)
	}
	c.mu.Lock()
	t, ok := c.threads[p.ThreadID]
	delete(c.threads, p.ThreadID)
	c.mu.Unlock()
	if !ok {
		return nil, jsonrpc.NewException(jsonrpc.ErrThreadNotFound, "no such thread: "+p.ThreadID, nil)
	}

	t.mu.Lock()
	sid := t.CliSessionID
	t.mu.Unlock()
	closeThread(t)

	return map[string]any{"thread_id": p.ThreadID, "cli_session_id": sid, "closed": true}, nil
}

type threadAttachParams struct {
	CliSessionID   string  `json:"cli_session_id"`
	Cwd            string  `json:"cwd"`
	PermissionMode *string `json:"permission_mode"`
	Model          *string `json:"model"`
}

// handleThreadAttach creates a thread bound to an existing claude CLI
// session, so a client can continue a conversation after this server (or its
// connection) was restarted. The process is spawned lazily with --resume on
// the first turn; cwd must match the one the session was created in, because
// the CLI stores sessions per project directory.
func (c *Conn) handleThreadAttach(raw json.RawMessage) (any, error) {
	var p threadAttachParams
	if err := json.Unmarshal(raw, &p); err != nil || !uuidRe.MatchString(p.CliSessionID) {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "cli_session_id must be a UUID", nil)
	}
	mode, err := c.checkPermissionMode(p.PermissionMode)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.threads {
		t.mu.Lock()
		same := t.CliSessionID == p.CliSessionID
		id := t.ID
		t.mu.Unlock()
		if same {
			return map[string]any{"thread_id": id, "cli_session_id": p.CliSessionID, "attached": false}, nil
		}
	}
	if len(c.threads) >= c.cfg.MaxThreads {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "thread limit reached for this connection", nil)
	}
	t := &Thread{
		ID:             idgen.UUIDv4(),
		CreatedAt:      nowMillis(),
		Cwd:            p.Cwd,
		PermissionMode: mode,
		CliSessionID:   p.CliSessionID,
	}
	if p.Model != nil {
		t.Model = *p.Model
	}
	c.threads[t.ID] = t
	return map[string]any{"thread_id": t.ID, "cli_session_id": p.CliSessionID, "attached": true}, nil
}

type threadIDParams struct {
	ThreadID string `json:"thread_id"`
}

func (c *Conn) lookupThread(id string) (*Thread, error) {
	c.mu.Lock()
	t, ok := c.threads[id]
	c.mu.Unlock()
	if !ok {
		return nil, jsonrpc.NewException(jsonrpc.ErrThreadNotFound, "no such thread: "+id, nil)
	}
	return t, nil
}

func (c *Conn) handleThreadResume(raw json.RawMessage) (any, error) {
	var p threadIDParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid params", nil)
	}
	t, err := c.lookupThread(p.ThreadID)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot(), nil
}

func (c *Conn) handleThreadFork(raw json.RawMessage) (any, error) {
	var p threadIDParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid params", nil)
	}
	src, err := c.lookupThread(p.ThreadID)
	if err != nil {
		return nil, err
	}

	src.mu.Lock()
	cliSessionID := src.CliSessionID
	cwd := src.Cwd
	mode := src.PermissionMode
	src.mu.Unlock()

	if cliSessionID == "" {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "thread has no claude session yet; run a turn first", nil)
	}

	c.mu.Lock()
	if len(c.threads) >= c.cfg.MaxThreads {
		c.mu.Unlock()
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "thread limit reached for this connection", nil)
	}
	fork := &Thread{
		ID:             idgen.UUIDv4(),
		CreatedAt:      nowMillis(),
		Cwd:            cwd,
		PermissionMode: mode,
		ForkFromCli:    cliSessionID,
	}
	c.threads[fork.ID] = fork
	c.mu.Unlock()

	return map[string]any{
		"thread_id":   fork.ID,
		"forked_from": p.ThreadID,
		"created_at":  fork.CreatedAt,
	}, nil
}

// ─── turn/* ─────────────────────────────────────────────────────────────────

type turnStartParams struct {
	ThreadID string  `json:"thread_id"`
	Content  string  `json:"content"`
	Model    *string `json:"model"`
}

func (c *Conn) handleTurnStart(raw json.RawMessage) (any, error) {
	var p turnStartParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid params", nil)
	}
	t, err := c.lookupThread(p.ThreadID)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.turnQueue) > 0 {
		return nil, jsonrpc.NewException(jsonrpc.ErrTurnBusy, "a turn is already active on this thread; use turn/steer", nil)
	}
	if p.Model != nil {
		t.Model = *p.Model
	}

	turn := &Turn{
		ID:          idgen.UUIDv4(),
		ThreadID:    t.ID,
		Status:      TurnActive,
		UserContent: p.Content,
		CreatedAt:   nowMillis(),
	}
	t.Turns = append(t.Turns, turn)
	t.turnQueue = append(t.turnQueue, turn.ID)

	if err := c.ensureSession(t); err != nil {
		t.turnQueue = t.turnQueue[:len(t.turnQueue)-1]
		turn.Status = TurnError
		turn.Error = err.Error()
		return nil, jsonrpc.NewException(jsonrpc.ErrInternal, "failed to start claude: "+err.Error(), nil)
	}
	if err := t.session.SendUserMessage(p.Content); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInternal, "failed to send turn: "+err.Error(), nil)
	}
	c.resetIdleTimer(t)

	return map[string]any{"turn_id": turn.ID}, nil
}

type turnSteerParams struct {
	ThreadID string `json:"thread_id"`
	Content  string `json:"content"`
}

func (c *Conn) handleTurnSteer(raw json.RawMessage) (any, error) {
	var p turnSteerParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid params", nil)
	}
	t, err := c.lookupThread(p.ThreadID)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.turnQueue) == 0 || t.session == nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrNoActiveTurn, "no active turn to steer; use turn/start", nil)
	}

	turn := &Turn{
		ID:          idgen.UUIDv4(),
		ThreadID:    t.ID,
		Status:      TurnActive,
		UserContent: p.Content,
		CreatedAt:   nowMillis(),
	}
	t.Turns = append(t.Turns, turn)
	t.turnQueue = append(t.turnQueue, turn.ID)

	if err := t.session.SendUserMessage(p.Content); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInternal, "failed to send steer message: "+err.Error(), nil)
	}
	c.resetIdleTimer(t)

	return map[string]any{"turn_id": turn.ID}, nil
}

func (c *Conn) handleTurnInterrupt(raw json.RawMessage) (any, error) {
	var p threadIDParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid params", nil)
	}
	t, err := c.lookupThread(p.ThreadID)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	if len(t.turnQueue) == 0 || t.session == nil {
		t.mu.Unlock()
		return nil, jsonrpc.NewException(jsonrpc.ErrNoActiveTurn, "no active turn to interrupt", nil)
	}
	activeTurnID := t.turnQueue[0]
	session := t.session
	t.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := session.Interrupt(ctx); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInternal, "interrupt failed: "+err.Error(), nil)
	}

	// The actual turn status (interrupted) is finalized asynchronously when
	// the CLI's result{terminal_reason:"aborted_streaming"} event arrives
	// and turn/completed fires; this return only confirms the request was
	// accepted by the process.
	return map[string]any{"turn_id": activeTurnID, "status": "interrupted"}, nil
}

// ─── approval/respond ───────────────────────────────────────────────────────

type approvalRespondParams struct {
	ThreadID       string  `json:"thread_id"`
	Approved       bool    `json:"approved"`
	PermissionMode *string `json:"permission_mode"`
}

func (c *Conn) handleApprovalRespond(raw json.RawMessage) (any, error) {
	var p approvalRespondParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid params", nil)
	}
	t, err := c.lookupThread(p.ThreadID)
	if err != nil {
		return nil, err
	}

	if !p.Approved {
		t.mu.Lock()
		mode := t.PermissionMode
		t.mu.Unlock()
		return map[string]any{"thread_id": t.ID, "permission_mode": mode}, nil
	}

	reqMode := p.PermissionMode
	if reqMode == nil {
		accept := string(ModeAcceptEdits)
		reqMode = &accept
	}
	mode, err := c.checkPermissionMode(reqMode)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	session := t.session
	t.mu.Unlock()

	if session != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		confirmed, err := session.SetPermissionMode(ctx, string(mode))
		cancel()
		if err == nil && confirmed != "" {
			mode = PermissionMode(confirmed)
		}
	}

	t.mu.Lock()
	t.PermissionMode = mode
	t.mu.Unlock()

	return map[string]any{"thread_id": t.ID, "permission_mode": mode}, nil
}

// ─── permission mode validation ─────────────────────────────────────────────

func (c *Conn) checkPermissionMode(raw *string) (PermissionMode, error) {
	if raw == nil {
		return ModeDefault, nil
	}
	mode := PermissionMode(*raw)
	if mode == ModeBypassPermissions {
		if !c.cfg.AllowBypassPermissions {
			return "", jsonrpc.NewException(jsonrpc.ErrPermissionRefused,
				"bypassPermissions is disabled; restart with --dangerously-allow-bypass-permissions", nil)
		}
		return mode, nil
	}
	if !clientSelectableModes[mode] {
		return "", jsonrpc.NewException(jsonrpc.ErrInvalidParams, "unknown permission_mode: "+string(mode), nil)
	}
	return mode, nil
}

// ─── discovery ──────────────────────────────────────────────────────────────

func handleModelList() (any, error) {
	return map[string]any{
		"models": []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5"},
	}, nil
}

func handleSkillsList() (any, error) {
	return map[string]any{
		"skills": []string{
			"read_file", "write_file", "edit_file", "bash",
			"glob", "grep", "web_fetch", "web_search", "task", "todo_write",
		},
	}, nil
}
