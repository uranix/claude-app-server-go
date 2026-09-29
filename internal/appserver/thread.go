package appserver

import (
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

		case "result":
			if turnID := popTurn(t); turnID != "" {
				if turn := t.findTurn(turnID); turn != nil {
					for _, d := range ev.PermissionDenials {
						toSend = append(toSend, pendingNotif{"turn/permission_denied", map[string]any{
							"thread_id":  t.ID,
							"turn_id":    turnID,
							"tool_name":  d.ToolName,
							"tool_input": d.ToolInput,
						}})
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
			}

		case "exit":
			leftover := t.turnQueue
			t.turnQueue = nil
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
	t.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}
}
