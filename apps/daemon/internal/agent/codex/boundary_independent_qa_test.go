package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func qaBoundaryRows(t *testing.T, data string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(data, "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) == nil && row["msg"] == "native turn boundary" {
			rows = append(rows, row)
		}
	}
	return rows
}

func TestQANativeBoundaryActualLifecycle(t *testing.T) {
	for _, mode := range []string{"complete", "cancel", "start-error", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			req, cfg, _ := preparationFixture(t)
			cfg.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			fixtureMode := mode
			if mode == "cancel" {
				fixtureMode = "complete"
			}
			e, err := testExecutor(t, fixtureMode, req, cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			rejectedCtx, reject := context.WithCancel(ctx)
			reject()
			out := make(chan proto.Envelope, 20)
			if turn, err := e.StartTurn(rejectedCtx, "rejected", proto.TextInput("answer"), out); turn != nil || err == nil {
				t.Fatal("rejection changed")
			}
			if len(qaBoundaryRows(t, logs.String())) != 0 {
				t.Fatal("rejection emitted summary")
			}
			input := "answer"
			if mode == "cancel" {
				input = "hold"
			}
			turn, startErr := e.StartTurn(ctx, "qa-run", proto.TextInput(input), out)
			if turn == nil {
				t.Fatal("missing owner", startErr)
			}
			if mode == "cancel" {
				if err := turn.Cancel(ctx); err != nil {
					t.Fatal(err)
				}
			}
			_, settleErr := turn.AwaitSettlement(ctx)
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			if (mode == "start-error" || mode == "disconnect") && (startErr == nil || settleErr == nil) {
				t.Fatal("failed native submission lost failure")
			}
			rows := qaBoundaryRows(t, logs.String())
			if len(rows) != 1 {
				t.Fatalf("summary count=%d", len(rows))
			}
			want := map[string]string{"complete": "ok", "cancel": "cancelled", "start-error": "error", "disconnect": "unknown"}[mode]
			if rows[0]["outcome"] != want {
				t.Errorf("native lifecycle %s outcome=%v want=%s", mode, rows[0]["outcome"], want)
			}
			t.Logf("QA_BOUNDARY %v", rows[0])
		})
	}
}

func TestQANativeFirstTextFiltering(t *testing.T) {
	var logs bytes.Buffer
	s := boundaryTestSession(&logs)
	s.observeMessages = true
	for _, raw := range []string{
		`{"threadId":"root","turnId":"stale","itemId":"i","delta":"SECRET_CANARY"}`,
		`{"threadId":"root","turnId":"55555555-5555-4555-8555-555555555555","itemId":"i","delta":""}`,
		`{"threadId":"child","turnId":"55555555-5555-4555-8555-555555555555","itemId":"i","delta":"SECRET_CANARY"}`,
	} {
		s.onAgentDelta(json.RawMessage(raw))
	}
	s.onItemCompleted(json.RawMessage(`{"threadId":"root","turnId":"55555555-5555-4555-8555-555555555555","item":{"id":"tool","type":"commandExecution","command":"SECRET_CANARY","status":"completed"}}`))
	if !s.boundary.textAt.IsZero() {
		t.Fatal("non-root/empty/tool selected")
	}
	s.onItemCompleted(json.RawMessage(`{"threadId":"root","turnId":"55555555-5555-4555-8555-555555555555","item":{"id":"message","type":"agentMessage","text":"SECRET_CANARY"}}`))
	first := s.boundary.textAt
	if first.IsZero() {
		t.Fatal("completed-only root text missed")
	}
	s.onAgentDelta(json.RawMessage(`{"threadId":"root","turnId":"55555555-5555-4555-8555-555555555555","itemId":"message","delta":"SECRET_CANARY"}`))
	if s.boundary.textAt != first {
		t.Fatal("duplicate replaced first")
	}
	s.boundary.finish(s, nil)
	rows := qaBoundaryRows(t, logs.String())
	if len(rows) != 1 || rows[0]["first_native_text_forwarded"] != true {
		t.Fatal(rows)
	}
	if strings.Contains(logs.String(), "SECRET_CANARY") {
		t.Fatal("secret leaked")
	}
}
