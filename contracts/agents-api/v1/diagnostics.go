package v1

import "time"

// ExecutionDiagnostic is the safe failure projection of a committed snapshot.
// It deliberately excludes native messages, parameters and executor identities.
type ExecutionDiagnostic struct {
	Code     string     `json:"code" binding:"required" enums:"unknown,harness_error,model_provider_required,runtime_unavailable,runtime_disconnected,runtime_preparation_failed,execution_interrupted,delivery_unconfirmed,input_rejected,executor_protocol_error,core_storage_failed,environment_connection_timeout,environment_unavailable,environment_provisioning_failed,authentication_error,rate_limit_exceeded,usage_limit_exceeded,server_overloaded,server_error,invalid_request,resource_not_found,request_timeout,context_length_exceeded,cyber_policy,connection_failed"`
	Source   string     `json:"source" binding:"required" enums:"turn,environment,environment_input"`
	FailedAt *time.Time `json:"failed_at" binding:"required" extensions:"x-nullable"`
}

// SessionDiagnostics is an OpenAgentCore extension, not an official OpenAI DTO.
type SessionDiagnostics struct {
	Object     string               `json:"object" binding:"required" enums:"agent.session_diagnostics"`
	SessionID  string               `json:"session_id" binding:"required"`
	TurnID     string               `json:"turn_id,omitempty"`
	Status     string               `json:"status" binding:"required" enums:"idle,in_progress,requires_action,failed"`
	Diagnostic *ExecutionDiagnostic `json:"diagnostic" binding:"required" extensions:"x-nullable"`
}

// TurnDiagnostics is an OpenAgentCore extension, not an official OpenAI DTO.
type TurnDiagnostics struct {
	Object     string               `json:"object" binding:"required" enums:"agent.turn_diagnostics"`
	SessionID  string               `json:"session_id" binding:"required"`
	TurnID     string               `json:"turn_id" binding:"required"`
	Status     string               `json:"status" binding:"required" enums:"queued,in_progress,waiting,completed,failed,cancelled"`
	Diagnostic *ExecutionDiagnostic `json:"diagnostic" binding:"required" extensions:"x-nullable"`
}
