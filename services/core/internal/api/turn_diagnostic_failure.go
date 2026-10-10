package api

import (
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func turnDiagnosticFailure(turn sessions.Turn) *DiagnosticFailure {
	if turn.Status != sessions.TurnFailed {
		return nil
	}
	code, status := sessions.DiagnosticFailureCode(turn.Outcome)
	params := CoreErrorDetails{}
	if code == "connection_failed" {
		params["http_status"] = CoreErrorNull()
		if status != nil {
			params["http_status"] = CoreErrorNumber(float64(*status))
		}
	}

	return &DiagnosticFailure{Code: code, Params: params, FailedAt: diagnosticTime(turn.CompletedAt)}
}
