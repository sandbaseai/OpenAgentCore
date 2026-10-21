package claudesdk

import (
	"sort"
	"strings"

	harnessconfiguration "github.com/MiniMax-AI/OpenAgentCore/internal/harnessconfig/claudesdk"
)

// Validate the frozen native provider before producing launch variables.
func providerEnvironment(value any) ([]string, error) {
	provider, err := harnessconfiguration.Configuration().ParseProvider(value)
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
	return []string{"ANTHROPIC_BASE_URL=" + provider.BaseURL, "ANTHROPIC_AUTH_TOKEN=" + provider.APIKey, "ANTHROPIC_CUSTOM_HEADERS=" + strings.Join(headers, "\n")}, nil
}

func withProvider(env, provider []string) []string {
	if provider == nil {
		return env
	}
	out := make([]string, 0, len(env)+len(provider))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "ANTHROPIC_API_KEY" && key != "ANTHROPIC_AUTH_TOKEN" && key != "ANTHROPIC_BASE_URL" && key != "ANTHROPIC_CUSTOM_HEADERS" {
			out = append(out, entry)
		}
	}
	return append(out, provider...)
}
