package execution

import (
	"context"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func (d *Dispatcher) readPreparedDirectory(ctx context.Context, peer *runtimegateway.Session, session sessions.Session, environment sessions.Environment, bound sessions.ExecutionDevice, read proto.WorkspaceReadPayload) directoryReadResult {
	owner, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var result directoryReadResult
	err := d.withPreparedWorkspace(owner, peer, session, environment, bound, func(ctx context.Context, handle string) error {
		read.Handle = handle
		result = readEnvironmentDirectory(ctx, peer, read)
		return result.err
	})
	if err != nil {
		return directoryReadResult{err: err}
	}
	return result
}

func (d *Dispatcher) withPreparedWorkspace(owner context.Context, peer *runtimegateway.Session, session sessions.Session, environment sessions.Environment, bound sessions.ExecutionDevice, consume func(context.Context, string) error) error {
	prepareStarted := time.Now()
	prepareObserved := false
	defer func() {
		if !prepareObserved {
			observeArtifactWorkspace(owner, "artifact_workspace_prepare", prepareStarted, ErrExecutionUnavailable)
		}
	}()
	req := proto.PromptRequestPayload{AgentKind: session.Engine, AgentStateKey: "agents-api-" + session.ID, WorkspaceReadOnly: true}
	if err := d.configurePreparedEnvironment(session, environment, bound, &req); err != nil {
		return ErrExecutionUnavailable
	}
	prepared, err := newPreparedStart(owner, peer)
	if err != nil {
		return ErrExecutionUnavailable
	}
	finishTransport := peer.ObserveArtifactTransport(owner, prepared.requestID)
	defer finishTransport()
	releaseAttempted := false
	defer func() {
		if !releaseAttempted {
			cleanupStarted := time.Now()
			prepared.close()
			observeArtifactWorkspace(owner, "artifact_workspace_cleanup", cleanupStarted, nil, "confirmation", "unobserved")
		} else {
			peer.UnsubscribePreparation(prepared.requestID)
		}
	}()
	prepare, stop := context.WithTimeout(owner, 10*time.Second)
	err = send(prepare, peer, proto.TypeExecutionPrepare, prepared.requestID, proto.ExecutionPreparePayload{SessionID: session.ID, Configuration: req})
	if err == nil {
		err = prepared.awaitDirectoryReady(prepare)
	}
	stop()
	observeArtifactWorkspace(owner, "artifact_workspace_prepare", prepareStarted, err)
	prepareObserved = true
	if err != nil {
		return ErrExecutionUnavailable
	}
	err = consume(owner, prepared.handle)
	releaseAttempted = true
	releaseStarted := time.Now()
	releaseErr := prepared.releaseDirectory()
	observeArtifactWorkspace(owner, "artifact_workspace_release", releaseStarted, releaseErr)
	if releaseErr != nil {
		return ErrExecutionUnavailable
	}
	return err
}

func (p *preparedStart) awaitDirectoryReady(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ErrExecutionUnavailable
		case env, ok := <-p.sub.Events:
			if !ok {
				return ErrExecutionUnavailable
			}
			status, err := p.observation(env)
			if err != nil || status.RunID != "" {
				return ErrExecutionUnavailable
			}
			if status.State == "ready" {
				return nil
			}
			if status.State != "preparing" {
				return ErrExecutionUnavailable
			}
		}
	}
}

func (p *preparedStart) releaseDirectory() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if send(ctx, p.peer, proto.TypeExecutionRelease, p.requestID, proto.ExecutionReleasePayload{Handle: p.handle}) != nil {
		return ErrExecutionUnavailable
	}
	for {
		select {
		case <-ctx.Done():
			return ErrExecutionUnavailable
		case env, ok := <-p.sub.Events:
			if !ok {
				return ErrExecutionUnavailable
			}
			status, err := p.controlStatus(env)
			if err != nil || status.RunID != "" {
				return ErrExecutionUnavailable
			}
			// A read-only preparation can expire after a confirmed export. Both
			// terminal receipts confirm cleanup without reviving its authority.
			if (status.State == "released" || status.State == "expired") && status.ErrorCode == "" {
				return nil
			}
			if status.State != "preparing" && status.State != "ready" {
				return ErrExecutionUnavailable
			}
		}
	}
}
