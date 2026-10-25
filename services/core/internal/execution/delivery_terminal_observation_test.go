package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgunit"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/sessionpg"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

// The Runtime is synthetic; delivery, its durable subscription and PostgreSQL
// journal are real. This catches misplaced boundary logs in the actual caller.
type terminalDeliveryConn struct {
	*artifactWorkspaceConn
	mode string
}

func (c *terminalDeliveryConn) WriteMessage(_ int, raw []byte) error {
	var request proto.Envelope
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	if request.Type != proto.TypeExecutionStart {
		return nil
	}
	if c.mode == "start_failure" {
		return errors.New("private start failure")
	}
	var start proto.ExecutionStartPayload
	if err := request.DecodePayload(&start); err != nil {
		return err
	}
	queue := func(kind, id string, payload any) {
		env, _ := proto.NewEnvelope(kind, id, payload)
		encoded, _ := json.Marshal(env)
		c.replies <- encoded
	}
	queue(proto.TypePreparationStatus, request.ID, proto.PreparationStatusPayload{Handle: start.Handle, ExecutorID: start.ExecutorID, RunID: start.RunID, State: "started", Revision: 2})
	if c.mode == "text" {
		queue(proto.TypeDelta, start.RunID, proto.DeltaPayload{Delta: "private terminal answer", Sequence: 1})
	}
	queue(proto.TypeDone, start.RunID, proto.DonePayload{})
	return nil
}

func TestDeliveryTerminalActualPath(t *testing.T) {
	for _, mode := range []string{"text", "empty", "start_failure"} {
		t.Run(mode, func(t *testing.T) {
			logs := captureExecutionLogs(t)
			f := newFinishObservationFixture(t, 0)
			receipt := f.start(t)
			f.dispatcher.SessionsReader = sessionpg.New(pgunit.NewPool(f.pool), nil)
			conn := &terminalDeliveryConn{artifactWorkspaceConn: &artifactWorkspaceConn{executionTraceConn: &executionTraceConn{closed: make(chan struct{})}, replies: make(chan []byte, 8)}, mode: mode}
			peer := runtimegateway.NewSession(conn, "device", "test", runtimegateway.NewRegistry(), func(string, ...any) {})
			peer.Start()
			defer peer.Close("test")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			prepared, err := newPreparedStart(ctx, peer)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.close()
			prepared.handle, prepared.executorID = "handle", "executor"
			result, status := f.dispatcher.deliver(ctx, f.tenant, f.session.ID, peer, proto.PromptRequestPayload{AgentKind: "codex"}, receipt.TurnID, proto.TextInput("private input"), receipt.Sequence, prepared)
			wantStatus := sessions.TurnCompleted
			if mode == "start_failure" {
				wantStatus = sessions.TurnFailed
			}
			if status != wantStatus || result.AppliedThrough != receipt.Sequence {
				t.Fatalf("status=%s error=%s", status, result.ErrorCode)
			}
			var boundaries []string
			for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
				var row map[string]any
				if err := json.Unmarshal(line, &row); err != nil {
					t.Fatal(err)
				}
				if row["msg"] == "executor done received" {
					boundaries = append(boundaries, "done")
				}
				if stage, _ := row["stage"].(string); stage == "delivery_final_drain" || stage == "delivery_final_flush" {
					boundaries = append(boundaries, stage)
					if row["session_id"] != f.session.ID || row["turn_id"] != receipt.TurnID || row["status"] != "ok" {
						t.Fatal("boundary identity/status mismatch")
					}
					if stage == "delivery_final_flush" {
						if mode == "start_failure" {
							if row["confirmation"] != "no_pending" || row["done_to_cleanup_ms"] != nil || row["pending_count"] != float64(0) {
								t.Fatal("unobserved done fabricated a confirmation")
							}
						} else if row["done_to_cleanup_ms"] == nil || row["confirmation"] == "unconfirmed" {
							t.Fatal("successful delivery lacks completion boundary")
						}
					}
				}
			}
			want := []string{"done", "delivery_final_drain", "delivery_final_flush"}
			if mode == "start_failure" {
				want = want[1:]
			}
			gotJSON, _ := json.Marshal(boundaries)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("boundaries=%s want=%s", gotJSON, wantJSON)
			}
			if mode != "start_failure" {
				ended, err := f.dispatcher.finishRun(f.tenant, f.session.ID, receipt.TurnID, result, status)
				if err != nil || ended.Status != sessions.TurnCompleted {
					t.Fatal("delivery journal cannot settle", err)
				}
				rows := terminalRows(t, logs.Bytes())
				if len(rows) != 1 || rows[0]["mapping_status"] != "confirmed" || rows[0]["event_id"] == "" {
					t.Fatal("settlement lacks exact terminal mapping")
				}
			}
			if bytes.Contains(logs.Bytes(), []byte("private terminal answer")) || bytes.Contains(logs.Bytes(), []byte("private input")) || bytes.Contains(logs.Bytes(), []byte("private start failure")) {
				t.Fatal("private payload leaked")
			}
		})
	}
}
