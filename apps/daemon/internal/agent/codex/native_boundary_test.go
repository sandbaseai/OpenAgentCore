package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/agent"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func boundaryTestSession(logs *bytes.Buffer) *Session {
	ctx := agent.WithBoundaryInput(context.Background(), time.Now().Add(-10*time.Millisecond), "33333333-3333-4333-8333-333333333333")
	s := &Session{boundary: newNativeBoundary(ctx, time.Now()), runID: "44444444-4444-4444-8444-444444444444", cancelCtx: context.Background(), out: make(chan proto.Envelope, 8), bufs: NewItemBuffers(), cfg: sessionConfig{logger: slog.New(slog.NewJSONHandler(logs, nil))}}
	s.setThreadID("root")
	s.beginRootTurn("root", "55555555-5555-4555-8555-555555555555")
	return s
}
func TestNativeBoundaryRPCOrdering(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "late_ack", true: "early_ack"}[early], func(t *testing.T) {
			var logs bytes.Buffer
			s := boundaryTestSession(&logs)
			c := NewJSONRPCClient(JSONRPCConfig{})
			c.alive = true
			ctx := withNativeBoundary(context.Background(), s.boundary)
			_, err := c.requestWithTimeout(ctx, "turn/start", map[string]string{"input": "SECRET"}, func(frame any) error {
				req := frame.(JsonRpcRequest)
				rawID, _ := json.Marshal(req.ID)
				reply := func() {
					c.handleResponse(rawID, json.RawMessage(`{"turn":{"id":"55555555-5555-4555-8555-555555555555"}}`), nil)
				}
				if early {
					reply()
					time.Sleep(12 * time.Millisecond)
				} else {
					time.Sleep(12 * time.Millisecond)
					go func() { time.Sleep(12 * time.Millisecond); reply() }()
				}
				return nil
			}, time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			s.boundary.finish(s, nil)
			s.boundary.finish(s, nil)
			var row map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
				t.Fatal(err)
			}
			if row["ack_before_write_return"] != early || row["turn_start_write_ms"].(float64) < 10 {
				t.Fatal(row)
			}
			if early && row["turn_start_ack_wait_ms"] != nil {
				t.Fatal("negative wait invented", row)
			}
			if !early && row["turn_start_ack_wait_ms"].(float64) < 10 {
				t.Fatal(row)
			}
			if strings.Contains(logs.String(), "SECRET") {
				t.Fatal("RPC params leaked")
			}
			t.Log("OBSERVATION_FIXTURE " + strings.TrimSpace(logs.String()))
		})
	}
}
func TestNativeBoundaryRootTextAndLateAck(t *testing.T) {
	var logs bytes.Buffer
	s := boundaryTestSession(&logs)
	s.onAgentDelta(json.RawMessage(`{"threadId":"child","turnId":"55555555-5555-4555-8555-555555555555","itemId":"i","delta":"SECRET"}`))
	if !s.boundary.textAt.IsZero() {
		t.Fatal("child selected")
	}
	s.onAgentDelta(json.RawMessage(`{"threadId":"root","turnId":"55555555-5555-4555-8555-555555555555","itemId":"i","delta":"SECRET"}`))
	first := s.boundary.textAt
	s.onAgentDelta(json.RawMessage(`{"threadId":"root","turnId":"55555555-5555-4555-8555-555555555555","itemId":"i","delta":"SECRET2"}`))
	s.boundary.write(time.Now().Add(-time.Millisecond), time.Now())
	s.boundary.ack(time.Now())
	if first != s.boundary.textAt {
		t.Fatal("duplicate replaced first text")
	}
	s.boundary.finish(s, nil)
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
		t.Fatal(err)
	}
	if row["first_native_text_observed"] != true || row["first_native_text_forwarded"] != true || row["turn_start_ack_observed"] != true {
		t.Fatal(row)
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Fatal("text leaked")
	}
	t.Log("OBSERVATION_FIXTURE " + strings.TrimSpace(logs.String()))
}
func TestNativeBoundaryUnknownAndCancelled(t *testing.T) {
	for _, written := range []bool{false, true} {
		var logs bytes.Buffer
		s := boundaryTestSession(&logs)
		if written {
			s.boundary.write(time.Now(), time.Now())
		}
		s.boundary.finish(s, context.Canceled)
		var row map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
			t.Fatal(err)
		}
		want := "cancelled"
		if written {
			want = "unknown"
		}
		if row["outcome"] != want || row["turn_start_ack_wait_ms"] != nil || row["first_native_text_to_forward_ms"] != nil {
			t.Fatal(row)
		}
		t.Log("OBSERVATION_FIXTURE " + strings.TrimSpace(logs.String()))
	}
}
func TestNativeBoundaryWriteFailureKeepsRequestFailure(t *testing.T) {
	var logs bytes.Buffer
	s := boundaryTestSession(&logs)
	c := NewJSONRPCClient(JSONRPCConfig{})
	c.alive = true
	sentinel := errors.New("SECRET")
	_, err := c.requestWithTimeout(withNativeBoundary(context.Background(), s.boundary), "turn/start", nil, func(any) error { return sentinel }, time.Second, nil)
	if !errors.Is(err, sentinel) {
		t.Fatal("request behavior changed", err)
	}
	s.boundary.finish(s, err)
	if strings.Contains(logs.String(), "SECRET") {
		t.Fatal("error leaked")
	}
}

