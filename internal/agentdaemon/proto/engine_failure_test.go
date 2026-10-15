package proto

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

func TestErrorClassificationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		raw, code string
		status    int
	}{
		{`{"error":"old"}`, "", 0},
		{`{"error":"old","code":"authentication_error","http_status":401}`, "authentication_error", 0},
		{`{"error":"old","code":"connection_failed","http_status":503}`, "connection_failed", 503},
		{`{"error":"old","code":"connection_failed","http_status":"secret"}`, "connection_failed", 0},
		{`{"error":"old","code":"connection_failed","http_status":99}`, "connection_failed", 0},
		{`{"error":"old","code":"connection_failed","http_status":600}`, "connection_failed", 0},
		{`{"error":"old","code":"future","http_status":503}`, "", 0},
		{`{"error":"old","code":{"secret":"value"},"http_status":503}`, "", 0},
		{`{"error":"old","code":null,"http_status":null}`, "", 0},
	} {
		var p ErrorPayload
		if err := json.Unmarshal([]byte(tc.raw), &p); err != nil {
			t.Fatal(err)
		}
		status := 0
		if p.HTTPStatus != nil {
			status = *p.HTTPStatus
		}
		if p.Error != "old" || p.Code != tc.code || status != tc.status {
			t.Fatalf("%s: %+v", tc.raw, p)
		}
		raw, _ := json.Marshal(p)
		var old struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &old) != nil || old.Error != "old" {
			t.Fatal("old peer compatibility", string(raw))
		}
	}
}

func TestProviderBillingClassificationAndCorrelation(t *testing.T) {
	for _, code := range []string{"connection_failed", "provider_billing_unavailable"} {
		raw := `{"error":"private-body","code":"` + code + `","http_status":402,"upstream_request_id":"88a8d808-59c1-4bf3-8950-b34ce57e028b"}`
		var p ErrorPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if p.Code != "provider_billing_unavailable" || p.HTTPStatus == nil || *p.HTTPStatus != 402 || p.UpstreamRequestID == "" {
			t.Fatal(p)
		}
	}
	for _, id := range []string{`"Bearer secret"`, `123`, `{"token":"secret"}`, `null`} {
		var p ErrorPayload
		if err := json.Unmarshal([]byte(`{"error":"unchanged","upstream_request_id":`+id+`}`), &p); err != nil || p.UpstreamRequestID != "" || p.Error != "unchanged" {
			t.Fatal(err, p)
		}
	}
}

// The four upstream 400 codes are the cross-repository contract literals.
var upstream400Codes = []string{"model_input_unsupported_image", "model_provider_format_rejected", "model_reasoning_state_invalid", "model_input_ends_with_assistant"}

func TestUpstream400CodeAndFingerprintSetsAreExactlyTheContract(t *testing.T) {
	want := slices.Clone(upstream400Codes)
	slices.Sort(want)
	var fromNormalize []string
	status := 400
	for _, code := range upstream400Codes {
		if got, kept := NormalizeEngineFailure(code, &status); got != code || kept == nil || *kept != 400 {
			t.Fatal("contract code not accepted with status 400:", code)
		}
		fromNormalize = append(fromNormalize, code)
	}
	var fromFingerprints []string
	for _, code := range fingerprintCode {
		if !slices.Contains(fromFingerprints, code) {
			fromFingerprints = append(fromFingerprints, code)
		}
	}
	slices.Sort(fromFingerprints)
	slices.Sort(fromNormalize)
	if !slices.Equal(fromFingerprints, want) || !slices.Equal(fromNormalize, want) {
		t.Fatalf("code sets differ from the contract: %v %v want %v", fromFingerprints, fromNormalize, want)
	}
	fingerprints := slices.Sorted(maps.Keys(fingerprintCode))
	wantFingerprints := []string{"model_no_image_input", "provider_messages_required", "reasoning_encrypted_unverifiable", "tool_result_input_image_rejected", "trailing_model_turn"}
	if !slices.Equal(fingerprints, wantFingerprints) {
		t.Fatal("fingerprint set changed:", fingerprints)
	}
	// An unlisted code with status 400 stays unaccepted.
	if code, _ := NormalizeEngineFailure("model_future_code", &status); code != "" {
		t.Fatal("unlisted code accepted", code)
	}
}

