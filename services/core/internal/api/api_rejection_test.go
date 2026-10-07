package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/execution"
)

func rejectionLogBuffer(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&output, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func rejectionRecords(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "api_request_rejected" {
			records = append(records, record)
		}
	}
	return records
}

func TestEnvironmentFileRejectionDiagnostics(t *testing.T) {
	output := rejectionLogBuffer(t)
	const canary = "private-canary-value"
	const valid = `{"type":"inline","data":"YWJj","path":"/workspace/private-canary-value"}`
	for _, tc := range []struct {
		name, body, reason, code, message string
		status                            int
		err                               error
		pending                           bool
		param                             string
	}{
		{name: "payload", body: `{"type":"inline","path":"/workspace/private-canary-value"}`, reason: "invalid_payload", code: "invalid_request", message: invalidInputMessage, status: 400},
		{name: "unknown field", body: `{"type":"inline","data":"YWJj","path":"/workspace/a","private-canary-value":"secret"}`, reason: "unknown_field", code: "invalid_request_error", message: "Unknown parameter: 'private-canary-value'.", status: 400, param: canary},
		{name: "path", body: `{"type":"inline","data":"YWJj","path":"/private-canary-value"}`, reason: "invalid_path", code: "invalid_request_error", message: errEnvironmentFileCreatePath.message, status: 400},
		{name: "base64", body: `{"type":"inline","data":"private-canary-value","path":"/workspace/a"}`, reason: "invalid_base64", code: "invalid_request", message: invalidInputMessage, status: 400},
		{name: "inline size", body: inlineCreateBody((5 << 20) + 1), reason: "inline_too_large", code: "invalid_request_error", message: errEnvironmentFileInlineTooLarge.message, status: 400},
		{name: "provisioning", body: `{"type":"file_id","file_id":"private-canary-value","path":"/workspace/private-canary-value"}`, reason: "hosted_environment_provisioning", code: "invalid_request_error", message: errHostedEnvironmentProvisioning.message, status: 400, pending: true},
		{name: "directory", body: valid, reason: "destination_directory", code: "invalid_request_error", message: errEnvironmentFileConflict.message, status: 400, err: fmt.Errorf("%s: %w", canary, execution.ErrEnvironmentFileDirectory)},
		{name: "unsafe", body: valid, reason: "destination_unsafe", code: "invalid_request_error", message: errEnvironmentFileUnsafe.message, status: 400, err: fmt.Errorf("%s: %w", canary, execution.ErrEnvironmentFileUnsafe)},
		{name: "unknown", body: valid, reason: "unknown", code: "execution_unavailable", message: "Execution is not available on this service.", status: 503, err: fmt.Errorf("%s: %w", canary, execution.ErrExecutionUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output.Reset()
			unavailable := 0
			handler, f := environmentFileCreateHandler(t, countEnvironmentFilesUnavailable(&unavailable))
			f.err = tc.err
			if tc.pending {
				f.environment.Status = "pending"
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/agents/environments/"+f.environment.ID+"/files?secret="+canary, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer files-key")
			request.Header.Set("OpenAI-Beta", "agents=v1")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Request-Id", canary)
			response := httptest.NewRecorder()
			handler.ServeHTTP(environmentFilesRecorder{response}, request)
			expected := httptest.NewRecorder()
			var param []string
			if tc.param != "" {
				param = []string{tc.param}
			}
			writeError(expected, tc.status, tc.code, tc.message, param...)
			if response.Code != expected.Code || response.Body.String() != expected.Body.String() {
				t.Fatalf("response changed: %d %s; expected %d %s", response.Code, response.Body, expected.Code, expected.Body)
			}
			records := rejectionRecords(t, output)
			if len(records) != 1 {
				t.Fatalf("expected one diagnostic, got %d", len(records))
			}
			record := records[0]
			want := map[string]any{"operation": "environment_file_create", "route": "/v1/agents/environments/{environment_id}/files", "status": float64(tc.status), "code": tc.code, "reason": tc.reason, "request_id": response.Header().Get("X-Request-Id")}
			carrier, err := obslog.ParseTraceparent(response.Header().Get(obslog.HeaderName))
			if err != nil {
				t.Fatal(err)
			}
			want["trace_id"], want["span_id"] = carrier.Trace.String(), carrier.Span.String()
			for key, value := range want {
				if record[key] != value {
					t.Fatalf("%s=%v want %v", key, record[key], value)
				}
			}
			if !requestIDPattern.MatchString(want["request_id"].(string)) {
				t.Fatal("missing server request ID")
			}
			for key := range record {
				if key != "time" && key != "level" && key != "msg" {
					if _, ok := want[key]; !ok {
						t.Fatalf("unapproved diagnostic field: %s", key)
					}
				}
			}
			for _, secret := range []string{canary, "files-key", f.environment.ID, f.environment.TenantID} {
				if strings.Contains(output.String(), secret) {
					t.Fatalf("sensitive data leaked: %s", secret)
				}
			}
		})
	}
}

