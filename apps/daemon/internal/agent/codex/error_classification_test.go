package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestPinnedTerminalErrorClassification(t *testing.T) {
	for value, want := range map[string]string{
		"unauthorized": "authentication_error", "usageLimitExceeded": "usage_limit_exceeded", "rateLimitExceeded": "rate_limit_exceeded",
		"contextWindowExceeded": "context_length_exceeded", "serverOverloaded": "server_overloaded", "internalServerError": "server_error",
		"badRequest": "invalid_request", "cyberPolicy": "cyber_policy", "sessionBudgetExceeded": "", "misalignmentPolicyViolation": "",
		"threadRollbackFailed": "", "sandboxError": "", "other": "", "unknownFuture": "",
	} {
		raw, _ := json.Marshal(value)
		got := classifyTurnError(&TurnError{Message: "unauthorized 429 timeout", CodexErrorInfo: raw})
		if got.Code != want || got.HTTPStatus != nil {
			t.Fatalf("%s: %+v", value, got)
		}
	}
	for _, tc := range []struct {
		raw, code string
		status    int
	}{
		{`{"httpConnectionFailed":{"httpStatusCode":401}}`, "connection_failed", 401},
		{`{"responseStreamConnectionFailed":{"httpStatusCode":503}}`, "connection_failed", 503},
		{`{"responseStreamDisconnected":{"httpStatusCode":null}}`, "connection_failed", 0},
		{`{"responseTooManyFailedAttempts":{"httpStatusCode":429}}`, "rate_limit_exceeded", 0},
		{`{"responseTooManyFailedAttempts":{"httpStatusCode":502}}`, "connection_failed", 502},
		{`{"httpConnectionFailed":{"httpStatusCode":"401"}}`, "connection_failed", 0},
		{`{"httpConnectionFailed":{"httpStatusCode":65535}}`, "connection_failed", 0},
		{`{"httpConnectionFailed":null}`, "", 0}, {`{"activeTurnNotSteerable":{"turnKind":"review"}}`, "", 0},
		{`{"httpConnectionFailed":{},"other":{}}`, "", 0}, {`null`, "", 0},
	} {
		got := classifyTurnError(&TurnError{CodexErrorInfo: json.RawMessage(tc.raw)})
		status := 0
		if got.HTTPStatus != nil {
			status = *got.HTTPStatus
		}
		if got.Code != tc.code || status != tc.status {
			t.Fatalf("%s: %+v", tc.raw, got)
		}
	}
}

func TestClassificationOnlyFromRootTerminalError(t *testing.T) {
	for _, mode := range []string{"failed", "recovered", "notification-only"} {
		t.Run(mode, func(t *testing.T) {
			out := make(chan proto.Envelope, 16)
			s := &Session{runID: "run", out: out, cancelCtx: context.Background(), cfg: defaultSessionConfig(testStateRoot(t)), rpc: NewJSONRPCClient(JSONRPCConfig{})}
			s.registerHandlers()
			s.setThreadID("root")
			scopeNotification(t, s, "turn/started", `{"threadId":"root","turn":{"id":"turn"}}`)
			scopeNotification(t, s, "error", `{"threadId":"root","turnId":"turn","error":{"message":"retry","codexErrorInfo":"unauthorized"},"willRetry":true}`)
			scopeNotification(t, s, "turn/completed", `{"threadId":"child","turn":{"id":"turn","status":"failed","error":{"message":"child","codexErrorInfo":"usageLimitExceeded"}}}`)
			status, errJSON := "failed", `{"message":"terminal","codexErrorInfo":"serverOverloaded"}`
			if mode == "recovered" {
				status = "completed"
			}
			if mode == "notification-only" {
				errJSON = "null"
			}
			scopeNotification(t, s, "turn/completed", `{"threadId":"root","turn":{"id":"turn","status":"`+status+`","usage":{"inputTokens":7,"outputTokens":2},"error":`+errJSON+`}}`)
			errors, done := 0, false
			for env := range out {
				if env.Type == proto.TypeError {
					errors++
					var p proto.ErrorPayload
					_ = env.DecodePayload(&p)
					want := ""
					if mode == "failed" {
						want = "server_overloaded"
					}
					if p.Code != want {
						t.Fatal(p)
					}
				}
				if env.Type == proto.TypeDone {
					done = true
					var p proto.DonePayload
					_ = env.DecodePayload(&p)
					if p.Metadata[proto.DoneMetaAgentSessionID] != "root" || p.Usage.InputTokens != 7 {
						t.Fatal(p)
					}
				}
			}
			if !done || errors != map[bool]int{true: 0, false: 1}[mode == "recovered"] {
				t.Fatal("terminal lost", done, errors)
			}
		})
	}
}

