package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func failedTurn(t *testing.T, result Result) sessions.Turn {
	t.Helper()
	outcome, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return sessions.Turn{ID: "turn-1", SessionID: "session-1", Status: sessions.TurnFailed, Outcome: outcome}
}

func engineFailure(code, text string) Result {
	return Result{ErrorCode: "engine_failed", EngineErrorCode: code, Error: text}
}

func TestDecideRetryClasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result Result
		retry  bool
		fp     string
	}{
		{"unclassified", engineFailure("", "claudesdk: execution_failed"), true, "unclassified"},
		{"invalid request", engineFailure("invalid_request", "boom"), true, "invalid_request"},
		{"server error", engineFailure("server_error", ""), true, "server_error"},
		{"rate limit", engineFailure("rate_limit_exceeded", ""), true, "rate_limit_exceeded"},
		{"connection", engineFailure("connection_failed", ""), true, "connection_failed"},
		{"authentication", engineFailure("authentication_error", ""), false, ""},
		{"usage limit", engineFailure("usage_limit_exceeded", ""), false, ""},
		{"model missing", engineFailure("resource_not_found", ""), false, ""},
		{"context length", engineFailure("context_length_exceeded", ""), false, ""},
		{"cancelled", Result{ErrorCode: "cancelled"}, false, ""},
		{"disconnected", Result{ErrorCode: "device_disconnected"}, false, ""},
		{"runtime connection closed", Result{ErrorCode: "event_stream_incomplete", EventStreamError: "connection_closed"}, true, "runtime_disconnected"},
		{"event window exceeded", Result{ErrorCode: "event_stream_incomplete", EventStreamError: "event_window_exceeded"}, false, ""},
		{"subscription failed", Result{ErrorCode: "event_stream_incomplete", EventStreamError: "subscription_failed"}, false, ""},
		{"A", engineFailure("invalid_request", "400: Field messages is required"), true, "missing_messages"},
		{"E", engineFailure("invalid_request", "Requests ending with a model turn are not supported"), true, "ends_with_model_turn"},
		{"tool args", engineFailure("invalid_request", "tool_use ids were found without tool_result"), true, "tool_args"},
		{"B", engineFailure("invalid_request", "The encrypted content could not be verified"), false, ""},
		{"C", engineFailure("invalid_request", "Models do not support image input"), false, ""},
		{"D", engineFailure("invalid_request", "tool_result contains input_image"), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decideRetry(failedTurn(t, tc.result), 0, 3)
			if d.Retry != tc.retry || (tc.retry && d.Fingerprint != tc.fp) {
				t.Fatalf("%+v", d)
			}
		})
	}
}

func TestDecideRetrySkipsPermanentHTTPStatus(t *testing.T) {
	for status, retry := range map[int]bool{401: false, 402: false, 403: false, 404: false, 502: true, 429: true} {
		status := status
		result := engineFailure("connection_failed", "")
		result.EngineHTTPStatus = &status
		if got := decideRetry(failedTurn(t, result), 0, 3).Retry; got != retry {
			t.Errorf("status %d: retry = %v, want %v", status, got, retry)
		}
	}
}
func TestDecideRetryLimitsAndCancel(t *testing.T) {
	turn := failedTurn(t, engineFailure("server_error", ""))
	if decideRetry(turn, 0, 0).Retry || decideRetry(turn, 3, 3).Retry || !decideRetry(turn, 2, 3).Retry {
		t.Fatal("attempt limits not honored")
	}
	turn.CancelRequestedAt = time.Now()
	if decideRetry(turn, 0, 3).Retry {
		t.Fatal("cancel must win")
	}
	turn.CancelRequestedAt = time.Time{}
	turn.Status = sessions.TurnCompleted
	if decideRetry(turn, 0, 3).Retry {
		t.Fatal("completed turn retried")
	}
}

func TestRetryHintEscalatesAndDoesNotLeak(t *testing.T) {
	secret := "sk-synthetic-secret-body"
	turn := failedTurn(t, engineFailure("invalid_request", "invalid tool arguments: "+secret))
	seen := map[string]bool{}
	for attempt := 0; attempt < 3; attempt++ {
		d := decideRetry(turn, attempt, 3)
		text := retryMessage(attempt+1, 3, d)
		if strings.Contains(text, secret) || !strings.HasPrefix(text, "[auto-retry ") {
			t.Fatal(text)
		}
		seen[d.Hint] = true
	}
	if len(seen) != 3 {
		t.Fatalf("hints did not escalate: %v", seen)
	}
}

func messageInputs(text string) []sessions.TurnInput {
	payload, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}})
	return []sessions.TurnInput{{Kind: "message", Payload: payload}}
}

func TestRetryAttemptParsing(t *testing.T) {
	if retryAttempt(messageInputs("hello")) != 0 || retryAttempt(nil) != 0 {
		t.Fatal("plain input counted as retry")
	}
	if got := retryAttempt(messageInputs("[auto-retry 2/3] failed")); got != 2 {
		t.Fatal(got)
	}
}

