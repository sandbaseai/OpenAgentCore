package execution

import (
	"context"
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgunit"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/sessionpg"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
	"testing"
	"time"
)

type qaBoundaryConn struct {
	*artifactWorkspaceConn
	trigger string
}

func (c *qaBoundaryConn) WriteMessage(_ int, raw []byte) error {
	var request proto.Envelope
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	if request.Type != proto.TypeExecutionStart {
		return nil
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
	count := 1
	if c.trigger == "size" {
		count = 64
	}
	for i := 0; i < count; i++ {
		queue(proto.TypeDelta, start.RunID, proto.DeltaPayload{Delta: "QA-TEXT", Sequence: uint64(i + 1)})
	}
	if c.trigger == "function_call" {
		queue(proto.TypeFunctionCall, start.RunID, map[string]any{})
	}
	if c.trigger == "timer" {
		go func() { time.Sleep(250 * time.Millisecond); queue(proto.TypeDone, start.RunID, proto.DonePayload{}) }()
	} else {
		queue(proto.TypeDone, start.RunID, proto.DonePayload{})
	}
	return nil
}
func TestQAActualDeliveryFourFlushTriggers(t *testing.T) {
	for _, trigger := range []string{"size", "timer", "function_call", "deferred_final"} {
		t.Run(trigger, func(t *testing.T) {
			logs := captureExecutionLogs(t)
			f := newFinishObservationFixture(t, 0)
			receipt := f.start(t)
			f.dispatcher.SessionsReader = sessionpg.New(pgunit.NewPool(f.pool), nil)
			conn := &qaBoundaryConn{artifactWorkspaceConn: &artifactWorkspaceConn{executionTraceConn: &executionTraceConn{closed: make(chan struct{})}, replies: make(chan []byte, 100)}, trigger: trigger}
			peer := runtimegateway.NewSessionWithOwner(conn, "device", f.tenant, "test", runtimegateway.NewRegistry(), func(string, ...any) {}, nil)
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
			f.dispatcher.deliver(ctx, f.tenant, f.session.ID, peer, proto.PromptRequestPayload{AgentKind: "codex"}, receipt.TurnID, proto.TextInput("QA-INPUT"), receipt.Sequence, prepared)
			rows := persistenceRows(t, logs.Bytes())
			if len(rows) != 1 || rows[0]["first_flush_trigger"] != trigger || rows[0]["append_count"] != float64(1) {
				t.Fatalf("trigger=%s rows=%v", trigger, rows)
			}
		})
	}
}
