package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestNativeCompletionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		items  []ThreadItem
		status string
		fail   bool
	}{
		{"reasoning only", []ThreadItem{{ID: "r", Type: "reasoning"}}, "completed", true},
		{"early commentary", []ThreadItem{{ID: "a", Type: "agentMessage", Phase: "commentary", Text: "SECRET"}, {ID: "r", Type: "reasoning"}}, "completed", true},
		{"tool then reasoning", []ThreadItem{{ID: "t", Type: "commandExecution", Command: "true", Status: "completed", ExitCode: new(int)}, {ID: "r", Type: "reasoning"}}, "completed", true},
		{"final answer", []ThreadItem{{ID: "r", Type: "reasoning"}, {ID: "a", Type: "agentMessage", Phase: "final_answer", Text: "SECRET"}}, "completed", false},
		{"unphased answer", []ThreadItem{{ID: "a", Type: "agentMessage", Text: "SECRET"}, {ID: "r", Type: "reasoning"}}, "completed", false},
		{"post final reasoning", []ThreadItem{{ID: "a", Type: "agentMessage", Phase: "final_answer", Text: "SECRET"}, {ID: "r", Type: "reasoning"}}, "completed", false},
		{"tool only", []ThreadItem{{ID: "t", Type: "commandExecution", Command: "true", Status: "completed", ExitCode: new(int)}}, "completed", false},
		{"empty", nil, "completed", false},
		{"unknown", []ThreadItem{{ID: "r", Type: "reasoning"}, {ID: "x", Type: "SECRET"}}, "completed", false},
		{"duplicate reasoning", []ThreadItem{{ID: "r", Type: "reasoning"}, {ID: "a", Type: "agentMessage", Text: "SECRET"}, {ID: "r", Type: "reasoning"}}, "completed", false},
		{"interrupted", []ThreadItem{{ID: "r", Type: "reasoning"}}, "interrupted", false},
	} {
		for _, visible := range []bool{true, false} {
			t.Run(tc.name+map[bool]string{true: " visible", false: " hidden"}[visible], func(t *testing.T) {
				var logs bytes.Buffer
				cfg := defaultSessionConfig(testStateRoot(t))
				cfg.logger = slog.New(slog.NewJSONHandler(&logs, nil))
				out := make(chan proto.Envelope, 64)
				s := &Session{runID: "run", out: out, cancelCtx: context.Background(), cfg: cfg}
				s.reasoningSummary = visible
				s.setThreadID("thread")
				s.beginRootTurn("thread", "turn")
				for _, item := range tc.items {
					raw, _ := json.Marshal(ItemCompletedNotification{ThreadID: "thread", TurnID: "turn", Item: item})
					s.onItemCompleted(raw)
					s.beginRootTurn("thread", "turn") // duplicate start must retain evidence
				}
				// Child and stale turn evidence cannot change the root outcome.
				s.onItemCompleted(json.RawMessage(`{"threadId":"child","turnId":"turn","item":{"id":"child","type":"reasoning"}}`))
				s.onItemCompleted(json.RawMessage(`{"threadId":"thread","turnId":"old","item":{"id":"old","type":"agentMessage","phase":"final_answer","text":"SECRET"}}`))
				raw, _ := json.Marshal(TurnCompletedNotification{ThreadID: "thread", Turn: Turn{ID: "turn", Status: tc.status}})
				s.onTurnCompleted(raw)
				s.onTurnCompleted(raw)
				failures, done := 0, 0
				for env := range out {
					if env.Type == proto.TypeError {
						var p proto.ErrorPayload
						_ = env.DecodePayload(&p)
						if p.Code != "incomplete_response" {
							t.Fatalf("code %q", p.Code)
						}
						failures++
					}
					if env.Type == proto.TypeDone {
						done++
					}
				}
				if (failures == 1) != tc.fail || done != 1 {
					t.Fatalf("failures=%d done=%d", failures, done)
				}
				if strings.Contains(logs.String(), "SECRET") {
					t.Fatal("content leaked")
				}
				if !strings.Contains(logs.String(), `"decision_source":"codex_adapter"`) {
					t.Fatal("missing decision source")
				}
			})
		}
	}
}
