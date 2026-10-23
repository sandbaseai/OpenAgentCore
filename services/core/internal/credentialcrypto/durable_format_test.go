package credentialcrypto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// Fixtures were generated with public synthetic plaintext and a fixed test key
// by source 4b16ddbbfe5ad5984547e54d60bb9a89c7c79d81, not by the implementation under test.
func TestDurableFormatV1(t *testing.T) {
	data, err := os.ReadFile("testdata/durable_format_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	c, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func([]byte) ([]byte, error){
		"workspace": func(b []byte) ([]byte, error) {
			return c.OpenWorkspaceConfiguration(b, "installation", "configuration")
		},
		"vault": func(b []byte) ([]byte, error) {
			return c.Open(b, Binding{TenantID: "tenant", VaultID: "vault", CredentialID: "credential", AuthType: "static_bearer", Destination: "https://example.invalid/mcp"})
		},
		"deployment":       func(b []byte) ([]byte, error) { return c.OpenSandboxDeployment(b, "installation", 7) },
		"deployment_model": func(b []byte) ([]byte, error) { return c.OpenDeploymentModelProvider(b, "codex") },
		"agent_model":      func(b []byte) ([]byte, error) { return c.OpenAgentModelExecution(b, "tenant", "agent") },
		"session_model":    func(b []byte) ([]byte, error) { return c.OpenModelExecution(b, "tenant", "session") },
		"file": func(b []byte) ([]byte, error) {
			return c.OpenEnvironmentFile(b, EnvironmentFileBinding{TenantID: "tenant", Resource: "session", OwnerID: "session", FileID: "file"})
		},
		"setup": func(b []byte) ([]byte, error) {
			return c.OpenEnvironmentSetup(b, EnvironmentSetupBinding{TenantID: "tenant", Resource: "session", OwnerID: "session", Field: "initialization"})
		},
		"skill": func(b []byte) ([]byte, error) {
			return c.OpenSkill(b, SkillBinding{TenantID: "tenant", SkillID: "skill", VersionID: "version", Version: "1"})
		},
	}
	for name, open := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := hex.DecodeString(fixtures[name])
			if err != nil {
				t.Fatal(err)
			}
			plain, err := open(b)
			if err != nil || string(plain) != "public synthetic durable-format fixture" {
				t.Fatal("stored format is unreadable")
			}
		})
	}
	got, err := c.Fingerprint("parsar.agents-api.model-provider-api-key.v1", "public-fixture-key")
	if err != nil || got != fixtures["provider_key_fingerprint"] {
		t.Fatal("durable fingerprint changed")
	}
}
