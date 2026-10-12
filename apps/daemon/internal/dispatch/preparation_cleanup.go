package dispatch

import "github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/workspaceobs"

func (r *Router) closePendingPreparationsLocked() []*workspaceobs.Scope {
	var observers []*workspaceobs.Scope
	for _, p := range r.preparations {
		p.timer.Stop()
		if p.executor != nil {
			continue
		}
		// Business ownership can settle before the terminal publication starts.
		// Shutdown must also finish that observation if publication is suppressed.
		observers = append(observers, p.observation)
		if !p.owns {
			continue
		}
		p.cancel()
		switch p.status.State {
		case "preparing", "ready":
			p.status.State, p.status.ErrorCode, p.status.Revision = "failed", "connection_closed", p.status.Revision+1
		}
		// A busy preparation drops ownership when readiness returns.
		if !p.busy {
			p.owns = false
		}
	}
	return observers
}
