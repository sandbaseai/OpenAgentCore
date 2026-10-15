package codex

import (
	"sync"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

type cancellationOutcomeState struct {
	notificationMu sync.Mutex
	mu             sync.Mutex
	terminal       *proto.DonePayload
}

func (s *Session) rememberOutcome(outcome proto.DonePayload) proto.DonePayload {
	s.usageMu.Lock()
	if usage := s.effectiveUsageLocked(); outcome.Usage.Provider == "" && usage != nil {
		outcome.Usage = s.usagePayload(*usage)
	}
	s.usageMu.Unlock()
	s.outcome.mu.Lock()
	s.outcome.terminal = &outcome
	s.outcome.mu.Unlock()
	return outcome
}

// CancellationOutcome remains readable after Cancel stops the native process.
func (s *Session) CancellationOutcome() proto.DonePayload {
	s.outcome.notificationMu.Lock()
	defer s.outcome.notificationMu.Unlock()
	s.outcome.mu.Lock()
	terminal := s.outcome.terminal
	s.outcome.mu.Unlock()
	if terminal != nil {
		return *terminal
	}
	outcome := proto.DonePayload{Metadata: map[string]any{}}
	if id := s.reportedThread(); id != "" {
		outcome.Metadata[proto.DoneMetaAgentSessionID] = id
		outcome.Metadata[proto.DoneMetaAgentSessionType] = "codex_thread"
	}
	s.attachRecoveries(outcome.Metadata, false)
	s.finalTextMu.Lock()
	outcome.Content = s.finalText
	s.finalTextMu.Unlock()
	if outcome.Content == "" {
		// While a recovery works, the failed Turn's streamed text waits in its
		// snapshot; a newer Turn's own buffer, once non-empty, wins above.
		outcome.Content = s.recoverySnapshotFinalText()
	}
	s.usageMu.Lock()
	if usage := s.effectiveUsageLocked(); usage != nil {
		outcome.Usage = s.usagePayload(*usage)
	}
	s.usageMu.Unlock()
	return outcome
}
