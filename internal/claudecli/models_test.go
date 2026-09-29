package claudecli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude answers every control_request with success (and a model list for
// initialize), logging each request line to log.
func fakeClaude(t *testing.T) (path, log string) {
	dir := t.TempDir()
	log = filepath.Join(dir, "requests.log")
	path = filepath.Join(dir, "claude")
	script := `#!/bin/sh
while read line; do
  echo "$line" >> ` + log + `
  id=$(echo "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
  case "$line" in
  *'"initialize"'*)
    echo '{"type":"control_response","response":{"subtype":"success","request_id":"'$id'","response":{"models":[{"value":"opus","resolvedModel":"claude-opus-5-5","displayName":"Opus","description":"big","supportsEffort":true,"supportedEffortLevels":["low","high"]},{"value":"haiku","resolvedModel":"claude-haiku-4-5","displayName":"Haiku","description":"fast"}]}}}';;
  *)
    echo '{"type":"control_response","response":{"subtype":"success","request_id":"'$id'"}}';;
  esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func TestProbeModels(t *testing.T) {
	claude, _ := fakeClaude(t)
	models, err := ProbeModels(context.Background(), claude, "", "0b9d3c2e-6f7a-4c1d-9e58-1a2b3c4d5e6f")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Value != "opus" || models[0].ResolvedModel != "claude-opus-5-5" ||
		models[0].DisplayName != "Opus" || !models[0].SupportsEffort || len(models[0].SupportedEffortLevels) != 2 ||
		models[1].Value != "haiku" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

func TestSetModel(t *testing.T) {
	claude, log := fakeClaude(t)
	s, err := Start(StartOptions{ClaudePath: claude, PermissionMode: "default", NewSessionID: "0b9d3c2e-6f7a-4c1d-9e58-1a2b3c4d5e6f"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.SetModel(ctx, "haiku"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), `"subtype":"set_model"`) || !strings.Contains(string(b), `"model":"haiku"`) {
		t.Fatalf("set_model not sent: %s", b)
	}
}
