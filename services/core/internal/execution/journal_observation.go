package execution

import (
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
)

// The execution journal ordinal is not the public Session event sequence.
// Keep only one receipt and the first failure plus eventual confirmation per Turn.
type journalTextObservation struct {
	received          time.Time
	sequence          int32
	attempts          int
	failed, confirmed bool
}

func (j *journal) observeFirstText(env proto.Envelope) {
	if !j.firstText.received.IsZero() || !hasText(env) {
		return
	}
	j.firstText.received = time.Now()
	j.firstText.sequence = j.next + int32(len(j.batch))
	obslog.Ctx(j.ctx).Info("journal first text received", "session_id", j.session, "turn_id", j.turn,
		"journal_sequence", j.firstText.sequence)
}

func (j *journal) observesTextBatch(count int) bool {
	o := &j.firstText
	return !o.received.IsZero() && !o.confirmed && o.sequence >= j.next && o.sequence < j.next+int32(count)
}

func (j *journal) observeTextCommit(count int, started time.Time, err error, eventID string) {
	o := &j.firstText
	if !j.observesTextBatch(count) {
		return
	}
	o.attempts++
	if err != nil && o.failed {
		return
	}
	status := "confirmed"
	if err != nil {
		status = "unconfirmed"
		o.failed = true
	} else {
		o.confirmed = true
	}
	mappingStatus := "unobserved"
	if eventID != "" {
		mappingStatus = status
	}
	obslog.Ctx(j.ctx).Info("journal first text persistence", "session_id", j.session, "turn_id", j.turn,
		"journal_sequence", o.sequence, "status", status, "attempts", o.attempts,
		"event_id", eventID, "mapping_status", mappingStatus,
		"had_unconfirmed_attempt", o.failed, "append_ms", time.Since(started).Milliseconds(),
		"receipt_to_confirmation_ms", durationIfConfirmed(o.received, err == nil))
}

// nil distinguishes a failed/unknown commit from a measured zero duration.
func durationIfConfirmed(start time.Time, confirmed bool) any {
	if !confirmed {
		return nil
	}
	return time.Since(start).Milliseconds()
}
