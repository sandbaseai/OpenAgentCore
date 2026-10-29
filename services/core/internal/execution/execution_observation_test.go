package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
)

// Gateway connections can finish asynchronously after their owning test exits.
// Their lifecycle logs are verified in runtimegateway, not by execution's
// single-record observation assertions.
type executionLogBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *executionLogBuffer) Write(p []byte) (int, error) {
	var row struct {
		Message string `json:"msg"`
	}
	if json.Unmarshal(p, &row) == nil && (row.Message == "runtime transport failed" || row.Message == "runtime connection closed") {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}
func (b *executionLogBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data.Bytes())
}
func (b *executionLogBuffer) String() string { return string(b.Bytes()) }
func (b *executionLogBuffer) Len() int       { b.mu.Lock(); defer b.mu.Unlock(); return b.data.Len() }
func (b *executionLogBuffer) Reset()         { b.mu.Lock(); defer b.mu.Unlock(); b.data.Reset() }

func TestExecutionLogCaptureExcludesConcurrentGatewayLifecycle(t *testing.T) {
	out := captureExecutionLogs(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			obslog.Bg().Warn("runtime transport failed", "operation", "read")
			obslog.Bg().Info("runtime connection closed")
		}
	}()
	recordInitialInputOrigin(t.Context(), "capture-fixture")
	wg.Wait()
	var row map[string]any
	if err := json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["session_id"] != "capture-fixture" {
		t.Fatal(row)
	}
}

func captureExecutionLogs(t *testing.T) *executionLogBuffer {
	t.Helper()
	var out executionLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&out, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &out
}

func TestReservationTraceSurvivesRecoveryWithoutRetainingObserver(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	first := reservationTrace(parent, "reservation-one")
	recovered := reservationTrace(t.Context(), "reservation-one")
	a, _ := obslog.TraceFromContext(first)
	b, _ := obslog.TraceFromContext(recovered)
	if a.Trace != b.Trace || a.Span == b.Span || a.Trace == reservationTraceID("reservation-two") {
		t.Fatal("reservation recovery did not retain identity with a distinct attempt span")
	}
	if _, err := obslog.ParseTraceparent(a.String()); err != nil {
		t.Fatal(err)
	}
	cancel()
	if !errors.Is(first.Err(), context.Canceled) || recovered.Err() != nil {
		t.Fatal("diagnostic context changed cancellation ownership")
	}
}

func TestExecutionStageStatusAndCredentialRedaction(t *testing.T) {
	out := captureExecutionLogs(t)
	ctx := reservationTrace(t.Context(), "reservation-one")
	for _, sample := range []struct {
		err    error
		status string
	}{
		{nil, "ok"}, {context.Canceled, "cancelled"}, {context.DeadlineExceeded, "timeout"},
		{errors.New("private provider URL and credential must never appear"), "error"},
	} {
		out.Reset()
		observeExecutionStage(ctx, "provider_create", time.Now().Add(-time.Millisecond), sample.err, "allocation_id", "allocation-one")
		var entry map[string]any
		if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["status"] != sample.status || entry["stage"] != "provider_create" || entry["trace_id"] != reservationTraceID("reservation-one").String() || entry["duration_ms"].(float64) < 1 {
			t.Fatal("stage identity/status/duration lost", entry)
		}
		if strings.Contains(out.String(), "credential") || strings.Contains(out.String(), "private provider") {
			t.Fatal("raw provider error leaked")
		}
	}
}

func TestExecutorControlLogsDistinguishPreparationFromHTTP(t *testing.T) {
	out := captureExecutionLogs(t)
	ctx := obslog.WithRequestID(reservationTrace(t.Context(), "reservation-one"), "http-one")
	prepared := &preparedStart{ctx: ctx, requestID: "prepare-one", executorID: "executor-one", createdAt: time.Now(), startSentAt: time.Now()}
	recordExecutorReadiness(prepared, proto.PreparationStatusPayload{ExecutorID: "executor-one", Reused: true})
	recordExecutorStart(prepared, "turn-one")
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry["request_id"] != "http-one" || entry["preparation_request_id"] != "prepare-one" || entry["trace_id"] != reservationTraceID("reservation-one").String() {
			t.Fatal("control log confused HTTP and preparation identity", entry)
		}
	}
}

