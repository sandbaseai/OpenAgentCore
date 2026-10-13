package runtimegateway

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
)

type artifactTransportStage struct {
	count, failed, active, unknown int
	total, max                     time.Duration
	overflow                       bool
}

func (m *artifactTransportStage) add(d time.Duration, failed bool) {
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
	if failed {
		m.failed++
	}
}

type artifactTransport struct {
	mu                              sync.Mutex
	scope                           textobs.ArtifactScope
	ctx                             context.Context
	connection, preparation, export string
	closed                          bool
	pending                         int
	stages                          [6]artifactTransportStage
}

const (
	artifactQueue = iota
	artifactEncode
	artifactWrite
	artifactDecode
	artifactDispatch
	artifactPredecode
)

// A queued frame carries only private metadata. It is never serialized.
type outboundFrame struct {
	proto.Envelope
	observation *artifactTransport
	queued      atomic.Int64
}

// ObserveArtifactTransport owns exactly one finite snapshot for this capture.
// Registry capacity bounds observation only; failure never limits a business call.
func (s *Session) ObserveArtifactTransport(ctx context.Context, preparation string) func() {
	scope, ok := textobs.Artifact(ctx)
	if !ok || !scope.ClaimTransport() {
		return func() {}
	}
	o := &artifactTransport{scope: scope, ctx: ctx, connection: s.transportConnection, preparation: preparation}
	s.artifactMu.Lock()
	if s.artifactCaptures == nil {
		s.artifactCaptures = make(map[string]*artifactTransport)
		s.artifactIDs = make(map[string]*artifactTransport)
	}
	if len(s.artifactCaptures) >= 64 || s.artifactIDs[preparation] != nil {
		scope.MarkTransportUnobserved()
	} else {
		s.artifactCaptures[scope.Capture] = o
		s.artifactIDs[preparation] = o
	}
	s.artifactMu.Unlock()
	return func() {
		s.artifactMu.Lock()
		if s.artifactCaptures[scope.Capture] == o {
			delete(s.artifactCaptures, scope.Capture)
		}
		if s.artifactIDs[preparation] == o {
			delete(s.artifactIDs, preparation)
		}
		if s.artifactIDs[o.export] == o {
			delete(s.artifactIDs, o.export)
		}
		s.artifactMu.Unlock()
		o.finish()
	}
}

func (s *Session) observeArtifactExport(ctx context.Context, id string) {
	scope, ok := textobs.Artifact(ctx)
	if !ok {
		return
	}
	s.artifactMu.Lock()
	defer s.artifactMu.Unlock()
	o := s.artifactCaptures[scope.Capture]
	if o == nil {
		return
	}
	if o.export != "" || s.artifactIDs[id] != nil {
		scope.MarkTransportUnobserved()
		return
	}
	o.export = id
	s.artifactIDs[id] = o
}
func (s *Session) artifactFor(env proto.Envelope) *artifactTransport {
	switch env.Type {
	case proto.TypeExecutionPrepare, proto.TypeExecutionRelease, proto.TypeWorkspaceExport, proto.TypePreparationStatus, proto.TypeWorkspaceExportResult:
	default:
		return nil
	}
	s.artifactMu.Lock()
	o := s.artifactIDs[env.ID]
	s.artifactMu.Unlock()
	return o
}
func (o *artifactTransport) frame(delta int) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if !o.closed {
		o.pending += delta
	}
	o.mu.Unlock()
}
func (o *artifactTransport) begin(stage int) time.Time {
	if o == nil {
		return time.Time{}
	}
	o.mu.Lock()
	if !o.closed {
		o.stages[stage].active++
	}
	o.mu.Unlock()
	return time.Now()
}
func (o *artifactTransport) end(stage int, started time.Time, err error) {
	if o == nil {
		return
	}
	elapsed := time.Since(started)
	o.mu.Lock()
	if !o.closed {
		o.stages[stage].active--
		o.stages[stage].add(elapsed, err != nil)
	}
	o.mu.Unlock()
}
func (o *artifactTransport) queue(stamp, dequeued int64) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	if stamp == 0 || stamp > dequeued {
		o.stages[artifactQueue].addUnknown(1)
		return
	}
	// Both timestamps come from this process's monotonic clock.
	o.stages[artifactQueue].add(time.Duration(dequeued-stamp), false)
}

var artifactClock = time.Now()

func (o *artifactTransport) finish() {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	stages, pending := o.stages, o.pending
	o.mu.Unlock()
	status := "observed"
	if !o.scope.TransportObserved() {
		status = "unobserved"
	}
	fields := []any{"capture_id", o.scope.Capture, "session_id", o.scope.Session, "turn_id", o.scope.Turn, "connection_id", o.connection, "preparation_request_id", o.preparation, "export_id", o.export, "scope_status", status, "outbound_unfinished_count", pending}
	complete := pending == 0 && status == "observed"
	for i, name := range []string{"outbound_queue_wait", "outbound_encode", "outbound_write", "inbound_decode", "inbound_dispatch", "read_return_to_decode_begin"} {
		m := stages[i]
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
		fields = append(fields, name+"_count", m.count, name+"_total_ms", total, name+"_max_ms", max, name+"_failed_count", m.failed, name+"_unknown_count", unknown, name+"_overflow", m.overflow)
	}
	fields = append(fields, "snapshot_complete", complete)
	obslog.Ctx(o.ctx).Info("artifact transport", fields...)
}

// addUnknown saturates before addition, including unfinished snapshot operations.
func (m *artifactTransportStage) addUnknown(n int) {
	const limit = 1000000
	if m.unknown > limit || n > limit-m.unknown {
		m.unknown = limit
		m.overflow = true
		return
	}
	m.unknown += n
}
