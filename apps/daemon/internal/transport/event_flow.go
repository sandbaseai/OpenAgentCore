package transport

import (
	"context"
	"errors"
	"sync"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

var errEventStreamStopped = errors.New("execution event stream is not active")
var errEventCredit = errors.New("invalid execution event credit")

// Waiting producers never hold the shared WebSocket writer or reader.
type eventFlow struct {
	mu                 sync.Mutex
	sent, acknowledged proto.ExecutionEventsAckPayload
	changed            chan struct{}
	stopped            bool
}

func (f *eventFlow) wakeLocked() { close(f.changed); f.changed = make(chan struct{}) }

func (c *Conn) handleEventFlow(env proto.Envelope) (bool, error) {
	if !c.eventCredit {
		if env.Type == proto.TypeExecutionEventsAck || env.Type == proto.TypeExecutionEventsStop {
			return true, errEventCredit
		}
		return false, nil
	}
	c.flowMu.Lock()
	defer c.flowMu.Unlock()
	switch env.Type {
	case proto.TypeExecutionStart:
		var start proto.ExecutionStartPayload
		if env.DecodePayload(&start) != nil || start.RunID == "" {
			return false, nil
		}
		if c.flows == nil {
			c.flows = make(map[string]*eventFlow)
		}
		if c.flows[start.RunID] == nil {
			if len(c.flows) >= 1024 {
				return true, errEventCredit
			}
			c.flows[start.RunID] = &eventFlow{changed: make(chan struct{})}
		}
		return false, nil
	case proto.TypeExecutionEventsAck, proto.TypeExecutionEventsStop:
		f := c.flows[env.ID]
		if f == nil {
			return true, nil
		} // A terminal Send may already have released it.
		f.mu.Lock()
		defer f.mu.Unlock()
		if env.Type == proto.TypeExecutionEventsStop {
			f.stopped = true
			f.wakeLocked()
			delete(c.flows, env.ID)
			return true, nil
		}
		var ack proto.ExecutionEventsAckPayload
		if env.DecodePayload(&ack) != nil || ack.Count < f.acknowledged.Count || ack.Bytes < f.acknowledged.Bytes || ack.Count > f.sent.Count || ack.Bytes > f.sent.Bytes {
			return true, errEventCredit
		}
		f.acknowledged = ack
		f.wakeLocked()
		return true, nil
	}
	return false, nil
}

func (c *Conn) reserveEvent(ctx context.Context, env proto.Envelope) (*eventReservation, error) {
	if !c.eventCredit || !proto.IsExecutionEvent(env.Type) {
		return nil, nil
	}
	size := proto.ExecutionEventBytes(env)
	if size > proto.ExecutionEventWindowBytes {
		return nil, errEventCredit
	}
	c.flowMu.Lock()
	f := c.flows[env.ID]
	c.flowMu.Unlock()
	if f == nil {
		return nil, errEventStreamStopped
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-c.closed:
			return nil, errClosedOrErr(c)
		default:
		}
		f.mu.Lock()
		if f.stopped {
			f.mu.Unlock()
			return nil, errEventStreamStopped
		}
		if f.sent.Count-f.acknowledged.Count < proto.ExecutionEventWindow && f.sent.Bytes-f.acknowledged.Bytes+size <= proto.ExecutionEventWindowBytes {
			f.sent.Count++
			f.sent.Bytes += size
			f.mu.Unlock()
			return &eventReservation{flow: f, size: size}, nil
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, errClosedOrErr(c)
		}
	}
}

func (c *Conn) finishEventFlow(env proto.Envelope) {
	if !c.eventCredit || env.Type != proto.TypeDone {
		return
	}
	_, _ = c.handleEventFlow(proto.Envelope{Type: proto.TypeExecutionEventsStop, ID: env.ID})
}

// A reservation is refundable only before the frame enters the writer queue.
// Once queued, a cancelled Send has an unknown write outcome and keeps credit.
type eventReservation struct {
	flow *eventFlow
	size uint64
}

func (r *eventReservation) refund() {
	if r == nil {
		return
	}
	r.flow.mu.Lock()
	defer r.flow.mu.Unlock()
	r.flow.sent.Count--
	r.flow.sent.Bytes -= r.size
	r.flow.wakeLocked()
}
