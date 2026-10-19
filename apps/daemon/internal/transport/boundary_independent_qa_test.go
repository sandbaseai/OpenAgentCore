package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestQAHeartbeatFirstFailureThenSuccessStaysSingle(t *testing.T) {
	var logs heartbeatLogBuffer
	c := &Conn{deviceID: "qa-device", connectionID: "qa-connection", sendCh: make(chan envelopeWithAck), closed: make(chan struct{})}
	defer c.Close()
	c.StartHeartbeats(context.Background(), time.Millisecond, func() proto.HeartbeatPayload { return proto.HeartbeatPayload{} }, slog.New(slog.NewJSONHandler(&logs, nil)))
	first := <-c.sendCh
	first.ack <- errors.New("fixture write failed")
	second := <-c.sendCh
	second.ack <- nil
	c.Close()
	count := 0
	for _, line := range strings.Split(string(logs.snapshot()), "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) == nil && row["msg"] == "runtime first heartbeat" {
			count++
			if row["outcome"] != "error" || row["first_heartbeat_send_ms"] == nil {
				t.Fatal(row)
			}
		}
	}
	if count != 1 {
		t.Fatalf("first attempt summaries=%d", count)
	}
}
