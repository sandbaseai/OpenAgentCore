// Package workspaceobs holds bounded, process-local workspace diagnostics.
// It has no wire representation, registry, background worker or configuration.
package workspaceobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/google/uuid"
)

type stage int

const (
	ReceiveDispatch stage = iota
	RequestQueue
	ExportRead
	ExportDone
	Sender
	OutboundQueue
	OutboundEncode
	OutboundWrite
	PrepareLocal
	ReleaseLocal
	ExportProducer
	ReadPredecode
	InboundDecode
	ExportWriter
	stageCount
)

var names = [stageCount]string{"receive_dispatch_wait", "request_queue_wait", "export_read", "export_done_wait", "sender_call", "outbound_queue_wait", "outbound_encode", "outbound_write", "prepare_local", "release_local", "export_producer", "read_return_to_decode_begin", "inbound_decode", "export_writer"}

type metric struct {
	count, active, failed, cancelled, unknown int
	total, max                                time.Duration
	overflow                                  bool
}
type arrivalKey struct{}
type scopeKey struct{}
type arrival struct {
	connection                           string
	delay                                time.Duration
	readReturned, decodeStarted, decoded time.Time
}

// ReceivedContext is called exactly when an internal receive carrier is consumed.
// It retains no envelope, payload or business state.
func ReceivedContext(ctx context.Context, connection string, decoded time.Time) context.Context {
	return ReceivedFrameContext(ctx, connection, time.Time{}, time.Time{}, decoded)
}
func ReceivedFrameContext(ctx context.Context, connection string, readReturned, decodeStarted, decoded time.Time) context.Context {
	return context.WithValue(ctx, arrivalKey{}, arrival{connection: connection, delay: time.Since(decoded), readReturned: readReturned, decodeStarted: decodeStarted, decoded: decoded})
}

type Scope struct {
	mu                   sync.Mutex
	ctx                  context.Context
	id, kind, connection string
	mismatch, closed     bool
	transportObserved    bool
	identityValid        bool
	metrics              [stageCount]metric
	bytes, chunks        int64
	completed            bool
	pending              int
	rounds               []*Round
	roundCount           int
	roundsTruncated      bool
	roundMetric          metric
}

func New(ctx context.Context, id, kind string) *Scope {
	a, _ := ctx.Value(arrivalKey{}).(arrival)
	// Protocol business validation is unchanged. Only canonical locally-generated
	// UUID identities are safe for this diagnostic field; arbitrary peer text is omitted.
	parsed, parseErr := uuid.Parse(id)
	valid := parseErr == nil && parsed.String() == id
	if !valid {
		id = ""
	}
	s := &Scope{ctx: ctx, id: id, kind: kind, connection: a.connection, identityValid: valid}
	s.Receive(ctx)
	return s
}
func (s *Scope) Context(ctx context.Context) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, s)
}
func FromContext(ctx context.Context) *Scope { s, _ := ctx.Value(scopeKey{}).(*Scope); return s }
func ForConnection(ctx context.Context, connection string) *Scope {
	s, _ := ctx.Value(scopeKey{}).(*Scope)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connection == "" || s.connection != connection {
		s.mismatch = true
		return nil
	}
	s.transportObserved = true
	return s
}
func (s *Scope) Receive(ctx context.Context) {
	if s == nil {
		return
	}
	a, ok := ctx.Value(arrivalKey{}).(arrival)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if !ok || a.connection == "" || a.connection != s.connection {
		s.metrics[ReceiveDispatch].addUnknown(1)
		s.mismatch = true
		return
	}
	s.add(ReceiveDispatch, a.delay, nil)
	if !a.readReturned.IsZero() && !a.decodeStarted.IsZero() {
		s.add(ReadPredecode, a.decodeStarted.Sub(a.readReturned), nil)
		s.add(InboundDecode, a.decoded.Sub(a.decodeStarted), nil)
	} else {
		s.metrics[ReadPredecode].addUnknown(1)
		s.metrics[InboundDecode].addUnknown(1)
	}
}
func (s *Scope) add(k stage, d time.Duration, err error) {
	m := &s.metrics[k]
	if d < 0 || d > 30*time.Minute || m.total > 30*time.Minute-d || m.count >= 1000000 {
		m.addUnknown(1)
		m.overflow = true
		return
	}
	m.count++
	m.total += d
	if d > m.max {
		m.max = d
	}
	if err != nil {
		m.failed++
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		m.cancelled++
	}
}
func (s *Scope) Begin(k stage) time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.Lock()
	if !s.closed {
		s.metrics[k].active++
	}
	s.mu.Unlock()
	return time.Now()
}
func (s *Scope) End(k stage, start time.Time, err error) {
	if s == nil {
		return
	}
	d := time.Since(start)
	s.mu.Lock()
	if !s.closed {
		s.metrics[k].active--
		s.add(k, d, err)
	}
	s.mu.Unlock()
}
func (s *Scope) Elapsed(k stage, start time.Time) {
	if s == nil {
		return
	}
	d := time.Since(start)
	s.mu.Lock()
	if !s.closed {
		s.add(k, d, nil)
	}
	s.mu.Unlock()
}
func (s *Scope) Data(n int, completed bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.bytes += int64(n)
		if n > 0 {
			s.chunks++
		}
		s.completed = s.completed || completed
	}
	s.mu.Unlock()
}

