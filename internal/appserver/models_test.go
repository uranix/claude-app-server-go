package appserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetModelCache() {
	modelCache.mu.Lock()
	modelCache.models, modelCache.fetched, modelCache.path = nil, time.Time{}, ""
	modelCache.mu.Unlock()
}

func TestValidModel(t *testing.T) {
	for _, ok := range []string{"", "opus", "sonnet[1m]", "claude-opus-5-5", "claude-haiku-4-5-20251001"} {
		if !validModel(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"--dangerously-skip-permissions", "-x", "a b", "a;b", "x\ny"} {
		if validModel(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestModelListFromCLIAndCache(t *testing.T) {
	resetModelCache()
	defer resetModelCache()
	dir := t.TempDir()
	count := filepath.Join(dir, "starts")
	claude := filepath.Join(dir, "claude")
	script := `#!/bin/sh
echo x >> ` + count + `
read line
id=$(echo "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
echo '{"type":"control_response","response":{"subtype":"success","request_id":"'$id'","response":{"models":[{"value":"fable","displayName":"Fable"}]}}}'
sleep 30
`
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewConn(DefaultConfig(claude), func(string, any) {})
	c.HandleRequest("initialize", nil)

	for i := 0; i < 2; i++ {
		res, err := c.HandleRequest("model/list", nil)
		if err != nil {
			t.Fatal(err)
		}
		m := res.(map[string]any)
		names := m["models"].([]string)
		if len(names) != 1 || names[0] != "fable" || m["live"] != true {
			t.Fatalf("unexpected list: %v", m)
		}
	}
	if b, _ := os.ReadFile(count); len(b) != 2 { // "x\n" once: the second call hit the cache
		t.Fatalf("CLI should be started once, got %q", b)
	}
}

func TestModelListFallsBackToStatic(t *testing.T) {
	resetModelCache()
	defer resetModelCache()
	c := NewConn(DefaultConfig("/nonexistent/claude"), func(string, any) {})
	c.HandleRequest("initialize", nil)
	res, err := c.HandleRequest("model/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["live"] != false || len(m["models"].([]string)) == 0 {
		t.Fatalf("expected the static fallback: %v", m)
	}
}

func TestThreadModelSelection(t *testing.T) {
	c := newTestConn(t)
	res, err := c.HandleRequest("thread/start", json.RawMessage(`{"model":"haiku"}`))
	if err != nil {
		t.Fatal(err)
	}
	tid := res.(map[string]any)["thread_id"].(string)
	th, _ := c.lookupThread(tid)
	if th.Model != "haiku" {
		t.Fatalf("thread/start model not applied: %q", th.Model)
	}

	// No process yet: the model is remembered for the next spawn.
	if _, err := c.HandleRequest("thread/set_model", json.RawMessage(`{"thread_id":"`+tid+`","model":"opus"}`)); err != nil {
		t.Fatal(err)
	}
	if th.Model != "opus" {
		t.Fatalf("set_model not stored: %q", th.Model)
	}

	for _, bad := range []string{
		`{"thread_id":"` + tid + `","model":"--evil"}`,
		`{"model":"opus"}`,
	} {
		if _, err := c.HandleRequest("thread/set_model", json.RawMessage(bad)); err == nil {
			t.Fatalf("expected an error for %s", bad)
		}
	}
	if _, err := c.HandleRequest("thread/start", json.RawMessage(`{"model":"--evil"}`)); err == nil {
		t.Fatal("thread/start must reject a flag-like model")
	}
}

func TestAppendSystemPromptReachesEverySpawn(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args")
	claude := filepath.Join(dir, "claude")
	// Logs the argument list of each spawn, then swallows stdin.
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + argsLog + "\ncat >/dev/null\n"
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewConn(DefaultConfig(claude), func(string, any) {})
	c.HandleRequest("initialize", nil)

	res, err := c.HandleRequest("thread/start", json.RawMessage(`{"cwd":"`+dir+`","append_system_prompt":"send files as links"}`))
	if err != nil {
		t.Fatal(err)
	}
	tid := res.(map[string]any)["thread_id"].(string)
	if _, err := c.HandleRequest("turn/start", json.RawMessage(`{"thread_id":"`+tid+`","content":"hi"}`)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var b []byte
	for time.Now().Before(deadline) {
		if b, _ = os.ReadFile(argsLog); len(b) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(b), "--append-system-prompt send files as links") {
		t.Fatalf("flag not passed: %q", b)
	}

	if _, err := c.HandleRequest("thread/start", json.RawMessage(`{"append_system_prompt":"`+strings.Repeat("x", maxSystemPromptBytes+1)+`"}`)); err == nil {
		t.Fatal("an oversized prompt must be rejected")
	}
}
