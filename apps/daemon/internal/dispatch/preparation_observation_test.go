package dispatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/workspaceobs"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type observationBuffer struct {
	sync.Mutex
	b bytes.Buffer
}

func (b *observationBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.b.Write(p)
}
func (b *observationBuffer) rows() []map[string]any {
	b.Lock()
	defer b.Unlock()
	var rows []map[string]any
	for _, line := range bytes.Split(b.b.Bytes(), []byte("\n")) {
		var row map[string]any
		if json.Unmarshal(line, &row) == nil && row["msg"] == "runtime workspace transport" {
			rows = append(rows, row)
		}
	}
	return rows
}
func TestWorkspacePreparationSummaryLifecycle(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "shutdown"}[shutdown], func(t *testing.T) {
			var logs observationBuffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			sender := &recSender{}
			r := preparationRouter(t, sender, time.Minute, func(context.Context, proto.PromptRequestPayload) (preparedFixture, error) {
				t.Error("workspace preparation started Harness")
				return nil, nil
			})
			request := preparationRequest()
			request.Configuration.WorkspaceReadOnly = true
			arrived := func() context.Context {
				return workspaceobs.ReceivedContext(t.Context(), "physical-connection", time.Now().Add(-10*time.Millisecond))
			}
			if err := r.Handle(arrived(), mustEnv(t, proto.TypeExecutionPrepare, "3e29a62c-64ef-4cde-8adc-608380e04478", request)); err != nil {
				t.Fatal(err)
			}
			ready := waitPreparationStatus(t, sender, "3e29a62c-64ef-4cde-8adc-608380e04478", "ready", "")
			// Duplicate request retains the same lifecycle and never adds another scope.
			if err := r.Handle(arrived(), mustEnv(t, proto.TypeExecutionPrepare, "3e29a62c-64ef-4cde-8adc-608380e04478", request)); err != nil {
				t.Fatal(err)
			}
			if shutdown {
				if err := r.Shutdown(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := r.Handle(arrived(), mustEnv(t, proto.TypeExecutionRelease, "3e29a62c-64ef-4cde-8adc-608380e04478", proto.ExecutionReleasePayload{Handle: ready.Handle})); err != nil {
					t.Fatal(err)
				}
				waitPreparationStatus(t, sender, "3e29a62c-64ef-4cde-8adc-608380e04478", "released", "")
			}
			deadline := time.Now().Add(time.Second)
			for len(logs.rows()) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if err := r.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows := logs.rows()
			if len(rows) != 1 {
				t.Fatalf("summaries=%d", len(rows))
			}
			row := rows[0]
			if row["scope"] != "preparation" || row["preparation_request_id"] != "3e29a62c-64ef-4cde-8adc-608380e04478" || row["prepare_local_count"] != float64(2) {
				t.Fatal(row)
			}
			if row["outbound_write_total_ms"] != nil {
				t.Fatal("mock Sender claimed socket write", row)
			}
			if shutdown && row["snapshot_complete"] != false {
				t.Fatal("shutdown claimed settled", row)
			}
		})
	}
}
