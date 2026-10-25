package dispatch

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/workspaceobs"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

type workspaceExport struct {
	id          string
	requests    chan *workspaceExportRequest
	observation *workspaceobs.Scope
	preparation *preparationState
	cancel      context.CancelFunc
}

type workspaceExportRequest struct {
	proto.WorkspaceExportPayload
	stamp   workspaceobs.QueueStamp
	arrival context.Context
}

func (r *Router) handleWorkspaceExport(ctx context.Context, env proto.Envelope) error {
	var request proto.WorkspaceExportPayload
	if len(env.ID) == 0 || len(env.ID) > proto.WorkspaceReadMaxIDBytes || len(env.Payload) > proto.WorkspaceReadMaxRequestBytes || env.DecodePayload(&request) != nil || !proto.ValidWorkspaceExportRequest(request) {
		return errors.New("dispatch: invalid workspace export request")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrRouterClosed
	}
	u := r.workspaceExport
	if request.Step != "begin" {
		if u == nil || u.id != env.ID {
			r.mu.Unlock()
			return r.sendWorkspaceExport(ctx, env.ID, proto.WorkspaceExportResultPayload{Outcome: "rejected", ErrorCode: "resource_unavailable"})
		}
		u.observation.Receive(ctx)
		if request.Step == "cancel" {
			u.cancel()
			r.mu.Unlock()
			return nil
		}
		queued := &workspaceExportRequest{WorkspaceExportPayload: request, arrival: ctx}
		select {
		case u.requests <- queued:
			queued.stamp.Admitted()
			r.mu.Unlock()
			return nil
		default:
			u.cancel()
			r.mu.Unlock()
			return errors.New("dispatch: workspace export request already pending")
		}
	}
	code := r.workspaceReadAdmissionLocked(proto.WorkspaceReadPayload{Handle: request.Handle, EnvironmentID: request.EnvironmentID})
	p := r.preparations[request.Handle]
	if u != nil || r.workspaceWrite != nil || !r.localWorkspace.CanExport() || code != "" || p == nil || p.executor != nil {
		r.mu.Unlock()
		return r.sendWorkspaceExport(ctx, env.ID, proto.WorkspaceExportResultPayload{Outcome: "rejected", ErrorCode: "resource_unavailable"})
	}
	// A live export owns its preparation until it settles. An expired timer
	// cannot be reclaimed: its callback may already be waiting for this lock.
	if p.timer != nil && !p.timer.Stop() {
		r.mu.Unlock()
		return r.sendWorkspaceExport(ctx, env.ID, proto.WorkspaceExportResultPayload{Outcome: "rejected", ErrorCode: "resource_unavailable"})
	}
	owner, stop := r.shutdownContext(p.ctx)
	owner, cancel := context.WithTimeout(owner, proto.WorkspaceExportTimeout)
	u = &workspaceExport{id: env.ID, preparation: p, requests: make(chan *workspaceExportRequest, 1), cancel: func() { cancel(); stop() }, observation: workspaceobs.New(ctx, env.ID, "export")}
	queued := &workspaceExportRequest{WorkspaceExportPayload: request, arrival: ctx}
	u.requests <- queued
	queued.stamp.Admitted()
	r.workspaceExport = u
	r.shutdownWG.Add(1)
	r.mu.Unlock()
	go r.runWorkspaceExport(owner, u)
	return nil
}

