package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/transport"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func toolSnapshotFixtures() []string {
	return []string{
		`{"type":"commandExecution","id":"cmd","command":"printf result","cwd":"/tmp","status":"completed","aggregatedOutput":"result\n","exitCode":0,"durationMs":37}`,
		`{"type":"commandExecution","id":"fail","command":"exit 2","status":"failed","aggregatedOutput":"failure details","exitCode":2,"durationMs":12}`,
		`{"type":"mcpToolCall","id":"mcp","server":"reference","tool":"lookup","arguments":{"id":"record"},"status":"completed","result":{"content":[{"type":"text","text":"answer"}],"structuredContent":{"version":9007199254740993}},"error":null}`,
		`{"type":"mcpToolCall","id":"mcp-error","server":"reference","tool":"lookup","arguments":{},"status":"failed","result":null,"error":{"message":"lookup failed"}}`,
		`{"type":"dynamicToolCall","id":"dynamic","tool":"lookup","namespace":"reference","arguments":["a","b"],"status":"completed","contentItems":[{"type":"inputText","text":"dynamic output"}],"success":true}`,
		`{"type":"fileChange","id":"edit","status":"completed","changes":[{"path":"/tmp/example","kind":{"type":"update","move_path":null},"diff":"-before\n+after"}]}`,
		`{"type":"webSearch","id":"search","query":"reference","action":{"type":"search","query":"reference","queries":["reference"]}}`,
	}
}

func TestToolObservations(t *testing.T) {
	for _, item := range toolSnapshotFixtures() {
		var source struct{ ID string }
		if err := json.Unmarshal([]byte(item), &source); err != nil {
			t.Fatal(err)
		}
		t.Run(source.ID, func(t *testing.T) {
			out := make(chan proto.Envelope, 4)
			s := &Session{runID: "run", out: out, cancelCtx: context.Background(), bufs: NewItemBuffers(), cfg: defaultSessionConfig(testStateRoot(t))}
			s.setThreadID("private-thread")
			s.onTurnStarted(json.RawMessage(`{"threadId":"private-thread","turn":{"id":"private-turn"}}`))
			raw := json.RawMessage(`{"threadId":"private-thread","turnId":"private-turn","item":` + item + `}`)
			s.onItemStarted(raw)
			s.onItemCompleted(raw)
			if len(out) != 2 {
				t.Fatalf("tool event count changed: %d", len(out))
			}
			for _, stage := range []string{"before", "after"} {
				event := <-out
				var tool proto.ToolCallPayload
				if event.DecodePayload(&tool) != nil || event.Type != proto.TypeToolCall || tool.ID != source.ID || tool.Stage != stage {
					t.Fatal(event)
				}
				if bytes.Contains(event.Payload, []byte("native_item")) {
					t.Fatal("engine-specific snapshot escaped adapter")
				}
				if tool.Observation == nil || stage == "before" && tool.Observation.Status != "in_progress" {
					t.Fatal(tool.Observation)
				}
				if source.ID == "mcp" && !bytes.Contains(tool.Observation.Output, []byte("9007199254740993")) {
					t.Fatal("structured output precision lost")
				}
			}
		})
	}
}

func TestCompletedCommandNULReachesWireAsText(t *testing.T) {
	received := make(chan proto.Envelope, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		var env proto.Envelope
		if ws.ReadJSON(&env) == nil {
			received <- env
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, transport.DialOptions{WSURL: "ws" + strings.TrimPrefix(server.URL, "http"), DeviceID: "test", Credential: "test", DaemonVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	out := make(chan proto.Envelope, 1)
	session := &Session{runID: "run", out: out, cancelCtx: ctx, cfg: defaultSessionConfig(testStateRoot(t))}
	env, err := proto.NewEnvelope(proto.TypeToolCall, "run", proto.ToolCallPayload{ID: "cmd", Name: "Bash", Stage: "after"})
	if err != nil {
		t.Fatal(err)
	}
	session.sendItemEvents([]proto.Envelope{env}, json.RawMessage(`{"item":{"id":"cmd","type":"commandExecution","command":"read","status":"completed","aggregatedOutput":"a\u0000中\\u0000","exitCode":0}}`))
	select {
	case final := <-out:
		if err := conn.Send(ctx, final); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case wire := <-received:
		var tool proto.ToolCallPayload
		if err := wire.DecodePayload(&tool); err != nil {
			t.Fatal(err)
		}
		var output string
		if tool.Observation == nil {
			t.Fatal("missing observation")
		}
		if err := json.Unmarshal(tool.Observation.Output, &output); err != nil {
			t.Fatal(err)
		}
		if output != `a\x00中\u0000` {
			t.Fatalf("wire snapshot=%q", output)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
