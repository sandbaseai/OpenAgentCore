package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/execution"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func TestExecutionPersistsLiveAndCancelledPartialOutput(t *testing.T) {
	h := newDispatchHarness(t)
	ctx := context.Background()
	input := h.message("start", "Stream then cancel")
	result := h.run(ctx, input.TurnID)
	h.read(testExecutionRequest)
	h.write(input.TurnID, proto.TypeDelta, proto.DeltaPayload{Delta: "已输出", Sequence: 1})
	h.write(input.TurnID, proto.TypeToolCall, proto.ToolCallPayload{ID: "tool-1", Name: "Bash", Stage: "before", Observation: &proto.ToolObservation{Kind: "command", Command: "pwd", Status: "in_progress"}})
	h.write(input.TurnID, proto.TypeToolCall, proto.ToolCallPayload{ID: "tool-1", Name: "Bash", Stage: "after", Observation: &proto.ToolObservation{Kind: "command", Command: "pwd", Status: "completed"}})
	h.write(input.TurnID, proto.TypeUsage, proto.UsagePayload{Usage: proto.Usage{InputTokens: 11, OutputTokens: 2}})
	reopened, pool := testStore(t)
	defer pool.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, err := reopened.ListTurnEvents(ctx, h.tenant, h.session.ID, input.TurnID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("events were not persisted during execution")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := requestCancel(ctx, h.s, h.tenant, h.session.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	cancelEnv := h.read(proto.TypePromptCancel)
	var cancel proto.PromptCancelPayload
	_ = cancelEnv.DecodePayload(&cancel)
	for i := range 30 {
		h.write(input.TurnID, proto.TypeDelta, proto.DeltaPayload{Delta: "片段", Sequence: uint64(i + 2)})
	}
	h.write(input.TurnID, proto.TypeInteractionDecisionAck, proto.InteractionDecisionAckPayload{DeliveryID: cancel.DeliveryID, Applied: true, Outcome: &proto.DonePayload{}})
	h.finished(result, sessions.TurnCancelled)
	events, err := reopened.ListTurnEvents(ctx, h.tenant, h.session.ID, input.TurnID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var kinds []string
	for i, event := range events {
		if event.Ordinal != int32(i+1) {
			t.Fatal("non-contiguous events")
		}
		kinds = append(kinds, event.Kind)
		if event.Kind == proto.TypeDelta {
			var delta proto.DeltaPayload
			_ = json.Unmarshal(event.Payload, &delta)
			text.WriteString(delta.Delta)
		}
	}
	if text.String() != "已输出"+strings.Repeat("片段", 30) {
		t.Fatalf("partial text lost: %q", text.String())
	}
	if len(events) != 36 || kinds[34] != "cancel_receipt" || kinds[35] != "execution_cancelled" {
		t.Fatalf("event order=%v", kinds)
	}
}

func TestExecutionDoesNotCompleteAfterEventPersistenceFailure(t *testing.T) {
	h := newDispatchHarness(t)
	ctx := context.Background()
	input := h.message("start", "Output beyond storage budget")
	result := h.run(ctx, input.TurnID)
	h.read(testExecutionRequest)
	_, pool := testStore(t)
	defer pool.Close()
	if _, err := pool.Exec(ctx, "UPDATE turns SET event_bytes=33554432 WHERE id=$1", input.TurnID); err != nil {
		t.Fatal(err)
	}
	h.write(input.TurnID, proto.TypeDelta, proto.DeltaPayload{Delta: "cannot be stored", Sequence: 1})
	h.write(input.TurnID, proto.TypeDone, proto.DonePayload{Content: "Do not report success", Usage: proto.Usage{InputTokens: 13}, Metadata: map[string]any{proto.DoneMetaAgentSessionID: "failed-native"}})
	turn := h.finished(result, sessions.TurnFailed)
	var outcome execution.Result
	_ = json.Unmarshal(turn.Outcome, &outcome)
	if outcome.ErrorCode != "event_persistence_failed" {
		t.Fatal(outcome.ErrorCode)
	}
	bound, err := sessionAdapter(h.s).GetSessionExecutionBinding(ctx, h.tenant, h.session.ID)
	if err != nil || bound.NativeSessionID != "failed-native" || outcome.Done.Usage.InputTokens != 13 {
		t.Fatalf("terminal failure lost native continuity or usage: %+v %+v %v", bound, outcome, err)
	}
	events, err := h.s.ListTurnEvents(ctx, h.tenant, h.session.ID, input.TurnID, 0, 100)
	if err != nil || len(events) != 1 || events[0].Kind != "execution_failed" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

// Write raw envelopes to exercise Core's defense independently of NewEnvelope.
func TestExecutionPersistsNULOutputAndCompletes(t *testing.T) {
	h := newDispatchHarness(t)
	ctx := context.Background()
	for _, key := range []string{"binary-output", "next-turn"} {
		input := h.message(key, "Read output safely")
		result := h.run(ctx, input.TurnID)
		h.read(testExecutionRequest)
		h.write(input.TurnID, proto.TypeToolCall, proto.ToolCallPayload{ID: "tool", Name: "Bash", Stage: "before", Observation: &proto.ToolObservation{Kind: "command", Status: "in_progress", Command: "read"}})
		for _, env := range []proto.Envelope{
			{Type: proto.TypeCommandOutput, ID: input.TurnID, Payload: json.RawMessage(`{"id":"tool","delta":"中\u0000文\\u0000"}`)},
			{Type: proto.TypeToolCall, ID: input.TurnID, Payload: json.RawMessage(`{"id":"tool","name":"Bash","stage":"after","result":{"output":"中\u0000文\\u0000"},"observation":{"kind":"command","status":"completed","command":"read","output":"中\u0000文\\u0000"}}`)},
		} {
			h.writeMu.Lock()
			err := h.conn.WriteJSON(env)
			h.writeMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
		}
		h.write(input.TurnID, proto.TypeDone, proto.DonePayload{Content: "Done"})
		h.finished(result, sessions.TurnCompleted)
		events, err := h.s.ListTurnEvents(ctx, h.tenant, h.session.ID, input.TurnID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, event := range events {
			switch event.Kind {
			case proto.TypeCommandOutput:
				var output proto.CommandOutputPayload
				if err := json.Unmarshal(event.Payload, &output); err != nil {
					t.Fatal(err)
				}
				if output.Delta != `中\x00文\u0000` {
					t.Fatalf("stream=%q", output.Delta)
				}
				found++
			case proto.TypeToolCall:
				var tool proto.ToolCallPayload
				if err := json.Unmarshal(event.Payload, &tool); err != nil {
					t.Fatal(err)
				}
				if tool.Stage == "before" {
					continue
				}
				var output string
				if err := json.Unmarshal(tool.Observation.Output, &output); err != nil {
					t.Fatal(err)
				}
				if output != `中\x00文\u0000` || tool.Result["output"] != output {
					t.Fatalf("snapshot=%q", output)
				}
				found++
			}
		}
		if found != 2 {
			t.Fatalf("missing output events: %d", found)
		}
	}
}