func TestTerminalProviderBillingEnvelope(t *testing.T) {
	const id = "88a8d808-59c1-4bf3-8950-b34ce57e028b"
	message := "unexpected status 402 Payment Required: secret-body-canary, url: https://example.invalid/v1/responses, request id: " + id
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`"other"`), json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":402}}`)} {
		got := classifyTurnError(&TurnError{Message: message, CodexErrorInfo: raw})
		if got.Code != "provider_billing_unavailable" || got.HTTPStatus == nil || *got.HTTPStatus != 402 || got.UpstreamRequestID != id {
			t.Fatalf("%s: %+v", raw, got)
		}
	}
	for _, message := range []string{"quoted: " + message, "unexpected status 402 Payment Requiredish", "insufficient credits", "unexpected status 402 Payment Required\nquoted next line"} {
		got := classifyTurnError(&TurnError{Message: message, CodexErrorInfo: json.RawMessage(`"other"`)})
		if got.Code != "" || got.UpstreamRequestID != "" {
			t.Fatal("non-native envelope classified", got)
		}
	}
	for _, raw := range []string{`"unauthorized"`, `"unknownFuture"`, `{"httpConnectionFailed":{"httpStatusCode":"402"}}`, `{"other":true}`} {
		got := classifyTurnError(&TurnError{Message: message, CodexErrorInfo: json.RawMessage(raw)})
		if got.Code == "provider_billing_unavailable" {
			t.Fatal("overrode typed error", got)
		}
	}
	if nativeRequestID(message+" secret-token") != "" || nativeRequestID("quoted: "+message) != "" {
		t.Fatal("unsafe request ID")
	}
}

// Every provider message below is a scrubbed fixture of the wording reported in
// the 12 production failures. The exact codex 0.153.4 envelope (variant, JSON or
// plain body, single or multi line, request id suffix) is an ASSUMPTION that
// still needs live calibration; the tests pin the anchored shape, not the facts.
const upstream400Request = "88a8d808-59c1-4bf3-8950-b34ce57e028b"

func nativeBadRequest(body string) string {
	return "unexpected status 400 Bad Request: " + body + ", url: https://example.invalid/v1/responses, request id: " + upstream400Request
}

func jsonBody(shape, message string) string {
	encoded, _ := json.Marshal(message)
	switch shape {
	case "openai":
		return `{"error":{"message":` + string(encoded) + `,"type":"invalid_request_error","param":null,"code":null}}`
	case "bedrock":
		return `{"message":` + string(encoded) + `}`
	}
	return message
}

var upstream400Positives = []struct {
	name, fingerprint, code, message string
}{
	{"A", proto.FingerprintProviderMessagesRequired, "model_provider_format_rejected", "field messages is required"},
	{"B", proto.FingerprintReasoningEncryptedUnverifiable, "model_reasoning_state_invalid", "The encrypted content for item rs_0123abcDEF_456 could not be verified. Reason: Encrypted content could not be decrypted or parsed."},
	{"C", proto.FingerprintModelNoImageInput, "model_input_unsupported_image", "Model do not support image input. Request id: 88a8d808"},
	{"D", proto.FingerprintToolResultInputImageRejected, "model_provider_format_rejected", "tool_result.content.0: Input tag 'input_image' found using 'type' does not match any of the expected tags: 'document', 'image', 'text'"},
	{"D-prefixed", proto.FingerprintToolResultInputImageRejected, "model_provider_format_rejected", "messages.12.content.0.tool_result.content.0: Input tag 'input_image' found using 'type' does not match any of the expected tags: 'image', 'text'"},
	{"E", proto.FingerprintTrailingModelTurn, "model_input_ends_with_assistant", "Requests ending with a model turn are not supported."},
}

