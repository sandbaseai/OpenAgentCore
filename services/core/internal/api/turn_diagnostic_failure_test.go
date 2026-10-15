package api

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func TestDiagnosticFailureWhitelist(t *testing.T) {
	cases := map[string][]string{
		"harness_error": {"engine_failed"}, "model_provider_required": {"model_provider_required"}, "runtime_unavailable": {"execution_device_unavailable", "execution_unavailable"}, "runtime_disconnected": {"device_disconnected", "event_stream_incomplete"}, "runtime_preparation_failed": {"preparation_start_failed", "preparation_interrupted"}, "execution_interrupted": {"execution_interrupted"},
		"delivery_unconfirmed":    {"delivery_unknown", "input_outcome_unknown", "cancel_unconfirmed", "cancel_outcome_unavailable", "function_result_unconfirmed"},
		"input_rejected":          {"invalid_input", "input_not_applied", "message_input_unsupported", "input_invalid_input", "input_run_inactive", "input_input_conflict", "input_input_limit", "input_unsupported", "input_rejected", "input_not_ready", "input_busy"},
		"executor_protocol_error": {"invalid_executor_result", "execution_state_unavailable", "execution_state_changed", "function_call_invalid", "function_result_invalid"}, "core_storage_failed": {"event_persistence_failed", "artifact_capture_failed"}, "internal_error": {"secret-canary", "input_secret-canary", "execution_state_secret-canary", ""},
	}
	catalog, err := os.ReadFile("../../../../contracts/agents-api/core-errors.md")
	if err != nil {
		t.Fatal(err)
	}
	produced := map[string]bool{}
	for want, inputs := range cases {
		if !strings.Contains(string(catalog), "`"+want+"`") {
			t.Fatal("uncatalogued diagnostics code", want)
		}
		for _, input := range inputs {
			raw, _ := json.Marshal(map[string]string{"error_code": input, "error": "raw-secret-canary"})
			got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: raw})
			produced[got.Code] = true
			encoded, _ := json.Marshal(got)
			if got.Code != want || strings.Contains(string(encoded), "canary") {
				t.Fatal(input, got)
			}
		}
	}
	// Runtime classifications share the observation fixture; Core mappings above
	// and Environment failures below exercise the actual diagnostic producers.
	raw, err := os.ReadFile("../modelconfiguration/testdata/observation_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var observations []struct {
		EngineErrorCode string `json:"engine_error_code"`
	}
	if err := json.Unmarshal(raw, &observations); err != nil {
		t.Fatal(err)
	}
	for _, observation := range observations {
		// Upstream 400 codes are only meaningful with their status and fingerprint.
		status := 400
		if code, _ := proto.NormalizeEngineFailure(observation.EngineErrorCode, &status); code != "" {
			outcome, _ := json.Marshal(map[string]any{"error_code": "engine_failed", "engine_error_code": code, "engine_http_status": status, "engine_fingerprint": fingerprintFor(code)})
			produced[turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: outcome}).Code] = true
		}
	}
	for _, failure := range []string{"", "environment_unavailable", "runtime_preparation_failed", "model_provider_required", "unknown", "provisioning"} {
		session := hostedFailureSession()
		if failure != "provisioning" {
			session.EnvironmentFailure = nil
			session.EnvironmentInputActivity = &sessions.EnvironmentInputActivity{Status: "failed", Failure: failure}
		} else {
			session.EnvironmentFailure = &sessions.EnvironmentFailure{}
		}
		h, _, _ := adminTestHandler(t, serveDiagnostics(diagnosticSnapshotStore{session: session}))
		w := diagnosticRequest(h, adminSessionsPath+session.ID+"/diagnostics", "Bearer admin")
		var response SessionDiagnostics
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Failure == nil {
			t.Fatal(w.Code, w.Body)
		}
		produced[response.Failure.Code] = true
	}
	field, _ := reflect.TypeFor[DiagnosticFailure]().FieldByName("Code")
	declared := strings.Split(field.Tag.Get("enums"), ",")
	for code := range produced {
		if !slices.Contains(declared, code) {
			t.Errorf("diagnostic code %q missing from schema", code)
		}
	}
	for _, code := range declared {
		if !produced[code] {
			t.Errorf("schema diagnostic code %q has no exercised producer", code)
		}
	}
	if got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: json.RawMessage(`{"error_code":"engine_failed",`)}); got.Code != "internal_error" {
		t.Fatal("malformed outcome accepted", got)
	}
	for _, status := range []string{sessions.TurnQueued, sessions.TurnInProgress, sessions.TurnWaiting, sessions.TurnCompleted, sessions.TurnCancelled} {
		if got := turnDiagnosticFailure(sessions.Turn{Status: status, Outcome: json.RawMessage(`{"error_code":"engine_failed"}`)}); got != nil {
			t.Fatal("nonfailure classified", status, got)
		}
	}
}

