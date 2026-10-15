package sessions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

var upstreamFingerprints = []string{proto.FingerprintProviderMessagesRequired, proto.FingerprintReasoningEncryptedUnverifiable, proto.FingerprintModelNoImageInput, proto.FingerprintToolResultInputImageRejected, proto.FingerprintTrailingModelTurn}

func TestEngineFailureSummaryTemplatesAreBoundedAndFixed(t *testing.T) {
	seen := map[string]bool{}
	for _, fingerprint := range upstreamFingerprints {
		summary := EngineFailureSummary(fingerprint, nil)
		if summary == "" || len(summary) > 240 || seen[summary] {
			t.Fatalf("%s: %q", fingerprint, summary)
		}
		seen[summary] = true
		if strings.ContainsAny(summary, "\r\n") {
			t.Fatal("multi-line summary", fingerprint)
		}
	}
	status := 400
	if summary := EngineFailureSummary("", &status); summary != "upstream returned HTTP 400 (unclassified)" {
		t.Fatal(summary)
	}
	other := 429
	for _, tc := range []struct {
		fingerprint string
		status      *int
	}{{"", nil}, {"", &other}, {"unknown_fingerprint", nil}, {"unknown_fingerprint", &other}} {
		if summary := EngineFailureSummary(tc.fingerprint, tc.status); summary != "" {
			t.Fatalf("%+v: %q", tc, summary)
		}
	}
}

func outcome(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDiagnosticUpstreamFailureRevalidatesPairing(t *testing.T) {
	const id = "88a8d808-59c1-4bf3-8950-b34ce57e028b"
	for _, fingerprint := range upstreamFingerprints {
		code, _ := proto.FingerprintCode(fingerprint)
		good := map[string]any{"error_code": "engine_failed", "error": "native-canary", "engine_error_code": code, "engine_http_status": 400, "engine_fingerprint": fingerprint, "engine_request_id": id}
		got := DiagnosticUpstreamFailure(outcome(t, good))
		if got.Fingerprint != fingerprint || got.RequestID != id || got.Summary != EngineFailureSummary(fingerprint, intPointer(400)) {
			t.Fatalf("%s: %+v", fingerprint, got)
		}
		if strings.Contains(got.Summary+got.Fingerprint+got.RequestID, "canary") {
			t.Fatal("native text leaked")
		}
		if classified, status := DiagnosticFailureCode(outcome(t, good)); classified != code || status == nil || *status != 400 {
			t.Fatalf("%s classified as %s", fingerprint, classified)
		}
		for name, mutate := range map[string]func(map[string]any){
			"missing status":    func(m map[string]any) { delete(m, "engine_http_status") },
			"wrong status":      func(m map[string]any) { m["engine_http_status"] = 429 },
			"missing print":     func(m map[string]any) { delete(m, "engine_fingerprint") },
			"unknown print":     func(m map[string]any) { m["engine_fingerprint"] = "other_print" },
			"mismatched print":  func(m map[string]any) { m["engine_fingerprint"] = fingerprintWithOtherCode(code) },
			"not engine_failed": func(m map[string]any) { m["error_code"] = "event_persistence_failed" },
			"other engine code": func(m map[string]any) { m["engine_error_code"] = "authentication_error" },
		} {
			bad := map[string]any{}
			for key, value := range good {
				bad[key] = value
			}
			mutate(bad)
			if got := DiagnosticUpstreamFailure(outcome(t, bad)); got != (UpstreamFailureDetail{}) {
				t.Fatalf("%s/%s accepted: %+v", fingerprint, name, got)
			}
		}
		// A request ID that is not a UUID is dropped while the pairing stays valid.
		good["engine_request_id"] = "Bearer secret"
		if got := DiagnosticUpstreamFailure(outcome(t, good)); got.Fingerprint != fingerprint || got.RequestID != "" {
			t.Fatalf("%s: %+v", fingerprint, got)
		}
	}
	if got := DiagnosticUpstreamFailure(json.RawMessage(`{"error_code":"engine_failed",`)); got != (UpstreamFailureDetail{}) {
		t.Fatal("malformed outcome accepted", got)
	}
}

// fingerprintWithOtherCode returns a fingerprint paired with a different code.
func fingerprintWithOtherCode(code string) string {
	for _, candidate := range upstreamFingerprints {
		if paired, _ := proto.FingerprintCode(candidate); paired != code {
			return candidate
		}
	}
	return ""
}

func intPointer(value int) *int { return &value }
