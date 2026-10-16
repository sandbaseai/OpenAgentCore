package codex

import (
	"encoding/json"
	"strings"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// Only the exact root terminal TurnError is authoritative. Error notifications,
// including exhausted/recovered retries, never become a sticky classification.
func classifyTurnError(failure *TurnError) proto.ErrorPayload {
	if failure == nil {
		return proto.ErrorPayload{}
	}
	var variant string
	var code string
	// Some native HTTP failures arrive as "other" without typed status. Only
	// the exact native terminal HTTP envelope is recognized, never accumulated
	// output, notification text or provider response-body keywords.
	if len(failure.CodexErrorInfo) == 0 || string(failure.CodexErrorInfo) == "null" ||
		(json.Unmarshal(failure.CodexErrorInfo, &variant) == nil && variant == "other") {
		const prefix = "unexpected status 402 Payment Required"
		message := failure.Message
		if !strings.ContainsAny(message, "\r\n") && (message == prefix || strings.HasPrefix(message, prefix+": ") || strings.HasPrefix(message, prefix+", ")) {
			status := 402
			return proto.ErrorPayload{Code: "provider_billing_unavailable", HTTPStatus: &status, UpstreamRequestID: nativeRequestID(message)}
		}
	}
	if json.Unmarshal(failure.CodexErrorInfo, &variant) == nil {
		switch variant {
		case "unauthorized":
			code = "authentication_error"
		case "usageLimitExceeded":
			code = "usage_limit_exceeded"
		case "rateLimitExceeded":
			code = "rate_limit_exceeded"
		case "contextWindowExceeded":
			code = "context_length_exceeded"
		case "serverOverloaded":
			code = "server_overloaded"
		case "internalServerError":
			code = "server_error"
		case "badRequest":
			code = "invalid_request"
		case "cyberPolicy":
			code = "cyber_policy"
		case "other":
			code = providerRejectionCode(failure.Message)
		}
		return proto.ErrorPayload{Code: code}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(failure.CodexErrorInfo, &object) != nil || len(object) != 1 {
		return proto.ErrorPayload{}
	}
	for variant, raw := range object {
		switch variant {
		case "httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts":
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil || fields == nil {
				return proto.ErrorPayload{}
			}
			var status *int
			_ = json.Unmarshal(fields["httpStatusCode"], &status)
			code = "connection_failed"
			if variant == "responseTooManyFailedAttempts" && status != nil && *status == 429 {
				code = "rate_limit_exceeded"
			}
			code, status = proto.NormalizeEngineFailure(code, status)
			return proto.ErrorPayload{Code: code, HTTPStatus: status, UpstreamRequestID: nativeRequestID(failure.Message)}
		}
	}
	return proto.ErrorPayload{}
}

// The native transport places its request ID last. Ignore arbitrary body text
// and reject multiline messages or anything other than a UUID suffix.
func nativeRequestID(message string) string {
	if !strings.HasPrefix(message, "unexpected status ") || strings.ContainsAny(message, "\r\n") {
		return ""
	}
	_, id, ok := strings.Cut(message, ", request id: ")
	if !ok {
		return ""
	}
	return proto.NormalizeProviderRequestID(id)
}

// Codex 0.153.4 reports a provider HTTP 400 as "other" and forwards the
// response body verbatim; its own "other" failures are prose. A JSON object with
// an object "error" member is the provider's rejection: a declared code in its
// own error.code or error.type wins, otherwise it is invalid_request. Prose, such
// as a plain-text 400 body, stays unclassified.
func providerRejectionCode(message string) string {
	var body struct {
		Error *struct {
			Code json.RawMessage `json:"code"`
			Type json.RawMessage `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(message), &body) != nil || body.Error == nil {
		return ""
	}
	for _, raw := range []json.RawMessage{body.Error.Code, body.Error.Type} {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			if code, _ := proto.NormalizeEngineFailure(name, nil); code != "" {
				return code
			}
		}
	}
	return "invalid_request"
}
