package runtimegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
)

type artifactSlowConn struct {
	*fakeConn
	entered, release chan struct{}
	once             sync.Once
}

func (c *artifactSlowConn) WriteMessage(kind int, data []byte) error {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.fakeConn.WriteMessage(kind, data)
}
func TestArtifactTransportActualLoopsAndSnapshot(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "in_flight"}[early], func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			conn := &artifactSlowConn{fakeConn: newFakeConn(), entered: make(chan struct{}), release: make(chan struct{})}
			s := newTestSession(conn, "device", "tenant", "test", nil, nil)
			defer s.Close("test")
			ctx := textobs.WithArtifact(t.Context(), "session", "turn")
			finish := s.ObserveArtifactTransport(ctx, "prepare")
			s.observeArtifactExport(ctx, "export")
			env, _ := proto.NewEnvelope(proto.TypeWorkspaceExport, "export", proto.WorkspaceExportPayload{Step: "begin", Handle: "SECRET", EnvironmentID: "SECRET"})
			if err := s.Send(ctx, env); err != nil {
				t.Fatal(err)
			}
			// The queue delay is deterministic: the writer has not started yet.
			time.Sleep(20 * time.Millisecond)
			s.Start()
			<-conn.entered
			if early {
				finish()
			}
			time.Sleep(20 * time.Millisecond)
			close(conn.release)
			reply, _ := proto.NewEnvelope(proto.TypeWorkspaceExportResult, "export", proto.WorkspaceExportResultPayload{Outcome: "completed"})
			raw, _ := json.Marshal(reply)
			conn.Feed(raw)
			deadline := time.Now().Add(time.Second)
			if !early {
				for {
					o := s.artifactFor(reply)
					o.mu.Lock()
					ready := o.stages[artifactDispatch].count == 1 && o.pending == 0
					o.mu.Unlock()
					if ready {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("loop did not finish")
					}
					time.Sleep(time.Millisecond)
				}
			}
			finish()
			finish()
			var row map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
				t.Fatal("not one summary", err)
			}
			if bytes.Contains(logs.Bytes(), []byte("SECRET")) {
				t.Fatal("private payload logged")
			}
			if row["preparation_request_id"] != "prepare" || row["export_id"] != "export" || row["connection_id"] == "" {
				t.Fatal("identity missing")
			}
			if early {
				if row["outbound_write_total_ms"] != nil || row["outbound_unfinished_count"] != float64(1) || row["snapshot_complete"] != false {
					t.Fatal("unfinished write claimed", row)
				}
			} else {
				if row["outbound_queue_wait_total_ms"].(float64) < 15 || row["outbound_write_total_ms"].(float64) < 15 {
					t.Fatal("delay missing", row)
				}
				if row["inbound_decode_count"] != float64(1) || row["inbound_dispatch_count"] != float64(1) {
					t.Fatal("inbound unobserved")
				}
			}
			s.artifactMu.Lock()
			remaining := len(s.artifactCaptures) + len(s.artifactIDs)
			s.artifactMu.Unlock()
			if remaining != 0 {
				t.Fatal("observer retained")
			}
		})
	}
}

func TestArtifactTransportScopeClaimAndBound(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	s := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
	defer s.Close("test")
	s.ObserveArtifactTransport(context.Background(), "outside")()
	if logs.Len() != 0 {
		t.Fatal("unscoped log")
	}
	ctx := textobs.WithArtifact(t.Context(), "s", "t")
	finish := s.ObserveArtifactTransport(ctx, "one")
	other := newTestSession(newFakeConn(), "device", "tenant", "test", nil, nil)
	defer other.Close("test")
	other.ObserveArtifactTransport(ctx, "two")()
	finish()
	var row map[string]any
	if json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row) != nil || row["scope_status"] != "unobserved" {
		t.Fatal("cross connection claim not unknown")
	}
	logs.Reset()
	var ends []func()
	for i := 0; i < 65; i++ {
		id := time.Unix(int64(i), 0).String()
		ends = append(ends, s.ObserveArtifactTransport(textobs.WithArtifact(t.Context(), "s", "t"), id))
	}
	s.artifactMu.Lock()
	count := len(s.artifactCaptures)
	s.artifactMu.Unlock()
	if count != 64 {
		t.Fatal("capacity changed", count)
	}
	for _, end := range ends {
		end()
	}
	if len(bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))) != 65 {
		t.Fatal("lost owner summaries")
	}
}
