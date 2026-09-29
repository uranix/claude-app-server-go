package appserver

import (
	"encoding/json"
	"time"

	"github.com/uranix/claude-app-server-go/internal/claudecli"
	"github.com/uranix/claude-app-server-go/internal/jsonrpc"
)

// pendingPerm is a can_use_tool request the CLI is blocked on.
type pendingPerm struct {
	ToolName    string
	Input       json.RawMessage
	Suggestions json.RawMessage
	timer       *time.Timer
}

const denyOnTimeoutMessage = "The permission request timed out without an answer, so it was denied."

// addPending records a request and arms its auto-deny timer. Caller holds t.mu.
func (c *Conn) addPending(t *Thread, session *claudecli.Session, ev claudecli.Event) time.Time {
	if t.pending == nil {
		t.pending = map[string]*pendingPerm{}
	}
	pp := &pendingPerm{ToolName: ev.ToolName, Input: ev.Input, Suggestions: ev.Suggestions}
	id := ev.RequestID
	pp.timer = time.AfterFunc(c.cfg.PermissionTimeout, func() { c.expirePending(t, session, id) })
	t.pending[id] = pp
	return time.Now().Add(c.cfg.PermissionTimeout)
}

// clearPending forgets every request (process gone, thread closed). Caller holds t.mu.
func clearPending(t *Thread) {
	for id, pp := range t.pending {
		pp.timer.Stop()
		delete(t.pending, id)
	}
}

// expirePending denies a request nobody answered. The CLI never gives up on
// its own, so without this a forgotten prompt would stall the turn forever.
func (c *Conn) expirePending(t *Thread, session *claudecli.Session, id string) {
	t.mu.Lock()
	pp := t.pending[id]
	delete(t.pending, id)
	t.mu.Unlock()
	if pp == nil {
		return // answered or cancelled in the meantime
	}
	_ = session.RespondPermission(id, map[string]any{"behavior": "deny", "message": denyOnTimeoutMessage})
	c.send("approval/cancelled", map[string]any{"thread_id": t.ID, "request_id": id, "reason": "timeout"})
}

type permissionRespondParams struct {
	ThreadID         string `json:"thread_id"`
	RequestID        string `json:"request_id"`
	Behavior         string `json:"behavior"` // allow | deny
	Message          string `json:"message"`  // shown to the model on deny
	ApplySuggestions bool   `json:"apply_suggestions"`
}

// handlePermissionRespond answers a pending approval/requested. With
// apply_suggestions the CLI's own suggested rule/mode updates are applied too
// (for example "accept edits for this session").
func (c *Conn) handlePermissionRespond(raw json.RawMessage) (any, error) {
	var p permissionRespondParams
	if err := json.Unmarshal(raw, &p); err != nil || p.RequestID == "" || (p.Behavior != "allow" && p.Behavior != "deny") {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "want thread_id, request_id and behavior allow|deny", nil)
	}
	t, err := c.lookupThread(p.ThreadID)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	pp := t.pending[p.RequestID]
	delete(t.pending, p.RequestID)
	session := t.session
	t.mu.Unlock()
	if pp == nil || session == nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "no such pending permission request (already answered, expired or cancelled)", nil)
	}
	pp.timer.Stop()

	payload := map[string]any{"behavior": p.Behavior}
	if p.Behavior == "allow" {
		payload["updatedInput"] = pp.Input
		if p.ApplySuggestions && len(pp.Suggestions) > 0 {
			payload["updatedPermissions"] = pp.Suggestions
		}
	} else {
		msg := p.Message
		if msg == "" {
			msg = "The user denied this request."
		}
		payload["message"] = msg
	}
	if err := session.RespondPermission(p.RequestID, payload); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInternal, "failed to answer the CLI: "+err.Error(), nil)
	}
	return map[string]any{"thread_id": t.ID, "request_id": p.RequestID, "behavior": p.Behavior}, nil
}
