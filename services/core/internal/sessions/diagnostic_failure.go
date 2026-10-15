package sessions

import (
	"encoding/json"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// DiagnosticFailureCode classifies only typed outcome metadata. Native text is never inspected.
func DiagnosticFailureCode(outcomeJSON json.RawMessage) (string, *int) {
	var outcome map[string]json.RawMessage
	var coreCode string
	if json.Unmarshal(outcomeJSON, &outcome) == nil {
		_ = json.Unmarshal(outcome["error_code"], &coreCode)
	}
	code := "internal_error"
	var httpStatus *int
	switch coreCode {
	case "engine_failed":
		code = "harness_error"
		// Optional malformed metadata cannot hide the authoritative Core error.
		var nativeCode string
		var nativeStatus *int
		_ = json.Unmarshal(outcome["engine_error_code"], &nativeCode)
		_ = json.Unmarshal(outcome["engine_http_status"], &nativeStatus)
		if classified, status := proto.NormalizeEngineFailure(nativeCode, nativeStatus); classified != "" {
			code = classified
			httpStatus = status
		}
	case "model_provider_required":
		code = "model_provider_required"
	case "execution_device_unavailable", "execution_unavailable":
		code = "runtime_unavailable"
	case "device_disconnected", "event_stream_incomplete":
		code = "runtime_disconnected"
	case "preparation_start_failed", "preparation_interrupted":
		code = "runtime_preparation_failed"
	case "execution_interrupted":
		code = "execution_interrupted"
	case "delivery_unknown", "input_outcome_unknown", "cancel_unconfirmed", "cancel_outcome_unavailable", "function_result_unconfirmed":
		code = "delivery_unconfirmed"
	case "invalid_input", "input_not_applied", "message_input_unsupported", "input_invalid_input", "input_run_inactive", "input_input_conflict", "input_input_limit", "input_unsupported", "input_rejected", "input_not_ready", "input_busy":
		code = "input_rejected"
	case "invalid_executor_result", "execution_state_unavailable", "execution_state_changed", "function_call_invalid", "function_result_invalid":
		code = "executor_protocol_error"
	case "event_persistence_failed", "artifact_capture_failed":
		code = "core_storage_failed"
	}
	return code, httpStatus
}

// EngineFailureSummary returns the fixed English summary for an upstream HTTP
// 400 fingerprint, or for a typed HTTP 400 without one. It is selected from a
// closed table, so no provider text, identifier or user content can enter it.
// Every template is at most 240 bytes.
func EngineFailureSummary(fingerprint string, httpStatus *int) string {
	switch fingerprint {
	case proto.FingerprintProviderMessagesRequired:
		return "upstream provider rejected the Responses request body (messages field required)"
	case proto.FingerprintReasoningEncryptedUnverifiable:
		return "upstream could not verify encrypted reasoning content carried in thread history"
	case proto.FingerprintModelNoImageInput:
		return "selected model does not accept image input"
	case proto.FingerprintToolResultInputImageRejected:
		return "upstream provider rejected an input_image block inside a tool result"
	case proto.FingerprintTrailingModelTurn:
		return "upstream rejected a request that ends with a model turn"
	}
	if httpStatus != nil && *httpStatus == 400 {
		return "upstream returned HTTP 400 (unclassified)"
	}
	return ""
}

// UpstreamFailureDetail is the validated upstream HTTP 400 evidence of a failed
// Turn. Fields are empty unless the outcome carries a paired code, fingerprint
// and status 400 for an engine failure.
type UpstreamFailureDetail struct {
	Fingerprint, Summary, RequestID string
}

// DiagnosticUpstreamFailure reads the typed fingerprint metadata of a failed
// Turn outcome. Like DiagnosticFailureCode it never inspects native text, and
// every value is re-validated because the outcome is only trusted as far as the
// shared pairing rules.
func DiagnosticUpstreamFailure(outcomeJSON json.RawMessage) UpstreamFailureDetail {
	var outcome map[string]json.RawMessage
	if json.Unmarshal(outcomeJSON, &outcome) != nil {
		return UpstreamFailureDetail{}
	}
	var coreCode, nativeCode, fingerprint, requestID string
	var nativeStatus *int
	_ = json.Unmarshal(outcome["error_code"], &coreCode)
	_ = json.Unmarshal(outcome["engine_error_code"], &nativeCode)
	_ = json.Unmarshal(outcome["engine_http_status"], &nativeStatus)
	_ = json.Unmarshal(outcome["engine_fingerprint"], &fingerprint)
	_ = json.Unmarshal(outcome["engine_request_id"], &requestID)
	if coreCode != "engine_failed" {
		return UpstreamFailureDetail{}
	}
	code, status := proto.NormalizeEngineFailure(nativeCode, nativeStatus)
	code, fingerprint = proto.NormalizeEngineFingerprint(code, fingerprint)
	if code == "" || fingerprint == "" || status == nil || *status != 400 {
		return UpstreamFailureDetail{}
	}
	return UpstreamFailureDetail{Fingerprint: fingerprint, Summary: EngineFailureSummary(fingerprint, status), RequestID: proto.NormalizeProviderRequestID(requestID)}
}
