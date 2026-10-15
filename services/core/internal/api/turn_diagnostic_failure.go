package api

import (
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func turnDiagnosticFailure(turn sessions.Turn) *DiagnosticFailure {
	if turn.Status != sessions.TurnFailed {
		return nil
	}
	code, status := sessions.DiagnosticFailureCode(turn.Outcome)
	params := CoreErrorDetails{}
	if code == "connection_failed" || code == "provider_billing_unavailable" {
		params["http_status"] = CoreErrorNull()
		if status != nil {
			params["http_status"] = CoreErrorNumber(float64(*status))
		}
	}

	if upstream := sessions.DiagnosticUpstreamFailure(turn.Outcome); upstream.Fingerprint != "" && isUpstream400DiagnosticCode(code) {
		params["fingerprint"] = CoreErrorString(upstream.Fingerprint)
		params["summary"] = CoreErrorString(upstream.Summary)
		if upstream.RequestID != "" {
			params["upstream_request_id"] = CoreErrorString(upstream.RequestID)
		}
	}
	// Only a failed Turn is diagnosed, so only its unsuccessful recoveries
	// appear here, whatever the final failure category; a Turn that recovered
	// is read from its stored outcome.
	if recoveries := sessions.DiagnosticRecoveries(turn.Outcome); recoveries.Present() {
		params["recovery_count"] = CoreErrorNumber(float64(recoveries.Count))
		params["recovery_fingerprints"] = CoreErrorStrings(recoveries.Fingerprints...)
		params["recovery_outcomes"] = CoreErrorStrings(recoveries.Outcomes...)
		params["recovery_dropped_reasoning_items"] = CoreErrorNumber(float64(recoveries.DroppedReasoningItems))
		params["recovery_dropped_images"] = CoreErrorNumber(float64(recoveries.DroppedImages))
	}
	return &DiagnosticFailure{Code: code, Params: params, FailedAt: diagnosticTime(turn.CompletedAt)}
}

func isUpstream400DiagnosticCode(code string) bool {
	switch code {
	case proto.EngineCodeInputUnsupportedImage, proto.EngineCodeProviderFormatRejected, proto.EngineCodeReasoningStateInvalid, proto.EngineCodeInputEndsWithAssistant:
		return true
	}
	return false
}
