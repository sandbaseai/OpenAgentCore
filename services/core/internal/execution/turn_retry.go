package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

// MaxTurnAutoRetries bounds OAC_TURN_AUTO_RETRIES; zero turns the feature off.
const MaxTurnAutoRetries = 5

const retryPrefix = "[auto-retry "

// retryBackoff is the wait before attempt n (1-based); later attempts reuse the last value.
var retryBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

// retryableEngineCodes are the engine failure classes a new attempt can plausibly fix.
// An empty code is an unclassified failure. Authentication, quota, missing model,
// context length and policy failures are never retried.
var retryableEngineCodes = map[string]bool{
	"": true, "invalid_request": true, "server_error": true, "server_overloaded": true,
	"rate_limit_exceeded": true, "request_timeout": true, "connection_failed": true,
}

// retryFingerprints match the lowercased executor error text. Entries without retry
// are failures stored in the executor's native history: a new user message cannot
// remove them, so retrying would only spend tokens. The first match wins.
var retryFingerprints = []struct {
	name   string
	all    []string
	retry  bool
	adjust bool // the model can fix it by changing the tool call
}{
	{"encrypted_reasoning", []string{"encrypted content", "could not be verified"}, false, false},
	{"image_unsupported", []string{"do not support image input"}, false, false},
	{"tool_result_image", []string{"tool_result", "input_image"}, false, false},
	{"missing_messages", []string{"field messages is required"}, true, false},
	{"ends_with_model_turn", []string{"requests ending with a model turn"}, true, false},
	{"tool_args", []string{"invalid_tool"}, true, true},
	{"tool_args", []string{"tool_use"}, true, true},
	{"tool_args", []string{"tool input"}, true, true},
	{"tool_args", []string{"invalid arguments"}, true, true},
	{"tool_args", []string{"invalid tool"}, true, true},
}

type retryDecision struct {
	Retry       bool
	Fingerprint string
	Reason      string // why not, for logs
	Hint        string
}

// decideRetry judges a failed Turn. attempt is the number of retries already made.
func decideRetry(turn sessions.Turn, attempt, limit int) retryDecision {
	skip := func(reason string) retryDecision { return retryDecision{Reason: reason} }
	var result Result
	switch {
	case limit <= 0:
		return skip("disabled")
	case attempt >= limit:
		return skip("attempts_exhausted")
	case turn.Status != sessions.TurnFailed:
		return skip("not_failed")
	case !turn.CancelRequestedAt.IsZero():
		return skip("cancel_requested")
	case json.Unmarshal(turn.Outcome, &result) != nil:
		return skip("outcome_unreadable")
	case result.ErrorCode != "engine_failed":
		return skip("error_code_not_retryable")
	case !retryableEngineCodes[result.EngineErrorCode]:
		return skip("engine_code_not_retryable")
	case permanentHTTPStatus(result.EngineHTTPStatus):
		return skip("engine_http_status_not_retryable")
	}
	name := cmpName(result.EngineErrorCode, "unclassified")
	adjust := false
	text := strings.ToLower(result.Error)
	for _, fp := range retryFingerprints {
		matched := true
		for _, part := range fp.all {
			matched = matched && strings.Contains(text, part)
		}
		if !matched {
			continue
		}
		if !fp.retry {
			return skip("unrecoverable_history:" + fp.name)
		}
		name, adjust = fp.name, fp.adjust
		break
	}
	return retryDecision{Retry: true, Fingerprint: name, Hint: retryHint(attempt+1, adjust)}
}

// permanentHTTPStatus reports upstream statuses a new attempt cannot fix, which beta
// can carry on a connection_failed classification.
func permanentHTTPStatus(status *int) bool {
	if status == nil {
		return false
	}
	switch *status {
	case 401, 402, 403, 404:
		return true
	}
	return false
}

