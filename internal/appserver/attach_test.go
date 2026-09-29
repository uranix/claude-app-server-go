package appserver

import (
	"encoding/json"
	"testing"
)

func newTestConn(t *testing.T) *Conn {
	c := NewConn(DefaultConfig("/nonexistent/claude"), func(string, any) {})
	if _, err := c.HandleRequest("initialize", nil); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestThreadStartReportsCliSessionID(t *testing.T) {
	c := newTestConn(t)
	res, err := c.HandleRequest("thread/start", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["cli_session_id"] != m["thread_id"] {
		t.Fatalf("cli_session_id should equal thread_id for fresh threads: %v", m)
	}
}

func TestThreadAttach(t *testing.T) {
	c := newTestConn(t)
	const sid = "0b9d3c2e-6f7a-4c1d-9e58-1a2b3c4d5e6f"
	res, err := c.HandleRequest("thread/attach", json.RawMessage(`{"cli_session_id":"`+sid+`","cwd":"/tmp"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	tid := m["thread_id"].(string)
	if m["attached"] != true {
		t.Fatalf("expected a new attachment: %v", m)
	}
	th, err := c.lookupThread(tid)
	if err != nil || th.CliSessionID != sid || th.Cwd != "/tmp" {
		t.Fatalf("thread not bound to the session: %+v %v", th, err)
	}

	// Attaching the same session again reuses the thread.
	res, err = c.HandleRequest("thread/attach", json.RawMessage(`{"cli_session_id":"`+sid+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	m = res.(map[string]any)
	if m["thread_id"] != tid || m["attached"] != false {
		t.Fatalf("expected the existing thread: %v", m)
	}
}

func TestThreadAttachValidation(t *testing.T) {
	c := newTestConn(t)
	for _, bad := range []string{`{}`, `{"cli_session_id":"--dangerous"}`, `{"cli_session_id":"nope"}`} {
		if _, err := c.HandleRequest("thread/attach", json.RawMessage(bad)); err == nil {
			t.Fatalf("expected an error for %s", bad)
		}
	}
	if _, err := c.HandleRequest("thread/attach",
		json.RawMessage(`{"cli_session_id":"0b9d3c2e-6f7a-4c1d-9e58-1a2b3c4d5e6f","permission_mode":"bypassPermissions"}`)); err == nil {
		t.Fatal("bypassPermissions must still be refused without the server flag")
	}
}

func TestThreadClose(t *testing.T) {
	c := newTestConn(t)
	res, _ := c.HandleRequest("thread/start", json.RawMessage(`{}`))
	tid := res.(map[string]any)["thread_id"].(string)

	out, err := c.HandleRequest("thread/close", json.RawMessage(`{"thread_id":"`+tid+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	// No turn ran, so there is no CLI session on disk to resume: empty id.
	if m := out.(map[string]any); m["cli_session_id"] != "" || m["closed"] != true {
		t.Fatalf("unexpected result: %v", m)
	}
	if _, err := c.lookupThread(tid); err == nil {
		t.Fatal("thread should be gone")
	}
	if _, err := c.HandleRequest("thread/close", json.RawMessage(`{"thread_id":"`+tid+`"}`)); err == nil {
		t.Fatal("closing twice should fail")
	}
}

func TestThreadCloseReturnsSessionForResume(t *testing.T) {
	c := newTestConn(t)
	const sid = "0b9d3c2e-6f7a-4c1d-9e58-1a2b3c4d5e6f"
	res, _ := c.HandleRequest("thread/attach", json.RawMessage(`{"cli_session_id":"`+sid+`"}`))
	tid := res.(map[string]any)["thread_id"].(string)
	out, err := c.HandleRequest("thread/close", json.RawMessage(`{"thread_id":"`+tid+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["cli_session_id"] != sid {
		t.Fatalf("close should return the session id for a later attach: %v", out)
	}
}

func TestThreadCloseFreesSlot(t *testing.T) {
	cfg := DefaultConfig("/nonexistent/claude")
	cfg.MaxThreads = 2
	c := NewConn(cfg, func(string, any) {})
	c.HandleRequest("initialize", nil)
	var ids []string
	for i := 0; i < 2; i++ {
		res, err := c.HandleRequest("thread/start", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.(map[string]any)["thread_id"].(string))
	}
	if _, err := c.HandleRequest("thread/start", json.RawMessage(`{}`)); err == nil {
		t.Fatal("limit should be enforced")
	}
	if _, err := c.HandleRequest("thread/close", json.RawMessage(`{"thread_id":"`+ids[0]+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.HandleRequest("thread/start", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("closing should free a slot: %v", err)
	}
}

func TestAutoModeIsSelectable(t *testing.T) {
	c := newTestConn(t)
	res, err := c.HandleRequest("thread/start", json.RawMessage(`{"permission_mode":"auto"}`))
	if err != nil {
		t.Fatalf("auto must be accepted: %v", err)
	}
	tid := res.(map[string]any)["thread_id"].(string)
	th, _ := c.lookupThread(tid)
	if th.PermissionMode != ModeAuto {
		t.Fatalf("mode = %q", th.PermissionMode)
	}
	if _, err := c.HandleRequest("thread/start", json.RawMessage(`{"permission_mode":"bogus"}`)); err == nil {
		t.Fatal("unknown modes must still be rejected")
	}
}
