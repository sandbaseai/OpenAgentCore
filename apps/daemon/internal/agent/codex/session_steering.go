package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/agent"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

type steeringTurn struct {
	mu      sync.Mutex
	id      string
	stopped bool
	// cancel is a single slot: it assumes at most one turn/steer is in flight.
	// Core's delivery loop holds one pending steer and sends the next only after
	// its acknowledgement. A second concurrent Steer finds the slot taken and
	// gets a definite error instead of silently replacing it; if Core ever sends
	// steers concurrently this must become a set indexed by cancelGen.
	cancel    context.CancelFunc
	cancelGen uint64
	// recovering is true from the moment an upstream recovery takes over a failed
	// Turn until its new Turn is acknowledged or the recovery is abandoned. The
	// failed Turn is already terminal natively, so input is never forwarded to it.
	recovering       bool
	recoveringTurnID string
	// recoveryCancel ends the recovery's own context; it is not the Turn context,
	// whose cancellation would also stop terminal delivery.
	recoveryCancel context.CancelFunc
}

// TurnSteerParams mirrors the native Codex app-server turn/steer request.
type TurnSteerParams struct {
	ThreadID       string      `json:"threadId"`
	ExpectedTurnID string      `json:"expectedTurnId"`
	Input          []UserInput `json:"input"`
}

var _ agent.Steerer = (*Session)(nil)

// Steer returns success only after Codex accepts input for this native turn.
func (s *Session) Steer(ctx context.Context, input proto.PromptSteerPayload) error {
	return s.steer(ctx, input, nil)
}

// SteerWithReceipt waits under the Run context after reporting the complete write.
func (s *Session) SteerWithReceipt(ctx context.Context, input proto.PromptSteerPayload, written func()) error {
	return s.steer(ctx, input, written)
}

func (s *Session) steer(ctx context.Context, input proto.PromptSteerPayload, written func()) error {
	if !s.beginOperation() {
		return agent.ErrSteeringInactive
	}
	defer s.endOperation()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.steering.mu.Lock()
	turnID, stopped, recovering := s.steering.id, s.steering.stopped, s.steering.recovering
	slotTaken := false
	var gen uint64
	if !stopped && !recovering {
		if slotTaken = s.steering.cancel != nil; !slotTaken {
			s.steering.cancelGen++
			gen = s.steering.cancelGen
			s.steering.cancel = cancel
		}
	}
	s.steering.mu.Unlock()
	defer func() {
		if gen == 0 {
			return
		}
		s.steering.mu.Lock()
		// A late exit must not erase a cancel that a later steer registered.
		if s.steering.cancelGen == gen {
			s.steering.cancel = nil
		}
		s.steering.mu.Unlock()
	}()
	if stopped || s.cancelCtx.Err() != nil {
		return agent.ErrSteeringInactive
	}
	if recovering || slotTaken {
		// Nothing was sent; Core keeps the input and offers it again.
		return agent.ErrSteeringNotReady
	}
	threadID := s.currentThreadID()
	if turnID == "" || threadID == "" {
		return agent.ErrSteeringNotReady
	}
	content, err := nativeInput(input.Input)
	if err != nil {
		return agent.ErrSteeringRejected
	}
	params := TurnSteerParams{ThreadID: threadID, ExpectedTurnID: turnID, Input: content}
	timeout := s.rpc.cfg.RequestTimeout
	if written != nil {
		timeout = 0
	}
	raw, err := s.rpc.requestWithTimeout(ctx, "turn/steer", params, func(frame any) error {
		if err := s.rpc.writeFrameContext(ctx, frame); err != nil {
			return err
		}
		if written != nil {
			written()
		}
		return nil
	}, timeout, nil)
	if err != nil {
		if s.steerAgainstRecoveringTurn(turnID) {
			return agent.ErrSteeringNotReady
		}
		var rejected *JsonRpcError
		if errors.As(err, &rejected) {
			return fmt.Errorf("%w: %v", agent.ErrSteeringRejected, err)
		}
		return err
	}
	var result struct {
		TurnID string `json:"turnId"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("codex: decode steering receipt: %w", err)
	}
	if result.TurnID != turnID {
		if s.steerAgainstRecoveringTurn(turnID) {
			return agent.ErrSteeringNotReady
		}
		return fmt.Errorf("codex: steering receipt does not match the active turn")
	}
	return nil
}

// steerAgainstRecoveringTurn reports whether a failed steer targeted the Turn
// that an upstream recovery is replacing. That Turn is already terminal, so no
// failure of the steer says anything about the user's input; it is offered again.
func (s *Session) steerAgainstRecoveringTurn(turnID string) bool {
	s.steering.mu.Lock()
	defer s.steering.mu.Unlock()
	return s.steering.recovering && turnID == s.steering.recoveringTurnID
}

func (s *Session) startSteering(threadID, turnID string) bool {
	if !s.isRootThread(threadID) || turnID == "" {
		return false
	}
	s.steering.mu.Lock()
	defer s.steering.mu.Unlock()
	if s.retiredTurns[turnID] || s.steering.stopped || s.steering.id != "" {
		return false
	}
	s.steering.id = turnID
	return true
}

// isRetired reports whether a Turn id belongs to a Turn this Session replaced.
func (s *Session) isRetired(turnID string) bool {
	s.steering.mu.Lock()
	defer s.steering.mu.Unlock()
	return s.retiredTurns[turnID]
}

// retiredSnapshotPlus copies the retired Turn ids and adds the current Turn so
// a successor Session ignores every Turn this one owned.
func (s *Session) retiredSnapshotPlus() map[string]bool {
	s.steering.mu.Lock()
	defer s.steering.mu.Unlock()
	retired := make(map[string]bool, len(s.retiredTurns)+1)
	for id := range s.retiredTurns {
		retired[id] = true
	}
	retired[s.steering.id] = true
	return retired
}

// stopSteering atomically retains the cancellation target and stops new input.
func (s *Session) stopSteering() (turnID string, active bool) {
	turnID, active, _ = s.stopSteeringForCancel()
	return turnID, active
}

// stopSteeringForCancel is stopSteering for the cancellation path. deferred
// reports that the target is a Turn an upstream recovery is replacing: the
// failed Turn is already terminal natively, and a rebuilt one is not yet bound,
// so there is nothing to interrupt now and the recovery ends itself.
func (s *Session) stopSteeringForCancel() (turnID string, active, deferred bool) {
	s.steering.mu.Lock()
	defer s.steering.mu.Unlock()
	turnID, active = s.steering.id, !s.steering.stopped
	deferred = s.steering.recovering && (turnID == "" || turnID == s.steering.recoveringTurnID)
	s.steering.stopped = true
	if s.steering.cancel != nil {
		s.steering.cancel()
	}
	// Ending the Turn ends any recovery working for it; stopped now answers input.
	s.steering.recovering, s.steering.recoveringTurnID = false, ""
	if s.steering.recoveryCancel != nil {
		s.steering.recoveryCancel()
		s.steering.recoveryCancel = nil
	}
	return turnID, active, deferred
}
