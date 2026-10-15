package codex

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestReadStdoutLoopOversizedFrameRetiresClient(t *testing.T) {
	pr, pw := io.Pipe()
	c := NewJSONRPCClient(JSONRPCConfig{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	c.stdout = pr
	c.alive = true
	p := &pendingRequest{resp: make(chan rpcResponse, 1)}
	c.pending["1"] = p
	go func() { _, _ = pw.Write([]byte(strings.Repeat("x", rpcStdoutBufferMax+1024))); _ = pw.Close() }()
	c.readStdoutLoop()
	select {
	case r := <-p.resp:
		if r.err == nil || !strings.Contains(r.err.Error(), "stdout unreadable") {
			t.Fatalf("pending request not failed: %v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending request not drained")
	}
	if c.Alive() {
		t.Fatal("client still alive after oversized frame")
	}
}