func TestUpstream400ClassificationRequiresStatusAndPairing(t *testing.T) {
	for fingerprint, code := range fingerprintCode {
		var p ErrorPayload
		raw := `{"error":"private","code":"` + code + `","http_status":400,"fingerprint":"` + fingerprint + `","upstream_request_id":"88a8d808-59c1-4bf3-8950-b34ce57e028b"}`
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Code != code || p.Fingerprint != fingerprint || p.HTTPStatus == nil || *p.HTTPStatus != 400 || p.UpstreamRequestID == "" || p.Error != "private" {
			t.Fatalf("%s: %v %+v", fingerprint, err, p)
		}
		// Normalizing an already decoded payload again changes nothing.
		again, fp := NormalizeEngineFingerprint(p.Code, p.Fingerprint)
		if again != p.Code || fp != p.Fingerprint {
			t.Fatal("not idempotent", fingerprint)
		}
		for _, invalid := range []string{
			`"http_status":null`, `"http_status":401`, `"http_status":"400"`, `"http_status":402`, ``,
		} {
			text := `{"error":"private","code":"` + code + `",` + invalid + `,"fingerprint":"` + fingerprint + `"}`
			if invalid == "" {
				text = `{"error":"private","code":"` + code + `","fingerprint":"` + fingerprint + `"}`
			}
			var q ErrorPayload
			if err := json.Unmarshal([]byte(text), &q); err != nil || q.Code != "" || q.Fingerprint != "" || q.HTTPStatus != nil || q.Error != "private" {
				t.Fatalf("unpaired status accepted: %s %+v", text, q)
			}
		}
	}
	for _, tc := range []struct{ name, raw string }{
		{"missing fingerprint", `{"error":"x","code":"model_input_unsupported_image","http_status":400}`},
		{"mismatched fingerprint", `{"error":"x","code":"model_input_unsupported_image","http_status":400,"fingerprint":"trailing_model_turn"}`},
		{"unknown fingerprint", `{"error":"x","code":"model_input_unsupported_image","http_status":400,"fingerprint":"model_future"}`},
		{"malformed fingerprint", `{"error":"x","code":"model_input_unsupported_image","http_status":400,"fingerprint":{"secret":"value"}}`},
		{"numeric fingerprint", `{"error":"x","code":"model_provider_format_rejected","http_status":400,"fingerprint":7}`},
	} {
		var p ErrorPayload
		if err := json.Unmarshal([]byte(tc.raw), &p); err != nil || p.Error != "x" || p.Code != "" || p.Fingerprint != "" || p.HTTPStatus != nil {
			t.Fatalf("%s: %v %+v", tc.name, err, p)
		}
	}
}

func TestFingerprintOnOtherCodesIsDroppedWithoutLosingTheCode(t *testing.T) {
	for _, raw := range []string{
		`{"error":"x","code":"authentication_error","fingerprint":"model_no_image_input"}`,
		`{"error":"x","fingerprint":"model_no_image_input"}`,
		`{"error":"x","code":"connection_failed","http_status":400,"fingerprint":"model_no_image_input"}`,
	} {
		var p ErrorPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Fingerprint != "" || p.Error != "x" {
			t.Fatalf("%s: %v %+v", raw, err, p)
		}
	}
	var p ErrorPayload
	if json.Unmarshal([]byte(`{"error":"x","code":"connection_failed","http_status":400,"fingerprint":"model_no_image_input"}`), &p) != nil || p.Code != "connection_failed" || p.HTTPStatus == nil || *p.HTTPStatus != 400 {
		t.Fatal("unrelated classification lost", p)
	}
}

func TestOldPeerWithoutFingerprintKeepsTerminalDecoding(t *testing.T) {
	var p ErrorPayload
	if err := json.Unmarshal([]byte(`{"error":"old","code":"rate_limit_exceeded"}`), &p); err != nil || p.Code != "rate_limit_exceeded" || p.Fingerprint != "" {
		t.Fatal(err, p)
	}
	// Marshaling omits an empty fingerprint, so older Cores see the previous wire shape.
	raw, _ := json.Marshal(ErrorPayload{Error: "e", Code: "rate_limit_exceeded"})
	if string(raw) != `{"error":"e","code":"rate_limit_exceeded"}` {
		t.Fatal(string(raw))
	}
}
