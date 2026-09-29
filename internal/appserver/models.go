package appserver

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"

	"github.com/uranix/claude-app-server-go/internal/claudecli"
	"github.com/uranix/claude-app-server-go/internal/idgen"
	"github.com/uranix/claude-app-server-go/internal/jsonrpc"
)

// modelNameRe restricts what may be passed on as `--model`: aliases such as
// "opus" or "sonnet[1m]" and full IDs. Notably it cannot start with '-'.
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,99}$`)

func validModel(m string) bool { return m == "" || modelNameRe.MatchString(m) }

// staticModels is only used when the CLI cannot be asked.
var staticModels = []claudecli.ModelInfo{
	{Value: "opus"}, {Value: "sonnet"}, {Value: "haiku"},
}

const modelCacheTTL = 10 * time.Minute

// modelCache is process-wide: the model list does not depend on the client
// connection, and asking costs a CLI process start.
var modelCache struct {
	mu      sync.Mutex
	models  []claudecli.ModelInfo
	fetched time.Time
	path    string
}

// cachedModels returns the CLI's model list, refreshed at most every
// modelCacheTTL. On failure it serves the stale list, or staticModels.
func cachedModels(ctx context.Context, claudePath, cwd string) ([]claudecli.ModelInfo, bool) {
	modelCache.mu.Lock()
	defer modelCache.mu.Unlock()
	if modelCache.models != nil && modelCache.path == claudePath && time.Since(modelCache.fetched) < modelCacheTTL {
		return modelCache.models, true
	}
	models, err := claudecli.ProbeModels(ctx, claudePath, cwd, idgen.UUIDv4())
	if err != nil {
		if modelCache.models != nil {
			return modelCache.models, true
		}
		return staticModels, false
	}
	modelCache.models, modelCache.fetched, modelCache.path = models, time.Now(), claudePath
	return models, true
}

// handleModelList reports the models the CLI offers. "models" keeps the
// original shape (a list of names, usable as `model`); "model_info" has the
// details. "live" is false when the list is only a static fallback.
func (c *Conn) handleModelList() (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	infos, live := cachedModels(ctx, c.cfg.ClaudePath, "")
	names := make([]string, 0, len(infos))
	for _, m := range infos {
		names = append(names, m.Value)
	}
	return map[string]any{"models": names, "model_info": infos, "live": live}, nil
}

// setModel changes a thread's model. A running process is switched live with
// set_model; otherwise the model is used when the process next spawns. Must be
// called without t.mu held, because the control request waits for the CLI.
func (c *Conn) setModel(t *Thread, model string) error {
	if !validModel(model) {
		return jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid model name", nil)
	}
	t.mu.Lock()
	session := t.session
	changed := t.Model != model
	t.Model = model
	t.mu.Unlock()

	if session == nil || !changed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := session.SetModel(ctx, model); err != nil {
		return jsonrpc.NewException(jsonrpc.ErrInternal, "set_model failed: "+err.Error(), nil)
	}
	return nil
}

type threadSetModelParams struct {
	ThreadID string `json:"thread_id"`
	Model    string `json:"model"`
}

func (c *Conn) handleThreadSetModel(raw json.RawMessage) (any, error) {
	var p threadSetModelParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, jsonrpc.NewException(jsonrpc.ErrInvalidParams, "invalid params", nil)
	}
	t, err := c.lookupThread(p.ThreadID)
	if err != nil {
		return nil, err
	}
	if err := c.setModel(t, p.Model); err != nil {
		return nil, err
	}
	return map[string]any{"thread_id": t.ID, "model": p.Model}, nil
}
