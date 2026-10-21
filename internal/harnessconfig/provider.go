package harnessconfig

import (
	"errors"
	"github.com/MiniMax-AI/OpenAgentCore/internal/modelprovider"
)

var ErrProviderHeadersUnsupported = errors.New("selected harness does not support model request headers")

func (c Configuration) ValidateProviderHeaders(headers map[string]string) error {
	if len(headers) > 0 && !c.AcceptsProviderHeaders {
		return ErrProviderHeadersUnsupported
	}
	return nil
}

func (c Configuration) AcceptsHarnessConfig() bool { return c.ValidateNativeConfig != nil }

// ParseProvider validates the frozen bundle against this adapter's native
// declaration before launching a Harness. Unsupported snapshots are not rewritten.
func (c Configuration) ParseProvider(raw any) (modelprovider.Provider, error) {
	provider, err := modelprovider.ParseProvider(raw)
	if err != nil {
		return modelprovider.Provider{}, err
	}
	if err := c.Validate(string(provider.Protocol), provider.ContextWindow, provider.MaxOutputTokens); err != nil {
		return modelprovider.Provider{}, err
	}
	if err := c.ValidateProviderHeaders(provider.Headers); err != nil {
		return modelprovider.Provider{}, err
	}
	return provider, nil
}
