package api

import (
	"encoding/json"
	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	"github.com/google/uuid"
	"net/http"
	"strings"
	"testing"
)

func TestModelHeadersDeploymentAndSavedAgentHTTP(t *testing.T) {
	for _, headers := range []string{`{"X-Activity":"private-header-canary"}`, `null`, `{"X-Activity":null}`, `"wrong"`, `{"X-A":"one","x-a":"two"}`} {
		valid := headers == `{"X-Activity":"private-header-canary"}` || headers == `null`
		provider := strings.TrimSuffix(savedProviderFixture, "}") + `,"headers":` + headers + `}`
		s := &coreProviderValidationStore{t: t}
		h, _, _ := adminTestHandler(t, s.configure)
		out := projectKeyHTTP(h, http.MethodPut, "/core/v1/harnesses/codex/model-configuration", "admin", `{"model":"fixture","model_provider":`+provider+`}`)
		if valid && (out.Code != 200 || s.writes != 1) || !valid && (out.Code != 400 || s.writes != 0) {
			t.Fatalf("deployment headers status=%d writes=%d", out.Code, s.writes)
		}
		if valid && headers != "null" && s.record.Headers["X-Activity"] != "private-header-canary" {
			t.Fatal("deployment store lost headers")
		}
		if strings.Contains(out.Body.String(), "private-header-canary") || strings.Contains(out.Body.String(), "X-Activity") {
			t.Fatal("deployment response disclosed headers")
		}
		for _, path := range []string{"/v1/agents", "/v1/agents/" + uuid.NewString()} {
			saved := &savedProviderStore{}
			handler, _, _ := testHandler(t, saved.serve)
			raw := `{"model":"fixture","x_agents_core":{"harness":"codex","model_provider":` + provider + `}}`
			response := credentialRequest(handler, http.MethodPost, path, raw)
			want := 200
			if path == "/v1/agents" {
				want = 201
			}
			if valid && (response.Code != want || saved.provider == nil) || !valid && (response.Code != 400 || saved.provider != nil) {
				t.Fatalf("saved headers path=%s status=%d", path, response.Code)
			}
			if valid && headers != "null" && saved.provider.Headers["X-Activity"] != "private-header-canary" {
				t.Fatal("saved store lost headers")
			}
			for _, body := range []string{response.Body.String(), string(saved.saved.Configuration)} {
				if strings.Contains(body, "private-header-canary") || strings.Contains(body, "X-Activity") {
					t.Fatal("saved safe view disclosed headers")
				}
			}
		}
	}
}

func TestModelHeadersClaudeAcceptedAcrossProviderInputRoutes(t *testing.T) {
	provider := strings.Replace(strings.TrimSuffix(savedProviderFixture, "}")+`,"headers":{"X-Activity":"daily-free"}}`, `"responses"`, `"anthropic"`, 1)
	raw := `{"model":"fixture","x_agents_core":{"harness":"claude_sdk","model_provider":` + provider + `}}`
	s := &coreProviderValidationStore{t: t}
	h, _, _ := adminTestHandler(t, s.configure)
	out := projectKeyHTTP(h, http.MethodPut, "/core/v1/harnesses/claude_sdk/model-configuration", "admin", `{"model":"fixture","model_provider":`+provider+`}`)
	if out.Code != 200 || s.writes != 1 || s.record.Headers["X-Activity"] != "daily-free" {
		t.Fatalf("deployment Claude header status %d", out.Code)
	}
	if strings.Contains(out.Body.String(), "daily-free") {
		t.Fatal("deployment response disclosed metadata")
	}
	for _, path := range []string{"/v1/agents", "/v1/agents/" + uuid.NewString()} {
		saved := &savedProviderStore{}
		handler, _, _ := testHandler(t, saved.serve)
		response := credentialRequest(handler, http.MethodPost, path, raw)
		want := 200
		if path == "/v1/agents" {
			want = 201
		}
		if response.Code != want || saved.provider == nil || saved.provider.Headers["X-Activity"] != "daily-free" || strings.Contains(response.Body.String(), "daily-free") {
			t.Fatalf("saved Claude header status %d", response.Code)
		}
	}
	var create v1.CreateAgentRequest
	if err := validateSavedCoreInput([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &create); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSavedAgent(create); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveAgentUpdate([]byte(raw)); err != nil {
		t.Fatal(err)
	}
}
