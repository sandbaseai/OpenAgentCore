package execution

import (
	"errors"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
)

// The execution journal ordinal is not the public Session event sequence.
// Keep only one receipt and the first failure plus eventual confirmation per Turn.
type journalTextObservation struct {
	received               time.Time
	sequence               int32
	attempts               int
	failed, confirmed      bool
	flushStarted           time.Time
	flushTrigger           string
	appendTotal, appendMax time.Duration
	appendFailed           int
	appendUnknown          int
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

func (j *journal) observeTextFlush(trigger string) {
	if j.firstText.flushStarted.IsZero() {
		j.firstText.flushStarted = time.Now()
		j.firstText.flushTrigger = trigger
	}
}

func (j *journal) observeTextCommit(count int, started time.Time, err error, eventID string) {
	o := &j.firstText
	if !j.observesTextBatch(count) {
		return
	}
	o.attempts++
	elapsed := time.Since(started)
	o.appendTotal += elapsed
	if elapsed > o.appendMax {
		o.appendMax = elapsed
	}
	if err != nil {
		o.appendFailed++
		// Journal validation/admission sentinels reject the transaction before
		// commit. Other errors do not prove whether a commit took effect.
		if !errors.Is(err, sessions.ErrInvalidInput) && !errors.Is(err, sessions.ErrNotFound) &&
			!errors.Is(err, sessions.ErrTurnConflict) && !errors.Is(err, sessions.ErrEventLimit) &&
			!errors.Is(err, sessions.ErrIdempotencyConflict) {
			o.appendUnknown++
		}
	}
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
		"receipt_to_confirmation_ms", durationIfConfirmed(o.received, err == nil),
		"batch_wait_ms", float64(o.flushStarted.Sub(o.received))/float64(time.Millisecond),
		"first_flush_trigger", o.flushTrigger, "append_count", o.attempts,
		"append_total_ms", float64(o.appendTotal)/float64(time.Millisecond),
		"append_max_ms", float64(o.appendMax)/float64(time.Millisecond),
		"append_failed_count", o.appendFailed, "append_unknown_count", o.appendUnknown)
}

// nil distinguishes a failed/unknown commit from a measured zero duration.
func durationIfConfirmed(start time.Time, confirmed bool) any {
	if !confirmed {
		return nil
	}
	return time.Since(start).Milliseconds()
}