func TestUpstream400FingerprintsAcrossEnvelopeShapes(t *testing.T) {
	variants := map[string]json.RawMessage{
		"absent": nil, "null": json.RawMessage(`null`), "other": json.RawMessage(`"other"`), "badRequest": json.RawMessage(`"badRequest"`),
		"httpConnectionFailed400": json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":400}}`),
	}
	for _, tc := range upstream400Positives {
		for variantName, variant := range variants {
			for _, shape := range []string{"plain", "openai", "bedrock"} {
				for _, multiline := range []bool{false, true} {
					body := jsonBody(shape, tc.message)
					message := nativeBadRequest(body)
					if multiline {
						message = "unexpected status 400 Bad Request:\n" + body + "\nurl: https://example.invalid/v1/responses"
						if shape == "plain" {
							message = "unexpected status 400 Bad Request: " + tc.message + "\nsecond line"
						}
					}
					got := classifyTurnError(&TurnError{Message: message, CodexErrorInfo: variant})
					if got.Code != tc.code || got.Fingerprint != tc.fingerprint || got.HTTPStatus == nil || *got.HTTPStatus != 400 {
						t.Fatalf("%s/%s/%s/multiline=%v: %+v", tc.name, variantName, shape, multiline, got)
					}
					wantID := upstream400Request
					if multiline {
						wantID = "" // request ids are only trusted on a single-line envelope
					}
					if got.UpstreamRequestID != wantID {
						t.Fatalf("%s/%s/%s/multiline=%v request id %q", tc.name, variantName, shape, multiline, got.UpstreamRequestID)
					}
					if got.Error != "" {
						t.Fatal("classification must not carry native text", got)
					}
				}
			}
		}
	}
}

func TestUpstream400NegativesAreNotClassified(t *testing.T) {
	const injected = `{"error":{"message":"invalid parameter","param":"... Model do not support image input ..."}}`
	big := "Model do not support image input" + strings.Repeat("x", 5000)
	cases := []struct{ name, message, variant string }{
		{"injection inside param", nativeBadRequest(injected), `"other"`},
		{"injection in nested JSON", nativeBadRequest(jsonBody("openai", "invalid parameter: field messages is required")), `"other"`},
		{"phrase not at start", nativeBadRequest("see: Model do not support image input"), `"other"`},
		{"keyword only", nativeBadRequest("image"), `"other"`},
		{"keyword encrypted", nativeBadRequest("encrypted"), `"other"`},
		{"plausible B prefix without id", nativeBadRequest("The encrypted content for item could not be verified"), `"other"`},
		{"B with injected id", nativeBadRequest("The encrypted content for item rs_1 x could not be verified"), `"other"`},
		{"parameter error", nativeBadRequest(jsonBody("openai", "Invalid value: 'input_image'.")), `"other"`},
		{"model not found", nativeBadRequest(jsonBody("openai", "The model `gpt-9` does not exist")), `"other"`},
		{"D without field path", nativeBadRequest("Input tag 'input_image' found using 'type' does not match any of the expected tags"), `"other"`},
		{"D with other tag", nativeBadRequest("tool_result.content.0: Input tag 'input_audio' found using 'type' does not match any of the expected tags"), `"other"`},
		{"E wrapped by outer error", nativeBadRequest(jsonBody("openai", "Provider returned error")), `"other"`},
		{"401 envelope", "unexpected status 401 Unauthorized: Model do not support image input", `"other"`},
		{"429 envelope", "unexpected status 429 Too Many Requests: Model do not support image input", `"other"`},
		{"402 envelope", "unexpected status 402 Payment Required: Model do not support image input", `"other"`},
		{"quoted envelope", "quoted: " + nativeBadRequest("Model do not support image input"), `"other"`},
		{"envelope prefix only", "unexpected status 400 Bad Requestish: Model do not support image input", `"other"`},
		{"no colon", "unexpected status 400 Bad Request Model do not support image input", `"other"`},
		{"over 4 KiB", nativeBadRequest(big), `"other"`},
		{"truncated JSON", "unexpected status 400 Bad Request: {\"error\":{\"message\":\"Model do not support image input", `"other"`},
		{"JSON without message", nativeBadRequest(`{"error":{"type":"x"}}`), `"other"`},
		{"message is not a string", nativeBadRequest(`{"message":{"text":"Model do not support image input"}}`), `"other"`},
		{"unauthorized variant", nativeBadRequest("Model do not support image input"), `"unauthorized"`},
		{"rate limit variant", nativeBadRequest("Model do not support image input"), `"rateLimitExceeded"`},
		{"context variant", nativeBadRequest("Model do not support image input"), `"contextWindowExceeded"`},
		{"connection failed 429", nativeBadRequest("Model do not support image input"), `{"httpConnectionFailed":{"httpStatusCode":429}}`},
		{"connection failed no status", nativeBadRequest("Model do not support image input"), `{"httpConnectionFailed":{"httpStatusCode":null}}`},
		{"connection failed string status", nativeBadRequest("Model do not support image input"), `{"httpConnectionFailed":{"httpStatusCode":"400"}}`},
		{"other object variant", nativeBadRequest("Model do not support image input"), `{"responseStreamDisconnected":{"httpStatusCode":400}}`},
		{"two variants", nativeBadRequest("Model do not support image input"), `{"httpConnectionFailed":{"httpStatusCode":400},"other":{}}`},
	}
	for _, tc := range cases {
		got := classifyTurnError(&TurnError{Message: tc.message, CodexErrorInfo: json.RawMessage(tc.variant)})
		if got.Fingerprint != "" || got.HTTPStatus != nil && *got.HTTPStatus == 400 && got.Code != "connection_failed" {
			t.Fatalf("%s classified: %+v", tc.name, got)
		}
		switch got.Code {
		case "model_input_unsupported_image", "model_provider_format_rejected", "model_reasoning_state_invalid", "model_input_ends_with_assistant":
			t.Fatalf("%s produced an upstream 400 code: %+v", tc.name, got)
		}
	}
}

