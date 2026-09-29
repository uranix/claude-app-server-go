package claudecli

import "testing"

// The CLI echoes a consumed prompt as a user message whose content is a plain
// string; that must produce a "consumed" event for our own message IDs only.
func TestReplayedPromptReportsConsumed(t *testing.T) {
	s := &Session{events: make(chan Event, 4), sent: map[string]bool{"11111111-2222-4333-8444-555555555555": true}}
	blocks := map[int]*blockState{}

	s.handleLine([]byte(`{"type":"user","uuid":"99999999-2222-4333-8444-555555555555","isReplay":true,"message":{"role":"user","content":"someone else"}}`), blocks)
	if len(s.events) != 0 {
		t.Fatal("foreign message id must not be reported")
	}
	s.handleLine([]byte(`{"type":"user","uuid":"11111111-2222-4333-8444-555555555555","isReplay":true,"message":{"role":"user","content":"hello"}}`), blocks)
	ev := <-s.events
	if ev.Kind != "consumed" || ev.MessageID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("got %+v", ev)
	}
	s.handleLine([]byte(`{"type":"user","uuid":"11111111-2222-4333-8444-555555555555","isReplay":true,"message":{"role":"user","content":"hello"}}`), blocks)
	if len(s.events) != 0 {
		t.Fatal("reported twice")
	}
}
