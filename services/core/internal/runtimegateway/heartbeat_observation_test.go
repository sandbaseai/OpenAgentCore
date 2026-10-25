package runtimegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimedevice"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type observationHeartbeatStore struct {
	err              error
	deleted          bool
	entered, release chan struct{}
}

func (f *observationHeartbeatStore) TouchRuntimeHeartbeat(context.Context, string) (runtimedevice.HeartbeatStatus, error) {
	return runtimedevice.HeartbeatStatus{}, nil
}
func (f *observationHeartbeatStore) TouchAgentDaemonHeartbeat(context.Context, runtimedevice.Heartbeat) (runtimedevice.HeartbeatStatus, error) {
	if f.entered != nil {
		close(f.entered)
		<-f.release
	}
	time.Sleep(12 * time.Millisecond)
	return runtimedevice.HeartbeatStatus{Deleted: f.deleted}, f.err
}
func TestHeartbeatObservationStoreOutcomes(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		deleted bool
		want    string
	}{{"success", nil, false, "ok"}, {"failure", errors.New("SECRET"), false, "error"}, {"cancel", context.Canceled, false, "cancelled"}, {"deleted", nil, true, "deleted"}} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(old)
			s := newTestSession(newFakeConn(), "11111111-1111-4111-8111-111111111111", "test", nil, nil)
			defer s.Close("test")
			s.heartbeat = &observationHeartbeatStore{err: test.err, deleted: test.deleted}
			env, _ := proto.NewEnvelope(proto.TypeHeartbeat, "", proto.HeartbeatPayload{})
			s.handleHeartbeat(env)
			s.handleHeartbeat(env)
			var rows []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var row map[string]any
				if json.Unmarshal([]byte(line), &row) == nil && row["msg"] == "runtime first heartbeat observed" {
					rows = append(rows, row)
				}
			}
			if len(rows) != 1 {
				t.Fatalf("summary count %d: %s", len(rows), logs.String())
			}
			row := rows[0]
			fixture, _ := json.Marshal(row)
			t.Logf("OBSERVATION_FIXTURE %s", fixture)
			if row["outcome"] != test.want || row["heartbeat_store_ms"].(float64) < 10 {
				t.Fatal(row)
			}
			if test.want != "ok" && row["capability_hint_ms"] != nil {
				t.Fatal("unreached hint claimed", row)
			}
			if strings.Contains(logs.String(), "SECRET") {
				t.Fatal("private error logged")
			}
		})
	}
}
func TestHeartbeatObservationCloseDuringStore(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	s := newTestSession(newFakeConn(), "11111111-1111-4111-8111-111111111111", "test", nil, nil)
	store := &observationHeartbeatStore{entered: make(chan struct{}), release: make(chan struct{})}
	s.heartbeat = store
	env, _ := proto.NewEnvelope(proto.TypeHeartbeat, "", proto.HeartbeatPayload{})
	done := make(chan struct{})
	go func() { s.handleHeartbeat(env); close(done) }()
	<-store.entered
	s.Close("fixture")
	close(store.release)
	<-done
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) == nil && row["msg"] == "runtime first heartbeat observed" {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("summary count %d: %s", len(rows), logs.String())
	}
	row := rows[0]
	fixture, _ := json.Marshal(row)
	t.Logf("OBSERVATION_FIXTURE %s", fixture)
	if row["outcome"] != "unknown" || row["heartbeat_store_ms"] != nil {
		t.Fatal(row)
	}
}

func TestHeartbeatObservedTextHandlerScalarFixture(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	s := newTestSession(newFakeConn(), "11111111-1111-4111-8111-111111111111", "test", nil, nil)
	s.heartbeat = &observationHeartbeatStore{err: context.Canceled}
	env, _ := proto.NewEnvelope(proto.TypeHeartbeat, "", proto.HeartbeatPayload{})
	s.handleHeartbeat(env)
	line := logs.String()
	if !strings.Contains(line, "heartbeat_store_ms=") || !strings.Contains(line, "capability_hint_ms=<nil>") || strings.Contains(line, "0x") {
		t.Fatal(line)
	}
	t.Logf("OBSERVATION_TEXT_FIXTURE %s", strings.TrimSpace(line))
}
