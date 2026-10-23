package providers

import (
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox/e2b"
	"github.com/google/uuid"
	"testing"
)

func TestComputeProfilesInheritOnlyCredentialAndRejectEndpointChanges(t *testing.T) {
	base := sandbox.Selection{Provider: "e2b", DeploymentSpec: sandbox.DeploymentSpec{Resources: sandbox.Resources{CPUs: 2, MemoryMiB: 2048}}, Configuration: &e2b.DeploymentConfiguration{Template: "default:" + uuid.NewString(), APIKey: "fixture-key"}}
	variant := &e2b.DeploymentConfiguration{Template: "large:" + uuid.NewString()}
	base.ComputeProfiles = map[string]sandbox.ComputeProfile{"memory-8g": {Resources: sandbox.Resources{CPUs: 4, MemoryMiB: 8192}, Configuration: variant}}
	got, err := Builtin().Normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	profile := got.ComputeProfiles["memory-8g"]
	if profile.Resources.CPUs != 4 || profile.Resources.MemoryMiB != 8192 || got.Resources.MemoryMiB != 2048 || profile.Configuration.(*e2b.DeploymentConfiguration).APIKey != "fixture-key" || variant.APIKey != "" {
		t.Fatal("selection mutated or lost isolation")
	}
	variant.APIURL = "https://api.other.example"
	variant.Domain = "other.example"
	if _, err = Builtin().Normalize(base); err == nil {
		t.Fatal("profile changed provider endpoint")
	}
	variant.APIURL = ""
	variant.Domain = ""
	base.ComputeProfiles["bad/name"] = base.ComputeProfiles["memory-8g"]
	if _, err = Builtin().Normalize(base); err == nil {
		t.Fatal("invalid profile accepted")
	}
}

func TestComputeProfilesDoNotRequireOptionalDiscovery(t *testing.T) {
	registry := Builtin()
	adapter := registry.adapters["docker"]
	adapter.Mode = sandbox.DeploymentDirect
	adapter.BuildLocal = nil
	adapter.NodeArtifacts = nil
	adapter.BuildDirect = func(sandbox.DirectConfig) (sandbox.SandboxProvider, error) {
		t.Fatal("preparation constructed compute")
		return nil, nil
	}
	registry.adapters["direct-fixture"] = adapter
	input := sandbox.Selection{Provider: "direct-fixture", DeploymentSpec: validRegistrationSpec(), Configuration: nodeConfiguration{}, ComputeProfiles: map[string]sandbox.ComputeProfile{"memory-8g": {Resources: sandbox.Resources{CPUs: 4, MemoryMiB: 8192}, Configuration: nodeConfiguration{}}}}
	prepared, err := registry.PrepareSelection(t.Context(), sandbox.DirectConfig{Selection: input})
	if err != nil || prepared.ComputeProfiles["memory-8g"].Resources.MemoryMiB != 8192 {
		t.Fatal("optional discovery blocked direct profile", err)
	}
	input.Provider = "docker"
	if _, err = registry.Normalize(input); err == nil {
		t.Fatal("node provider admitted compute profiles")
	}
}
