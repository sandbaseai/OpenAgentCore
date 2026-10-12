package runtimegateway

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
	"github.com/google/uuid"
)

// ExportWorkspaceOutputs consumes bounded chunks and requires the exporter completion receipt.
func (s *Session) ExportWorkspaceOutputs(ctx context.Context, request proto.WorkspaceExportPayload, consume func(io.Reader) error) error {
	request.Step = "begin"
	if !proto.ValidWorkspaceExportRequest(request) || consume == nil {
		return errors.New("agentdaemon gateway: invalid workspace export")
	}
	id := uuid.NewString()
	s.workspaceExportMu.Lock()
	if s.IsClosed() {
		s.workspaceExportMu.Unlock()
		return ErrSessionClosed
	}
	if len(s.workspaceExports) != 0 {
		s.workspaceExportMu.Unlock()
		return errors.New("agentdaemon gateway: workspace export capacity")
	}
	replies := make(chan proto.Envelope, 1)
	s.workspaceExports = map[string]chan proto.Envelope{id: replies}
	s.workspaceExportMu.Unlock()
	defer func() { s.workspaceExportMu.Lock(); delete(s.workspaceExports, id); s.workspaceExportMu.Unlock() }()
	s.observeArtifactExport(ctx, id)
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	r := &workspaceExportReader{ctx: ctx, peer: s, id: id, replies: replies, request: request}
	var outcome error
	if scope, ok := textobs.Artifact(ctx); ok {
		r.observation = &workspaceExportObservation{started: time.Now()}
		defer func() { r.observe(scope, outcome) }()
	}
	defer func() {
		if !r.completed {
			stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			env, _ := proto.NewEnvelope(proto.TypeWorkspaceExport, id, proto.WorkspaceExportPayload{Step: "cancel"})
			started := time.Now()
			cancelErr := s.Send(stop, env)
			if r.observation != nil {
				r.observation.cancelEnqueueMS = float64(time.Since(started).Microseconds()) / 1000
				r.observation.cancelEnqueued = cancelErr == nil
			}
		}
	}()
	outcome = consume(r)
	if outcome != nil {
		return outcome
	}
	// Archive decoders may stop at their trailer before the native exit receipt.
	_, outcome = io.Copy(io.Discard, r)
	return outcome
}

type workspaceExportReader struct {
	ctx         context.Context
	peer        *Session
	id          string
	replies     <-chan proto.Envelope
	request     proto.WorkspaceExportPayload
	data        []byte
	offset      int64
	completed   bool
	observation *workspaceExportObservation
}

func (r *workspaceExportReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		if r.observation != nil {
			r.observation.delivered += int64(n)
		}
		return n, nil
	}
	if r.completed {
		return 0, io.EOF
	}
	env, err := proto.NewEnvelope(proto.TypeWorkspaceExport, r.id, r.request)
	if err != nil {
		return 0, err
	}
	started := time.Now()
	err = r.peer.Send(r.ctx, env)
	if o := r.observation; o != nil {
		o.enqueue += time.Since(started)
		if r.request.Step == "begin" {
			o.begin++
		} else {
			o.next++
		}
	}
	if err != nil {
		return 0, err
	}
	env, err = r.receipt()
	if err != nil {
		return 0, err
	}
	var result proto.WorkspaceExportResultPayload
	if len(env.Payload) > proto.WorkspaceExportMaxFrameBytes || env.DecodePayload(&result) != nil || result.Offset != r.offset {
		return 0, errors.New("agentdaemon gateway: invalid export receipt")
	}
	if result.Outcome == "completed" && len(result.Data) == 0 && result.ErrorCode == "" {
		r.completed = true
		if o := r.observation; o != nil {
			o.completedMS = float64(time.Since(o.started).Microseconds()) / 1000
		}
		return 0, io.EOF
	}
	if result.Outcome != "chunk" || result.ErrorCode != "" || len(result.Data) == 0 || len(result.Data) > proto.WorkspaceExportChunkBytes || int64(len(result.Data)) > proto.WorkspaceExportMaxBytes-r.offset {
		return 0, errors.New("agentdaemon gateway: workspace export incomplete")
	}
	if o := r.observation; o != nil {
		if o.chunks == 0 {
			o.firstDataMS = float64(time.Since(o.started).Microseconds()) / 1000
		}
		o.chunks++
		o.received += int64(len(result.Data))
	}
	r.data = result.Data
	r.offset += int64(len(result.Data))
	r.request = proto.WorkspaceExportPayload{Step: "next", Offset: r.offset}
	return r.Read(p)
}

func (r *workspaceExportReader) receipt() (proto.Envelope, error) {
	started := time.Now()
	defer func() {
		if o := r.observation; o != nil {
			elapsed := time.Since(started)
			o.wait += elapsed
			if elapsed > o.waitMax {
				o.waitMax = elapsed
			}
			o.waits++
		}
	}()
	select {
	case env, ok := <-r.replies:
		if !ok {
			return proto.Envelope{}, ErrSessionClosed
		}
		return env, nil
	case <-r.ctx.Done():
		return proto.Envelope{}, r.ctx.Err()
	case <-r.peer.closed:
		return proto.Envelope{}, ErrSessionClosed
	}
}

// One summary per registered export, never per chunk or Read call. Send only
// enqueues a frame; receipt wait includes remote work and transport scheduling.
type workspaceExportObservation struct {
	started                                   time.Time
	enqueue, wait, waitMax                    time.Duration
	begin, next, waits, chunks                int
	received, delivered                       int64
	firstDataMS, completedMS, cancelEnqueueMS any
	cancelEnqueued                            bool
}

func (r *workspaceExportReader) observe(scope textobs.ArtifactScope, err error) {
	o := r.observation
	status := "ok"
	switch {
	case errors.Is(err, context.Canceled):
		status = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		status = "timeout"
	case err != nil:
		status = "error"
	}
	obslog.Ctx(r.ctx).Info("artifact export reader", "session_id", scope.Session, "turn_id", scope.Turn, "capture_id", scope.Capture,
		"export_id", r.id, "status", status, "duration_ms", float64(time.Since(o.started).Microseconds())/1000,
		"enqueue_ms", float64(o.enqueue.Microseconds())/1000, "begin_count", o.begin, "next_count", o.next,
		"receipt_wait_ms", float64(o.wait.Microseconds())/1000, "receipt_wait_max_ms", float64(o.waitMax.Microseconds())/1000, "receipt_wait_count", o.waits,
		"chunk_count", o.chunks, "received_bytes", o.received, "delivered_bytes", o.delivered,
		"first_data_ms", o.firstDataMS, "completed_receipt_ms", o.completedMS, "completed", r.completed,
		"cancel_enqueue_ms", o.cancelEnqueueMS, "cancel_enqueued", o.cancelEnqueued)
}

func (s *Session) dispatchWorkspaceExport(env proto.Envelope) {
	s.workspaceExportMu.Lock()
	defer s.workspaceExportMu.Unlock()
	if replies := s.workspaceExports[env.ID]; replies != nil {
		select {
		case replies <- env:
		default:
			close(replies)
			delete(s.workspaceExports, env.ID)
		}
	}
}

func (s *Session) closeWorkspaceExports() {
	s.workspaceExportMu.Lock()
	defer s.workspaceExportMu.Unlock()
	for id, replies := range s.workspaceExports {
		close(replies)
		delete(s.workspaceExports, id)
	}
}
