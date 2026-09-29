package appserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type notif struct {
	method string
	params map[string]any
}

// permFixture runs a Conn against a fake claude that asks one permission
// question and then finishes its turn once it is answered.
type permFixture struct {
	t        *testing.T
	c        *Conn
	notifs   chan notif
	argsLog  string
	respLog  string
	threadID string
}

// scriptBody is the part of the fake CLI after the user message arrives.
const askAndWait = `
echo '{"type":"system","subtype":"init","session_id":"S","permissionMode":"default","model":"m"}'
echo '{"type":"control_request","request_id":"req1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/x","content":"hi"},"tool_use_id":"tu1","description":"x","permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]}}'
read resp
printf '%s\n' "$resp" >> RESP
echo '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"S"}'
sleep 30
`

const askThenCancel = `
echo '{"type":"system","subtype":"init","session_id":"S","permissionMode":"default","model":"m"}'
echo '{"type":"control_request","request_id":"req1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/x"},"tool_use_id":"tu1"}}'
sleep 0.3
echo '{"type":"control_cancel_request","request_id":"req1"}'
echo '{"type":"result","subtype":"error_during_execution","terminal_reason":"aborted_tools","is_error":true,"session_id":"S"}'
sleep 30
`

func newPermFixture(t *testing.T, body string, timeout time.Duration, threadParams string) *permFixture {
	dir := t.TempDir()
	f := &permFixture{t: t, notifs: make(chan notif, 64), argsLog: filepath.Join(dir, "args"), respLog: filepath.Join(dir, "resp")}
	claude := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + f.argsLog + "\nread line\n" + strings.ReplaceAll(body, "RESP", f.respLog)
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(claude)
	if timeout > 0 {
		cfg.PermissionTimeout = timeout
	}
	f.c = NewConn(cfg, func(method string, params any) {
		b, _ := json.Marshal(params)
		var m map[string]any
		json.Unmarshal(b, &m)
		f.notifs <- notif{method, m}
	})
	t.Cleanup(f.c.Close)
	if _, err := f.c.HandleRequest("initialize", nil); err != nil {
		t.Fatal(err)
	}
	res, err := f.c.HandleRequest("thread/start", json.RawMessage(threadParams))
	if err != nil {
		t.Fatal(err)
	}
	f.threadID = res.(map[string]any)["thread_id"].(string)
	if _, err := f.c.HandleRequest("turn/start", json.RawMessage(`{"thread_id":"`+f.threadID+`","content":"go"}`)); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *permFixture) wait(method string) notif {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case n := <-f.notifs:
			if n.method == method {
				return n
			}
		case <-deadline:
			f.t.Fatalf("no %s notification", method)
		}
	}
}

func (f *permFixture) respond(params string) (any, error) {
	return f.c.HandleRequest("permission/respond", json.RawMessage(params))
}

func (f *permFixture) response() map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(f.respLog); len(b) > 0 {
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &m); err != nil {
				f.t.Fatalf("bad control_response %q: %v", b, err)
			}
			env := m["response"].(map[string]any)
			if m["type"] != "control_response" || env["subtype"] != "success" || env["request_id"] != "req1" {
				f.t.Fatalf("bad control_response envelope: %v", m)
			}
			return env["response"].(map[string]any)
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatal("the CLI never received an answer")
	return nil
}

const promptsOn = `{"permission_prompts":true}`

func TestPermissionAllowWithSuggestions(t *testing.T) {
	f := newPermFixture(t, askAndWait, 0, promptsOn)
	req := f.wait("approval/requested").params
	if req["tool_name"] != "Write" || req["request_id"] != "req1" || req["thread_id"] != f.threadID ||
		req["tool_use_id"] != "tu1" || req["expires_at"] == nil {
		t.Fatalf("unexpected request: %v", req)
	}
	if in := req["input"].(map[string]any); in["file_path"] != "/x" {
		t.Fatalf("input not forwarded: %v", in)
	}
	if s := req["suggestions"].([]any); len(s) != 1 {
		t.Fatalf("suggestions not forwarded: %v", req["suggestions"])
	}

	if _, err := f.respond(`{"thread_id":"` + f.threadID + `","request_id":"req1","behavior":"allow","apply_suggestions":true}`); err != nil {
		t.Fatal(err)
	}
	r := f.response()
	if r["behavior"] != "allow" || r["updatedInput"].(map[string]any)["file_path"] != "/x" {
		t.Fatalf("bad allow payload: %v", r)
	}
	if up := r["updatedPermissions"].([]any); len(up) != 1 || up[0].(map[string]any)["mode"] != "acceptEdits" {
		t.Fatalf("suggestions not applied: %v", r)
	}
	f.wait("turn/completed")

	if b, _ := os.ReadFile(f.argsLog); !strings.Contains(string(b), "--permission-prompt-tool stdio") {
		t.Fatalf("the CLI was not asked to route prompts to the host: %s", b)
	}
	// Answering twice must fail.
	if _, err := f.respond(`{"thread_id":"` + f.threadID + `","request_id":"req1","behavior":"allow"}`); err == nil {
		t.Fatal("a second answer must be rejected")
	}
}

