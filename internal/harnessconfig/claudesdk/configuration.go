// Package claudesdk owns the qualified Claude SDK provider declaration.
package claudesdk

import "github.com/MiniMax-AI/OpenAgentCore/internal/harnessconfig"

func Configuration() harnessconfig.Configuration {
	return harnessconfig.Configuration{AcceptsProviderHeaders: true, ValidateNativeConfig: validateNativeConfig, Providers: []harnessconfig.Provider{
		{Protocol: "anthropic"},
	}}
}
