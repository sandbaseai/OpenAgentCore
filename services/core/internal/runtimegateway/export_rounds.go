package runtimegateway

import (
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"time"
)

// The private receipt carries no new wire fields. Its timestamp precedes channel delivery.
type workspaceExportReceipt struct {
	proto.Envelope
	dispatched time.Time
}
type exportRound struct {
	Index    int    `json:"round_index"`
	Phase    string `json:"phase"`
	Offset   int64  `json:"offset"`
	Observed bool   `json:"observed"`
	Outcome  string `json:"outcome"`
	Duration any    `json:"request_send_to_receipt_dispatch_ms"`
}

func stageMS(d time.Duration, count int) any {
	if count == 0 {
		return nil
	}
	return float64(d.Microseconds()) / 1000
}
func (r *workspaceExportReader) observeRound(start time.Time, request proto.WorkspaceExportPayload, outcome string) {
	o := r.observation
	if o == nil {
		return
	}
	if o.roundCount >= 1000000 {
		o.roundsTruncated = true
		o.roundMetric.overflow = true
		return
	}
	o.roundCount++
	round := exportRound{Index: o.roundCount, Phase: request.Step, Offset: request.Offset, Outcome: outcome}
	d := r.dispatched.Sub(start)
	if outcome != "unknown" && !r.dispatched.IsZero() && d >= 0 && d <= 30*time.Minute {
		round.Observed = true
		round.Duration = stageMS(d, 1)
		o.roundMetric.add(d, false)
	} else {
		if !r.dispatched.IsZero() && (d < 0 || d > 30*time.Minute) {
			o.roundMetric.overflow = true
		}
		o.roundMetric.addUnknown(1)
	}
	if len(o.rounds) < 8 {
		o.rounds = append(o.rounds, round)
	} else {
		o.roundsTruncated = true
	}
}
