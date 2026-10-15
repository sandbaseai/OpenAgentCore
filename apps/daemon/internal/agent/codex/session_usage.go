package codex

import (
	"encoding/json"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func (s *Session) beginUsageTurn(turnID string) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.usageTurnID == turnID {
		return
	}
	s.usageTurnID = turnID
	s.usageBaseline = s.usageTotal
	s.latestUsage = nil
}

func (s *Session) onUsageUpdated(raw json.RawMessage) {
	var p ThreadTokenUsageUpdatedNotification
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isRootThread(p.ThreadID) {
		s.usageMu.Lock()
		if s.resumeUsageThreadID != "" && p.ThreadID == s.resumeUsageThreadID && p.TurnID != "" && p.TokenUsage != nil && p.TokenUsage.Total != nil {
			s.resumeUsageTotal = p.TokenUsage.Total
		}
		s.usageMu.Unlock()
		return
	}
	if s.terminal.Load() || s.isRetired(p.TurnID) {
		return
	}
	// Taken before usageMu: the gate reads the thread id, which is ordered ahead
	// of the usage lock.
	if s.recoveryGateClosed(p.ThreadID) {
		return
	}
	var observed *TurnUsage
	s.usageMu.Lock()
	defer func() {
		s.usageMu.Unlock()
		// Native notifications are ordered by the RPC reader. Publish before it
		// reads completion, without holding the snapshot lock across backpressure.
		if observed != nil {
			s.emitUsage(*observed)
		}
	}()
	// A notification admitted just before a recovery switched or rolled back the
	// root thread must not write into usage state that now belongs to another.
	if s.usageThreadID != "" && p.ThreadID != s.usageThreadID {
		return
	}
	if p.TokenUsage != nil && p.TokenUsage.Total != nil && p.TurnID != "" {
		// app-server replays the previous thread total after resume and
		// before turn/started. It establishes a baseline, not new usage.
		if s.usageTurnID == "" {
			s.usageTotal = *p.TokenUsage.Total
			return
		}
		if p.TurnID != s.usageTurnID {
			return
		}
		s.usageTotal = *p.TokenUsage.Total
		u := subtractUsage(s.usageTotal, s.usageBaseline)
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 {
			// The thread total has not advanced past this Turn's baseline. It
			// reports no usage for this Turn, e.g. when native sends it after an
			// interrupt before any response reported usage; publishing the
			// difference would turn missing usage into measured zeros.
			return
		}
		observed = &u
	} else if p.Usage != nil && s.usageTurnID != "" && (p.TurnID == "" || p.TurnID == s.usageTurnID) {
		u := *p.Usage
		observed = &u
	}
	if observed != nil {
		s.latestUsage = observed
		// Core keeps the last Usage it receives, so every publication is cumulative
		// over the Turn including what earlier recovered threads consumed.
		observed = s.effectiveUsageLocked()
	}
}

// effectiveUsageLocked is the one place the Turn's cumulative usage is derived;
// publication, settlement and cancellation all call it. The caller holds usageMu.
//
//   - Not recovering: usageCarry (what earlier recovered threads used) plus the
//     current thread's usage. Without a carry this is latestUsage itself.
//   - Recovering, not yet switched: latestUsage still holds the failed Turn that
//     usageCarry already includes, so the carry alone is the answer.
//   - Recovering, switched: latestUsage was reset and holds only the new Turn.
func (s *Session) effectiveUsageLocked() *TurnUsage {
	if s.usageInRecovery && !s.usageSwitched {
		return s.usageCarry
	}
	return addUsage(s.usageCarry, s.latestUsage)
}

// addUsage sums two usage values. A nil side contributes nothing, and the sum is
// complete only when both observed parts are.
func addUsage(a, b *TurnUsage) *TurnUsage {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return &TurnUsage{
		observed:              true,
		complete:              a.complete && b.complete,
		ReasoningOutputTokens: a.ReasoningOutputTokens + b.ReasoningOutputTokens,
		InputTokens:           a.InputTokens + b.InputTokens,
		OutputTokens:          a.OutputTokens + b.OutputTokens,
		CachedInputTokens:     a.CachedInputTokens + b.CachedInputTokens,
		CacheReadInputTokens:  a.CacheReadInputTokens + b.CacheReadInputTokens,
		TotalTokens:           a.TotalTokens + b.TotalTokens,
	}
}

func subtractUsage(total, baseline TurnUsage) TurnUsage {
	return TurnUsage{
		observed: true,
		complete: total.completeTokens() && (!baseline.observed || baseline.completeTokens()) &&
			total.InputTokens >= baseline.InputTokens && total.OutputTokens >= baseline.OutputTokens &&
			total.CachedInputTokens >= baseline.CachedInputTokens &&
			total.ReasoningOutputTokens >= baseline.ReasoningOutputTokens && total.TotalTokens >= baseline.TotalTokens,
		ReasoningOutputTokens: max(0, total.ReasoningOutputTokens-baseline.ReasoningOutputTokens),
		InputTokens:           max(0, total.InputTokens-baseline.InputTokens),
		OutputTokens:          max(0, total.OutputTokens-baseline.OutputTokens),
		CachedInputTokens:     max(0, total.CachedInputTokens-baseline.CachedInputTokens),
		CacheReadInputTokens:  max(0, total.CacheReadInputTokens-baseline.CacheReadInputTokens),
		TotalTokens:           max(0, total.TotalTokens-baseline.TotalTokens),
	}
}

func (u *TurnUsage) UnmarshalJSON(raw []byte) error {
	type plain TurnUsage
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*u = TurnUsage(value)
	u.observed, u.complete = true, true
	for _, key := range []string{"inputTokens", "outputTokens", "cachedInputTokens", "reasoningOutputTokens", "totalTokens"} {
		var n *int64
		if json.Unmarshal(fields[key], &n) != nil || n == nil || *n < 0 {
			u.complete = false
		}
	}
	return nil
}

func (u TurnUsage) completeTokens() bool {
	return u.complete && u.CachedInputTokens <= u.InputTokens && u.ReasoningOutputTokens <= u.OutputTokens &&
		u.InputTokens <= u.TotalTokens && u.OutputTokens == u.TotalTokens-u.InputTokens
}

func (s *Session) usagePayload(u TurnUsage) proto.Usage {
	result := proto.Usage{Provider: "openai", Model: s.resolvedModel, InputTokens: int32(u.InputTokens), OutputTokens: int32(u.OutputTokens)}
	if u.completeTokens() {
		result.Tokens = &proto.TokenUsage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens), CachedInputTokens: int64(u.CachedInputTokens), ReasoningOutputTokens: int64(u.ReasoningOutputTokens), TotalTokens: int64(u.TotalTokens)}
	}
	return result
}