func TestPermissionAllowOnceDoesNotApplySuggestions(t *testing.T) {
	f := newPermFixture(t, askAndWait, 0, promptsOn)
	f.wait("approval/requested")
	f.respond(`{"thread_id":"` + f.threadID + `","request_id":"req1","behavior":"allow"}`)
	if _, has := f.response()["updatedPermissions"]; has {
		t.Fatal("suggestions must only be applied on request")
	}
}

func TestPermissionDeny(t *testing.T) {
	f := newPermFixture(t, askAndWait, 0, promptsOn)
	f.wait("approval/requested")
	if _, err := f.respond(`{"thread_id":"` + f.threadID + `","request_id":"req1","behavior":"deny","message":"not now"}`); err != nil {
		t.Fatal(err)
	}
	if r := f.response(); r["behavior"] != "deny" || r["message"] != "not now" {
		t.Fatalf("bad deny payload: %v", r)
	}
}

func TestPermissionDenyDefaultMessage(t *testing.T) {
	f := newPermFixture(t, askAndWait, 0, promptsOn)
	f.wait("approval/requested")
	f.respond(`{"thread_id":"` + f.threadID + `","request_id":"req1","behavior":"deny"}`)
	if r := f.response(); r["message"] == "" || r["message"] == nil {
		t.Fatalf("deny needs a message the model can act on: %v", r)
	}
}

func TestPermissionTimeoutDeniesAutomatically(t *testing.T) {
	f := newPermFixture(t, askAndWait, 300*time.Millisecond, promptsOn)
	f.wait("approval/requested")
	n := f.wait("approval/cancelled").params
	if n["reason"] != "timeout" || n["request_id"] != "req1" {
		t.Fatalf("unexpected cancel: %v", n)
	}
	if r := f.response(); r["behavior"] != "deny" || !strings.Contains(r["message"].(string), "timed out") {
		t.Fatalf("expected an automatic deny: %v", r)
	}
	f.wait("turn/completed")
	if _, err := f.respond(`{"thread_id":"` + f.threadID + `","request_id":"req1","behavior":"allow"}`); err == nil {
		t.Fatal("answering an expired request must fail")
	}
}

func TestPermissionCancelledByCLI(t *testing.T) {
	f := newPermFixture(t, askThenCancel, 0, promptsOn)
	f.wait("approval/requested")
	if n := f.wait("approval/cancelled").params; n["reason"] != "cancelled" || n["request_id"] != "req1" {
		t.Fatalf("unexpected cancel: %v", n)
	}
	// An abort while a tool was pending is an interrupt, not an error.
	if n := f.wait("turn/completed").params; n["status"] != "interrupted" {
		t.Fatalf("aborted_tools should end as interrupted: %v", n)
	}
	if _, err := f.respond(`{"thread_id":"` + f.threadID + `","request_id":"req1","behavior":"allow"}`); err == nil {
		t.Fatal("a cancelled request cannot be answered")
	}
}

func TestPermissionPromptsAreOptIn(t *testing.T) {
	f := newPermFixture(t, askAndWait, 0, `{}`)
	time.Sleep(300 * time.Millisecond) // the fake CLI ignores the missing flag, but must not be given it
	if b, _ := os.ReadFile(f.argsLog); strings.Contains(string(b), "permission-prompt-tool") {
		t.Fatalf("clients that did not opt in must not get the flag: %s", b)
	}
}

func TestPermissionRespondValidation(t *testing.T) {
	f := newPermFixture(t, askAndWait, 0, promptsOn)
	f.wait("approval/requested")
	for _, bad := range []string{
		`{"thread_id":"` + f.threadID + `","request_id":"req1","behavior":"maybe"}`,
		`{"thread_id":"` + f.threadID + `","behavior":"allow"}`,
		`{"thread_id":"` + f.threadID + `","request_id":"nope","behavior":"allow"}`,
		`{"thread_id":"missing","request_id":"req1","behavior":"allow"}`,
	} {
		if _, err := f.respond(bad); err == nil {
			t.Errorf("expected an error for %s", bad)
		}
	}
	// The request is still pending and can be answered properly.
	if _, err := f.respond(`{"thread_id":"` + f.threadID + `","request_id":"req1","behavior":"allow"}`); err != nil {
		t.Fatalf("a rejected call must not consume the request: %v", err)
	}
}
