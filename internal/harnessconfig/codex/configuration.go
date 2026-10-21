// Package codex owns the qualified Codex provider configuration declaration.
package codex

import "github.com/MiniMax-AI/OpenAgentCore/internal/harnessconfig"

func Configuration() harnessconfig.Configuration {
	return harnessconfig.Configuration{AcceptsProviderHeaders: true, ValidateNativeConfig: validateNativeConfig, Providers: []harnessconfig.Provider{
		{Protocol: "responses"},
	}}
}
