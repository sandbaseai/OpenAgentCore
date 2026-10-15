package codex

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// Only the exact root terminal TurnError is authoritative. Error notifications,
// including exhausted/recovered retries, never become a sticky classification.
func classifyTurnError(failure *TurnError) proto.ErrorPayload {
	if failure == nil {
		return proto.ErrorPayload{}
	}
	// Upstream HTTP 400 patterns are checked before the generic variant mapping,
	// whose badRequest branch would otherwise return first.
	if classified, ok := classifyUpstream400(failure); ok {
		return classified
	}
	var variant string
	var code string
	// Some native HTTP failures arrive as "other" without typed status. Only
	// the exact native terminal HTTP envelope is recognized, never accumulated
	// output or notification text. The HTTP 400 exception above reads the
	// provider message of that same envelope, anchored at its start.
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

const (
	upstream400Envelope = "unexpected status 400 Bad Request"
	// Longer native messages are never matched; the envelope is short and the
	// recognized provider bodies are far smaller.
	upstream400MaxMessage = 4096
)

// Matched only at the start of the error message parsed from the body, never
// anywhere inside it, so echoed user or tool text cannot select a fingerprint.
// The observed provider wording is an assumption until calibrated against a live
// codex 0.153.4 run (see the Core upstream-400 design, V1).
var (
	reasoningEncryptedPattern = regexp.MustCompile(`^The encrypted content for item rs_[A-Za-z0-9_]+ could not be verified`)
	toolResultImagePattern    = regexp.MustCompile(`^(?:messages\.[0-9]+\.content\.[0-9]+\.)?tool_result\.content\.[0-9]+: Input tag 'input_image'[^\r\n]{0,80}? does not match any of the expected tags`)
)

// classifyUpstream400 recognizes fixed provider rejections that arrive inside
// codex's native HTTP 400 envelope. It reads native text, but its output is only
// a fingerprint from a finite set; nothing from the text is retained.
func classifyUpstream400(failure *TurnError) (proto.ErrorPayload, bool) {
	if !upstream400Variant(failure.CodexErrorInfo) {
		return proto.ErrorPayload{}, false
	}
	message, ok := upstream400ErrorMessage(failure.Message)
	if !ok {
		return proto.ErrorPayload{}, false
	}
	var fingerprint string
	switch {
	case strings.HasPrefix(message, "field messages is required"):
		fingerprint = proto.FingerprintProviderMessagesRequired
	case reasoningEncryptedPattern.MatchString(message):
		fingerprint = proto.FingerprintReasoningEncryptedUnverifiable
	case strings.HasPrefix(message, "Model do not support image input"):
		fingerprint = proto.FingerprintModelNoImageInput
	case toolResultImagePattern.MatchString(message):
		fingerprint = proto.FingerprintToolResultInputImageRejected
	case strings.HasPrefix(message, "Requests ending with a model turn are not supported"):
		fingerprint = proto.FingerprintTrailingModelTurn
	default:
		return proto.ErrorPayload{}, false
	}
	code, _ := proto.FingerprintCode(fingerprint)
	status := 400
	return proto.ErrorPayload{Code: code, HTTPStatus: &status, UpstreamRequestID: nativeRequestID(failure.Message), Fingerprint: fingerprint}, true
}

// An HTTP 400 may be reported untyped (null or "other"), as "badRequest", or as
// an HTTP connection failure that carries status 400. Every other variant,
// including other statuses, keeps its own classification.
func upstream400Variant(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return name == "other" || name == "badRequest"
	}
	var object map[string]*struct {
		HTTPStatusCode *int `json:"httpStatusCode"`
	}
	if json.Unmarshal(raw, &object) != nil || len(object) != 1 {
		return false
	}
	failed := object["httpConnectionFailed"]
	return failed != nil && failed.HTTPStatusCode != nil && *failed.HTTPStatusCode == 400
}

// upstream400ErrorMessage returns the provider's error message from the native
// envelope "unexpected status 400 Bad Request: <body>[, url: ...]". A JSON body
// yields error.message or the top-level message (Bedrock style); any other body
// is used as text. Multi-line bodies are allowed.
func upstream400ErrorMessage(message string) (string, bool) {
	if len(message) > upstream400MaxMessage || !strings.HasPrefix(message, upstream400Envelope+":") {
		return "", false
	}
	body := strings.TrimLeft(message[len(upstream400Envelope)+1:], " \t\r\n")
	if !strings.HasPrefix(body, "{") {
		return body, true
	}
	// The decoder stops after the first value, so codex's trailing ", url: ..."
	// suffix does not make a valid body unreadable.
	var document struct {
		Message json.RawMessage `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.NewDecoder(strings.NewReader(body)).Decode(&document) != nil {
		return "", false
	}
	var nested struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(document.Error, &nested) == nil && nested.Message != "" {
		return nested.Message, true
	}
	var top string
	if json.Unmarshal(document.Message, &top) == nil && top != "" {
		return top, true
	}
	return "", false
}
