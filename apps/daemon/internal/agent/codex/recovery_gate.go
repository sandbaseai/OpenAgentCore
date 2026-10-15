package codex

import (
	"encoding/json"
	"errors"
)

// The acknowledgement gate holds everything a rebuilt Turn sends before its
// turn/start reply is acknowledged. Native may start the Turn, stream items and
// ask for application-side function calls ahead of that reply. If the reply then
// fails, none of it may have been published or executed, or a later retry would
// repeat it. Held notifications are replayed in arrival order once the recovery
// is committed; held function calls wait for the same moment. An abandoned
// recovery discards the lot.
//
// All gate state is guarded by steeringTurn.mu, so it adds no lock ordering.
const (
	maxGatedFrames = 4096
	maxGatedBytes  = 8 << 20
)

var errRecoveryGateOverflow = errors.New("codex: rebuilt turn traffic exceeded the acknowledgement buffer")

type gatedFrame struct {
	handler NotificationHandler
	raw     json.RawMessage
}

type recoveryGate struct {
	threadID string
	queue    []gatedFrame
	bytes    int
	waiters  int
	// saw reports that the rebuilt thread sent anything before the reply.
	saw      bool
	overflow bool
	aborted  bool
	released chan struct{}
	done     bool
}

func newRecoveryGate(threadID string) *recoveryGate {
	return &recoveryGate{threadID: threadID, released: make(chan struct{})}
}

// finishLocked releases the waiting function calls. An aborted gate drops what
// it held and makes the waiters fail. Idempotent.
func (g *recoveryGate) finishLocked(abort bool) {
	if g.done {
		return
	}
	g.done = true
	g.aborted = abort
	if abort {
		g.queue, g.bytes = nil, 0
	}
	close(g.released)
}

// overflowLocked stops buffering and asks the recovery to give up. The caller
// holds steering.mu.
func (s *Session) overflowLocked(g *recoveryGate) {
	if g.overflow {
		return
	}
	g.overflow = true
	g.queue, g.bytes = nil, 0
	s.cfg.logger.Warn("codex: rebuilt turn traffic exceeded the acknowledgement buffer", "run_id", s.runID)
	if s.steering.recoveryCancel != nil {
		s.steering.recoveryCancel()
	}
}

// gated wraps a notification handler so traffic of a rebuilt thread is held
// until its turn/start reply is acknowledged.
func (s *Session) gated(h NotificationHandler) NotificationHandler {
	return func(raw json.RawMessage) {
		s.steering.mu.Lock()
		g := s.steering.gate
		s.steering.mu.Unlock()
		if g == nil {
			h(raw)
			return
		}
		var scope struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(raw, &scope)
		s.steering.mu.Lock()
		if s.steering.gate == g && scope.ThreadID != "" && scope.ThreadID == g.threadID {
			g.saw = true
			if !g.overflow {
				if len(g.queue)+g.waiters >= maxGatedFrames || g.bytes+len(raw) > maxGatedBytes {
					s.overflowLocked(g)
				} else {
					g.queue = append(g.queue, gatedFrame{handler: h, raw: append(json.RawMessage(nil), raw...)})
					g.bytes += len(raw)
				}
			}
			s.steering.mu.Unlock()
			return
		}
		s.steering.mu.Unlock()
		h(raw)
	}
}

// awaitAckGate blocks a server request of the rebuilt thread until the
// recovery is acknowledged. It reports false when the request must be refused:
// the recovery was abandoned or the Session was cancelled.
func (s *Session) awaitAckGate(raw json.RawMessage) bool {
	var scope struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(raw, &scope)
	s.steering.mu.Lock()
	g := s.steering.gate
	if g == nil || scope.ThreadID == "" || scope.ThreadID != g.threadID {
		s.steering.mu.Unlock()
		return true
	}
	g.saw = true
	// Once the gate has overflowed the recovery is being torn down: refuse
	// immediately instead of parking another goroutine and its params.
	if g.overflow {
		s.steering.mu.Unlock()
		return false
	}
	g.waiters++
	// A waiting request retains its raw params, so it counts against the byte limit too.
	g.bytes += len(raw)
	if len(g.queue)+g.waiters > maxGatedFrames || g.bytes > maxGatedBytes {
		s.overflowLocked(g)
		g.waiters--
		s.steering.mu.Unlock()
		return false
	}
	s.steering.mu.Unlock()
	select {
	case <-g.released:
	case <-s.cancelCtx.Done():
		return false
	}
	s.steering.mu.Lock()
	defer s.steering.mu.Unlock()
	return !g.aborted
}

// dropGateLocked discards the gate of an abandoned recovery.
func (s *Session) dropGateLocked() {
	if g := s.steering.gate; g != nil {
		g.finishLocked(true)
		s.steering.gate = nil
	}
	s.steering.replaying = false
}

// replayGated feeds held notifications to their handlers in arrival order.
func (s *Session) replayGated(queue []gatedFrame) {
	for _, frame := range queue {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.cfg.logger.Error("codex: panic replaying held notification", "run_id", s.runID, "panic", r)
				}
			}()
			frame.handler(frame.raw)
		}()
	}
}