func cmpName(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// retryHint escalates with the attempt number. It never carries the original error text.
func retryHint(attempt int, adjust bool) string {
	switch {
	case attempt <= 1 && adjust:
		return "Check the parameters of your last tool call and correct them before calling it again."
	case attempt == 2:
		return "If the same error happens again, complete the step a different way and do not repeat the same tool call arguments."
	case attempt >= 3:
		return "Do not call the tool that just failed again; use the information you already have to give a result."
	}
	return ""
}

func retryMessage(attempt, limit int, d retryDecision) string {
	text := fmt.Sprintf("%s%d/%d] The previous attempt failed (%s). Continue the task from where you stopped.", retryPrefix, attempt, limit, d.Fingerprint)
	if d.Hint != "" {
		text += "\n" + d.Hint
	}
	return text
}

// retryAttempt reads how many retries led to this Turn from its first input.
func retryAttempt(inputs []sessions.TurnInput) int {
	for _, input := range inputs {
		var payload struct {
			Input []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		if input.Kind != "message" || json.Unmarshal(input.Payload, &payload) != nil {
			continue
		}
		for _, message := range payload.Input {
			for _, part := range message.Content {
				var n, m int
				if strings.HasPrefix(part.Text, retryPrefix) {
					if _, err := fmt.Sscanf(part.Text, retryPrefix+"%d/%d]", &n, &m); err == nil && n > 0 {
						return n
					}
				}
				return 0
			}
		}
	}
	return 0
}

// retryHost is what a retry needs from Core; Worker plus the Sessions reader provide it.
type retryHost interface {
	ListTurns(ctx context.Context, tenant, session, cursor string, limit int, ascending bool) (sessions.TurnPage, error)
	ListTurnInputs(ctx context.Context, tenant, session, turn string, after int64, limit int) ([]sessions.TurnInput, error)
	SubmitInputs(ctx context.Context, tenant, session, key string, inputs []sessions.Input) ([]sessions.InputReceipt, error)
}

type workerRetryHost struct {
	sessions.Reader
	worker *Worker
}

func (h workerRetryHost) SubmitInputs(ctx context.Context, tenant, session, key string, inputs []sessions.Input) ([]sessions.InputReceipt, error) {
	return h.worker.SubmitInputs(ctx, tenant, session, key, inputs)
}

// maybeAutoRetry schedules a retry for a Turn that just failed. It never blocks the caller.
func (w *Worker) maybeAutoRetry(ctx context.Context, tenant string, turn sessions.Turn) {
	limit := w.dispatcher.TurnAutoRetries
	if limit <= 0 || turn.Status != sessions.TurnFailed {
		return
	}
	host := workerRetryHost{Reader: w.dispatcher.SessionsReader, worker: w}
	go autoRetry(ctx, host, tenant, turn, limit, sleepContext)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// autoRetry waits, rechecks that the failed Turn is still the newest and not
// cancelled, then submits one retry message. The idempotency key is derived from
// the failed Turn, so a repeated call cannot start a second Turn.
func autoRetry(ctx context.Context, host retryHost, tenant string, turn sessions.Turn, limit int, wait func(context.Context, time.Duration) error) {
	logger := log.Ctx(ctx)
	read, cancel := context.WithTimeout(ctx, 10*time.Second)
	inputs, err := host.ListTurnInputs(read, tenant, turn.SessionID, turn.ID, 0, 5)
	cancel()
	if err != nil {
		logger.Warn("oac-core turn auto-retry skipped", "session_id", turn.SessionID, "turn_id", turn.ID, "decision", "skip:inputs_unavailable")
		return
	}
	attempt := retryAttempt(inputs)
	decision := decideRetry(turn, attempt, limit)
	fields := []any{"session_id", turn.SessionID, "turn_id", turn.ID, "attempt", attempt + 1, "max", limit, "fingerprint", decision.Fingerprint}
	if !decision.Retry {
		logger.Info("oac-core turn auto-retry skipped", append(fields, "decision", "skip:"+decision.Reason)...)
		return
	}
	if wait(ctx, retryBackoff[min(attempt, len(retryBackoff)-1)]) != nil {
		return
	}
	submit, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	page, err := host.ListTurns(submit, tenant, turn.SessionID, "", 1, false)
	if err != nil || len(page.Turns) != 1 || page.Turns[0].ID != turn.ID {
		logger.Info("oac-core turn auto-retry skipped", append(fields, "decision", "skip:newer_turn_or_session_gone")...)
		return
	}
	if !decideRetry(page.Turns[0], attempt, limit).Retry {
		logger.Info("oac-core turn auto-retry skipped", append(fields, "decision", "skip:cancel_requested")...)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "agent.session.input.message",
		"input": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": retryMessage(attempt+1, limit, decision)},
		}}},
	})
	if _, err := host.SubmitInputs(submit, tenant, turn.SessionID, "auto-retry:"+turn.ID, []sessions.Input{{Kind: "message", Payload: payload}}); err != nil {
		logger.Warn("oac-core turn auto-retry submit failed", append(fields, "decision", "skip:submit_failed")...)
		return
	}
	logger.Info("oac-core turn auto-retry submitted", append(fields, "decision", "retry")...)
}
