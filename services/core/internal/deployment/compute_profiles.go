package deployment

import (
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox"
)

// ComputeProfileRecord freezes one administrator-approved selection without credentials.
type ComputeProfileRecord struct {
	Generation    uint64                 `json:"generation"`
	Specification sandbox.DeploymentSpec `json:"specification"`
	Configuration json.RawMessage        `json:"configuration" swaggertype:"object"`
	Metadata      json.RawMessage        `json:"metadata" swaggertype:"object"`
}

func (s *Service) profiles(d Record, base sandbox.Selection) (map[string]sandbox.ComputeProfile, error) {
	var records map[string]ComputeProfileRecord
	if len(d.ComputeProfiles) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(d.ComputeProfiles, &records); err != nil {
		return nil, ErrConflict
	}
	result := make(map[string]sandbox.ComputeProfile, len(records))
	for name, item := range records {
		if !sandbox.ValidComputeProfile(name) || item.Generation == 0 || item.Generation >= d.Generation {
			return nil, ErrConflict
		}
		c, err := s.registry.Decode(d.Provider, sandbox.ConfigurationRecord{Public: item.Configuration, Metadata: item.Metadata})
		if err != nil {
			return nil, ErrConflict
		}
		owner := sandbox.Selection{Provider: d.Provider, DeploymentSpec: item.Specification, Configuration: c}
		if base.HasCredential() {
			owner, err = s.registry.WithCredential(owner, base)
			if err != nil {
				return nil, err
			}
		}
		result[name] = sandbox.ComputeProfile{Resources: owner.Resources, Configuration: owner.Configuration}
	}
	return result, nil
}
