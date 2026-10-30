package codex

import (
	"strings"
	"sync"
)

// completionEvidence observes root native items, even when reasoning is hidden.
// It does not decide business success or require artifacts from tool-only tasks.
type completionEvidence struct {
	mu    sync.Mutex
	seen  map[string]bool
	last  string
	final bool
}

func (e *completionEvidence) observe(item ThreadItem) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seen[item.ID] {
		return
	}
	if e.seen == nil {
		e.seen = make(map[string]bool)
	}
	e.seen[item.ID] = true
	// Only finite types reach logs; an unknown item prevents a positive diagnosis.
	switch item.Type {
	case "reasoning", "agentMessage", "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "webSearch":
		e.last = item.Type
	default:
		e.last = "other"
	}
	if item.Type == "agentMessage" && (item.Phase == "final_answer" || item.Phase == "") && strings.TrimSpace(item.Text) != "" {
		e.final = true
	}
}

func (e *completionEvidence) snapshot() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last, e.final
}
