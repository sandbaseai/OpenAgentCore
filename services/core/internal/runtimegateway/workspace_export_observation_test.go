package runtimegateway

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
)

func TestWorkspaceExportObservationBoundaries(t *testing.T) {
	for _, mode := range []string{"empty_tar", "small_buffer", "slow_receipt", "slow_completed", "missing_completed", "bad_offset", "cancel", "disconnect", "consume_failure", "unscoped"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			prior := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(prior)
			s := NewSession(newFakeConn(), "device", "tenant", "test", nil, nil)
			defer s.Close("test")
			deadline := 3 * time.Second
			if mode == "missing_completed" {
				deadline = 150 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), deadline)
			defer cancel()
			if mode != "unscoped" {
				ctx = textobs.WithArtifact(ctx, "session", "turn")
			}
			var archive bytes.Buffer
			if err := tar.NewWriter(&archive).Close(); err != nil {
				t.Fatal(err)
			}
			body := archive.Bytes()
			if len(body) != 1024 {
				t.Fatalf("empty tar bytes %d", len(body))
			}
			if mode == "small_buffer" {
				body = bytes.Repeat([]byte("PRIVATE-CONTENT-CANARY"), 64)
			}
			done := make(chan error, 1)
			sentinel := errors.New("PRIVATE-ERROR-CANARY")
			go func() {
				done <- s.ExportWorkspaceOutputs(ctx, proto.WorkspaceExportPayload{Handle: "PRIVATE-HANDLE-CANARY", EnvironmentID: "PRIVATE-ENV-CANARY"}, func(r io.Reader) error {
					if mode == "consume_failure" {
						_, err := r.Read(make([]byte, 1))
						if err != nil {
							return err
						}
						return sentinel
					}
					if mode == "empty_tar" {
						_, err := tar.NewReader(r).Next()
						if err == io.EOF {
							return nil
						}
						return err
					}
					// Small consumer reads, including zero-length reads, must not duplicate chunks or requests.
					p := make([]byte, 7)
					for {
						if _, err := r.Read(nil); err != nil {
							return err
						}
						_, err := r.Read(p)
						if err == io.EOF {
							return nil
						}
						if err != nil {
							return err
						}
					}
				})
			}()
			offset := 0
			requests := 0
			for {
				var env proto.Envelope
				select {
				case env = <-s.sendCh:
				case <-time.After(4 * time.Second):
					t.Fatal("request timeout")
				}
				var req proto.WorkspaceExportPayload
				if env.DecodePayload(&req) != nil {
					t.Fatal("request decode")
				}
				requests++
				if req.Offset != int64(offset) {
					t.Fatalf("offset %d, expected %d", req.Offset, offset)
				}
				if mode == "bad_offset" {
					exportReply(t, s, env.ID, proto.WorkspaceExportResultPayload{Outcome: "chunk", Offset: 1, Data: []byte("PRIVATE-CONTENT-CANARY")})
					break
				}
				if mode == "cancel" {
					cancel()
					break
				}
				if mode == "disconnect" {
					s.Close("test")
					break
				}
				if offset == len(body) {
					if mode == "missing_completed" {
						break
					}
					if mode == "slow_completed" {
						time.Sleep(30 * time.Millisecond)
					}
					select {
					case err := <-done:
						t.Fatalf("returned before completed: %v", err)
					default:
					}
					exportReply(t, s, env.ID, proto.WorkspaceExportResultPayload{Outcome: "completed", Offset: int64(offset)})
					break
				}
				if mode == "slow_receipt" {
					time.Sleep(30 * time.Millisecond)
				}
				end := min(offset+512, len(body))
				exportReply(t, s, env.ID, proto.WorkspaceExportResultPayload{Outcome: "chunk", Offset: int64(offset), Data: body[offset:end]})
				offset = end
				if mode == "consume_failure" {
					break
				}
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("export did not settle")
			}
			failed := mode == "missing_completed" || mode == "bad_offset" || mode == "cancel" || mode == "disconnect" || mode == "consume_failure"
			if (err != nil) != failed {
				t.Fatalf("outcome: %v", err)
			}
			if mode == "missing_completed" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("missing receipt did not preserve deadline")
			}
			if mode == "consume_failure" && !errors.Is(err, sentinel) {
				t.Fatal("changed consumer error")
			}
			if strings.Contains(logs.String(), "PRIVATE-") {
				t.Fatal("private content leaked")
			}
			var rows []map[string]any
			for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var row map[string]any
				if json.Unmarshal(line, &row) != nil {
					t.Fatal("invalid log")
				}
				if row["msg"] == "artifact export reader" {
					rows = append(rows, row)
				}
			}
			if mode == "unscoped" {
				if len(rows) != 0 {
					t.Fatal("unscoped observation")
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("got %d summary rows", len(rows))
			}
			row := rows[0]
			if row["session_id"] != "session" || row["turn_id"] != "turn" || row["capture_id"] == "" || row["export_id"] == "" {
				t.Fatal("missing identity")
			}
			if row["begin_count"] != float64(1) || row["next_count"] != float64(requests-1) || row["receipt_wait_count"] != float64(requests) {
				t.Fatalf("request counts: %v", row)
			}
			if row["completed"] != !failed {
				t.Fatal("completed receipt state")
			}
			if failed {
				if row["completed_receipt_ms"] != nil {
					t.Fatal("invented completed boundary")
				}
			} else {
				if row["received_bytes"] != float64(len(body)) || row["delivered_bytes"] != float64(len(body)) || row["chunk_count"] != float64((len(body)+511)/512) {
					t.Fatalf("byte counts: %v", row)
				}
				if row["first_data_ms"] == nil || row["completed_receipt_ms"] == nil {
					t.Fatal("missing boundary")
				}
			}
			if mode == "empty_tar" && (row["begin_count"] != float64(1) || row["next_count"] != float64(2)) {
				t.Fatal("empty tar completion request missing")
			}
			if mode == "consume_failure" && (row["received_bytes"] != float64(512) || row["delivered_bytes"] != float64(1)) {
				t.Fatal("delivered conflated with received")
			}
			if mode == "bad_offset" && (row["received_bytes"] != float64(0) || row["first_data_ms"] != nil) {
				t.Fatal("invalid receipt counted as data")
			}
			if mode == "slow_receipt" || mode == "slow_completed" {
				if row["receipt_wait_ms"].(float64) < 25 || row["receipt_wait_max_ms"].(float64) < 25 {
					t.Fatal("receipt delay unobserved")
				}
				if row["duration_ms"].(float64) < row["receipt_wait_ms"].(float64) {
					t.Fatal("recursive wait double counted")
				}
			}
		})
	}
}
