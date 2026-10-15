package proto

import (
	"encoding/json"
	"regexp"
)

var providerRequestIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NormalizeProviderRequestID permits only UUID correlation identifiers, never arbitrary headers.
func NormalizeProviderRequestID(id string) string {
	if providerRequestIDPattern.MatchString(id) {
		return id
	}
	return ""
}

// Upstream HTTP 400 fingerprints. Each names one fixed failure pattern that a
// Runtime recognizes from the native terminal error; the paired code is owned
// here so a Runtime cannot attach a classification to a different pattern.
const (
	FingerprintProviderMessagesRequired       = "provider_messages_required"
	FingerprintReasoningEncryptedUnverifiable = "reasoning_encrypted_unverifiable"
	FingerprintModelNoImageInput              = "model_no_image_input"
	FingerprintToolResultInputImageRejected   = "tool_result_input_image_rejected"
	FingerprintTrailingModelTurn              = "trailing_model_turn"
)

const (
	EngineCodeInputUnsupportedImage  = "model_input_unsupported_image"
	EngineCodeProviderFormatRejected = "model_provider_format_rejected"
	EngineCodeReasoningStateInvalid  = "model_reasoning_state_invalid"
	EngineCodeInputEndsWithAssistant = "model_input_ends_with_assistant"
)

var fingerprintCode = map[string]string{
	FingerprintProviderMessagesRequired:       EngineCodeProviderFormatRejected,
	FingerprintReasoningEncryptedUnverifiable: EngineCodeReasoningStateInvalid,
	FingerprintModelNoImageInput:              EngineCodeInputUnsupportedImage,
	FingerprintToolResultInputImageRejected:   EngineCodeProviderFormatRejected,
	FingerprintTrailingModelTurn:              EngineCodeInputEndsWithAssistant,
}

// FingerprintCode returns the code paired with a known fingerprint.
func FingerprintCode(fingerprint string) (string, bool) {
	code, ok := fingerprintCode[fingerprint]
	return code, ok
}

// NormalizeEngineFailure accepts only neutral, finite classifications. Optional
// metadata from newer or malformed peers never changes terminal/usage decoding.
func NormalizeEngineFailure(code string, status *int) (string, *int) {
	switch code {
	case "authentication_error", "rate_limit_exceeded", "usage_limit_exceeded", "server_overloaded", "server_error", "invalid_request", "resource_not_found", "request_timeout", "context_length_exceeded", "cyber_policy":
		return code, nil
	case "connection_failed":
		if status != nil && *status >= 100 && *status <= 599 {
			if *status == 402 {
				return "provider_billing_unavailable", status
			}
			return code, status
		}
		return code, nil
	case "provider_billing_unavailable":
		if status != nil && *status == 402 {
			return code, status
		}
		return code, nil
	case EngineCodeInputUnsupportedImage, EngineCodeProviderFormatRejected, EngineCodeReasoningStateInvalid, EngineCodeInputEndsWithAssistant:
		// Upstream 400 classifications are meaningless without their status.
		if status != nil && *status == 400 {
			return code, status
		}
		return "", nil
	default:
		return "", nil
	}
}

// NormalizeEngineFingerprint pairs a fingerprint with its code. An unknown
// fingerprint is dropped. An upstream 400 code without its exact fingerprint is
// dropped together with the fingerprint, because the code alone proves nothing.
// A fingerprint on any other code is dropped and the code is kept.
func NormalizeEngineFingerprint(code, fingerprint string) (string, string) {
	expected, known := fingerprintCode[fingerprint]
	if !known {
		fingerprint = ""
	}
	if isUpstream400Code(code) {
		if !known || expected != code {
			return "", ""
		}
		return code, fingerprint
	}
	return code, ""
}

func isUpstream400Code(code string) bool {
	switch code {
	case EngineCodeInputUnsupportedImage, EngineCodeProviderFormatRejected, EngineCodeReasoningStateInvalid, EngineCodeInputEndsWithAssistant:
		return true
	}
	return false
}

func (p *ErrorPayload) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Error       string          `json:"error"`
		Code        json.RawMessage `json:"code"`
		HTTPStatus  json.RawMessage `json:"http_status"`
		RequestID   json.RawMessage `json:"upstream_request_id"`
		Fingerprint json.RawMessage `json:"fingerprint"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	var code string
	var status *int
	_ = json.Unmarshal(wire.Code, &code)
	_ = json.Unmarshal(wire.HTTPStatus, &status)
	p.Error = wire.Error
	p.Code, p.HTTPStatus = NormalizeEngineFailure(code, status)
	var fingerprint string
	_ = json.Unmarshal(wire.Fingerprint, &fingerprint)
	paired, fingerprint := NormalizeEngineFingerprint(p.Code, fingerprint)
	if paired != p.Code {
		p.HTTPStatus = nil
	}
	p.Code, p.Fingerprint = paired, fingerprint
	var requestID string
	_ = json.Unmarshal(wire.RequestID, &requestID)
	p.UpstreamRequestID = NormalizeProviderRequestID(requestID)
	return nil
}