func TestDiagnosticNativeClassification(t *testing.T) {
	codes := []string{"authentication_error", "rate_limit_exceeded", "usage_limit_exceeded", "server_overloaded", "server_error", "invalid_request", "resource_not_found", "request_timeout", "context_length_exceeded", "cyber_policy", "connection_failed"}
	catalog, err := os.ReadFile("../../../../contracts/agents-api/core-errors.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			if !strings.Contains(string(catalog), "`"+code+"`") {
				t.Fatal("uncatalogued code", code)
			}
			for _, status := range []any{nil, 100, 429, 599, 99, 600, 503.5, "503", true, map[string]any{"token": "secret-canary"}} {
				raw, _ := json.Marshal(map[string]any{"error_code": "engine_failed", "error": "secret-canary", "engine_error_code": code, "engine_http_status": status})
				got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: raw})
				encoded, _ := json.Marshal(got)
				if got.Code != code || strings.Contains(string(encoded), "canary") {
					t.Fatal(string(encoded))
				}
				params, _ := json.Marshal(got.Params)
				want := `{}`
				if code == "connection_failed" {
					want = `{"http_status":null}`
					if n, ok := status.(int); ok && n >= 100 && n <= 599 {
						b, _ := json.Marshal(map[string]int{"http_status": n})
						want = string(b)
					}
				}
				if string(params) != want {
					t.Fatalf("status=%v params=%s want=%s", status, params, want)
				}
			}
		})
	}
	for _, optional := range []string{``, `,"engine_error_code":null`, `,"engine_error_code":17`, `,"engine_error_code":{"code":"authentication_error"}`, `,"engine_error_code":"secret-canary"`, `,"done":{"engine_error_code":"authentication_error"}`, `,"Engine_Error_Code":"authentication_error"`} {
		got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: json.RawMessage(`{"error_code":"engine_failed"` + optional + `}`)})
		if got.Code != "harness_error" || len(got.Params) != 0 {
			t.Fatal(optional, got)
		}
	}
	for core, want := range map[string]string{"event_persistence_failed": "core_storage_failed", "artifact_capture_failed": "core_storage_failed", "event_stream_incomplete": "runtime_disconnected", "cancel_unconfirmed": "delivery_unconfirmed", "invalid_executor_result": "executor_protocol_error", "secret-canary": "internal_error"} {
		raw, _ := json.Marshal(map[string]any{"error_code": core, "engine_error_code": "connection_failed", "engine_http_status": 503})
		got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: raw})
		if got.Code != want || len(got.Params) != 0 {
			t.Fatal(got)
		}
	}
	for _, status := range []string{sessions.TurnQueued, sessions.TurnInProgress, sessions.TurnWaiting, sessions.TurnCompleted, sessions.TurnCancelled} {
		if got := turnDiagnosticFailure(sessions.Turn{Status: status, Outcome: json.RawMessage(`{"error_code":"engine_failed","engine_error_code":"authentication_error"}`)}); got != nil {
			t.Fatal(status, got)
		}
	}
}

// fingerprintFor returns the fingerprint paired with an upstream 400 code, or
// an empty string for every other code.
func fingerprintFor(code string) string {
	for _, fingerprint := range []string{proto.FingerprintProviderMessagesRequired, proto.FingerprintReasoningEncryptedUnverifiable, proto.FingerprintModelNoImageInput, proto.FingerprintToolResultInputImageRejected, proto.FingerprintTrailingModelTurn} {
		if paired, _ := proto.FingerprintCode(fingerprint); paired == code {
			return fingerprint
		}
	}
	return ""
}

// These four literals are the cross-repository contract with the Agents service.
var upstream400DiagnosticCodes = []string{"model_input_unsupported_image", "model_provider_format_rejected", "model_reasoning_state_invalid", "model_input_ends_with_assistant"}