func TestRejectionDiagnosticsPreserveResponseWriter(t *testing.T) {
	output := rejectionLogBuffer(t)
	for _, stream := range []bool{false, true} {
		output.Reset()
		out := &detailDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		var observed []string
		handler := responseHeadersWithErrors(coreErrorResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			environmentFileDiagnostic(w, r.Context(), rejectionUnknown)
			deadline := time.Unix(1234, 0)
			if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Fatal(err)
				}
				_, _ = w.Write([]byte("data: safe\n\n"))
				environmentFileDiagnostic(w, nil, rejectionInvalidPath)
				reportAPIError(w, "invalid_request_error")
			} else {
				writeJSON(w, 201, map[string]string{"result": "safe"})
			}
		})), func(code string) { observed = append(observed, code) })
		handler.ServeHTTP(out, httptest.NewRequest("POST", "/fixture", nil))
		if !out.deadline.Equal(time.Unix(1234, 0)) || out.Header().Get("Openai-Processing-Ms") == "" || len(observed) != 0 || len(rejectionRecords(t, output)) != 0 {
			t.Fatal("success/stream behavior changed")
		}
		if stream && (!out.Flushed || out.Body.String() != "data: safe\n\n") {
			t.Fatal("SSE changed")
		}
	}
	output.Reset()
	var observed []string
	handler := responseHeadersWithErrors(coreErrorResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		environmentFileDiagnostic(w, r.Context(), apiRejectionReason(255))
		writeError(w, 400, "secret-code-canary", "secret-message-canary", "secret-param-canary")
		reportAPIError(w, "late-code")
		w.WriteHeader(500)
	})), func(code string) { observed = append(observed, code) })
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/secret-path-canary", nil))
	records := rejectionRecords(t, output)
	if len(records) != 1 || records[0]["code"] != "unknown" || records[0]["reason"] != "unknown" || strings.Contains(output.String(), "canary") || !reflect.DeepEqual(observed, []string{"secret-code-canary"}) {
		t.Fatal("observer/allowlist changed")
	}
}

func TestEnvironmentFileDiagnosticsStayWithinCreateHandler(t *testing.T) {
	output := rejectionLogBuffer(t)
	h, f := environmentFileCreateHandler(t)
	f.environment.Status = "pending"
	if response := requestEnvironmentFiles(h, f.environment.ID, "", "files-key"); response.Code != 400 {
		t.Fatal("expected provisioning rejection")
	}
	if response := requestCreateEnvironmentFile(h, f.environment.ID, `{"type":"inline","data":"","path":"/workspace/a"}`, ""); response.Code != 401 {
		t.Fatal("expected authentication rejection")
	}
	if len(rejectionRecords(t, output)) != 0 {
		t.Fatal("diagnostic escaped create handler scope")
	}
	f.environment.Status = "connected"
	if response := requestCreateEnvironmentFile(h, f.environment.ID, `{"type":"inline","data":"","path":"/workspace/a"}`, "files-key"); response.Code != 201 || f.writes != 1 {
		t.Fatal("success changed")
	}
	if len(rejectionRecords(t, output)) != 0 {
		t.Fatal("success logged as rejection")
	}
}

func TestEnvironmentFileDiagnosticUsesHandlerTraceContext(t *testing.T) {
	output := rejectionLogBuffer(t)
	const inbound = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	for _, parent := range []string{"", inbound} {
		output.Reset()
		h, f := environmentFileCreateHandler(t)
		f.environment.Status = "pending"
		r := httptest.NewRequest("POST", "/v1/agents/environments/"+f.environment.ID+"/files", strings.NewReader(`{"type":"inline","data":"","path":"/workspace/a"}`))
		r.Header.Set("Authorization", "Bearer files-key")
		r.Header.Set("OpenAI-Beta", "agents=v1")
		r.Header.Set(obslog.HeaderName, parent)
		out := httptest.NewRecorder()
		h.ServeHTTP(environmentFilesRecorder{out}, r)
		records := rejectionRecords(t, output)
		carrier, err := obslog.ParseTraceparent(out.Header().Get(obslog.HeaderName))
		if err != nil || out.Code != 400 || len(records) != 1 {
			t.Fatalf("missing response/diagnostic trace: %v", err)
		}
		if parent != "" && out.Header().Get(obslog.HeaderName) != parent {
			t.Fatal("inbound carrier changed")
		}
		if records[0]["request_id"] != out.Header().Get("X-Request-Id") || records[0]["trace_id"] != carrier.Trace.String() || records[0]["span_id"] != carrier.Span.String() {
			t.Fatal("diagnostic did not use the traced handler context")
		}
	}
}