func TestUpstream400UnmatchedKeepsPreviousClassification(t *testing.T) {
	// Without a fingerprint, the generic mapping is exactly the previous behavior.
	generic := nativeBadRequest(jsonBody("openai", "Invalid value for parameter 'temperature'"))
	if got := classifyTurnError(&TurnError{Message: generic, CodexErrorInfo: json.RawMessage(`"badRequest"`)}); got.Code != "invalid_request" || got.Fingerprint != "" || got.HTTPStatus != nil {
		t.Fatal("badRequest mapping changed", got)
	}
	if got := classifyTurnError(&TurnError{Message: generic, CodexErrorInfo: json.RawMessage(`"other"`)}); got.Code != "" || got.Fingerprint != "" {
		t.Fatal("unmatched 400 must stay unclassified", got)
	}
	if got := classifyTurnError(&TurnError{Message: generic, CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":400}}`)}); got.Code != "connection_failed" || got.HTTPStatus == nil || *got.HTTPStatus != 400 || got.Fingerprint != "" {
		t.Fatal("typed 400 mapping changed", got)
	}
}

func TestUpstream400ClassificationNeverCarriesNativeText(t *testing.T) {
	const canary = "sk-canary-secret-token"
	body := jsonBody("openai", "Model do not support image input "+canary)
	got := classifyTurnError(&TurnError{Message: nativeBadRequest(body) + " " + canary, CodexErrorInfo: json.RawMessage(`"other"`)})
	raw, _ := json.Marshal(got)
	if got.Fingerprint != proto.FingerprintModelNoImageInput || strings.Contains(string(raw), canary) || strings.Contains(string(raw), "Model do not") {
		t.Fatal(string(raw))
	}
}

func TestUpstream400TerminalPathAndTurnFailedRegression(t *testing.T) {
	run := func(method, params string) []proto.Envelope {
		out := make(chan proto.Envelope, 16)
		s := &Session{runID: "run", out: out, cancelCtx: context.Background(), cfg: defaultSessionConfig(testStateRoot(t)), rpc: NewJSONRPCClient(JSONRPCConfig{})}
		s.registerHandlers()
		s.setThreadID("root")
		scopeNotification(t, s, "turn/started", `{"threadId":"root","turn":{"id":"turn"}}`)
		scopeNotification(t, s, method, params)
		var events []proto.Envelope
		for env := range out {
			events = append(events, env)
		}
		return events
	}
	message, _ := json.Marshal(nativeBadRequest(jsonBody("openai", "Model do not support image input")))
	completed := run("turn/completed", `{"threadId":"root","turn":{"id":"turn","status":"failed","error":{"message":`+string(message)+`,"codexErrorInfo":"other"}}}`)
	var failure proto.ErrorPayload
	found := false
	for _, env := range completed {
		if env.Type == proto.TypeError {
			found = env.DecodePayload(&failure) == nil
		}
	}
	if !found || failure.Code != "model_input_unsupported_image" || failure.Fingerprint != proto.FingerprintModelNoImageInput || failure.HTTPStatus == nil || *failure.HTTPStatus != 400 || failure.UpstreamRequestID != upstream400Request {
		t.Fatalf("terminal path lost the fingerprint: %+v", failure)
	}
	// turn/failed carries no TurnError in this adapter, so it must stay unclassified.
	failedErrors := 0
	for _, env := range run("turn/failed", `{"threadId":"root","turn":{"id":"turn","status":"failed","error":{"message":`+string(message)+`,"codexErrorInfo":"other"}}}`) {
		if env.Type == proto.TypeError {
			var p proto.ErrorPayload
			failedErrors++
			if env.DecodePayload(&p) != nil || p.Code != "" || p.Fingerprint != "" || p.HTTPStatus != nil {
				t.Fatalf("turn/failed became classified: %+v", p)
			}
		}
	}
	if failedErrors != 1 {
		t.Fatal("turn/failed emitted no error to inspect")
	}
}
