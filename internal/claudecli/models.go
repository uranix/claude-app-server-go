package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ModelInfo describes one selectable model as reported by the CLI itself.
type ModelInfo struct {
	Value                 string   `json:"value"` // what --model / set_model accept
	ResolvedModel         string   `json:"resolved_model"`
	DisplayName           string   `json:"display_name"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supports_effort"`
	SupportedEffortLevels []string `json:"supported_effort_levels,omitempty"`
}

// SupportedModels asks the running CLI which models it offers (the "models"
// field of the control protocol's initialize response).
func (s *Session) SupportedModels(ctx context.Context) ([]ModelInfo, error) {
	resp, err := s.controlRequest(ctx, map[string]any{"subtype": "initialize"})
	if err != nil {
		return nil, err
	}
	var body struct {
		Models []struct {
			Value                 string   `json:"value"`
			ResolvedModel         string   `json:"resolvedModel"`
			DisplayName           string   `json:"displayName"`
			Description           string   `json:"description"`
			SupportsEffort        bool     `json:"supportsEffort"`
			SupportedEffortLevels []string `json:"supportedEffortLevels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(resp, &body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(body.Models))
	for _, m := range body.Models {
		out = append(out, ModelInfo{
			Value: m.Value, ResolvedModel: m.ResolvedModel, DisplayName: m.DisplayName,
			Description: m.Description, SupportsEffort: m.SupportsEffort,
			SupportedEffortLevels: m.SupportedEffortLevels,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("claude reported no models")
	}
	return out, nil
}

// SetModel switches the model of the running session; it applies from the
// next request the CLI makes.
func (s *Session) SetModel(ctx context.Context, model string) error {
	_, err := s.controlRequest(ctx, map[string]any{"subtype": "set_model", "model": model})
	return err
}

// Settings is what the CLI applies to its next request.
type Settings struct {
	Model         string
	Effort        string // "" when no effort is sent
	ContextWindow int
}

// Settings asks the running CLI for its model, effort and context window.
// Both requests are answered locally, without an API call.
func (s *Session) Settings(ctx context.Context) (Settings, error) {
	var out Settings
	raw, err := s.controlRequest(ctx, map[string]any{"subtype": "get_settings"})
	if err != nil {
		return out, err
	}
	var st struct {
		Applied struct {
			Model  string  `json:"model"`
			Effort *string `json:"effort"`
		} `json:"applied"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return out, err
	}
	out.Model = st.Applied.Model
	if st.Applied.Effort != nil {
		out.Effort = *st.Applied.Effort
	}
	raw, err = s.controlRequest(ctx, map[string]any{"subtype": "get_context_usage"})
	if err != nil {
		return out, err
	}
	var cu struct {
		MaxTokens int `json:"maxTokens"`
	}
	if err := json.Unmarshal(raw, &cu); err != nil {
		return out, err
	}
	out.ContextWindow = cu.MaxTokens
	return out, nil
}

// ProbeModels starts a throwaway CLI process, reads its model list and stops
// it again. No turn is run, so it costs no tokens.
func ProbeModels(ctx context.Context, claudePath, cwd, sessionID string) ([]ModelInfo, error) {
	s, err := Start(StartOptions{ClaudePath: claudePath, Cwd: cwd, PermissionMode: "default", NewSessionID: sessionID})
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = s.Close()
		go func() { // reap the process and drain events so the reader can finish
			for range s.Events() {
			}
		}()
		go func() { _ = s.Wait() }()
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.SupportedModels(ctx)
}