func TestUpstream400CodesAreInEveryEnumAndMatchTheProtoMapping(t *testing.T) {
	field, _ := reflect.TypeFor[DiagnosticFailure]().FieldByName("Code")
	publicField, _ := reflect.TypeFor[v1.ExecutionDiagnostic]().FieldByName("Code")
	core, public := strings.Split(field.Tag.Get("enums"), ","), strings.Split(publicField.Tag.Get("enums"), ",")
	catalog, err := os.ReadFile("../../../../contracts/agents-api/core-errors.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range upstream400DiagnosticCodes {
		if !slices.Contains(core, code) || !slices.Contains(public, code) || !strings.Contains(string(catalog), "`"+code+"`") {
			t.Fatalf("%s missing from an enum or the catalog", code)
		}
	}
	// The mapping must produce exactly the contract literals, no more.
	produced := map[string]bool{}
	for _, fingerprint := range []string{proto.FingerprintProviderMessagesRequired, proto.FingerprintReasoningEncryptedUnverifiable, proto.FingerprintModelNoImageInput, proto.FingerprintToolResultInputImageRejected, proto.FingerprintTrailingModelTurn} {
		code, ok := proto.FingerprintCode(fingerprint)
		if !ok {
			t.Fatal(fingerprint)
		}
		produced[code] = true
	}
	if len(produced) != len(upstream400DiagnosticCodes) {
		t.Fatal("mapping produces a different code set", produced)
	}
	for _, code := range upstream400DiagnosticCodes {
		if !produced[code] {
			t.Fatal("contract code has no fingerprint", code)
		}
	}
}

func TestUpstream400DiagnosticParamsAreFlatBoundedAndFreeOfNativeText(t *testing.T) {
	const id = "88a8d808-59c1-4bf3-8950-b34ce57e028b"
	for _, fingerprint := range []string{proto.FingerprintProviderMessagesRequired, proto.FingerprintReasoningEncryptedUnverifiable, proto.FingerprintModelNoImageInput, proto.FingerprintToolResultInputImageRejected, proto.FingerprintTrailingModelTurn} {
		code, _ := proto.FingerprintCode(fingerprint)
		raw, _ := json.Marshal(map[string]any{"error_code": "engine_failed", "error": "raw-secret-canary", "engine_error_code": code, "engine_http_status": 400, "engine_fingerprint": fingerprint, "engine_request_id": id})
		got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: raw})
		if got.Code != code {
			t.Fatalf("%s: %+v", fingerprint, got)
		}
		if validated := validCoreDetails(got.Params); len(validated) != len(got.Params) {
			t.Fatal("params rejected by the shared detail validator", got.Params)
		}
		encoded, _ := json.Marshal(got.Params)
		var params map[string]any
		if json.Unmarshal(encoded, &params) != nil {
			t.Fatal(string(encoded))
		}
		if len(params) != 3 || params["fingerprint"] != fingerprint || params["upstream_request_id"] != id {
			t.Fatalf("unexpected keys: %s", encoded)
		}
		summary, _ := params["summary"].(string)
		if summary == "" || len(summary) > 240 || strings.Contains(string(encoded), "canary") {
			t.Fatalf("summary: %s", encoded)
		}
		// The request ID key is omitted when no trusted ID exists.
		raw, _ = json.Marshal(map[string]any{"error_code": "engine_failed", "engine_error_code": code, "engine_http_status": 400, "engine_fingerprint": fingerprint, "engine_request_id": "Bearer secret"})
		if got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: raw}); len(got.Params) != 2 {
			t.Fatalf("%s: %v", fingerprint, got.Params)
		}
	}
	// Anything that does not hold the full pairing falls back without params.
	for _, outcome := range []string{
		`{"error_code":"engine_failed","engine_error_code":"model_input_unsupported_image","engine_fingerprint":"model_no_image_input"}`,
		`{"error_code":"engine_failed","engine_error_code":"model_input_unsupported_image","engine_http_status":400,"engine_fingerprint":"trailing_model_turn"}`,
		`{"error_code":"engine_failed","engine_error_code":"authentication_error","engine_http_status":400,"engine_fingerprint":"model_no_image_input"}`,
		`{"error_code":"event_persistence_failed","engine_error_code":"model_input_unsupported_image","engine_http_status":400,"engine_fingerprint":"model_no_image_input"}`,
	} {
		got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: json.RawMessage(outcome)})
		if len(got.Params) != 0 {
			t.Fatalf("%s: %+v", outcome, got)
		}
	}
	// A paired code without a stored fingerprint is still reported, with no params.
	if got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: json.RawMessage(`{"error_code":"engine_failed","engine_error_code":"model_input_unsupported_image","engine_http_status":400}`)}); got.Code != "model_input_unsupported_image" || len(got.Params) != 0 {
		t.Fatal(got)
	}
	// Existing categories keep exactly their previous params.
	status := 400
	raw, _ := json.Marshal(map[string]any{"error_code": "engine_failed", "engine_error_code": "connection_failed", "engine_http_status": status, "engine_fingerprint": "model_no_image_input"})
	if got := turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: raw}); got.Code != "connection_failed" || len(got.Params) != 1 {
		t.Fatal("connection_failed params changed", got)
	}
}

