package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func upstream400Failure(t *testing.T, fingerprint string, text string) proto.Envelope {
	t.Helper()
	code, _ := proto.FingerprintCode(fingerprint)
	status := 400
	env, err := proto.NewEnvelope(proto.TypeError, "run", proto.ErrorPayload{Error: text, Code: code, HTTPStatus: &status, Fingerprint: fingerprint, UpstreamRequestID: "88a8d808-59c1-4bf3-8950-b34ce57e028b"})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestUpstream400FingerprintMergesIntoOutcomeAndSurvivesSettlement(t *testing.T) {
	for _, drain := range []bool{false, true} {
		result := Result{ErrorCode: "engine_failed"}
		settlement, _ := proto.NewEnvelope(proto.TypeError, "run", proto.ErrorPayload{Error: "executor Turn settlement failed"})
		done, _ := proto.NewEnvelope(proto.TypeDone, "run", proto.DonePayload{Usage: proto.Usage{InputTokens: 7}})
		events := []proto.Envelope{upstream400Failure(t, proto.FingerprintModelNoImageInput, "raw native text"), settlement, done}
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
		if result.EngineErrorCode != "model_input_unsupported_image" || result.EngineFingerprint != proto.FingerprintModelNoImageInput || result.EngineHTTPStatus == nil || *result.EngineHTTPStatus != 400 || result.Done.Usage.InputTokens != 7 {
			t.Fatalf("drain=%v: %+v", drain, result)
		}
		raw, _ := json.Marshal(result)
		var persisted Result
		if json.Unmarshal(raw, &persisted) != nil || persisted.EngineFingerprint != result.EngineFingerprint {
			t.Fatal(string(raw))
		}
	}
	// A later accepted classification replaces the fingerprint with it; a
	// classification from another Turn never inherits one.
	result := Result{}
	if err := result.mergeObservation(upstream400Failure(t, proto.FingerprintTrailingModelTurn, "x")); err != nil {
		t.Fatal(err)
	}
	unrelated, _ := proto.NewEnvelope(proto.TypeError, "run", proto.ErrorPayload{Error: "x", Code: "authentication_error"})
	if err := result.mergeObservation(unrelated); err != nil || result.EngineErrorCode != "authentication_error" || result.EngineFingerprint != "" || result.EngineHTTPStatus != nil {
		t.Fatalf("fingerprint outlived its classification: %v %+v", err, result)
	}
	next := Result{}
	if err := next.mergeObservation(unrelated); err != nil || next.EngineFingerprint != "" {
		t.Fatal(err, next)
	}
}

func TestUpstream400MalformedFingerprintMetadataIsDropped(t *testing.T) {
	for _, payload := range []string{
		`{"error":"x","code":"model_input_unsupported_image","http_status":400}`,
		`{"error":"x","code":"model_input_unsupported_image","http_status":429,"fingerprint":"model_no_image_input"}`,
		`{"error":"x","code":"model_input_unsupported_image","http_status":400,"fingerprint":"trailing_model_turn"}`,
		`{"error":"x","code":"model_input_unsupported_image","http_status":400,"fingerprint":{"canary":1}}`,
	} {
		result := Result{}
		if err := result.mergeObservation(proto.Envelope{Type: proto.TypeError, Payload: json.RawMessage(payload)}); err != nil {
			t.Fatal(err)
		}
		if result.EngineErrorCode != "" || result.EngineFingerprint != "" || result.EngineHTTPStatus != nil {
			t.Fatalf("%s: %+v", payload, result)
		}
	}
}

func TestTurnCompletionLogsFingerprintAndSummaryWithoutNativeText(t *testing.T) {
	const canary = "sk-canary-secret-token"
	logs := captureExecutionLogs(t)
	status := 400
	result := Result{ErrorCode: "engine_failed", Error: "unexpected status 400 Bad Request: " + canary, EngineErrorCode: "model_reasoning_state_invalid", EngineHTTPStatus: &status,
		EngineRequestID: "88a8d808-59c1-4bf3-8950-b34ce57e028b", EngineFingerprint: proto.FingerprintReasoningEncryptedUnverifiable}
	observeTerminalCommit(context.Background(), "session", "turn", sessions.TurnFailed, sessions.TurnFailed, 1, time.Now(), "event", nil, result)
	rows := terminalRows(t, logs.Bytes())
	if len(rows) != 1 {
		t.Fatal(logs.String())
	}
	row := rows[0]
	want := map[string]any{"error_code": "engine_failed", "engine_error_code": "model_reasoning_state_invalid", "engine_http_status": float64(400),
		"engine_request_id": "88a8d808-59c1-4bf3-8950-b34ce57e028b", "engine_fingerprint": proto.FingerprintReasoningEncryptedUnverifiable,
		"engine_summary": "upstream could not verify encrypted reasoning content carried in thread history"}
	for key, value := range want {
		if row[key] != value {
			t.Fatalf("%s = %v, want %v", key, row[key], value)
		}
	}
	if strings.Contains(logs.String(), canary) || strings.Contains(logs.String(), "unexpected status") {
		t.Fatal("native text reached the log")
	}
	// A completed Turn without failure evidence adds no failure fields.
	logs.Reset()
	observeTerminalCommit(context.Background(), "session", "turn", sessions.TurnCompleted, sessions.TurnCompleted, 1, time.Now(), "event", nil, Result{})
	for _, key := range []string{"error_code", "engine_error_code", "engine_http_status", "engine_request_id", "engine_fingerprint", "engine_summary"} {
		if bytes.Contains(logs.Bytes(), []byte(`"`+key+`"`)) {
			t.Fatalf("%s logged for a Turn without failure: %s", key, logs.String())
		}
	}
}

func TestEngineFailureAttrsOnlyLogWhitelistedCoreCodes(t *testing.T) {
	// Input rejection codes embed an executor-supplied suffix and must not be logged verbatim.
	for _, code := range []string{"input_secret-canary", "secret-canary", ""} {
		for _, attr := range engineFailureAttrs(Result{ErrorCode: code}) {
			if text, ok := attr.(string); ok && strings.Contains(text, "canary") {
				t.Fatalf("%q leaked: %v", code, attr)
			}
		}
	}
	attrs := engineFailureAttrs(Result{ErrorCode: "input_busy"})
	if len(attrs) != 2 || attrs[0] != "error_code" || attrs[1] != "input_busy" {
		t.Fatal(attrs)
	}
	// An untyped 400 is summarized without any fingerprint.
	status := 400
	attrs = engineFailureAttrs(Result{EngineErrorCode: "connection_failed", EngineHTTPStatus: &status})
	if !strings.Contains(strings.Join(func() (v []string) {
		for _, a := range attrs {
			if s, ok := a.(string); ok {
				v = append(v, s)
			}
		}
		return
	}(), " "), "upstream returned HTTP 400 (unclassified)") {
		t.Fatal(attrs)
	}
}
