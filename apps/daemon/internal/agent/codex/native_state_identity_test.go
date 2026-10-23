package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Operational rehearsal against pinned native Codex and a synthetic local provider.
func TestNativeStateIdentitySurvivesExecutorRestart(t *testing.T) {
	binary := os.Getenv("OAC_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("pinned native required")
	}
	version, err := exec.Command(binary, "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "codex-cli 0.153.4" {
		t.Fatal("requires pinned Codex 0.153.4")
	}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("request decode")
			w.WriteHeader(400)
			return
		}
		raw, _ := json.Marshal(body)
		if n == 2 && !strings.Contains(string(raw), "NAMESPACE-RETAIN-729") {
			t.Error("resumed request lost original conversation input")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"id": fmt.Sprintf("msg_%d", n), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "SAVED", "annotations": []any{}}}}
		response := map[string]any{"id": fmt.Sprintf("resp_%d", n), "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12, "input_tokens_details": map[string]int{"cached_tokens": 0}}}
		for _, event := range []map[string]any{{"type": "response.created", "response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}}, {"type": "response.output_item.added", "output_index": 0, "item": item}, {"type": "response.output_text.delta", "item_id": item["id"], "output_index": 0, "content_index": 0, "delta": "SAVED"}, {"type": "response.output_item.done", "output_index": 0, "item": item}, {"type": "response.completed", "response": response}} {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
	}))
	defer server.Close()
	t.Setenv("OAC_RUNTIME_HOME", t.TempDir())
	for _, name := range []string{"CODEX_EXEC_SERVER_URL", "CODEX_EXEC_SERVER_NOISE_REGISTRY_URL", "CODEX_EXEC_SERVER_NOISE_ENVIRONMENT_ID", "CODEX_EXEC_SERVER_NOISE_AUTH_TOKEN"} {
		t.Setenv(name, "")
	}
	root := testStateRoot(t)
	cfg := defaultSessionConfig(root)
	cfg.codexBinary = binary
	cfg.logger = slog.New(slog.DiscardHandler)
	id := "e4dd7710-9dab-449d-b6ec-ab0f74d05c2a"
	req := proto.PromptRequestPayload{AgentKind: "codex", AgentStateKey: "agents-api-" + id, DisableExecutionEnvironment: true, DisableSubagents: true, AgentOptions: map[string]any{"model": "gpt-5.5", "model_provider": map[string]any{"protocol": "responses", "base_url": server.URL + "/v1", "api_key": "fixture"}}}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		e, err := newExecutor(ctx, req, cfg)
		if err != nil {
			t.Fatalf("prepare phase %d: %v", i, err)
		}
		output := make(chan proto.Envelope, 256)
		prompt := "Remember NAMESPACE-RETAIN-729."
		if i == 1 {
			prompt = "Return the previous code."
		}
		turn, err := e.StartTurn(ctx, fmt.Sprintf("ops-%d", i), proto.TextInput(prompt), output)
		if err != nil {
			t.Fatal(err)
		}
		for event := range output {
			if event.Type == proto.TypeError {
				t.Fatal("native error")
			}
		}
		if _, err := turn.AwaitSettlement(ctx); err != nil {
			t.Fatal(err)
		}
		native := turn.(*Session).currentThreadID()
		if native == "" {
			t.Fatal("missing native identity")
		}
		if i == 1 && native != req.AgentSessionID {
			t.Fatal("native identity changed")
		}
		if err = e.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			req.AgentSessionID = native
			req.RequireExistingNativeSession = true
			req.AgentStateKey = proto.SessionStateKey(id)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected exactly two inputs, got %d", attempts.Load())
	}
	t.Log("same native thread, original input retained, exactly two inputs with canonical namespace preserved")
}
