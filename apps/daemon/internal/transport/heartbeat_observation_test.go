package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type heartbeatLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *heartbeatLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *heartbeatLogBuffer) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.Buffer.Bytes()...)
}
func TestFirstHeartbeatObservationActualSend(t *testing.T) {
	var logs heartbeatLogBuffer
	c := &Conn{deviceID: "11111111-1111-4111-8111-111111111111", connectionID: "22222222-2222-4222-8222-222222222222", sendCh: make(chan envelopeWithAck), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartHeartbeats(ctx, time.Hour, func() proto.HeartbeatPayload { time.Sleep(12 * time.Millisecond); return proto.HeartbeatPayload{} }, slog.New(slog.NewJSONHandler(&logs, nil)))
	frame := <-c.sendCh
	time.Sleep(12 * time.Millisecond)
	frame.ack <- nil
	deadline := time.Now().Add(time.Second)
	for {
		c.firstHeartbeat.mu.Lock()
		done := c.firstHeartbeat.finished
		c.firstHeartbeat.mu.Unlock()
		if done && len(logs.snapshot()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not finished")
		}
		time.Sleep(time.Millisecond)
	}
	c.Close()
	t.Logf("OBSERVATION_FIXTURE %s", bytes.TrimSpace(logs.snapshot()))
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.snapshot()), &row); err != nil {
		t.Fatal(err)
	}
	if row["capability_payload_build_ms"].(float64) < 10 || row["first_heartbeat_send_ms"].(float64) < 10 || row["outcome"] != "ok" {
		t.Fatal(row)
	}
}

func TestFirstHeartbeatCloseDuringPayload(t *testing.T) {
	var logs heartbeatLogBuffer
	c := &Conn{deviceID: "11111111-1111-4111-8111-111111111111", connectionID: "22222222-2222-4222-8222-222222222222", sendCh: make(chan envelopeWithAck), closed: make(chan struct{})}
	entered, release := make(chan struct{}), make(chan struct{})
	c.StartHeartbeats(context.Background(), time.Hour, func() proto.HeartbeatPayload { close(entered); <-release; return proto.HeartbeatPayload{} }, slog.New(slog.NewJSONHandler(&logs, nil)))
	<-entered
	c.Close()
	close(release)
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.snapshot()), &row); err != nil {
		t.Fatal(err)
	}
	if row["outcome"] != "unknown" || row["capability_payload_build_ms"] != nil || row["first_heartbeat_send_ms"] != nil {
		t.Fatal(row)
	}
	t.Logf("OBSERVATION_FIXTURE %s", bytes.TrimSpace(logs.snapshot()))
}

func TestHeartbeatTextHandlerScalarFixture(t *testing.T) {
	var logs bytes.Buffer
	c := &Conn{deviceID: "11111111-1111-4111-8111-111111111111", connectionID: "22222222-2222-4222-8222-222222222222"}
	c.firstHeartbeat.begin(slog.New(slog.NewTextHandler(&logs, nil)))
	c.firstHeartbeat.record(true, time.Now().Add(-time.Millisecond))
	c.firstHeartbeat.finish(c, "unknown")
	line := logs.String()
	if !strings.Contains(line, "capability_payload_build_ms=1") || !strings.Contains(line, "first_heartbeat_send_ms=<nil>") || strings.Contains(line, "0x") {
		t.Fatal(line)
	}
	t.Logf("OBSERVATION_TEXT_FIXTURE %s", strings.TrimSpace(line))
}