type fakeRetryHost struct {
	mu        sync.Mutex
	latest    []sessions.Turn
	inputs    []sessions.TurnInput
	submitted map[string]int
	keys      []string
	texts     []string
	err       error
}

func (f *fakeRetryHost) GetSession(context.Context, string, string) (sessions.Session, error) {
	var latest *sessions.Turn
	if len(f.latest) > 0 {
		latest = &f.latest[0]
	}
	return sessions.Session{LastTurn: latest}, f.err
}

func (f *fakeRetryHost) ListTurnInputs(context.Context, string, string, string, int64, int) ([]sessions.TurnInput, error) {
	return f.inputs, nil
}

func (f *fakeRetryHost) SubmitInputs(_ context.Context, _, _, key string, inputs []sessions.Input) ([]sessions.InputReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	f.texts = append(f.texts, string(inputs[0].Payload))
	return nil, nil
}

func noWait(context.Context, time.Duration) error { return nil }

func TestAutoRetrySubmitsOnceWithStableKey(t *testing.T) {
	turn := failedTurn(t, engineFailure("server_error", "secret-body"))
	host := &fakeRetryHost{latest: []sessions.Turn{turn}}
	var waited time.Duration
	autoRetry(t.Context(), host, "tenant", turn, 3, func(_ context.Context, d time.Duration) error { waited = d; return nil })
	autoRetry(t.Context(), host, "tenant", turn, 3, noWait)
	if len(host.keys) != 2 || host.keys[0] != "auto-retry:turn-1" || host.keys[1] != host.keys[0] || waited != retryBackoff[0] {
		t.Fatal(host.keys, waited)
	}
	if strings.Contains(host.texts[0], "secret-body") || !strings.Contains(host.texts[0], "[auto-retry 1/3]") {
		t.Fatal(host.texts[0])
	}
}

func TestAutoRetryBackoffGrowsAndStopsAtLimit(t *testing.T) {
	turn := failedTurn(t, engineFailure("server_error", ""))
	host := &fakeRetryHost{latest: []sessions.Turn{turn}, inputs: messageInputs("[auto-retry 2/3] x")}
	var waited time.Duration
	autoRetry(t.Context(), host, "t", turn, 3, func(_ context.Context, d time.Duration) error { waited = d; return nil })
	if waited != retryBackoff[2] || len(host.keys) != 1 || !strings.Contains(host.texts[0], "[auto-retry 3/3]") {
		t.Fatal(waited, host.keys)
	}
	host = &fakeRetryHost{latest: []sessions.Turn{turn}, inputs: messageInputs("[auto-retry 3/3] x")}
	autoRetry(t.Context(), host, "t", turn, 3, noWait)
	if len(host.keys) != 0 {
		t.Fatal("retried past the limit")
	}
}

func TestAutoRetrySkips(t *testing.T) {
	turn := failedTurn(t, engineFailure("server_error", ""))
	newer := sessions.Turn{ID: "turn-2", Status: sessions.TurnQueued}
	cancelled := turn
	cancelled.CancelRequestedAt = time.Now()
	for name, host := range map[string]*fakeRetryHost{
		"newer turn":        {latest: []sessions.Turn{newer}},
		"session gone":      {err: errors.New("not found")},
		"cancel after wait": {latest: []sessions.Turn{cancelled}},
	} {
		autoRetry(t.Context(), host, "t", turn, 3, noWait)
		if len(host.keys) != 0 {
			t.Fatal(name)
		}
	}
	host := &fakeRetryHost{latest: []sessions.Turn{turn}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	autoRetry(ctx, host, "t", turn, 3, sleepContext)
	if len(host.keys) != 0 {
		t.Fatal("submitted after context cancel")
	}
	autoRetry(t.Context(), host, "t", turn, 0, noWait)
	if len(host.keys) != 0 {
		t.Fatal("disabled switch still retried")
	}
}

func TestIncompleteResponseRetryBound(t *testing.T) {
	turn := failedTurn(t, engineFailure("incomplete_response", "private native text"))
	for _, tc := range []struct {
		attempt, limit int
		retry          bool
	}{{0, 0, false}, {0, 3, true}, {1, 3, false}, {2, 5, false}} {
		d := decideRetry(turn, tc.attempt, tc.limit)
		if d.Retry != tc.retry {
			t.Fatalf("%+v: %+v", tc, d)
		}
		if d.Retry && (!strings.Contains(d.Hint, "Do not repeat completed side effects") || strings.Contains(retryMessage(1, 3, d), "private native text")) {
			t.Fatal("unsafe recovery hint")
		}
	}
	turn.CancelRequestedAt = time.Now()
	if decideRetry(turn, 0, 3).Retry {
		t.Fatal("cancelled turn retried")
	}
}
