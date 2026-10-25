package runtimegateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestQAHeartbeatReconnectDuringOldStore(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(logger)
	reg := NewRegistry()
	old := newTestSession(newFakeConn(), "qa-device", "fixture", reg, nil)
	reg.Register(old)
	blocked := &observationHeartbeatStore{entered: make(chan struct{}), release: make(chan struct{})}
	old.heartbeat = blocked
	env, _ := proto.NewEnvelope(proto.TypeHeartbeat, "", proto.HeartbeatPayload{})
	done := make(chan struct{})
	go func() { old.handleHeartbeat(env); close(done) }()
	<-blocked.entered
	old.Close("replace")
	next := newTestSession(newFakeConn(), "qa-device", "fixture", reg, nil)
	defer next.Close("test")
	reg.Register(next)
	next.handleHeartbeat(env)
	close(blocked.release)
	<-done
	next.handleHeartbeat(env)
	var rows []map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) == nil && row["msg"] == "runtime first heartbeat observed" {
			rows = append(rows, row)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("per-connection summary count=%d", len(rows))
	}
	if rows[0]["connection_id"] == rows[1]["connection_id"] {
		t.Fatal("reconnect reused identity")
	}
	if rows[0]["outcome"] != "unknown" || rows[0]["heartbeat_store_ms"] != nil || rows[0]["capability_hint_ms"] != nil {
		t.Fatal(rows[0])
	}
	if rows[1]["outcome"] != "ok" {
		t.Fatal(rows[1])
	}
	if got, err := reg.LookupDevice("qa-device"); err != nil || got != next {
		t.Fatal("old close displaced replacement", err)
	}
}