func (r *Router) runWorkspaceExport(ctx context.Context, u *workspaceExport) {
	defer r.shutdownWG.Done()
	ctx = u.observation.Context(ctx)
	outcome := "cancelled"
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		started := u.observation.Begin(workspaceobs.ExportProducer)
		err := r.localWorkspace.ExportOutputs(ctx, u.observation.Writer(writer))
		u.observation.End(workspaceobs.ExportProducer, started, err)
		_ = writer.CloseWithError(err)
	}()
	defer func() {
		u.cancel()
		_ = reader.Close()
		<-done
		r.mu.Lock()
		if r.workspaceExport == u {
			r.workspaceExport = nil
		}
		p := u.preparation
		if p != nil && p.timer != nil && p.ctx.Err() == nil && p.owns && p.status.State == "ready" {
			// Resume the original idle deadline; export never renews an idle handle.
			p.timer.Reset(max(0, time.Until(p.deadline)))
		}
		r.mu.Unlock()
		u.observation.Finish(outcome)
	}()
	// Closing the reader unblocks a pending pipe read on cancellation or shutdown.
	stopRead := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()) })
	defer stopRead()
	var offset int64
	buffer := make([]byte, proto.WorkspaceExportChunkBytes)
	for {
		roundCtx := ctx
		select {
		case request := <-u.requests:
			request.stamp.Received(u.observation, workspaceobs.RequestQueue)
			roundCtx = u.observation.RoundContext(ctx, request.arrival, request.Step, request.Offset)
			if request.Offset != offset {
				outcome = "invalid_request"
				_ = r.sendWorkspaceExport(roundCtx, u.id, proto.WorkspaceExportResultPayload{Outcome: "failed", Offset: offset, ErrorCode: "invalid_request"})
				return
			}
		case <-ctx.Done():
			outcome = "cancelled"
			return
		}
		result := readWorkspaceExportChunk(workspaceExportSource{Reader: reader, observation: u.observation}, buffer, offset)
		offset += int64(len(result.Data))
		if result.Outcome == "completed" {
			// The next owner may start immediately after receiving completion.
			started := u.observation.Begin(workspaceobs.ExportDone)
			<-done
			u.observation.End(workspaceobs.ExportDone, started, nil)
			r.mu.Lock()
			if r.workspaceExport == u {
				r.workspaceExport = nil
			}
			r.mu.Unlock()
		}
		sendErr := r.sendWorkspaceExport(roundCtx, u.id, result)
		if sendErr != nil {
			outcome = "send_failed"
		} else {
			outcome = result.Outcome
		}
		if sendErr != nil || result.Outcome != "chunk" {
			return
		}
	}
}

// Keep source-read observations independent of transport chunk aggregation.
type workspaceExportSource struct {
	io.Reader
	observation *workspaceobs.Scope
}

func (r workspaceExportSource) Read(p []byte) (int, error) {
	started := r.observation.Begin(workspaceobs.ExportRead)
	n, err := r.Reader.Read(p)
	observedErr := err
	if err == io.EOF {
		observedErr = nil
	}
	r.observation.End(workspaceobs.ExportRead, started, observedErr)
	r.observation.Data(n, err == io.EOF)
	return n, err
}

func readWorkspaceExportChunk(reader io.Reader, buffer []byte, offset int64) proto.WorkspaceExportResultPayload {
	result := proto.WorkspaceExportResultPayload{Offset: offset}
	var size int
	for size < len(buffer) {
		n, err := reader.Read(buffer[size:])
		size += n
		// Preserve the source error: io.ReadFull cannot distinguish a clean tail
		// from an exporter that itself fails with io.ErrUnexpectedEOF.
		if (err != nil && err != io.EOF) || (n == 0 && err == nil) || int64(size) > proto.WorkspaceExportMaxBytes-offset {
			result.Outcome, result.ErrorCode = "failed", "export_failed"
			return result
		}
		if err == io.EOF {
			if size == 0 {
				result.Outcome = "completed"
				return result
			}
			break
		}
	}
	result.Outcome, result.Data = "chunk", buffer[:size]
	return result
}

func (r *Router) sendWorkspaceExport(ctx context.Context, id string, result proto.WorkspaceExportResultPayload) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	env, err := proto.NewEnvelope(proto.TypeWorkspaceExportResult, id, result)
	if err != nil {
		return err
	}
	workspaceobs.RoundFromContext(ctx).SetResponse(result.Outcome)
	observer := workspaceobs.FromContext(ctx)
	started := observer.Begin(workspaceobs.Sender)
	err = r.sender.Send(ctx, env)
	observer.End(workspaceobs.Sender, started, err)
	return err
}
