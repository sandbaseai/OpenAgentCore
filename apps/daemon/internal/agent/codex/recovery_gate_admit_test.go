package codex

import (
	"context"
	"encoding/json"
	"testing"
)

// A request admitted on the read loop stays charged to its gate even when the
// gate is detached before the handler goroutine runs.
func TestAdmitAckGateChargesSynchronously(t *testing.T) {
	s := &Session{cancelCtx: context.Background()}
	g := newRecoveryGate("th")
	s.steering.gate = g
	raw := json.RawMessage(`{"threadId":"th"}`)
	wait := s.admitAckGate(raw, "id-1")
	if wait == nil {
		t.Fatal("request of the gated thread was not admitted")
	}
	s.steering.mu.Lock()
	if g.waiters != 1 || g.bytes != len(raw)+len(`"id-1"`) {
		t.Fatalf("waiters=%d bytes=%d, want the request charged", g.waiters, g.bytes)
	}
	s.steering.gate = nil
	g.finishLocked(false)
	s.steering.mu.Unlock()
	if !wait() {
		t.Fatal("released gate refused the request")
	}
	if s.admitAckGate(json.RawMessage(`{"threadId":"other"}`), 1) != nil {
		t.Fatal("foreign thread must bypass the gate")
	}
}
