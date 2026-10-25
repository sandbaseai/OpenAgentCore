package runtimegateway

import (
	"bytes"
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
	"io"
	"log/slog"
	"testing"
	"time"
)

// Real ExportWorkspaceOutputs reader, not a synthetic observeRound invocation:
// wrong-ID receipts cannot release it, ten small chunks retain exact offsets,
// and terminal completed adds no extra request after the eleventh receipt.
func TestIndependentC2SerialRoundsPairingAndCap(t *testing.T) {
	var logs bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prior)
	s := newTestSession(newFakeConn(), "device", "test", nil, nil)
	defer s.Close("test")
	ctx := textobs.WithArtifact(t.Context(), "5889faac-763b-4a6b-bae6-e809fe65e0cd", "4fbfe7d7-cfdb-4b96-917f-79b3d251c8b5")
	done := make(chan error, 1)
	var body []byte
	go func() {
		done <- s.ExportWorkspaceOutputs(ctx, proto.WorkspaceExportPayload{Handle: "PRIVATE_CANARY", EnvironmentID: "private-environment"}, func(r io.Reader) error { var err error; body, err = io.ReadAll(r); return err })
	}()
	for i := 0; i < 11; i++ {
		var frame proto.Envelope
		select {
		case f := <-s.sendCh:
			frame = f.Envelope
		case <-time.After(time.Second):
			t.Fatal("request absent")
		}
		var req proto.WorkspaceExportPayload
		if e := frame.DecodePayload(&req); e != nil {
			t.Fatal(e)
		}
		phase := "next"
		if i == 0 {
			phase = "begin"
		}
		if req.Step != phase || req.Offset != int64(i) {
			t.Fatalf("request changed %+v", req)
		}
		wrong, _ := proto.NewEnvelope(proto.TypeWorkspaceExportResult, "not-this-export", proto.WorkspaceExportResultPayload{Outcome: "completed", Offset: int64(i)})
		s.dispatchWorkspaceExport(wrong)
		if i == 0 {
			select {
			case <-done:
				t.Fatal("wrong ID released reader")
			case <-time.After(25 * time.Millisecond):
			}
		}
		result := proto.WorkspaceExportResultPayload{Outcome: "chunk", Offset: int64(i), Data: []byte{'x'}}
		if i == 10 {
			result.Outcome = "completed"
			result.Data = nil
		}
		reply, _ := proto.NewEnvelope(proto.TypeWorkspaceExportResult, frame.ID, result)
		s.dispatchWorkspaceExport(reply)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("not complete")
	}
	if string(body) != "xxxxxxxxxx" || len(s.sendCh) != 0 {
		t.Fatal("wire request/content changed")
	}
	var row map[string]any
	if e := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); e != nil {
		t.Fatal(e)
	}
	if row["round_count"] != float64(11) || row["request_send_to_receipt_dispatch_count"] != float64(11) || row["rounds_truncated"] != true {
		t.Fatal(row)
	}
	var rounds []map[string]any
	json.Unmarshal([]byte(row["rounds_json"].(string)), &rounds)
	if len(rounds) != 8 || rounds[0]["request_send_to_receipt_dispatch_ms"].(float64) < 20 {
		t.Fatal(rounds)
	}
	if bytes.Contains(logs.Bytes(), []byte("PRIVATE_CANARY")) {
		t.Fatal("canary leak")
	}
}
