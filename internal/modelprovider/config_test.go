package modelprovider

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseProviderValidatesFrozenBundle(t *testing.T) {
	valid := map[string]any{"protocol": "responses", "base_url": "https://model.example/v1", "api_key": "fixture-upstream-key", "context_window": int32(64000), "max_output_tokens": int32(4096)}
	for _, protocol := range []Protocol{Anthropic, Responses, ChatCompletions} {
		t.Run(string(protocol), func(t *testing.T) {
			raw := copyBundle(valid)
			raw["protocol"] = string(protocol)
			got, err := ParseProvider(raw)
			want := Provider{Protocol: protocol, BaseURL: "https://model.example/v1", APIKey: "fixture-upstream-key", ContextWindow: 64000, MaxOutputTokens: 4096}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("bundle changed or rejected: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"unknown field", "native_options", map[string]any{}}, {"alias", "protocol", "openai"},
		{"missing key", "api_key", ""}, {"newline key", "api_key", "fixture\nkey"},
		{"URL credentials", "base_url", "https://user:secret@model.example/v1"},
		{"remote HTTP", "base_url", "http://model.example/v1"},
		{"query", "base_url", "https://model.example/v1?key=secret"},
		{"fragment", "base_url", "https://model.example/v1#secret"},
		{"negative limit", "context_window", -1}, {"excess output", "max_output_tokens", 64001},
		{"fractional limit", "context_window", 1.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := copyBundle(valid)
			raw[tc.field] = tc.value
			if _, err := ParseProvider(raw); !errors.Is(err, ErrConfiguration) {
				t.Fatalf("invalid bundle accepted: %v", err)
			}
		})
	}
	for _, raw := range []any{nil, []any{}, "provider", map[string]any{}} {
		if _, err := ParseProvider(raw); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("invalid root accepted: %v", err)
		}
	}
}

func copyBundle(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	return out
}

func TestHeadersValidation(t *testing.T) {
	for _, headers := range []map[string]string{nil, {}, {"X-Sandbase-Benefit": "daily-free", "X-Empty": "", "X-Literal": "${literal}:a"}} {
		if err := ValidateHeaders(headers); err != nil {
			t.Fatal(err)
		}
	}
	invalid := []map[string]string{{"X-A": "a", "x-a": "b"}, {"X-A": "\r\nInjected: true"}, {"X-A": " spaced"}, {"X-A": "tail "}, {"X-A": "非ASCII"}, {"Bad:Name": "x"}, {"": "x"}, {strings.Repeat("X", 129): "x"}, {"X-A": strings.Repeat("a", 4097)}}
	for _, name := range []string{"Authorization", "X-API-Key", "X-Goog-Api-Key", "Proxy-Connection", "Host", "Cookie", "Content-Type", "Accept", "Transfer-Encoding", "User-Agent"} {
		invalid = append(invalid, map[string]string{name: "private-canary"})
	}
	many := map[string]string{}
	for i := 0; i < 33; i++ {
		many["X-"+strings.Repeat("a", i)] = "x"
	}
	invalid = append(invalid, many)
	large := map[string]string{}
	for i := 0; i < 5; i++ {
		large["X-"+strings.Repeat("a", i)] = strings.Repeat("a", 4096)
	}
	invalid = append(invalid, large)
	for _, headers := range invalid {
		if err := ValidateHeaders(headers); err != ErrConfiguration {
			t.Fatal("invalid headers accepted or disclosed")
		}
	}
}

func TestHeadersJSONRejectsLossyInput(t *testing.T) {
	for _, raw := range []string{`{"X-A":null}`, `{"X-A":3}`, `{"X-A":[]}`, `{"X-A":"a","X-A":"b"}`, `{"X-A":"a","x-a":"b"}`, `[]`} {
		var headers Headers
		if json.Unmarshal([]byte(raw), &headers) == nil {
			t.Fatal("lossy headers JSON accepted")
		}
	}
	var headers Headers
	if json.Unmarshal([]byte(`{"X-A":""}`), &headers) != nil || headers["X-A"] != "" {
		t.Fatal("explicit empty string must remain valid")
	}
}
