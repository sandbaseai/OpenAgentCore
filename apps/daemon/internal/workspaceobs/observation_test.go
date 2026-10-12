package workspaceobs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

func TestSnapshotBoundsUnknownAndPrivacy(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	ctx := ReceivedContext(context.Background(), "connection", time.Now().Add(-20*time.Millisecond))
	s := New(ctx, "export", "export")
	started := s.Begin(ExportRead)
	s.End(ExportRead, started, nil)
	s.Begin(ExportDone)
	s.Pending(1)
	s.Data(17, false)
	var stamp QueueStamp
	stamp.Received(s, RequestQueue)
	s.Finish("cancelled")
	s.Finish("completed")
	s.End(ExportDone, started, nil)
	s.Data(999, true)
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
		t.Fatal(err)
	}
	if row["snapshot_complete"] != false || row["export_done_wait_unknown_count"] != float64(1) || row["export_done_wait_total_ms"] != nil || row["request_queue_wait_unknown_count"] != float64(1) {
		t.Fatal(row)
	}
	if row["read_bytes"] != float64(17) || row["outbound_unfinished_count"] != float64(1) || row["outbound_write_total_ms"] != nil {
		t.Fatal(row)
	}
	if row["receive_dispatch_wait_total_ms"].(float64) < 20 {
		t.Fatal(row)
	}
}
func TestPhysicalConnectionAndQueue(t *testing.T) {
	ctx := ReceivedContext(t.Context(), "first", time.Now())
	s := New(ctx, "reused-id", "preparation")
	bound := s.Context(ctx)
	if ForConnection(bound, "second") != nil || !s.mismatch {
		t.Fatal("cross-connection attribution")
	}
	if ForConnection(bound, "first") != s {
		t.Fatal("lost owner")
	}
	var stamp QueueStamp
	stamp.Admitted()
	time.Sleep(10 * time.Millisecond)
	stamp.Received(s, RequestQueue)
	if s.metrics[RequestQueue].count != 1 || s.metrics[RequestQueue].total < 10*time.Millisecond {
		t.Fatal(s.metrics)
	}
	if FromContext(t.Context()) != nil || ForConnection(t.Context(), "first") != nil {
		t.Fatal("scope escape")
	}
}

func TestPeerControlledIdentityIsNeverLogged(t *testing.T) {
	for _, id := range []string{"secret-canary", string(bytes.Repeat([]byte("secret-canary"), 10000))} {
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
		s := New(ReceivedContext(t.Context(), "connection", time.Now()), id, "export")
		s.Finish("cancelled")
		slog.SetDefault(previous)
		var row map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
			t.Fatal(err)
		}
		if row["identity_valid"] != false || row["export_id"] != "" || row["snapshot_complete"] != false || bytes.Contains(logs.Bytes(), []byte("secret-canary")) {
			t.Fatal("peer text leaked")
		}
	}
}

func TestSuppressedPublicationSnapshotIsIncomplete(t *testing.T) {
	for _, outcome := range []string{"shutdown", "suspended"} {
		t.Run(outcome, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			s := New(ReceivedContext(t.Context(), "connection", time.Now()), "3e29a62c-64ef-4cde-8adc-608380e04478", "preparation")
			s.Finish(outcome)
			s.Finish("expired")
			var row map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
				t.Fatal(err)
			}
			if row["outcome"] != outcome || row["snapshot_complete"] != false || row["sender_call_total_ms"] != nil || row["outbound_write_total_ms"] != nil {
				t.Fatal(row)
			}
		})
	}
}
