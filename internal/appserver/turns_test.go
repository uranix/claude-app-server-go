package appserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake CLI that echoes each consumed message the way the real one does with
// --replay-user-messages, and emits results as the script says.
const helpers = `
uuid_of() { echo "$1" | sed -n 's/.*"uuid":"\([^"]*\)".*/\1/p'; }
consume() { echo '{"type":"user","uuid":"'"$(uuid_of "$1")"'","isReplay":true,"message":{"role":"user","content":"x"}}'; }
result() { echo '{"type":"result","subtype":"success","is_error":false,"result":"'"$1"'","session_id":"S"}'; }
init() { echo '{"type":"system","subtype":"init","session_id":"S","permissionMode":"default","model":"m"}'; }
`

// Two messages, ONE result: the second was folded into the running turn.
const foldedScript = helpers + `
read l1; init; consume "$l1"
read l2; consume "$l2"
result both
read l3; consume "$l3"; result third
sleep 30
`

// Two messages, two results: the second waited and ran as its own turn.
const separateScript = helpers + `
read l1; init; consume "$l1"; result first
read l2; consume "$l2"; result second
sleep 30
`

// A CLI without replay echoes: one result ends one turn.
const legacyScript = helpers + `
read l1; init; result first
read l2; result second
sleep 30
`

type turnFixture struct {
	t      *testing.T
	c      *Conn
	notifs chan notif
	tid    string
}

func newTurnFixture(t *testing.T, script string) *turnFixture {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &turnFixture{t: t, notifs: make(chan notif, 64)}
	f.c = NewConn(DefaultConfig(claude), func(method string, params any) {
		b, _ := json.Marshal(params)
		var m map[string]any
		json.Unmarshal(b, &m)
		f.notifs <- notif{method, m}
	})
	t.Cleanup(f.c.Close)
	f.c.HandleRequest("initialize", nil)
	res, err := f.c.HandleRequest("thread/start", json.RawMessage(`{"cwd":"`+dir+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	f.tid = res.(map[string]any)["thread_id"].(string)
	return f
}

func (f *turnFixture) call(method, extra string) string {
	f.t.Helper()
	res, err := f.c.HandleRequest(method, json.RawMessage(`{"thread_id":"`+f.tid+`","content":"m"`+extra+`}`))
	if err != nil {
		f.t.Fatalf("%s: %v", method, err)
	}
	return res.(map[string]any)["turn_id"].(string)
}

// completed collects turn/completed turn IDs until n arrived or the wait ended.
func (f *turnFixture) completed(n int, wait time.Duration) []string {
	var ids []string
	deadline := time.After(wait)
	for len(ids) < n {
		select {
		case x := <-f.notifs:
			if x.method == "turn/completed" {
				ids = append(ids, x.params["turn_id"].(string))
			}
		case <-deadline:
			return ids
		}
	}
	return ids
}

func (f *turnFixture) queueLen() int {
	th, _ := f.c.lookupThread(f.tid)
	th.mu.Lock()
	defer th.mu.Unlock()
	return len(th.turnQueue)
}

func TestFoldedMessageEndsBothTurns(t *testing.T) {
	f := newTurnFixture(t, foldedScript)
	t1 := f.call("turn/start", "")
	t2 := f.call("turn/steer", "")

	got := f.completed(2, 5*time.Second)
	if len(got) != 2 || got[0] != t1 || got[1] != t2 {
		t.Fatalf("one result must complete both turns, oldest first: got %v, want [%s %s]", got, t1, t2)
	}
	if n := f.queueLen(); n != 0 {
		t.Fatalf("the queue must be empty afterwards (the thread used to stay busy forever), has %d", n)
	}

	// The thread is usable again: turn/start is not refused as busy.
	t3 := f.call("turn/start", "")
	if got := f.completed(1, 5*time.Second); len(got) != 1 || got[0] != t3 {
		t.Fatalf("a later turn must complete normally: %v", got)
	}
}

func TestSeparateResultsEndOneTurnEach(t *testing.T) {
	f := newTurnFixture(t, separateScript)
	t1 := f.call("turn/start", "")
	if got := f.completed(1, 5*time.Second); len(got) != 1 || got[0] != t1 {
		t.Fatalf("first result: %v", got)
	}
	// Steer after the first result: the queue is empty, so this is refused and the
	// client starts a new turn, which the second result then ends on its own.
	t2 := f.call("turn/start", "")
	if got := f.completed(1, 5*time.Second); len(got) != 1 || got[0] != t2 {
		t.Fatalf("second result must end only the second turn: %v", got)
	}
	if n := f.queueLen(); n != 0 {
		t.Fatalf("queue has %d entries", n)
	}
}

func TestResultDoesNotOverCompleteUnconsumedTurns(t *testing.T) {
	// msg2 is sent while turn 1 runs but the CLI only consumes it after result 1.
	script := helpers + `
read l1; init; consume "$l1"
read l2
result first
consume "$l2"; result second
sleep 30
`
	f := newTurnFixture(t, script)
	t1 := f.call("turn/start", "")
	t2 := f.call("turn/steer", "")
	got := f.completed(2, 5*time.Second)
	if len(got) != 2 || got[0] != t1 || got[1] != t2 {
		t.Fatalf("each turn ends with its own result: got %v", got)
	}
}

func TestLegacyCLIWithoutEchoesStillEndsOneTurnPerResult(t *testing.T) {
	f := newTurnFixture(t, legacyScript)
	t1 := f.call("turn/start", "")
	if got := f.completed(1, 5*time.Second); len(got) != 1 || got[0] != t1 {
		t.Fatalf("got %v", got)
	}
	t2 := f.call("turn/start", "")
	if got := f.completed(1, 5*time.Second); len(got) != 1 || got[0] != t2 {
		t.Fatalf("got %v", got)
	}
}

func TestTurnsCarryTheirMessageID(t *testing.T) {
	f := newTurnFixture(t, foldedScript)
	f.call("turn/start", `,"message_id":"0b9d3c2e-6f7a-4c1d-9e58-1a2b3c4d5e6f"`)
	f.call("turn/steer", "") // no id given: the server assigns one
	th, _ := f.c.lookupThread(f.tid)
	th.mu.Lock()
	defer th.mu.Unlock()
	if th.Turns[0].MessageID != "0b9d3c2e-6f7a-4c1d-9e58-1a2b3c4d5e6f" {
		t.Errorf("client message id not kept: %q", th.Turns[0].MessageID)
	}
	if th.Turns[1].MessageID == "" || strings.Count(th.Turns[1].MessageID, "-") != 4 {
		t.Errorf("a message id must be assigned when none is given: %q", th.Turns[1].MessageID)
	}
}
