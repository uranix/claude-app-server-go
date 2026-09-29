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
