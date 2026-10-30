package codex

import (
	"encoding/json"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func (s *Session) onAgentDelta(raw json.RawMessage) {
	received := time.Now()
	var p AgentMessageDeltaNotification
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	if !s.isRootTurn(p.ThreadID, p.TurnID) || p.Delta == "" || p.ItemID == "" {
		return
	}
	first := s.boundary.text(received, p.TurnID)
	_ = FoldDeltaIntoBuffer(s.bufs, p.ItemID, p.Delta)
	seq := s.deltaSeq.Add(1)
	env, err := proto.NewEnvelope(proto.TypeDelta, s.runID, proto.DeltaPayload{ItemID: p.ItemID, Delta: p.Delta, Sequence: seq})
	if err != nil {
		return
	}
	sent := s.trySend(env)
	if first {
		s.boundary.forward(received, sent)
	}
}

func (s *Session) onItemStarted(raw json.RawMessage) {
	var p ItemStartedNotification
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	if !s.isRootTurn(p.ThreadID, p.TurnID) || p.Item.ID == "" {
		return
	}
	s.observeMessage(p.Item, "in_progress", nil)
	envs, err := DispatchStartedItem(s.runID, p.Item)
	if err != nil {
		s.cfg.logger.Warn("codex: dispatch started item failed", "run_id", s.runID, "err", err)
		return
	}
	s.sendItemEvents(envs, raw)
}

func (s *Session) onItemCompleted(raw json.RawMessage) {
	received := time.Now()
	s.observeSubagentIdentity(raw)
	var p ItemCompletedNotification
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	// Record native application before observation backpressure can delay publication.
	s.confirmFunctionResult(raw)
	if !s.isRootTurn(p.ThreadID, p.TurnID) || p.Item.ID == "" {
		return
	}
	if p.Item.Type == "reasoning" {
		s.completion.observe(p.Item)
		s.completeReasoning(p.Item)
		return
	}
	envs, text, err := DispatchCompletedItem(s.runID, p.Item, s.bufs)
	if err != nil {
		s.cfg.logger.Warn("codex: dispatch completed item failed", "run_id", s.runID, "err", err)
		return
	}
	s.sendItemEvents(envs, raw)
	messageText := p.Item.Text
	if messageText == "" {
		messageText = text
	}
	completedItem := p.Item
	completedItem.Text = messageText
	s.completion.observe(completedItem)
	first := false
	if p.Item.Type == "agentMessage" && messageText != "" {
		first = s.boundary.text(received, p.TurnID)
	}
	sent := s.observeMessage(p.Item, "completed", &messageText)
	if first {
		s.boundary.forward(received, sent)
	}
	if text != "" {
		s.appendFinalText(text)
	}
}

func (s *Session) observeMessage(item ThreadItem, status string, text *string) bool {
	if item.Type != "agentMessage" {
		return false
	}
	env, err := proto.NewEnvelope(proto.TypeOutputMessage, s.runID, proto.OutputMessagePayload{ID: item.ID, Status: status, Phase: item.Phase, Text: text})
	if err == nil {
		return s.trySend(env)
	}
	return false
}
