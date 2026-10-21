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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// The pinned native binary talks to a synthetic Responses HTTP endpoint.
func TestNativeModelHeadersEveryTurn(t *testing.T) {
	binary := os.Getenv("OAC_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("requires pinned Codex binary")
	}
	if version, err := exec.Command(binary, "--version").Output(); err != nil || strings.TrimSpace(string(version)) != "codex-cli 0.153.4" {
		t.Fatal("requires Codex 0.153.4")
	}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Sandbase-Benefit") != "daily-free" || r.Header.Get("X-Literal") != "${unchanged}:value" {
			t.Error("model request lost headers")
		}
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"message":"synthetic retry","type":"server_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"id": fmt.Sprintf("msg_%d", n), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "HEADER-OK", "annotations": []any{}}}}
		if n == 2 {
			item = map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_fixture", "name": "lookup", "arguments": "{}", "status": "completed"}
		}
		response := map[string]any{"id": fmt.Sprintf("resp_%d", n), "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12, "input_tokens_details": map[string]int{"cached_tokens": 0}}}
		for _, event := range []map[string]any{{"type": "response.created", "response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}}, {"type": "response.output_item.added", "output_index": 0, "item": item}, {"type": "response.output_text.delta", "item_id": item["id"], "output_index": 0, "content_index": 0, "delta": "HEADER-OK"}, {"type": "response.output_item.done", "output_index": 0, "item": item}, {"type": "response.completed", "response": response}} {
			body, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], body)
		}
	}))
	defer server.Close()
	t.Setenv("OAC_RUNTIME_HOME", t.TempDir())
	for _, name := range []string{"CODEX_EXEC_SERVER_URL", "CODEX_EXEC_SERVER_NOISE_REGISTRY_URL", "CODEX_EXEC_SERVER_NOISE_ENVIRONMENT_ID", "CODEX_EXEC_SERVER_NOISE_AUTH_TOKEN"} {
		t.Setenv(name, "")
	}
	cfg := defaultSessionConfig(testStateRoot(t))
	cfg.codexBinary = binary
	cfg.logger = slog.New(slog.DiscardHandler)
	req := proto.PromptRequestPayload{AgentKind: "codex", AgentStateKey: "header-native", DisableExecutionEnvironment: true, DisableSubagents: true, FunctionTools: []proto.FunctionTool{{Name: "lookup", Description: "Fixture", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}, AgentOptions: map[string]any{"model": "gpt-5.5", "model_provider": map[string]any{"protocol": "responses", "base_url": server.URL + "/v1", "api_key": "fixture", "headers": map[string]string{"X-Sandbase-Benefit": "daily-free", "X-Literal": "${unchanged}:value"}}}}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	e, err := newExecutor(ctx, req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	for i := 0; i < 3; i++ {
		output := make(chan proto.Envelope, 256)
		turn, err := e.StartTurn(ctx, fmt.Sprintf("turn-%d", i), proto.TextInput("Reply HEADER-OK."), output)
		if err != nil {
			t.Fatal(err)
		}
		var done proto.DonePayload
		for event := range output {
			if event.Type == proto.TypeFunctionCall {
				var call proto.FunctionCallPayload
				_ = event.DecodePayload(&call)
				resultText := "fixture result"
				if err := turn.(*Session).SubmitFunctionResult(ctx, proto.FunctionResultPayload{CallID: call.CallID, Success: true, Content: []proto.InputContent{{Type: "input_text", Text: &resultText}}}); err != nil {
					t.Fatal(err)
				}
			}
			if event.Type == proto.TypeError {
				t.Error("native turn failed")
			}
			if event.Type == proto.TypeDone {
				_ = event.DecodePayload(&done)
			}
		}
		if _, err = turn.AwaitSettlement(ctx); err != nil {
			t.Fatal(err)
		}
		if done.Content != "HEADER-OK" {
			t.Fatalf("unexpected output %q", done.Content)
		}
		if i == 1 {
			req.AgentSessionID = turn.(*Session).currentThreadID()
			req.RequireExistingNativeSession = true
			if err = e.Close(ctx); err != nil {
				t.Fatal(err)
			}
			e, err = newExecutor(ctx, req, cfg)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if attempts.Load() < 5 {
		t.Fatal("retry and three turns not observed")
	}
}
