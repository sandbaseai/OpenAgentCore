package execution

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func TestEngineClassificationSurvivesDrainWithoutChangingSettlement(t *testing.T) {
	for _, prior := range []string{"", "event_persistence_failed", "cancel_unconfirmed", "event_stream_incomplete"} {
		result := Result{ErrorCode: prior}
		writer := &recoveringWriter{}
		j := journal{writer: writer, next: 1}
		events := make(chan proto.Envelope, 3)
		failure, _ := proto.NewEnvelope(proto.TypeError, "run", proto.ErrorPayload{Error: "raw native text", Code: "authentication_error"})
		usage, _ := proto.NewEnvelope(proto.TypeUsage, "run", proto.Usage{InputTokens: 7})
		done, _ := proto.NewEnvelope(proto.TypeDone, "run", map[string]any{"metadata": map[string]any{proto.DoneMetaAgentSessionID: "native"}})
		events <- failure
		events <- usage
		events <- done
		close(events)
		if err := j.drain(events, &result); err != nil {
			t.Fatal(err)
		}
		if result.ErrorCode != prior || result.EngineErrorCode != "authentication_error" || result.Done.Usage.InputTokens != 7 || result.Done.Metadata[proto.DoneMetaAgentSessionID] != "native" {
			t.Fatal(result)
		}
		reply := cancellationResult{ack: proto.InteractionDecisionAckPayload{Applied: true, Outcome: &proto.DonePayload{Usage: proto.Usage{InputTokens: 9}, Metadata: map[string]any{proto.DoneMetaAgentSessionID: "native"}}}}
		if err := recordCancellation(context.Background(), &j, reply, &result); err != nil {
			t.Fatal(err)
		}
		if result.ErrorCode != prior || result.Done.Usage.InputTokens != 9 {
			t.Fatal(result)
		}
		raw, _ := json.Marshal(result)
		var persisted Result
		if json.Unmarshal(raw, &persisted) != nil || persisted.EngineErrorCode != result.EngineErrorCode {
			t.Fatal(string(raw))
		}
		unknown := proto.Envelope{Type: proto.TypeError, Payload: json.RawMessage(`{"error":"new","code":{"arbitrary":"value"},"http_status":"secret"}`)}
		if err := result.mergeObservation(unknown); err != nil || result.EngineErrorCode != "authentication_error" || result.ErrorCode != prior || result.Done.Usage.InputTokens != 9 {
			t.Fatal(err, result)
		}
	}
}

func TestClassifiedErrorCannotOverrideCancellationReceipt(t *testing.T) {
	for _, drainFirst := range []bool{true, false} {
		result := Result{ErrorCode: "engine_failed"}
		j := journal{writer: &recoveringWriter{}, next: 1}
		failure, _ := proto.NewEnvelope(proto.TypeError, "run", proto.ErrorPayload{Code: "rate_limit_exceeded"})
		events := make(chan proto.Envelope, 1)
		events <- failure
		close(events)
		ack := make(chan cancellationResult, 1)
		ack <- cancellationResult{ack: proto.InteractionDecisionAckPayload{Applied: true, Outcome: &proto.DonePayload{Usage: proto.Usage{InputTokens: 9}, Metadata: map[string]any{proto.DoneMetaAgentSessionID: "native"}}}}
		if drainFirst {
			if err := j.drain(events, &result); err != nil {
				t.Fatal(err)
			}
		}
		status := finishDelivery(context.Background(), &j, &result, ack, false, nil)
		if !drainFirst {
			if err := j.drain(events, &result); err != nil {
				t.Fatal(err)
			}
		}
		if status != sessions.TurnCancelled || result.Done.Usage.InputTokens != 9 || result.Done.Metadata[proto.DoneMetaAgentSessionID] != "native" || result.EngineErrorCode != "rate_limit_exceeded" {
			t.Fatalf("receipt lost authority: %s %+v", status, result)
		}
	}
}

func TestBillingFailureOutcomePreservesSafeCorrelation(t *testing.T) {
	status := 402
	failure, _ := proto.NewEnvelope(proto.TypeError, "run", proto.ErrorPayload{Code: "connection_failed", HTTPStatus: &status, UpstreamRequestID: "88a8d808-59c1-4bf3-8950-b34ce57e028b"})
	result := Result{ErrorCode: "engine_failed"}
	if err := result.mergeObservation(failure); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	var saved Result
	if json.Unmarshal(raw, &saved) != nil || saved.EngineErrorCode != "provider_billing_unavailable" || saved.EngineRequestID == "" || saved.EngineHTTPStatus == nil || *saved.EngineHTTPStatus != 402 {
		t.Fatal(string(raw))
	}
}

func TestEngineClassificationSurvivesUnclassifiedSettlement(t *testing.T) {
	for _, drain := range []bool{false, true} {
		result := Result{ErrorCode: "engine_failed"}
		failure, _ := proto.NewEnvelope(proto.TypeError, "turn", proto.ErrorPayload{Error: "claudesdk: execution_failed", Code: "resource_not_found"})
		settlement, _ := proto.NewEnvelope(proto.TypeError, "turn", proto.ErrorPayload{Error: "executor Turn settlement failed"})
		done, _ := proto.NewEnvelope(proto.TypeDone, "turn", proto.DonePayload{Usage: proto.Usage{InputTokens: 7}})
		events := []proto.Envelope{failure, settlement, done}
		if drain {
			queued := make(chan proto.Envelope, len(events))
			for _, event := range events {
				queued <- event
			}
			close(queued)
			j := journal{writer: &recoveringWriter{}, next: 1}
			if err := j.drain(queued, &result); err != nil {
				t.Fatal(err)
			}
		} else {
			for _, event := range events {
				if err := result.mergeObservation(event); err != nil {
					t.Fatal(err)
				}
			}
		}
		if result.EngineErrorCode != "resource_not_found" || result.EngineHTTPStatus != nil || result.ErrorCode != "engine_failed" || result.Done.Usage.InputTokens != 7 {
			t.Fatalf("drain=%v: %+v", drain, result)
		}
		// Each delivery creates its own Result; generic errors in another Turn stay unknown.
		next := Result{}
		settlement.ID = "next-turn"
		if err := next.mergeObservation(settlement); err != nil || next.EngineErrorCode != "" || next.EngineHTTPStatus != nil {
			t.Fatalf("classification crossed Turns: %v %+v", err, next)
		}
	}
}

func TestEngineClassificationUpdatesOnlyFromAcceptedMetadata(t *testing.T) {
	status := 502
	result := Result{}
	for _, step := range []struct {
		payload string
		code    string
		status  *int
	}{
		{`{"code":"connection_failed","http_status":502}`, "connection_failed", &status},
		{`{"error":"settlement failed"}`, "connection_failed", &status},
		{`{"code":"unknown_code","http_status":401}`, "connection_failed", &status},
		{`{"code":{"invalid":true},"http_status":"invalid"}`, "connection_failed", &status},
		{`{"code":"resource_not_found","http_status":404}`, "resource_not_found", nil},
		{`{"code":"authentication_error"}`, "authentication_error", nil},
	} {
		if err := result.mergeObservation(proto.Envelope{Type: proto.TypeError, Payload: json.RawMessage(step.payload)}); err != nil {
			t.Fatal(err)
		}
		if result.EngineErrorCode != step.code || (result.EngineHTTPStatus == nil) != (step.status == nil) || (step.status != nil && *result.EngineHTTPStatus != *step.status) {
			t.Fatalf("payload=%s: %+v", step.payload, result)
		}
	}
}
