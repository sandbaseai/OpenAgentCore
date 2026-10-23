package api

import (
	"encoding/json"
	"errors"
	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	"testing"
)

func TestComputeProfileExtensionPreservesOfficialEnvironment(t *testing.T) {
	raw := json.RawMessage(`{"type":"openai_hosted"}`)
	for _, value := range []string{`null`, `""`, `"BAD"`, `"a/b"`, `42`, `"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`} {
		_, err := preparationEnvironmentInput(raw, &v1.SessionExecutionInput{Environment: json.RawMessage(`{"compute_profile":` + value + `}`)})
		if !errors.Is(err, errComputeProfile) {
			t.Fatalf("%s: %v", value, err)
		}
	}
	result, err := preparationEnvironmentInput(raw, &v1.SessionExecutionInput{Environment: json.RawMessage(`{"compute_profile":"memory-8g"}`)})
	if err != nil || string(result) != string(raw) {
		t.Fatal(string(result), err)
	}
	if _, err = preparationEnvironmentInput(json.RawMessage(`{"type":"self_hosted","workspace_directory":"/work"}`), &v1.SessionExecutionInput{Environment: json.RawMessage(`{"compute_profile":"memory-8g"}`)}); !errors.Is(err, errComputeProfile) {
		t.Fatal(err)
	}
	if _, err = preparationEnvironmentInput(json.RawMessage(`{"type":"openai_hosted","compute_profile":"memory-8g"}`), nil); err == nil {
		t.Fatal("official placement accepted extension field")
	}
}
