// Package modelprovider validates the frozen upstream connection supplied to a native Harness.
package modelprovider

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"
)

type Protocol string

const (
	Anthropic       Protocol = "anthropic"
	Responses       Protocol = "responses"
	ChatCompletions Protocol = "chat_completions"
)

// Provider is the confidential, frozen upstream bundle supplied by Core.
// Engine selection and model identity remain separate execution inputs.
type Provider struct {
	Protocol        Protocol `json:"protocol"`
	BaseURL         string   `json:"base_url"`
	APIKey          string   `json:"api_key"`
	Headers         Headers  `json:"headers,omitempty"`
	ContextWindow   int32    `json:"context_window,omitempty"`
	MaxOutputTokens int32    `json:"max_output_tokens,omitempty"`
}

var ErrConfiguration = errors.New("invalid model provider configuration")

func ParseProvider(raw any) (Provider, error) {
	var provider Provider
	body, err := json.Marshal(raw)
	if err != nil {
		return provider, ErrConfiguration
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&provider) != nil {
		return Provider{}, ErrConfiguration
	}
	if err := provider.Validate(); err != nil {
		return Provider{}, err
	}
	return provider, nil
}

// Protocols is the single vocabulary of supported upstream protocol formats.
// The Harness catalog generator projects it to the TypeScript client.
func Protocols() []Protocol { return []Protocol{Anthropic, Responses, ChatCompletions} }

func (p Protocol) Valid() bool { return slices.Contains(Protocols(), p) }

func (p Provider) Validate() error {
	if !p.Protocol.Valid() {
		return ErrConfiguration
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(p.BaseURL, "\x00\r\n") {
		return ErrConfiguration
	}
	// Remote providers require HTTPS. Runtime-local providers may use loopback
	// HTTP; Core retains stricter public provider admission.
	if u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback()) {
		return ErrConfiguration
	}
	if strings.TrimSpace(p.APIKey) == "" || len(p.APIKey) > 16384 || strings.ContainsAny(p.APIKey, "\x00\r\n") {
		return ErrConfiguration
	}
	if !TokenLimitsValid(p.ContextWindow, p.MaxOutputTokens) {
		return ErrConfiguration
	}
	return ValidateHeaders(p.Headers)
}

// ValidateHeaders validates non-sensitive, literal model request metadata.
// Errors deliberately omit names and values supplied by callers.
func ValidateHeaders(headers map[string]string) error {
	if len(headers) > 32 {
		return ErrConfiguration
	}
	seen := make(map[string]bool, len(headers))
	total := 0
	for name, value := range headers {
		lower := strings.ToLower(name)
		if len(name) == 0 || len(name) > 128 || seen[lower] || len(value) > 4096 || strings.TrimSpace(value) != value {
			return ErrConfiguration
		}
		seen[lower] = true
		switch lower {
		case "anthropic-version", "anthropic-beta", "openai-beta", "x-goog-api-key", "proxy-connection", "authorization", "proxy-authorization", "x-api-key", "api-key", "host", "connection", "keep-alive", "proxy-authenticate", "te", "trailer", "transfer-encoding", "upgrade", "content-length", "content-type", "accept", "accept-encoding", "cookie", "set-cookie", "user-agent":
			return ErrConfiguration
		}
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
				return ErrConfiguration
			}
		}
		for _, c := range value {
			if c < 0x20 || c > 0x7e {
				return ErrConfiguration
			}
		}
		total += len(name) + len(value)
		if total > 16384 {
			return ErrConfiguration
		}
	}
	return nil
}

// Headers decodes only string-valued objects, without silently converting null
// entries to empty strings or overwriting repeated field names.
type Headers map[string]string

func (h *Headers) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*h = nil
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrConfiguration
	}
	result := Headers{}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return ErrConfiguration
		}
		name, ok := token.(string)
		if !ok || seen[strings.ToLower(name)] {
			return ErrConfiguration
		}
		seen[strings.ToLower(name)] = true
		token, err = decoder.Token()
		if err != nil {
			return ErrConfiguration
		}
		value, ok := token.(string)
		if !ok {
			return ErrConfiguration
		}
		result[name] = value
	}
	if _, err = decoder.Token(); err != nil {
		return ErrConfiguration
	}
	if err = ValidateHeaders(result); err != nil {
		return err
	}
	*h = result
	return nil
}

// TokenLimitsValid holds the token-limit rule: unset is zero, and an output
// limit never exceeds a set context window.
func TokenLimitsValid(contextWindow, maxOutputTokens int32) bool {
	return contextWindow >= 0 && maxOutputTokens >= 0 && (contextWindow == 0 || maxOutputTokens <= contextWindow)
}
