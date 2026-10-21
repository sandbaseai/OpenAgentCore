package mcode

import (
	"fmt"
	"maps"

	harnessconfiguration "github.com/MiniMax-AI/OpenAgentCore/internal/harnessconfig/mcode"
	"github.com/MiniMax-AI/OpenAgentCore/internal/modelprovider"
)

func modelProviderConfig(raw any, model string) (map[string]any, error) {
	provider, err := harnessconfiguration.Configuration().ParseProvider(raw)
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
	return map[string]any{
		"name": "Configured provider", "kind": "custom", "enabled": true, "api": api,
		"options": options,
		"models": map[string]any{model: map[string]any{
			"name": model, "tool_call": true,
			"limit": map[string]any{"context": provider.ContextWindow, "output": provider.MaxOutputTokens},
		}},
	}, nil
}
