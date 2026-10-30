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

// NormalizeEngineFailure accepts only neutral, finite classifications. Optional
// metadata from newer or malformed peers never changes terminal/usage decoding.
func NormalizeEngineFailure(code string, status *int) (string, *int) {
	switch code {
	case "incomplete_response", "authentication_error", "rate_limit_exceeded", "usage_limit_exceeded", "server_overloaded", "server_error", "invalid_request", "resource_not_found", "request_timeout", "context_length_exceeded", "cyber_policy":
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
	default:
		return "", nil
	}
}

func (p *ErrorPayload) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Error      string          `json:"error"`
		Code       json.RawMessage `json:"code"`
		HTTPStatus json.RawMessage `json:"http_status"`
		RequestID  json.RawMessage `json:"upstream_request_id"`
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
	var requestID string
	_ = json.Unmarshal(wire.RequestID, &requestID)
	p.UpstreamRequestID = NormalizeProviderRequestID(requestID)
	return nil
}
