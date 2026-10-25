package api

import (
	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
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

	return &DiagnosticFailure{Code: code, Params: params, FailedAt: diagnosticTime(turn.CompletedAt)}
}

// publicTurnErrors maps the diagnostics code to the official
// SessionTurnErrorCodeResource with a fixed customer-safe sentence: native
// model categories keep their names and a Runtime that disconnected or failed
// its preparation is sandbox_error. runtime_unavailable stays internal_error
// because it also covers Core failing to dispatch the Turn. Native text never
// reaches it.
var publicTurnErrors = map[string]string{
	"context_length_exceeded": "The request exceeds the model's context window.",
	"usage_limit_exceeded":    "The organization has reached a usage, plan, or billing limit.",
	"rate_limit_exceeded":     "The request exceeds the available rate limit.",
	"server_overloaded":       "The model service is temporarily overloaded.",
	"cyber_policy":            "The request was rejected by a safety policy.",
	"connection_failed":       "The request could not connect to the model service.",
	"server_error":            "The model service encountered an unexpected error.",
	"authentication_error":    "The API credentials are invalid or lack the required access.",
	"invalid_request":         "The request contains invalid input or configuration.",
	"resource_not_found":      "The requested model or resource is unavailable.",
	"request_timeout":         "The request timed out before the model service responded.",
	"sandbox_error":           "The request could not complete in its execution environment.",
}

func publicTurnError(diagnosticCode string) *v1.TurnError {
	code := diagnosticCode
	switch diagnosticCode {
	case "runtime_disconnected", "runtime_preparation_failed":
		code = "sandbox_error"
	}
	if message, ok := publicTurnErrors[code]; ok {
		return &v1.TurnError{Code: code, Message: message}
	}
	return &v1.TurnError{Code: "internal_error", Message: "The execution could not complete."}
}