var clock = time.Now()

// QueueStamp travels with one private queue element. A receiver that runs before
// the sender records successful admission reports unknown, never waits.
type QueueStamp struct{ admitted atomic.Int64 }

func (q *QueueStamp) Admitted() {
	if q != nil {
		q.admitted.Store(int64(time.Since(clock)))
	}
}
func (q *QueueStamp) Received(s *Scope, k stage) {
	if q == nil || s == nil {
		return
	}
	end := int64(time.Since(clock))
	start := q.admitted.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if start == 0 || start > end {
		s.metrics[k].addUnknown(1)
		return
	}
	s.add(k, time.Duration(end-start), nil)
}

func (s *Scope) Pending(delta int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.pending += delta
	}
	s.mu.Unlock()
}

func (s *Scope) Finish(outcome string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	roundFields := s.roundFieldsLocked()
	roundsComplete := s.roundMetric.active == 0 && s.roundMetric.unknown == 0 && !s.roundMetric.overflow
	metrics, mismatch, bytes, chunks, completed, pending, transportObserved := s.metrics, s.mismatch, s.bytes, s.chunks, s.completed, s.pending, s.transportObserved
	s.mu.Unlock()
	fields := []any{"connection_id", s.connection, "scope", s.kind, "identity_valid", s.identityValid, "outcome", outcome, "connection_mismatch", mismatch, "read_bytes", bytes, "chunk_count", chunks, "local_eof", completed, "outbound_unfinished_count", pending, "transport_observed", transportObserved}
	if s.kind == "export" {
		fields = append(fields, "export_id", s.id)
	} else {
		fields = append(fields, "preparation_request_id", s.id)
	}
	complete := s.identityValid && !mismatch && pending == 0 && outcome != "shutdown" && outcome != "suspended" && (metrics[Sender].count == 0 || transportObserved)
	for k, name := range names {
		m := metrics[k]
		var total, max any
		if m.count > 0 {
			total = float64(m.total.Microseconds()) / 1000
			max = float64(m.max.Microseconds()) / 1000
		}
		m.addUnknown(m.active)
		unknown := m.unknown
		if unknown > 0 {
			complete = false
		}
		fields = append(fields, name+"_count", m.count, name+"_total_ms", total, name+"_max_ms", max, name+"_failed_count", m.failed, name+"_cancelled_count", m.cancelled, name+"_unknown_count", unknown, name+"_overflow", m.overflow)
	}
	fields = append(fields, "export_writer_observed", metrics[ExportWriter].count > 0)
	fields = append(fields, roundFields...)
	fields = append(fields, "snapshot_complete", complete && roundsComplete)
	obslog.Ctx(s.ctx).Info("runtime workspace transport", fields...)
}

// addUnknown saturates before addition, including unfinished snapshot operations.
func (m *metric) addUnknown(n int) {
	const limit = 1000000
	if m.unknown > limit || n > limit-m.unknown {
		m.unknown = limit
		m.overflow = true
		return
	}
	m.unknown += n
}
