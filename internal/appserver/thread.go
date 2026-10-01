package appserver

import (
	"context"
	"time"

	"github.com/uranix/claude-app-server-go/internal/claudecli"
	"github.com/uranix/claude-app-server-go/internal/idgen"
)

// currentTurnID returns the turn currently being processed by the CLI
// (the front of the queue), or "" if none.
func currentTurnID(t *Thread) string {
	if len(t.turnQueue) == 0 {
		return ""
	}
	return t.turnQueue[0]
}

// popTurn removes and returns the front of the queue (the turn whose
// processing just finished), or "" if the queue was already empty.
func popTurn(t *Thread) string {
	if len(t.turnQueue) == 0 {
		return ""
	}
	id := t.turnQueue[0]
	t.turnQueue = t.turnQueue[1:]
	return id
}

// popCompleted removes and returns the turns covered by one CLI result: every
// queued turn, oldest first, whose message the CLI has consumed. A message sent
// while the CLI was busy is folded into the running turn, so a single result can
// end several turns; assuming one result per message would leave the extra turns
// queued forever and the thread stuck as busy. When no consumption was reported
// (a CLI without replay echoes) it falls back to one turn per result.
func popCompleted(t *Thread) []string {
	var ids []string
	for len(t.turnQueue) > 0 {
		turn := t.findTurn(t.turnQueue[0])
		if turn == nil || !t.consumed[turn.MessageID] {
			break
		}
		delete(t.consumed, turn.MessageID)
		ids = append(ids, t.turnQueue[0])
		t.turnQueue = t.turnQueue[1:]
	}
	if len(ids) == 0 {
		if id := popTurn(t); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// ensureSession spawns a claude process for the thread if one isn't already
// running, resuming its CLI session (or forking from another thread's) so
// conversation context survives idle reaps. Caller must hold t.mu.
func (c *Conn) ensureSession(t *Thread) error {
	if t.session != nil {
		return nil
	}

	opts := claudecli.StartOptions{
		ClaudePath:     c.cfg.ClaudePath,
		Cwd:            t.Cwd,
		PermissionMode: string(t.PermissionMode),
		Model:          t.Model,

		AppendSystemPrompt: t.AppendSystemPrompt,
		PermissionPrompts:  t.PermissionPrompts,
	}
	switch {
	case t.CliSessionID != "":
		opts.ResumeID = t.CliSessionID
	case t.ForkFromCli != "":
		opts.ResumeID = t.ForkFromCli
		opts.ForkSession = true
	default:
		opts.NewSessionID = t.ID
	}

	session, err := claudecli.Start(opts)
	if err != nil {
		return err
	}
	t.session = session
	go c.runEventLoop(t, session)
	c.startIdleTimer(t)
	return nil
}

type pendingNotif struct {
	method string
	params any
}

// runEventLoop translates one thread's claudecli.Session events into
// JSON-RPC notifications, updating turn/thread state under t.mu and only
// calling the (possibly slow) Sender after releasing the lock.
func (c *Conn) runEventLoop(t *Thread, session *claudecli.Session) {
	for ev := range session.Events() {
		var toSend []pendingNotif

		t.mu.Lock()
		switch ev.Kind {
		case "init":
			if t.CliSessionID == "" {
				t.CliSessionID = ev.SessionID
			}
			if ev.PermissionMode != "" {
				t.PermissionMode = PermissionMode(ev.PermissionMode)
			}
			if ev.Model != "" {
				t.Model = ev.Model
			}
			go c.sendSettings(t, session)

		case "usage":
			toSend = append(toSend, pendingNotif{"thread/usage", map[string]any{
				"thread_id": t.ID, "model": ev.Model, "context_tokens": ev.ContextTokens,
				"input_tokens": ev.InputTokens, "output_tokens": ev.OutputTokens,
			}})

		case "progress":
			if turnID := currentTurnID(t); turnID != "" {
				toSend = append(toSend, pendingNotif{"item/progress", map[string]any{
					"thread_id": t.ID,
					"turn_id":   turnID,
					"delta":     map[string]any{"type": ev.DeltaType, "text": ev.Text},
				}})
			}

		case "item":
			if turnID := currentTurnID(t); turnID != "" {
				if turn := t.findTurn(turnID); turn != nil {
					si := StoredItem{ID: idgen.Hex(8), CreatedAt: nowMillis(), Item: ev.Item}
					if len(turn.Items) < c.cfg.MaxItemsPerTurn {
						turn.Items = append(turn.Items, si)
					}
					toSend = append(toSend, pendingNotif{"item/created", map[string]any{
						"thread_id": t.ID,
						"turn_id":   turnID,
						"item":      si,
					}})
				}
			}

		case "compacting":
			toSend = append(toSend, pendingNotif{"context/compacting", map[string]any{"thread_id": t.ID}})

		case "compacted":
			toSend = append(toSend, pendingNotif{"context/compacted", map[string]any{
				"thread_id": t.ID, "trigger": ev.Trigger, "pre_tokens": ev.PreTokens, "post_tokens": ev.PostTokens,
			}})

		case "consumed":
			if t.consumed == nil {
				t.consumed = map[string]bool{}
			}
			t.consumed[ev.MessageID] = true
			toSend = append(toSend, pendingNotif{"message/consumed", map[string]any{
				"thread_id": t.ID, "message_id": ev.MessageID,
			}})

		case "result":
			for n, turnID := range popCompleted(t) {
				turn := t.findTurn(turnID)
				if turn == nil {
					continue
				}
				if n == 0 { // the denials belong to the result, report them once
					for _, d := range ev.PermissionDenials {
						toSend = append(toSend, pendingNotif{"turn/permission_denied", map[string]any{
							"thread_id":  t.ID,
							"turn_id":    turnID,
							"tool_name":  d.ToolName,
							"tool_input": d.ToolInput,
						}})
					}
				}
				turn.CompletedAt = nowMillis()
				if ev.Status == claudecli.StatusError {
					turn.Status = TurnError
					turn.Error = ev.ResultText
					toSend = append(toSend, pendingNotif{"turn/error", map[string]any{
						"thread_id": t.ID,
						"turn_id":   turnID,
						"error":     ev.ResultText,
					}})
				} else {
					status := TurnCompleted
					if ev.Status == claudecli.StatusInterrupted {
						status = TurnInterrupted
					}
					turn.Status = status
					toSend = append(toSend, pendingNotif{"turn/completed", map[string]any{
						"thread_id":    t.ID,
						"turn_id":      turnID,
						"status":       status,
						"items_count":  len(turn.Items),
						"completed_at": turn.CompletedAt,
					}})
				}
			}

		case "permission_request":
			expires := c.addPending(t, session, ev)
			toSend = append(toSend, pendingNotif{"approval/requested", map[string]any{
				"thread_id":   t.ID,
				"turn_id":     currentTurnID(t),
				"request_id":  ev.RequestID,
				"tool_name":   ev.ToolName,
				"tool_use_id": ev.ToolUseID,
				"description": ev.Description,
				"input":       ev.Input,
				"suggestions": ev.Suggestions,
				"expires_at":  expires.UnixMilli(),
			}})

		case "permission_cancel":
			if pp := t.pending[ev.RequestID]; pp != nil {
				pp.timer.Stop()
				delete(t.pending, ev.RequestID)
				toSend = append(toSend, pendingNotif{"approval/cancelled", map[string]any{
					"thread_id": t.ID, "request_id": ev.RequestID, "reason": "cancelled",
				}})
			}

		case "exit":
			clearPending(t)
			leftover := t.turnQueue
			t.turnQueue = nil
			t.consumed = nil
			t.session = nil
			if ev.Err != nil {
				for _, id := range leftover {
					if turn := t.findTurn(id); turn != nil {
						turn.Status = TurnError
						turn.Error = "claude process exited unexpectedly"
						turn.CompletedAt = nowMillis()
						toSend = append(toSend, pendingNotif{"turn/error", map[string]any{
							"thread_id": t.ID,
							"turn_id":   id,
							"error":     turn.Error,
						}})
					}
				}
			}
		}
		t.mu.Unlock()

		for _, n := range toSend {
			c.send(n.method, n.params)
		}
		if ev.Kind == "exit" {
			return
		}
	}
}

// sendSettings reports the model, effort and context window the CLI applies,
// as thread/settings. Runs on its own goroutine: the control requests are
// answered through the event loop's reader.
func (c *Conn) sendSettings(t *Thread, session *claudecli.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := session.Settings(ctx)
	if err != nil {
		return
	}
	c.send("thread/settings", map[string]any{
		"thread_id": t.ID, "model": st.Model, "effort": st.Effort, "context_window": st.ContextWindow,
	})
}

// startIdleTimer arms (or re-arms) the reap timer. Caller must hold t.mu.
func (c *Conn) startIdleTimer(t *Thread) {
	if t.idleTimer != nil {
		t.idleTimer.Stop()
	}
	t.idleTimer = time.AfterFunc(c.cfg.IdleTimeout, func() { c.reapIfIdle(t) })
}

// resetIdleTimer pushes the reap deadline out; call on any turn activity.
// Caller must hold t.mu.
func (c *Conn) resetIdleTimer(t *Thread) {
	if t.idleTimer != nil {
		t.idleTimer.Reset(c.cfg.IdleTimeout)
	}
}

// reapIfIdle closes a thread's process once it has had no in-flight turns
// for the idle timeout. The next turn/start transparently respawns with
// --resume, so conversation context is preserved.
func (c *Conn) reapIfIdle(t *Thread) {
	t.mu.Lock()
	idle := len(t.turnQueue) == 0 && t.session != nil
	var session *claudecli.Session
	if idle {
		session = t.session
		t.session = nil
	}
	t.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}
}

// closeThread terminates a thread's process unconditionally, used when
// evicting threads on connection close.
func closeThread(t *Thread) {
	t.mu.Lock()
	if t.idleTimer != nil {
		t.idleTimer.Stop()
	}
	session := t.session
	t.session = nil
	clearPending(t)
	t.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}
}
