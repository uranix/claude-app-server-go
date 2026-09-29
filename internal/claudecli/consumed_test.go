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

func TestSyntheticReplyAndCompactionEvents(t *testing.T) {
	s := &Session{events: make(chan Event, 8)}
	blocks := map[int]*blockState{}

	s.handleLine([]byte(`{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text","text":"## Context Usage"}]}}`), blocks)
	if ev := <-s.events; ev.Kind != "item" || ev.Item.Text != "## Context Usage" {
		t.Fatalf("synthetic reply: %+v", ev)
	}
	s.handleLine([]byte(`{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}`), blocks)
	s.handleLine([]byte(`{"type":"assistant","message":{"model":"claude-haiku-4-5","content":[{"type":"text","text":"streamed already"}]}}`), blocks)
	if len(s.events) != 0 {
		t.Fatalf("unexpected events: %d", len(s.events))
	}

	s.handleLine([]byte(`{"type":"system","subtype":"status","status":"compacting"}`), blocks)
	s.handleLine([]byte(`{"type":"system","subtype":"status","status":null,"compact_result":"success"}`), blocks)
	s.handleLine([]byte(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"manual","pre_tokens":36735,"post_tokens":1671}}`), blocks)
	if ev := <-s.events; ev.Kind != "compacting" {
		t.Fatalf("got %+v", ev)
	}
	if ev := <-s.events; ev.Kind != "compacted" || ev.Trigger != "manual" || ev.PreTokens != 36735 || ev.PostTokens != 1671 {
		t.Fatalf("got %+v", ev)
	}
	if len(s.events) != 0 {
		t.Fatal("extra events")
	}
}
