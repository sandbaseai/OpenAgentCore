package proto

import (
	"encoding/json"
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
