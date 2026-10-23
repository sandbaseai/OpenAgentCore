package claudesdk

import (
	"sort"
	"strconv"
	"strings"

	harnessconfiguration "github.com/MiniMax-AI/OpenAgentCore/internal/harnessconfig/claudesdk"
)

// Validate the frozen native provider before producing launch variables.
func providerEnvironment(value any) ([]string, error) {
	provider, err := harnessconfiguration.Declaration.Configuration.ParseProvider(value)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(provider.Headers))
	for name := range provider.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	headers := make([]string, 0, len(names))
	for _, name := range names {
		headers = append(headers, name+": "+provider.Headers[name])
	}
	// An explicit empty value clears any previous native header selection.
	env := []string{"ANTHROPIC_BASE_URL=" + provider.BaseURL, "ANTHROPIC_AUTH_TOKEN=" + provider.APIKey, "ANTHROPIC_CUSTOM_HEADERS=" + strings.Join(headers, "\n")}
	// Native uses the context window only for a model it does not know.
	if provider.ContextWindow > 0 {
		env = append(env, "CLAUDE_CODE_MAX_CONTEXT_TOKENS="+strconv.Itoa(int(provider.ContextWindow)))
	}
	if provider.MaxOutputTokens > 0 {
		env = append(env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS="+strconv.Itoa(int(provider.MaxOutputTokens)))
	}
	return env, nil
}

func withProvider(env, provider []string) []string {
	if provider == nil {
		return env
	}
	out := make([]string, 0, len(env)+len(provider))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "ANTHROPIC_CUSTOM_HEADERS" && key != "ANTHROPIC_API_KEY" && key != "ANTHROPIC_AUTH_TOKEN" && key != "ANTHROPIC_BASE_URL" && key != "CLAUDE_CODE_MAX_CONTEXT_TOKENS" && key != "CLAUDE_CODE_MAX_OUTPUT_TOKENS" {
			out = append(out, entry)
		}
	}
	return append(out, provider...)
}
