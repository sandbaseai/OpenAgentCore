package execution

import (
	"context"
	"encoding/json"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
)

func observeTerminalCommit(ctx context.Context, session, turn, requested, returned string, attempt int, started time.Time, eventID string, err error, result Result) {
	confirmation := "unconfirmed"
	if err == nil {
		confirmation = "confirmed"
	}
	mapping := "unobserved"
	if eventID != "" {
		mapping = confirmation
	}
	attrs := []any{"session_id", session, "turn_id", turn,
		"requested_turn_status", requested, "returned_turn_status", returned, "attempt", attempt, "confirmation", confirmation, "event_id", eventID, "mapping_status", mapping}
	observeExecutionStage(ctx, "turn_completion", started, err, append(attrs, engineFailureAttrs(result)...)...)
}

// engineFailureAttrs lists the failure evidence a Turn outcome carries. Every
// value is a Core enum, a validated HTTP status or UUID, or a fixed summary
// template. Result.Error is native text and never reaches a log.
func engineFailureAttrs(result Result) []any {
	var attrs []any
	// Some Core codes embed executor-supplied text, so only whitelisted codes are logged.
	raw, _ := json.Marshal(map[string]string{"error_code": result.ErrorCode})
	if code, _ := sessions.DiagnosticFailureCode(raw); code != "internal_error" {
		attrs = append(attrs, "error_code", result.ErrorCode)
	}
	if result.EngineErrorCode != "" {
		attrs = append(attrs, "engine_error_code", result.EngineErrorCode)
	}
	if result.EngineHTTPStatus != nil {
		attrs = append(attrs, "engine_http_status", *result.EngineHTTPStatus)
	}
	if result.EngineRequestID != "" {
		attrs = append(attrs, "engine_request_id", result.EngineRequestID)
	}
	if result.EngineFingerprint != "" {
		attrs = append(attrs, "engine_fingerprint", result.EngineFingerprint)
	}
	if summary := sessions.EngineFailureSummary(result.EngineFingerprint, result.EngineHTTPStatus); summary != "" {
		attrs = append(attrs, "engine_summary", summary)
	}
	return attrs
}

// Only artifact capture activates these fixed workspace boundaries. Directory
// browsing keeps its existing path without new diagnostic scope or behavior.
func withArtifactObservation(ctx context.Context, session, turn string) context.Context {
	return textobs.WithArtifact(ctx, session, turn)
}
func observeArtifactWorkspace(ctx context.Context, stage string, started time.Time, err error, attrs ...any) {
	scope, ok := textobs.Artifact(ctx)
	if !ok {
		return
	}
	observeExecutionStage(ctx, stage, started, err, append([]any{"session_id", scope.Session, "turn_id", scope.Turn, "capture_id", scope.Capture}, attrs...)...)
}
func observeArtifactSkip(ctx context.Context, session, turn, reason string) {
	obslog.Ctx(ctx).Info("artifact capture skipped", "session_id", session, "turn_id", turn, "reason", reason)
}
