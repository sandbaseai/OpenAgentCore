package codex

import (
	"context"
	"errors"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/agent"
	"sync"
	"time"
)

type nativeBoundaryKey struct{}
type nativeBoundary struct {
	mu                                  sync.Mutex
	finished                            bool
	preparation                         string
	input, threadMS, writeMS, forwardMS *float64
	mode                                string
	writeEnd, ackAt, textAt             time.Time
	turnID                              string
	forwarded                           bool
}

func boundaryMS(d time.Duration) *float64 { v := float64(d) / float64(time.Millisecond); return &v }
func newNativeBoundary(ctx context.Context, entered time.Time) *nativeBoundary {
	in := agent.BoundaryInputFrom(ctx)
	o := &nativeBoundary{preparation: in.PreparationID, mode: "unobserved"}
	if !in.Received.IsZero() {
		o.input = boundaryMS(entered.Sub(in.Received))
	}
	return o
}
func withNativeBoundary(ctx context.Context, o *nativeBoundary) context.Context {
	if o == nil {
		return ctx
	}
	return context.WithValue(ctx, nativeBoundaryKey{}, o)
}
func nativeBoundaryFrom(ctx context.Context) *nativeBoundary {
	o, _ := ctx.Value(nativeBoundaryKey{}).(*nativeBoundary)
	return o
}
func (o *nativeBoundary) thread(mode string, started time.Time) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	o.mode = mode
	o.threadMS = boundaryMS(time.Since(started))
}
func (o *nativeBoundary) write(start, end time.Time) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	o.writeMS = boundaryMS(end.Sub(start))
	o.writeEnd = end
}
func (o *nativeBoundary) ack(at time.Time) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.finished {
		o.ackAt = at
	}
}
func (o *nativeBoundary) text(at time.Time, turn string) bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || !o.textAt.IsZero() {
		return false
	}
	o.textAt = at
	o.turnID = turn
	return true
}
func (o *nativeBoundary) forward(start time.Time, sent bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	o.forwardMS = boundaryMS(time.Since(start))
	o.forwarded = sent
}
func (o *nativeBoundary) finish(s *Session, err error) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.finished {
		o.mu.Unlock()
		return
	}
	o.finished = true
	var ackWait *float64
	var before any
	if !o.ackAt.IsZero() && !o.writeEnd.IsZero() {
		early := o.ackAt.Before(o.writeEnd)
		before = early
		if !early {
			ackWait = boundaryMS(o.ackAt.Sub(o.writeEnd))
		}
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	if s.cancelled.Load() || errors.Is(err, context.Canceled) {
		outcome = "cancelled"
	}
	if o.writeMS != nil && o.ackAt.IsZero() {
		outcome = "unknown"
	}
	var preparation, nativeTurn any
	if o.preparation != "" {
		preparation = o.preparation
	}
	s.steering.mu.Lock()
	if o.turnID == "" {
		o.turnID = s.steering.id
	}
	s.steering.mu.Unlock()
	if o.turnID != "" {
		nativeTurn = o.turnID
	}
	fields := []any{"run_id", s.runID, "preparation_request_id", preparation, "native_turn_id", nativeTurn, "input_to_start_turn_ms", boundaryLogDuration(o.input), "input_to_start_turn_observed", o.input != nil, "thread_resolve_ms", boundaryLogDuration(o.threadMS), "thread_resolve_observed", o.threadMS != nil, "thread_mode", o.mode, "turn_start_write_ms", boundaryLogDuration(o.writeMS), "turn_start_write_observed", o.writeMS != nil, "turn_start_ack_wait_ms", boundaryLogDuration(ackWait), "turn_start_ack_observed", !o.ackAt.IsZero(), "ack_before_write_return", before, "first_native_text_to_forward_ms", boundaryLogDuration(o.forwardMS), "first_native_text_observed", !o.textAt.IsZero(), "first_native_text_forwarded", o.forwarded, "outcome", outcome}
	o.mu.Unlock()
	s.cfg.logger.Info("native turn boundary", fields...)
}

// Log scalar values: TextHandler formats non-nil pointers as addresses.
func boundaryLogDuration(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
