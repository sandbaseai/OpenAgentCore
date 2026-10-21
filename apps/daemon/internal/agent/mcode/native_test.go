package mcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// Opt in with the installed native CLI; the default test gate uses protocol fixtures.
func TestNativeMCodeACP(t *testing.T)         { testNativeMCodeHeaders(t, "anthropic", false) }
func TestNativeMCodeChatHeaders(t *testing.T) { testNativeMCodeHeaders(t, "chat_completions", true) }
func testNativeMCodeHeaders(t *testing.T, protocol string, workspace bool) {
	binary := os.Getenv("OAC_TEST_MCODE_INTEGRATION_BIN")
	if binary == "" {
		t.Skip("set OAC_TEST_MCODE_INTEGRATION_BIN to run native ACP smoke test")
	}
	req := testRequest(t)
	t.Setenv("OAC_RUNTIME_MCODE_BIN", binary)
	var mu sync.Mutex
	var requests []string
	attempts := 0
	var toolSent atomic.Bool
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if r.Header.Get("X-Sandbase-Benefit") != "daily-free" || r.Header.Get("X-Literal") != "${unchanged}:value" {
			t.Error("native model request lost frozen headers")
		}
		if strings.HasSuffix(r.URL.Path, "/input_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":10}`))
			return
		}
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"fixture retry"}}`))
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		raw, _ := json.Marshal(body)
		mu.Lock()
		requests = append(requests, string(raw))
		mu.Unlock()
		if protocol == "chat_completions" {
			tool := ""
			if !toolSent.Load() {
				for _, raw := range body["tools"].([]any) {
					function, ok := raw.(map[string]any)["function"].(map[string]any)
					if !ok {
						function = raw.(map[string]any)
					}
					name, _ := function["name"].(string)
					if strings.HasSuffix(name, "workspace_read") && toolSent.CompareAndSwap(false, true) {
						tool = name
						break
					}
				}

			}
			writeNativeChatResponse(w, tool)
		} else {
			writeNativeResponse(w)
		}
	}))
	defer model.Close()
	req.AgentOptions["model_provider"] = map[string]any{"protocol": protocol, "base_url": model.URL, "api_key": "fixture-only", "headers": map[string]string{"X-Sandbase-Benefit": "daily-free", "X-Literal": "${unchanged}:value"}, "context_window": 64000, "max_output_tokens": 4096}
	req.AgentOptions["system_prompt"] = "SP-MCODE-672: reply as instructed."
	req.Input = proto.TextInput("Reply OAC-MCODE-OK.")

	req.RunID, req.Input = "", nil
	stateRoot := testStateRoot(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	var config *WorkspaceConfig
	if workspace {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("HEADER-TOOL-CONTINUATION"), 0600); err != nil {
			t.Fatal(err)
		}
		node, err := exec.LookPath("node")
		if err != nil {
			t.Fatal(err)
		}
		config = &WorkspaceConfig{Binary: binary, Node: node, Bridge: filepath.Join(filepath.Dir(filepath.Dir(binary)), "bridge.mjs"), Directory: root, Network: "enabled", Scratch: t.TempDir()}
		req.DisableExecutionEnvironment = false
		req.LocalEnvironment = &proto.LocalEnvironment{ID: "headers-workspace", WorkspaceRoot: root, NetworkAccess: "enabled"}
	}
	factory := NewExecutorFactory(stateRoot, config)
	owner, err := factory(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close(context.Background()) }()
	runNumber := 0
	run := func() proto.DonePayload {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		t.Cleanup(cancel)
		out := make(chan proto.Envelope, 64)
		runNumber++
		turn, err := owner.StartTurn(ctx, fmt.Sprintf("headers-%d", runNumber), proto.TextInput("Reply OAC-MCODE-OK."), out)
		if err != nil {
			t.Fatal(err)
		}
		var done proto.DonePayload
		for event := range out {
			if event.Type == proto.TypeError {
				t.Fatalf("native ACP failure: %s", event.Payload)
			}
			if event.Type == proto.TypeDone {
				_ = json.Unmarshal(event.Payload, &done)
			}
		}
		if _, err = turn.AwaitSettlement(ctx); err != nil {
			t.Fatal(err)
		}
		if done.Content != "OAC-MCODE-OK" {
			t.Fatalf("native output=%q", done.Content)
		}
		if _, ok := done.Metadata[proto.DoneMetaAgentSessionID].(string); !ok {
			t.Fatal("native session ID missing")
		}
		return done
	}
	done := run()
	run()
	if err = owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	firstRequests := strings.Join(requests, "\n")
	requests = nil
	mu.Unlock()
	if !strings.Contains(firstRequests, "SP-MCODE-672") {
		t.Fatal("native instructions missing")
	}
	req.RunID = ""
	req.AgentSessionID = done.Metadata[proto.DoneMetaAgentSessionID].(string)
	req.AgentOptions["system_prompt"] = "SP-MCODE-NEW: reply concisely."
	req.AgentOptions["model"] = "fixture-new"
	req.Input = nil
	owner, err = factory(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	run()
	mu.Lock()
	resumed := strings.Join(requests, "\n")
	mu.Unlock()
	if !strings.Contains(resumed, "SP-MCODE-NEW") {
		t.Fatal("resume retained stale instructions")
	}
	if !strings.Contains(resumed, `"model":"fixture-new"`) {
		t.Fatal("resume did not use the updated model")
	}
	mu.Lock()
	attemptCount := attempts
	mu.Unlock()
	if workspace && (!strings.Contains(firstRequests, "HEADER-TOOL-CONTINUATION") || attemptCount < 5) {
		t.Fatalf("native tool continuation was not captured attempts=%d", attemptCount)
	}
	if attemptCount < 4 {
		t.Fatal("retry and resume were not observed")
	}
	t.Log("native new/retry/resume model request headers verified")
}

func writeNativeResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(kind string, payload any) {
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_qa", "type": "message", "role": "assistant", "model": "fixture", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 20, "output_tokens": 0}}})
	send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
	send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "OAC-MCODE-OK"}})
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 8}})
	send("message_stop", map[string]string{"type": "message_stop"})
}

func writeNativeChatResponse(w http.ResponseWriter, tool string) {
	w.Header().Set("Content-Type", "text/event-stream")
	delta := map[string]any{"role": "assistant", "content": "OAC-MCODE-OK"}
	finish := "stop"
	if tool != "" {
		delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_fixture", "type": "function", "function": map[string]any{"name": tool, "arguments": `{"path":"fixture.txt"}`}}}}
		finish = "tool_calls"
	}
	for _, choice := range []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}, {"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
		body, _ := json.Marshal(map[string]any{"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1, "model": "fixture", "choices": []any{choice}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}
