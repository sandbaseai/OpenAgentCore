package codex

import (
	"context"
	"fmt"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func (s *Session) startNative(ctx context.Context, plan SessionPlan, req proto.PromptRequestPayload) error {
	threadStarted := time.Now()
	mode := "reuse"
	if s.currentThreadID() == "" {
		mode = "start"
		if req.AgentSessionID != "" || req.RequireExistingNativeSession {
			mode = "resume"
		}
		if err := s.resolveThread(req, plan); err != nil {
			s.boundary.thread(mode, threadStarted)
			return err
		}
	}
	s.boundary.thread(mode, threadStarted)

	input, err := nativeInput(req.Input)
	if err != nil {
		return err
	}
	turnParams := TurnStartParams{
		ThreadID:     s.currentThreadID(),
		Input:        input,
		OutputSchema: plan.OutputSchema,
	}
	turnCtx, turnCancel := context.WithTimeout(ctx, 10*time.Second)
	_, ackErr := s.rpc.requestWithResult(withNativeBoundary(turnCtx, s.boundary), "turn/start", turnParams, s.bindTurnResult)
	turnCancel()
	if ackErr != nil {
		s.cfg.logger.Warn("codex: turn/start ack failed", "run_id", s.runID, "err", ackErr)
		return fmt.Errorf("codex: turn/start: %w", ackErr)
	}

	return nil
}
