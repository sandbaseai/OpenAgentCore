package dispatch

import (
	"bytes"
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/workspaceobs"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type exportObservationBuffer struct {
	sync.Mutex
	b bytes.Buffer
}

func (b *exportObservationBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.b.Write(p)
}
func TestWorkspaceExportSummaryOwnedAndBounded(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "multichunk", true: "cancel"}[cancelled], func(t *testing.T) {
			var logs exportObservationBuffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			r, s, request := exporterRouter(t, "normal")
			send := func(id string, p proto.WorkspaceExportPayload) {
				t.Helper()
				env, _ := proto.NewEnvelope(proto.TypeWorkspaceExport, id, p)
				ctx := workspaceobs.ReceivedContext(t.Context(), "connection", time.Now().Add(-5*time.Millisecond))
				if err := r.Handle(ctx, env); err != nil {
					t.Fatal(err)
				}
			}
			foreign := request
			foreign.Handle = "private-path-canary"
			send("foreign", foreign)
			if readExport(t, s).Outcome != "rejected" {
				t.Fatal("foreign accepted")
			}
			send("54dedddf-27c9-4a06-9b1f-244d0bc373e5", request)
			var offset int64
			for {
				result := readExport(t, s)
				if result.Outcome == "completed" {
					break
				}
				if result.Outcome != "chunk" {
					t.Fatal(result.Outcome)
				}
				offset += int64(len(result.Data))
				if cancelled {
					send("54dedddf-27c9-4a06-9b1f-244d0bc373e5", proto.WorkspaceExportPayload{Step: "cancel"})
					break
				}
				send("54dedddf-27c9-4a06-9b1f-244d0bc373e5", proto.WorkspaceExportPayload{Step: "next", Offset: offset})
			}
			r.shutdownWG.Wait()
			logs.Lock()
			defer logs.Unlock()
			var rows []map[string]any
			for _, line := range bytes.Split(logs.b.Bytes(), []byte("\n")) {
				var row map[string]any
				if json.Unmarshal(line, &row) == nil && row["msg"] == "runtime workspace transport" {
					rows = append(rows, row)
				}
			}
			if len(rows) != 1 {
				t.Fatalf("summaries=%d", len(rows))
			}
			row := rows[0]
			if row["export_id"] != "54dedddf-27c9-4a06-9b1f-244d0bc373e5" || row["read_bytes"] != float64(offset) || row["export_producer_count"] != float64(1) {
				t.Fatal(row)
			}
			if row["export_writer_count"].(float64) < 1 {
				t.Fatal("writer missing", row)
			}
			if cancelled {
				if row["outcome"] != "cancelled" || row["export_done_wait_total_ms"] != nil {
					t.Fatal(row)
				}
			} else if row["outcome"] != "completed" || row["local_eof"] != true || row["export_done_wait_count"] != float64(1) {
				t.Fatal(row)
			}
			if bytes.Contains(logs.b.Bytes(), []byte("private-path-canary")) {
				t.Fatal("private request leaked")
			}
		})
	}
}