func TestFailedTurnDiagnosticsShowOnlyUnsuccessfulRecoveries(t *testing.T) {
	const old, next = "019e0000-0000-7000-8000-000000000001", "019e0000-0000-7000-8000-000000000002"
	entry := func(fingerprint, outcome string, attempt int, reason string, reasoning, images int) map[string]any {
		e := map[string]any{"fingerprint": fingerprint, "action": "rebuild_thread", "attempt": attempt, "outcome": outcome,
			"dropped": map[string]any{"reasoning_items": reasoning, "images": images}, "old_native_session_id": old}
		if attempt > 0 {
			e["new_native_session_id"] = next
		}
		if reason != "" {
			e["reason"] = reason
		}
		return e
	}
	outcome := func(code, fingerprint string, recoveries any) json.RawMessage {
		// Consecutive records must chain: each old ID is the previous record's new ID.
		if list, ok := recoveries.([]any); ok {
			prev := ""
			for i, item := range list {
				e := item.(map[string]any)
				if i > 0 && prev != "" {
					e["old_native_session_id"] = prev
					if _, has := e["new_native_session_id"]; has {
						e["new_native_session_id"] = fmt.Sprintf("019e0000-0000-7000-8000-%012d", i+2)
					}
				}
				if v, has := e["new_native_session_id"].(string); has {
					prev = v
				}
			}
		}
		o := map[string]any{"error_code": "engine_failed", "error": "raw-secret-canary", "engine_error_code": code, "engine_http_status": 400, "engine_fingerprint": fingerprint}
		if recoveries != nil {
			o["recoveries"] = recoveries
		}
		raw, _ := json.Marshal(o)
		return raw
	}
	params := func(t *testing.T, turn sessions.Turn) map[string]any {
		t.Helper()
		got := turnDiagnosticFailure(turn)
		if validated := validCoreDetails(got.Params); len(validated) != len(got.Params) {
			t.Fatal("params rejected by the shared detail validator", got.Params)
		}
		encoded, _ := json.Marshal(got.Params)
		if strings.Contains(string(encoded), "canary") || strings.Contains(string(encoded), old) || strings.Contains(string(encoded), next) {
			t.Fatalf("native text or Session ID in params: %s", encoded)
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	failed := func(raw json.RawMessage) sessions.Turn {
		return sessions.Turn{Status: sessions.TurnFailed, Outcome: raw}
	}

	exhausted := params(t, failed(outcome(proto.EngineCodeReasoningStateInvalid, proto.FingerprintReasoningEncryptedUnverifiable,
		[]any{entry(proto.FingerprintReasoningEncryptedUnverifiable, "exhausted", 1, "", 6, 2)})))
	want := map[string]any{"fingerprint": proto.FingerprintReasoningEncryptedUnverifiable, "recovery_count": float64(1), "recovery_fingerprints": []any{proto.FingerprintReasoningEncryptedUnverifiable},
		"recovery_outcomes": []any{"exhausted"}, "recovery_dropped_reasoning_items": float64(6), "recovery_dropped_images": float64(2)}
	for key, value := range want {
		if !reflect.DeepEqual(exhausted[key], value) {
			t.Fatalf("%s = %v, want %v", key, exhausted[key], value)
		}
	}
	if len(exhausted) != len(want)+1 { // plus summary
		t.Fatalf("unexpected keys: %v", exhausted)
	}

	// An abandoned attempt is not a switch: count 0, outcome aborted, never the reason or IDs.
	aborted := params(t, failed(outcome(proto.EngineCodeInputUnsupportedImage, proto.FingerprintModelNoImageInput,
		[]any{entry(proto.FingerprintModelNoImageInput, "aborted", 0, "current_input_has_images", 0, 0)})))
	if aborted["recovery_count"] != float64(0) || !reflect.DeepEqual(aborted["recovery_outcomes"], []any{"aborted"}) || aborted["recovery_dropped_images"] != float64(0) {
		t.Fatalf("%v", aborted)
	}

	// Two records keep their order; counts add up.
	two := params(t, failed(outcome(proto.EngineCodeReasoningStateInvalid, proto.FingerprintReasoningEncryptedUnverifiable, []any{
		entry(proto.FingerprintModelNoImageInput, "exhausted", 1, "", 0, 3), entry(proto.FingerprintReasoningEncryptedUnverifiable, "exhausted", 2, "", 5, 0)})))
	if two["recovery_count"] != float64(2) || !reflect.DeepEqual(two["recovery_fingerprints"], []any{proto.FingerprintModelNoImageInput, proto.FingerprintReasoningEncryptedUnverifiable}) ||
		two["recovery_dropped_reasoning_items"] != float64(5) || two["recovery_dropped_images"] != float64(3) {
		t.Fatalf("%v", two)
	}

	// A record that recovered is not listed; it still counts as a switch.
	mixed := params(t, failed(outcome(proto.EngineCodeReasoningStateInvalid, proto.FingerprintReasoningEncryptedUnverifiable, []any{
		entry(proto.FingerprintModelNoImageInput, "exhausted", 1, "", 0, 1), entry(proto.FingerprintReasoningEncryptedUnverifiable, "recovered", 2, "", 2, 0)})))
	if mixed["recovery_count"] != float64(2) || !reflect.DeepEqual(mixed["recovery_outcomes"], []any{"exhausted"}) {
		t.Fatalf("%v", mixed)
	}
	onlyRecovered := params(t, failed(outcome(proto.EngineCodeReasoningStateInvalid, proto.FingerprintReasoningEncryptedUnverifiable,
		[]any{entry(proto.FingerprintReasoningEncryptedUnverifiable, "recovered", 1, "", 1, 0)})))
	if _, present := onlyRecovered["recovery_count"]; present {
		t.Fatalf("a recovered record produced recovery keys: %v", onlyRecovered)
	}

	// Malformed records, no records, or another failure category add no recovery keys.
	for name, raw := range map[string]json.RawMessage{
		"none":       outcome(proto.EngineCodeReasoningStateInvalid, proto.FingerprintReasoningEncryptedUnverifiable, nil),
		"unknown":    outcome(proto.EngineCodeReasoningStateInvalid, proto.FingerprintReasoningEncryptedUnverifiable, []any{map[string]any{"fingerprint": "x"}}),
		"too many":   outcome(proto.EngineCodeReasoningStateInvalid, proto.FingerprintReasoningEncryptedUnverifiable, []any{entry(proto.FingerprintModelNoImageInput, "aborted", 0, "cancelled", 0, 0), entry(proto.FingerprintModelNoImageInput, "aborted", 0, "cancelled", 0, 0), entry(proto.FingerprintModelNoImageInput, "aborted", 0, "cancelled", 0, 0)}),
		"not a list": outcome(proto.EngineCodeReasoningStateInvalid, proto.FingerprintReasoningEncryptedUnverifiable, "secret-canary"),
	} {
		for key := range params(t, failed(raw)) {
			if strings.HasPrefix(key, "recovery_") {
				t.Fatalf("%s: %s present", name, key)
			}
		}
	}
	other, _ := json.Marshal(map[string]any{"error_code": "engine_failed", "engine_error_code": "authentication_error",
		"recoveries": []any{entry(proto.FingerprintModelNoImageInput, "exhausted", 1, "", 0, 1)}})
	if got := turnDiagnosticFailure(failed(other)); got.Code != "authentication_error" || len(got.Params) != 0 {
		t.Fatalf("recovery keys leaked onto another category: %+v", got)
	}

	// A Turn that did not fail has no diagnostic at all, whatever it recovered from.
	for _, status := range []string{sessions.TurnCompleted, sessions.TurnCancelled, sessions.TurnInProgress} {
		raw := outcome(proto.EngineCodeReasoningStateInvalid, proto.FingerprintReasoningEncryptedUnverifiable, []any{entry(proto.FingerprintReasoningEncryptedUnverifiable, "recovered", 1, "", 1, 0)})
		if got := turnDiagnosticFailure(sessions.Turn{Status: status, Outcome: raw}); got != nil {
			t.Fatal(status, got)
		}
	}
}
