package runtimegateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

var ErrSubscriberOverflow = errors.New("execution subscriber buffer overflow")

type Subscription struct {
	Events             <-chan proto.Envelope
	ch                 chan proto.Envelope
	mu                 sync.Mutex
	err                error
	closed             bool
	received, consumed proto.ExecutionEventsAckPayload
	peer               *Session
	runID              string
}

func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Subscription) closeLocked(err error) {
	if !s.closed {
		s.err, s.closed = err, true
		close(s.ch)
	}
}

// SubscribeDurable reports transport loss and overflow separately from native
// execution events. Callers must inspect Err after Events closes.
func (s *Session) SubscribeDurable(runID string) (*Subscription, error) {
	if runID == "" {
		return nil, fmt.Errorf("agentdaemon gateway: Subscribe requires non-empty runID")
	}
	ch := make(chan proto.Envelope, proto.ExecutionEventWindow)
	sub := &Subscription{Events: ch, ch: ch, peer: s, runID: runID}
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	if s.IsClosed() {
		return nil, ErrSessionClosed
	}
	if existing := s.subs[runID]; existing != nil {
		existing.mu.Lock()
		existing.closeLocked(ErrSessionClosed)
		existing.mu.Unlock()
	}
	s.subs[runID] = sub
	s.reg.AttachRun(runID, s)
	return sub, nil
}

func (s *Session) Unsubscribe(runID string) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	if sub := s.subs[runID]; sub != nil {
		sub.mu.Lock()
		sub.closeLocked(nil)
		sub.mu.Unlock()
		delete(s.subs, runID)
	}
	s.reg.DetachRun(runID)
}

func (s *Session) closeSubscription(sub *Subscription) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed {
		return
	}
	sub.closeLocked(ErrSessionClosed)
}

func (s *Session) dispatchToSubscriber(env proto.Envelope) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	sub := s.subs[env.ID]
	if sub == nil {
		return
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	size := proto.ExecutionEventBytes(env)
	if sub.received.Count-sub.consumed.Count >= proto.ExecutionEventWindow || sub.received.Bytes-sub.consumed.Bytes+size > proto.ExecutionEventWindowBytes {
		sub.closeLocked(ErrSubscriberOverflow)
	} else {
		select {
		case sub.ch <- env:
			sub.received.Count++
			sub.received.Bytes += size
			if env.Type == proto.TypeDone {
				sub.closeLocked(nil)
			}
		default:
			sub.closeLocked(ErrSubscriberOverflow)
		}
	}
	if sub.closed {
		delete(s.subs, env.ID)
		s.reg.DetachRun(env.ID)
	}
}

// Consumed returns transport credit after the caller has processed an event.
// It is not a persistence receipt. Call exactly once per received event.
func (s *Subscription) Consumed(ctx context.Context, env proto.Envelope) error {
	s.mu.Lock()
	s.consumed.Count++
	s.consumed.Bytes += proto.ExecutionEventBytes(env)
	ack := s.consumed
	closed := s.closed
	s.mu.Unlock()
	if closed || env.Type == proto.TypeDone {
		return nil
	}
	frame, _ := proto.NewEnvelope(proto.TypeExecutionEventsAck, s.runID, ack)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := s.peer.Send(ctx, frame)
	if err != nil {
		s.mu.Lock()
		closed = s.closed
		s.mu.Unlock()
		// Terminal arrival can race a credit write. No producer needs credit
		// once the subscription has closed; its own Err retains any failure.
		if closed {
			return nil
		}
	}
	return err
}

// Stop tells a producer blocked on credit that this consumer has finished.
func (s *Subscription) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.peer.Send(ctx, proto.Envelope{Type: proto.TypeExecutionEventsStop, ID: s.runID})
}

// FailureReason contains only finite local categories, never Runtime payloads.
func (s *Subscription) FailureReason() string {
	switch {
	case errors.Is(s.Err(), ErrSubscriberOverflow):
		return "event_window_exceeded"
	case errors.Is(s.Err(), ErrSessionClosed):
		return "connection_closed"
	case s.Err() != nil:
		return "subscription_failed"
	default:
		return ""
	}
}
