package execution

import (
	"context"
	"maps"

	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func (d *Dispatcher) sessionModelOptions(ctx context.Context, session sessions.Session) (map[string]any, error) {
	provider, err := d.SessionsReader.SessionModelExecution(ctx, session.TenantID, session.ID)
	if err != nil {
		return nil, err
	}
	return resolvedSessionModelOptions(provider, session.Engine)
}

func resolvedSessionModelOptions(provider *v1.ModelProviderInput, engine string) (map[string]any, error) {
	if err := provider.ValidateHarness(engine); err != nil {
		return nil, err
	}
	bundle := map[string]any{
		"protocol": string(provider.Protocol), "base_url": provider.BaseURL, "api_key": provider.APIKey,
		"context_window": provider.ContextWindow, "max_output_tokens": provider.MaxOutputTokens,
	}
	if len(provider.Headers) != 0 {
		bundle["headers"] = maps.Clone(provider.Headers)
	}
	return map[string]any{"model_provider": bundle}, nil
}
