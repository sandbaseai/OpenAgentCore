package mcode

import (
	"fmt"
	"maps"

	harnessconfiguration "github.com/MiniMax-AI/OpenAgentCore/internal/harnessconfig/mcode"
	"github.com/MiniMax-AI/OpenAgentCore/internal/modelprovider"
)

func modelProviderConfig(raw any, model, effort string) (map[string]any, error) {
	provider, err := harnessconfiguration.Declaration.Configuration.ParseProvider(raw)
	if err != nil {
		return nil, fmt.Errorf("mcode: %w", err)
	}
	var api string
	switch provider.Protocol {
	case modelprovider.Anthropic:
		api = "anthropic-messages"
	case modelprovider.ChatCompletions:
		api = "openai-completions"
	case modelprovider.Responses:
		api = "openai-responses"
	}
	options := map[string]any{"baseURL": provider.BaseURL, "apiKey": provider.APIKey}
	if len(provider.Headers) > 0 {
		options["headers"] = maps.Clone(provider.Headers)
	}
	native := map[string]any{"name": model}
	// Absent limits are omitted so the native defaults apply.
	limit := map[string]any{}
	if provider.ContextWindow > 0 {
		limit["context"] = provider.ContextWindow
	}
	if provider.MaxOutputTokens > 0 {
		limit["output"] = provider.MaxOutputTokens
	}
	if len(limit) > 0 {
		native["limit"] = limit
	}
	if effort != "" {
		// The Session selects this effort through ACP thinkingEffort.
		native["thinking"] = map[string]any{"effortOptions": []string{effort}}
	}
	return map[string]any{
		"name": "Configured provider", "kind": "custom", "enabled": true, "api": api,
		"options": options,
		"models":  map[string]any{model: native},
	}, nil
}
