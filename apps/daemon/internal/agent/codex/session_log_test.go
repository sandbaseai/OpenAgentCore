package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// Terminal logs retain safe metadata while the private runtime frame retains the native error.
func TestEmitTerminal_LogsErrorMessage(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	out := make(chan proto.Envelope, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Session{
		runID:     "run-test-123",
		cfg:       sessionConfig{logger: logger},
		out:       out,
		cancelCtx: ctx,
	}
	s.setThreadID("thread-abc")

	const message = "codex: thread/start: bad provider config"
	s.emitTerminal(message, true)

	logs := buf.String()
	if strings.Contains(logs, message) {
		t.Fatal("native error body leaked to logs")
	}
	for _, want := range []string{
		`"msg":"codex: emitting terminal error"`,
		`"run_id":"run-test-123"`,
		`"thread_id":"thread-abc"`,
		`"message_len":`,
		`"level":"WARN"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, logs)
		}
	}

	// Side-effects on out channel: TypeError + TypeDone.
	got := drainEnvelopes(out)
	if len(got) != 2 {
		t.Fatalf("envelope count = %d, want 2 (error + done); got=%+v", len(got), got)
	}
	if got[0].Type != proto.TypeError {
		t.Fatalf("first envelope type = %q, want error", got[0].Type)
	}
	var errPayload proto.ErrorPayload
	_ = json.Unmarshal(got[0].Payload, &errPayload)
	if errPayload.Error != message {
		t.Fatalf("error payload = %q, want %q", errPayload.Error, message)
	}
	if got[1].Type != proto.TypeDone {
		t.Fatalf("second envelope type = %q, want done", got[1].Type)
	}
}

// TestEmitTerminal_LogsDoneMessage covers the success-path log so a
// future change that flips asError=false on a real prompt completion
// still leaves a trace in the daemon log.
func TestEmitTerminal_LogsDoneMessage(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	out := make(chan proto.Envelope, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Session{
		runID:     "run-test-456",
		cfg:       sessionConfig{logger: logger},
		out:       out,
		cancelCtx: ctx,
	}

	s.emitTerminal("hello world", false)

	logs := buf.String()
	for _, want := range []string{
		`"msg":"codex: emitting terminal done"`,
		`"run_id":"run-test-456"`,
		`"message_len":11`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, logs)
		}
	}
	// asError=false: only TypeDone, no TypeError.
	got := drainEnvelopes(out)
	if len(got) != 1 || got[0].Type != proto.TypeDone {
		t.Fatalf("envelopes = %+v, want exactly 1 done", got)
	}
}

// TestOnErrorNotif_LogsAndBuffers verifies the codex `error`
// notification path stays inspectable. Codex's gateway / provider
// failures (a 401 from a misconfigured custom header, a 400 from
// Azure missing api-version) arrive on this single channel; without
// the log, the daemon shows ~13s of silence and then a TypeError
// envelope the upstream connector can't decode.
func TestOnErrorNotif_LogsAndBuffers(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	s := &Session{runID: "run-x", cfg: sessionConfig{logger: logger}}
	s.setThreadID("thread-y")
	s.onTurnStarted(json.RawMessage(`{"threadId":"thread-y","turn":{"id":"turn"}}`))

	raw, _ := json.Marshal(ErrorNotification{ThreadID: "thread-y", TurnID: "turn", Message: "401 from gateway: invalid X-Sub-Module header"})
	s.onErrorNotif(raw)

	if got := s.peekLastErrText(); !strings.Contains(got, "401") {
		t.Fatalf("lastErrText not buffered: %q", got)
	}
	logs := buf.String()
	if strings.Contains(logs, "401 from gateway") {
		t.Fatal("notification body leaked")
	}
	for _, want := range []string{
		`"msg":"codex: error notification received"`,
		`"thread_id":"thread-y"`,
		`"message_len":`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, logs)
		}
	}
}

// TestOnTurnFailed_LogsBufferedError pins that turn/failed includes
// "we already have buffered error text" in its log line. If onErrorNotif
// fired first and stashed the real cause, the post-mortem can correlate
// the two events by run_id alone.
func TestOnTurnFailed_LogsBufferedError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	out := make(chan proto.Envelope, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Session{
		runID:     "run-z",
		cfg:       sessionConfig{logger: logger},
		out:       out,
		cancelCtx: ctx,
	}
	s.setThreadID("thread")
	s.onTurnStarted(json.RawMessage(`{"threadId":"thread","turn":{"id":"t-1"}}`))
	// Simulate codex sending the real error first…
	rawErr, _ := json.Marshal(ErrorNotification{ThreadID: "thread", TurnID: "t-1", Message: "platform-api 500"})
	s.onErrorNotif(rawErr)
	// …then turn/failed.
	rawFail, _ := json.Marshal(TurnCompletedNotification{ThreadID: "thread", Turn: Turn{ID: "t-1", Status: "failed"}})
	s.onTurnFailed(rawFail)

	logs := buf.String()
	if !strings.Contains(logs, `"msg":"codex: turn/failed received"`) {
		t.Fatalf("turn/failed log missing\n%s", logs)
	}
	if !strings.Contains(logs, `"last_err_text_present":true`) {
		t.Fatalf("turn/failed log must note that an upstream error was buffered\n%s", logs)
	}
}

func TestTerminalTurnKeepsRPCAliveUntilSessionCancel(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Session)
	}{
		{
			name: "completed",
			run: func(s *Session) {
				raw, _ := json.Marshal(TurnCompletedNotification{
					ThreadID: "thread-completed",
					Turn:     Turn{ID: "turn-1", Status: "completed"},
				})
				s.onTurnCompleted(raw)
			},
		},
		{
			name: "failed",
			run: func(s *Session) {
				raw, _ := json.Marshal(TurnCompletedNotification{
					ThreadID: "thread-failed",
					Turn:     Turn{ID: "turn-2", Status: "failed"},
				})
				s.onTurnFailed(raw)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rpc, _, cleanup := NewTestClient()
			defer cleanup()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			out := make(chan proto.Envelope, 4)
			s := &Session{
				runID:     "run-terminal-" + tt.name,
				cfg:       sessionConfig{logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
				out:       out,
				rpc:       rpc.JSONRPCClient,
				cancelCtx: ctx,
				cancelFn:  cancel,
			}
			s.setThreadID("thread-" + tt.name)
			turnID := "turn-1"
			if tt.name == "failed" {
				turnID = "turn-2"
			}
			start, _ := json.Marshal(TurnStartedNotification{ThreadID: s.currentThreadID(), Turn: Turn{ID: turnID}})
			s.onTurnStarted(start)

			tt.run(s)

			if !rpc.Alive() {
				t.Fatal("terminal turn must keep the codex RPC client alive during the idle window")
			}
			select {
			case <-ctx.Done():
				t.Fatal("terminal turn must not cancel the session context")
			default:
			}

		})
	}
}

func drainEnvelopes(out <-chan proto.Envelope) []proto.Envelope {
	var got []proto.Envelope
	for {
		select {
		case env := <-out:
			got = append(got, env)
		default:
			return got
		}
	}
}

func TestTerminalBillingLogsOnlySafeCorrelation(t *testing.T) {
	var buf bytes.Buffer
	out := make(chan proto.Envelope, 4)
	s := &Session{runID: "run", cfg: sessionConfig{logger: slog.New(slog.NewJSONHandler(&buf, nil))}, out: out, cancelCtx: context.Background()}
	message := "unexpected status 402 Payment Required: secret-canary, url: https://private.invalid, request id: 88a8d808-59c1-4bf3-8950-b34ce57e028b"
	s.emitTerminalFailure(message, true, classifyTurnError(&TurnError{Message: message}))
	logs := buf.String()
	for _, want := range []string{`"error_code":"provider_billing_unavailable"`, `"http_status":402`, `"upstream_request_id":"88a8d808-59c1-4bf3-8950-b34ce57e028b"`} {
		if !strings.Contains(logs, want) {
			t.Fatal("missing safe correlation", logs)
		}
	}
	if strings.Contains(logs, "secret-canary") || strings.Contains(logs, "private.invalid") {
		t.Fatal("native body leaked")
	}
}
