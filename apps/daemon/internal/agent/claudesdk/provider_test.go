package claudesdk

import (
	"slices"
	"strings"
	"testing"
)

func TestProviderHeadersEveryPreparation(t *testing.T) {
	raw := map[string]any{"protocol": "anthropic", "base_url": "https://example.test", "api_key": "fixture", "headers": map[string]string{"X-B": "two:colon", "X-A": "one", "X-Empty": ""}}
	for range 3 {
		provider, err := providerEnvironment(raw)
		if err != nil {
			t.Fatal(err)
		}
		env := withProvider([]string{"ANTHROPIC_CUSTOM_HEADERS=X-Stale: wrong", "OTHER=keep"}, provider)
		if !slices.Contains(env, "ANTHROPIC_CUSTOM_HEADERS=X-A: one\nX-B: two:colon\nX-Empty: ") {
			t.Fatal("headers missing or changed")
		}
		for _, entry := range env {
			if strings.Contains(entry, "X-Stale") {
				t.Fatal("stale headers retained")
			}
		}
	}
	delete(raw, "headers")
	provider, err := providerEnvironment(raw)
	if err != nil || !slices.Contains(provider, "ANTHROPIC_CUSTOM_HEADERS=") {
		t.Fatal("ambient headers were not cleared")
	}
}
func TestWorkspaceProviderHeadersReachNativeOptions(t *testing.T) {
	config := workspaceFixture(t)
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Stale: ambient")
	config.Env = append(config.Env, "ANTHROPIC_CUSTOM_HEADERS=X-Stale: config")
	req := workspaceRequest()
	req.AgentOptions["model_provider"] = map[string]any{"protocol": "anthropic", "base_url": "https://example.test", "api_key": "fixture", "headers": map[string]string{"X-Activity": "daily-free"}}
	start, env, err := prepareConfiguration(config, req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(start.Workspace.EnvNames, "ANTHROPIC_CUSTOM_HEADERS") || !slices.Contains(env, "ANTHROPIC_CUSTOM_HEADERS=X-Activity: daily-free") {
		t.Fatal("headers missing from native workspace projection")
	}
	for _, entry := range env {
		if strings.Contains(entry, "X-Stale") {
			t.Fatal("ambient header selection survived")
		}
	}
	delete(req.AgentOptions["model_provider"].(map[string]any), "headers")
	_, env, err = prepareConfiguration(config, req)
	if err != nil || !slices.Contains(env, "ANTHROPIC_CUSTOM_HEADERS=") {
		t.Fatal("headerless provider did not clear ambient headers", err)
	}
}
func TestProviderHeadersRejectAuthenticationAndMalformedValues(t *testing.T) {
	for _, headers := range []any{map[string]string{"Authorization": "override"}, map[string]string{"Anthropic-Version": "override"}, map[string]string{"X-Activity": "bad\nvalue"}, map[string]any{"X-Activity": nil}} {
		_, err := providerEnvironment(map[string]any{"protocol": "anthropic", "base_url": "https://example.test", "api_key": "fixture", "headers": headers})
		if err == nil {
			t.Fatal("invalid header accepted")
		}
	}
}
