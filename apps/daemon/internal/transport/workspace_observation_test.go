package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/transport"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/workspaceobs"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type safeObservationBuffer struct {
	sync.Mutex
	b bytes.Buffer
}

func (b *safeObservationBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.b.Write(p)
}
func (b *safeObservationBuffer) Bytes() []byte {
	b.Lock()
	defer b.Unlock()
	return append([]byte(nil), b.b.Bytes()...)
}
func TestWorkspaceObservationActualWebsocketLoops(t *testing.T) {
	var logs safeObservationBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(&slowPredecodeHandler{Handler: slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})}))
	defer slog.SetDefault(previous)
	gw := newFakeGateway()
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer server.Close()
	conn, err := transport.Dial(t.Context(), transport.DialOptions{WSURL: httpToWS(server.URL), DeviceID: "device", Credential: "canary-secret", DaemonVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer := <-gw.connCh
	request, _ := proto.NewEnvelope(proto.TypeWorkspaceExport, "export", proto.WorkspaceExportPayload{Step: "next", Offset: 0})
	if err = peer.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	// Wait for decode, then delay the CLI-equivalent consumer independently.
	deadline := time.Now().Add(time.Second)
	for len(conn.Recv()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("not enqueued")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	received := <-conn.Recv()
	scope := workspaceobs.New(received.Context(context.Background()), received.ID, "export")
	reply, _ := proto.NewEnvelope(proto.TypeWorkspaceExportResult, "export", proto.WorkspaceExportResultPayload{Outcome: "completed"})
	roundCtx := scope.RoundContext(scope.Context(t.Context()), received.Context(t.Context()), "next", 0)
	workspaceobs.RoundFromContext(roundCtx).SetResponse("completed")
	if err = conn.Send(roundCtx, reply); err != nil {
		t.Fatal(err)
	}
	scope.Finish("completed")
	scope.Finish("completed")
	var rows []map[string]any
	for _, line := range bytes.Split(logs.Bytes(), []byte("\n")) {
		var row map[string]any
		if json.Unmarshal(line, &row) == nil && row["msg"] == "runtime workspace transport" {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("summaries=%d", len(rows))
	}
	row := rows[0]
	if row["outbound_write_count"] != float64(1) || row["outbound_encode_count"] != float64(1) || row["connection_id"] == "" || row["outbound_unfinished_count"] != float64(0) {
		t.Fatal(row)
	}
	if row["read_return_to_decode_begin_count"] != float64(1) || row["inbound_decode_count"] != float64(1) {
		t.Fatal("predecode missing", row)
	}
	if row["request_read_to_response_write_count"] != float64(1) || row["read_return_to_decode_begin_total_ms"].(float64) < 20 {
		t.Fatal("round/predecode delay absent", row)
	}
	if row["receive_dispatch_wait_total_ms"].(float64) < 10 {
		t.Fatal("receive interval missing", row)
	}
	if bytes.Contains(logs.Bytes(), []byte("canary-secret")) {
		t.Fatal("credential logged")
	}
}

type slowPredecodeHandler struct{ slog.Handler }

func (h *slowPredecodeHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "transport: received WS frame" {
		time.Sleep(30 * time.Millisecond)
	}
	return h.Handler.Handle(ctx, r)
}