func TestNativeBoundaryActualThreadResolution(t *testing.T) {
	for _, mode := range []string{"start", "resume", "reuse"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			s := boundaryTestSession(&logs)
			client, server, cleanup := NewTestClient()
			defer cleanup()
			s.rpc = client.JSONRPCClient
			s.resolvedModel = "fixture"
			s.steering = steeringTurn{}
			req := proto.PromptRequestPayload{Input: proto.TextInput("SECRET")}
			if mode != "reuse" {
				s.threadID = ""
			}
			if mode == "resume" {
				req.AgentSessionID = "root"
			}
			calls := make(chan []string, 1)
			go func() {
				methods := []string{}
				if mode != "reuse" {
					method, err := SendCannedResponse(context.Background(), server, map[string]any{"thread": map[string]any{"id": "root"}, "model": "fixture"})
					if err != nil {
						calls <- nil
						return
					}
					methods = append(methods, method)
				}
				method, err := SendCannedResponse(context.Background(), server, map[string]any{"turn": map[string]any{"id": "55555555-5555-4555-8555-555555555555"}})
				if err != nil {
					calls <- nil
					return
				}
				calls <- append(methods, method)
			}()
			err := s.startNative(context.Background(), SessionPlan{Model: "fixture"}, req)
			if err != nil {
				t.Fatal(err)
			}
			methods := <-calls
			want := 1
			if mode != "reuse" {
				want = 2
			}
			if len(methods) != want || methods[len(methods)-1] != "turn/start" {
				t.Fatal(methods)
			}
			s.boundary.finish(s, nil)
			var row map[string]any
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var candidate map[string]any
				if json.Unmarshal([]byte(line), &candidate) == nil && candidate["msg"] == "native turn boundary" {
					row = candidate
				}
			}
			if row["thread_mode"] != mode || row["thread_resolve_observed"] != true || row["turn_start_ack_observed"] != true {
				t.Fatal(row)
			}
			fixture, _ := json.Marshal(row)
			t.Logf("OBSERVATION_FIXTURE %s", fixture)
		})
	}
}

func TestNativeBoundaryTextHandlerScalarFixture(t *testing.T) {
	var logs bytes.Buffer
	s := boundaryTestSession(&logs)
	s.cfg.logger = slog.New(slog.NewTextHandler(&logs, nil))
	s.boundary.thread("reuse", time.Now().Add(-time.Millisecond))
	s.boundary.write(time.Now().Add(-2*time.Millisecond), time.Now())
	s.boundary.finish(s, context.Canceled)
	line := logs.String()
	if !strings.Contains(line, "input_to_start_turn_ms=10") || !strings.Contains(line, "thread_resolve_ms=1") || !strings.Contains(line, "turn_start_write_ms=2") || !strings.Contains(line, "turn_start_ack_wait_ms=<nil>") || strings.Contains(line, "0x") {
		t.Fatal(line)
	}
	t.Logf("OBSERVATION_TEXT_FIXTURE %s", strings.TrimSpace(line))
}