func TestLifecycleGateObservationPreservesCancellation(t *testing.T) {
	out := captureExecutionLogs(t)
	r := &runtimeLifecycle{ctx: t.Context(), gate: make(chan struct{}, 1), nodeID: "node-one"}
	r.gate <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.lock(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(r.gate) != 1 || !strings.Contains(out.String(), `"status":"cancelled"`) {
		t.Fatal("cancelled waiter changed gate or log status")
	}
}

type executionTraceConn struct {
	writes chan []byte
	closed chan struct{}
	once   sync.Once
}

func (c *executionTraceConn) ReadMessage() (int, []byte, error) { <-c.closed; return 0, nil, io.EOF }
func (c *executionTraceConn) WriteMessage(_ int, data []byte) error {
	c.writes <- append([]byte(nil), data...)
	return nil
}
func (c *executionTraceConn) WriteControl(int, []byte, time.Time) error { return nil }
func (c *executionTraceConn) SetReadLimit(int64)                        {}
func (c *executionTraceConn) SetReadDeadline(time.Time) error           { return nil }
func (c *executionTraceConn) SetWriteDeadline(time.Time) error          { return nil }
func (c *executionTraceConn) Close() error                              { c.once.Do(func() { close(c.closed) }); return nil }

func TestExecutionSendPreservesPayloadAndCarriesTrace(t *testing.T) {
	for _, traced := range []bool{false, true} {
		conn := &executionTraceConn{writes: make(chan []byte, 2), closed: make(chan struct{})}
		peer := runtimegateway.NewSession(conn, "device-one", "test", runtimegateway.NewRegistry(), func(string, ...any) {})
		peer.Start()
		ctx := t.Context()
		if traced {
			ctx = reservationTrace(ctx, "reservation-one")
		}
		payload := proto.ExecutionStartPayload{Handle: "handle-one", ExecutorID: "executor-one", RunID: "turn-one", Input: proto.TextInput("private input preserved on wire")}
		if err := send(ctx, peer, proto.TypeExecutionStart, "prepare-one", payload); err != nil {
			peer.Close("test done")
			t.Fatal(err)
		}
		select {
		case data := <-conn.writes:
			var env proto.Envelope
			if err := json.Unmarshal(data, &env); err != nil {
				t.Fatal(err)
			}
			var got proto.ExecutionStartPayload
			if err := env.DecodePayload(&got); err != nil {
				t.Fatal(err)
			}
			expected, _ := json.Marshal(payload)
			actual, _ := json.Marshal(got)
			if env.ID != "prepare-one" || env.Type != proto.TypeExecutionStart || !bytes.Equal(expected, actual) {
				t.Fatal("trace changed execution identity or payload")
			}
			if traced {
				carrier, _ := obslog.TraceFromContext(ctx)
				if env.Trace != carrier.String() {
					t.Fatal("execution trace did not reach actual gateway wire")
				}
			} else if env.Trace != "" {
				t.Fatal("missing trace changed into a fabricated caller trace")
			}
		case <-time.After(time.Second):
			t.Fatal("gateway did not send")
		}
		peer.Close("test done")
	}
}

func TestInitialInputOriginKeepsHTTPTraceAndSessionIdentity(t *testing.T) {
	out := captureExecutionLogs(t)
	ctx, origin := obslog.StartBackgroundTrace(t.Context())
	recordInitialInputOrigin(ctx, "session-one")
	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["session_id"] != "session-one" || entry["trace_id"] != origin.Trace.String() {
		t.Fatal("initial input origin lost its HTTP/session bridge", entry)
	}
	if _, present := entry["reservation_id"]; present {
		t.Fatal("creation fabricated a reservation identity")
	}
}

func TestAdmissionStageDiagnosticsUseFiniteStatusAndSafeFields(t *testing.T) {
	out := captureExecutionLogs(t)
	stages := []string{"execution_prepare_send", "execution_prepare_wait", "environment_initialize_commit", "runtime_reconcile"}
	for _, stage := range stages {
		for _, sample := range []struct {
			err    error
			status string
		}{{nil, "ok"}, {context.Canceled, "cancelled"}, {context.DeadlineExceeded, "timeout"}, {errors.New("SECRET fixture error body"), "error"}} {
			out.Reset()
			observeExecutionStage(reservationTrace(t.Context(), "reservation"), stage, time.Now(), sample.err, "session_id", "session", "reservation_id", "reservation")
			var entry map[string]any
			if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			if entry["stage"] != stage || entry["status"] != sample.status || entry["duration_ms"].(float64) < 0 || entry["reservation_id"] != "reservation" {
				t.Fatal("invalid stage schema", entry)
			}
			if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "fixture error body") {
				t.Fatal("raw failure leaked")
			}
		}
	}
}
