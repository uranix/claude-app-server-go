// Package appserver implements the claude-app-server JSON-RPC methods on
// top of a persistent claudecli.Session per thread.
package appserver

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/uranix/claude-app-server-go/internal/claudecli"
)

// PermissionMode maps directly to claude's --permission-mode flag.
type PermissionMode string

const (
	ModeDefault           PermissionMode = "default"
	ModePlan              PermissionMode = "plan"
	ModeAcceptEdits       PermissionMode = "acceptEdits"
	ModeBypassPermissions PermissionMode = "bypassPermissions"
	ModeDontAsk           PermissionMode = "dontAsk"
)

// clientSelectableModes are the modes a client may request without the
// server having opted in via a startup flag.
var clientSelectableModes = map[PermissionMode]bool{
	ModeDefault:     true,
	ModePlan:        true,
	ModeAcceptEdits: true,
	ModeDontAsk:     true,
}

type TurnStatus string

const (
	TurnActive      TurnStatus = "active"
	TurnCompleted   TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnError       TurnStatus = "error"
)

// StoredItem is one atomic unit of turn content, as reported by item/created.
type StoredItem struct {
	ID        string         `json:"id"`
	CreatedAt int64          `json:"created_at"`
	Item      claudecli.Item `json:"item"`
}

// Turn is a single user request and everything the agent did in response.
type Turn struct {
	ID          string       `json:"id"`
	ThreadID    string       `json:"thread_id"`
	Status      TurnStatus   `json:"status"`
	UserContent string       `json:"user_content"`
	Items       []StoredItem `json:"items"`
	CreatedAt   int64        `json:"created_at"`
	CompletedAt int64        `json:"completed_at,omitempty"`
	Error       string       `json:"error,omitempty"`
}

// Thread is a conversation, backed by (at most) one live claude process.
type Thread struct {
	mu sync.Mutex

	ID             string
	CreatedAt      int64
	Cwd            string
	PermissionMode PermissionMode
	Model          string

	Turns []*Turn

	// CliSessionID is the claude CLI's own session id, captured from
	// system/init. It may differ from ID for forked threads and is what
	// --resume targets after an idle reap.
	CliSessionID string
	ForkFromCli  string

	session      *claudecli.Session
	turnQueue    []string // turn IDs sent to the CLI, oldest-first
	idleTimer    *time.Timer
	lastActivity time.Time
}

func (t *Thread) findTurn(id string) *Turn {
	for _, tn := range t.Turns {
		if tn.ID == id {
			return tn
		}
	}
	return nil
}

func (t *Thread) snapshot() ThreadSnapshot {
	return ThreadSnapshot{
		ThreadID:       t.ID,
		CreatedAt:      t.CreatedAt,
		Cwd:            t.Cwd,
		PermissionMode: string(t.PermissionMode),
		Turns:          t.Turns,
	}
}

// ThreadSnapshot is the shape returned by thread/resume.
type ThreadSnapshot struct {
	ThreadID       string  `json:"thread_id"`
	CreatedAt      int64   `json:"created_at"`
	Cwd            string  `json:"cwd"`
	PermissionMode string  `json:"permission_mode"`
	Turns          []*Turn `json:"turns"`
}

// Sender delivers a JSON-RPC notification (method + params) to the client
// that owns a connection. Transports implement this by serializing to NDJSON
// / a WS text frame.
type Sender func(method string, params any)

func nowMillis() int64 { return time.Now().UnixMilli() }

func marshalRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
