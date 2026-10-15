package codex

import (
	"context"
	"fmt"
	"strings"
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
		ThreadID: s.currentThreadID(),
		Input:    input,
	}
	collaboration, err := s.collaborationMode(plan)
	if err != nil {
		return err
	}
	turnParams.CollaborationMode = collaboration
	turnCtx, turnCancel := context.WithTimeout(ctx, 10*time.Second)
	_, ackErr := s.rpc.requestWithResult(withNativeBoundary(turnCtx, s.boundary), "turn/start", turnParams, s.bindTurnResult)
	turnCancel()
	if ackErr != nil {
		s.cfg.logger.Warn("codex: turn/start ack failed", "run_id", s.runID, "err", ackErr)
		return fmt.Errorf("codex: turn/start: %w", ackErr)
	}

	return nil
}

// collaborationMode derives the per-Turn collaboration settings from the plan.
func (s *Session) collaborationMode(plan SessionPlan) (*CollaborationMode, error) {
	model := strings.TrimSpace(s.resolvedModel)
	if model == "" {
		return nil, fmt.Errorf("codex: collaboration mode requires a resolved model")
	}
	var developerInstructions *string
	if plan.SystemPrompt != "" {
		developerInstructions = &plan.SystemPrompt
	}
	return &CollaborationMode{
		Mode: CollaborationModeDefault,
		Settings: CollaborationModeSettings{
			ReasoningEffort:       plan.ModelReasoningEffort,
			Model:                 model,
			DeveloperInstructions: developerInstructions,
		},
	}, nil
}
