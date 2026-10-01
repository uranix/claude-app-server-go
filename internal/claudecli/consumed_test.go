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

// message_start reports the input of an API call, message_delta its output;
// a subagent's calls count as tokens but not as the main context.
func TestUsageEvents(t *testing.T) {
	s := &Session{events: make(chan Event, 8)}
	blocks := map[int]*blockState{}

	s.handleLine([]byte(`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000,"output_tokens":7}}}}`), blocks)
	if ev := <-s.events; ev.Kind != "usage" || ev.InputTokens != 3210 || ev.ContextTokens != 3210 || ev.OutputTokens != 0 || ev.Model != "claude-opus-5-5" {
		t.Fatalf("message_start: %+v", ev)
	}
	s.handleLine([]byte(`{"type":"stream_event","parent_tool_use_id":"tu1","event":{"type":"message_start","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":500}}}}`), blocks)
	if ev := <-s.events; ev.InputTokens != 500 || ev.ContextTokens != 0 || ev.Model != "" {
		t.Fatalf("subagent message_start: %+v", ev)
	}
	s.handleLine([]byte(`{"type":"stream_event","parent_tool_use_id":"tu1","event":{"type":"message_delta","usage":{"output_tokens":5}}}`), blocks)
	if ev := <-s.events; ev.OutputTokens != 5 || ev.ContextTokens != 0 {
		t.Fatalf("subagent message_delta: %+v", ev)
	}
	s.handleLine([]byte(`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"input_tokens":10,"output_tokens":41}}}`), blocks)
	if ev := <-s.events; ev.OutputTokens != 41 || ev.ContextTokens != 3251 || ev.InputTokens != 0 {
		t.Fatalf("message_delta: %+v", ev)
	}
}

// command_lifecycle "started" reports consumption before the replay echo,
// which then must not report it again.
func TestLifecycleStartedReportsConsumed(t *testing.T) {
	id := "11111111-2222-4333-8444-555555555555"
	s := &Session{events: make(chan Event, 4), sent: map[string]bool{id: true}}
	blocks := map[int]*blockState{}

	s.handleLine([]byte(`{"type":"command_lifecycle","command_uuid":"`+id+`","state":"queued"}`), blocks)
	if len(s.events) != 0 {
		t.Fatal("queued must not count as consumed")
	}
	s.handleLine([]byte(`{"type":"command_lifecycle","command_uuid":"`+id+`","state":"started"}`), blocks)
	if ev := <-s.events; ev.Kind != "consumed" || ev.MessageID != id {
		t.Fatalf("got %+v", ev)
	}
	s.handleLine([]byte(`{"type":"user","uuid":"`+id+`","isReplay":true,"message":{"role":"user","content":"hi"}}`), blocks)
	s.handleLine([]byte(`{"type":"command_lifecycle","command_uuid":"`+id+`","state":"completed"}`), blocks)
	if len(s.events) != 0 {
		t.Fatal("reported twice")
	}
}
