package api

import (
	"context"
	"time"

	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/google/uuid"
)

// Each connection samples first text and terminal events of at most 64 Turns plus
// one missing-ID slot. Never evict an identity and accidentally resample it.
// Reconnects have separate identity; these are not globally first Turn events.
const streamTextTurnLimit = 64

type streamTextObservation struct {
	ctx                      context.Context
	session, connection      string
	cursor                   int64
	seen                     map[string]uint8
	unknownSeen              uint8
	limitLogged, active      bool
	terminal                 bool
	loop                     streamLoopObservation
	textObserved             bool
	event                    v1.SessionEvent
	sequence                 int64
	readAt                   time.Time
	writeMS, flushMS         any
	writeStatus, flushStatus string
}

func newStreamTextObservation(ctx context.Context, session string, cursor int64) *streamTextObservation {
	return &streamTextObservation{ctx: ctx, session: session, connection: uuid.NewString(), cursor: cursor}
}

func (o *streamTextObservation) begin(event v1.SessionEvent, sequence int64, readAt time.Time) {
	if o.active {
		return
	}
	text := event.Type == "agent.session.turn.output_text.delta" && event.Delta != nil && *event.Delta != "" ||
		event.Type == "agent.session.turn.output_text.done" && event.Text != nil && *event.Text != ""
	terminal := v1.TerminalTurnEvent(event.Type)
	if !text && !terminal {
		return
	}
	flag := uint8(1)
	if terminal {
		flag = 2
	}
	if event.TurnID == "" {
		if o.unknownSeen&flag != 0 {
			return
		}
		o.unknownSeen |= flag
	} else {
		if o.seen[event.TurnID]&flag != 0 {
			return
		}
		_, known := o.seen[event.TurnID]
		if !known && len(o.seen) >= streamTextTurnLimit {
			if !o.limitLogged {
				o.limitLogged = true
				obslog.Ctx(o.ctx).Info("session stream text observation limit", "session_id", o.session, "connection_id", o.connection, "status", "unobserved", "reason", "turn_limit", "turn_limit", streamTextTurnLimit)
			}
			return
		}
		if o.seen == nil {
			o.seen = make(map[string]uint8)
		}
		o.seen[event.TurnID] |= flag
	}
	o.active, o.event, o.sequence, o.readAt = true, event, sequence, readAt
	o.terminal = terminal
	o.writeMS, o.flushMS = nil, nil
	o.writeStatus, o.flushStatus = "unobserved", "unobserved"
	fields := o.fields()
	if !terminal {
		o.textObserved = true
		fields = append(fields, o.loop.fields(readAt)...)
	}
	obslog.Ctx(o.ctx).Info(o.message("read"), fields...)
}

func (o *streamTextObservation) fields() []any {
	return []any{"session_id", o.session, "turn_id", o.event.TurnID, "event_id", o.event.EventID,
		"session_event_sequence", o.sequence, "connection_id", o.connection, "start_cursor", o.cursor,
		"read_at", o.readAt.UTC().Format(time.RFC3339Nano), "scope", o.scope(), "event_type", o.event.Type, "turn_identity_observed", o.event.TurnID != ""}
}

func (o *streamTextObservation) wrote(start time.Time, written, expected int, err error) {
	if o == nil || !o.active {
		return
	}
	o.writeMS, o.writeStatus = time.Since(start).Milliseconds(), "success"
	if err != nil {
		o.writeStatus = "error"
	} else if written != expected {
		o.writeStatus = "short_write"
	}
}
func (o *streamTextObservation) flushed(start time.Time, err error) {
	if o == nil || !o.active {
		return
	}
	o.flushMS, o.flushStatus = time.Since(start).Milliseconds(), "success"
	if err != nil {
		o.flushStatus = "error"
	}
}
func (o *streamTextObservation) end(err error) {
	if !o.active {
		return
	}
	o.active = false
	status := "success"
	if o.writeStatus != "success" || o.flushStatus != "success" {
		status = "incomplete"
	}
	if err != nil {
		status = "error"
	}
	fields := append(o.fields(), "status", status, "write_status", o.writeStatus, "flush_status", o.flushStatus,
		"write_ms", o.writeMS, "flush_ms", o.flushMS, "read_to_emit_return_ms", time.Since(o.readAt).Milliseconds())
	obslog.Ctx(o.ctx).Info(o.message("output"), fields...)
	// Avoid retaining a text payload for the lifetime of a long-lived stream.
	o.event = v1.SessionEvent{}
}

func (o *streamTextObservation) message(boundary string) string {
	if o.terminal {
		return "session stream terminal " + boundary
	}
	return "session stream first text " + boundary
}
func (o *streamTextObservation) scope() string {
	if o.terminal {
		return "terminal_per_turn_connection"
	}
	return "first_text_per_turn_connection"
}

// Counters are connection-prefix snapshots, never per-Turn exclusive costs.
// No observation retains a page, text payload or unbounded timing history.
type streamDurationCounter struct {
	count      int64
	total, max time.Duration
}

func (c *streamDurationCounter) add(d time.Duration) {
	c.count++
	c.total += d
	if d > c.max {
		c.max = d
	}
}
func (c streamDurationCounter) fields(prefix string) []any {
	var total, maximum any
	if c.count > 0 {
		total, maximum = streamMS(c.total), streamMS(c.max)
	}
	return []any{prefix + "_count", c.count, prefix + "_total_ms", total, prefix + "_max_ms", maximum}
}
func streamMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

type streamLoopObservation struct {
	emptyReads, authorityCacheHits    int64
	poll, authority, list             streamDurationCounter
	lastEmpty                         time.Time
	lastPoll, lastAuthority, lastList any
	currentList                       any
}

func (o *streamLoopObservation) listed(start, end time.Time, empty bool, err error) {
	d := end.Sub(start)
	o.list.add(d)
	o.currentList = streamMS(d)
	if !o.lastEmpty.IsZero() {
		o.lastList = streamMS(d)
	}
	if err == nil && empty {
		o.emptyReads++
		o.lastEmpty = end
		o.lastPoll, o.lastAuthority, o.lastList = nil, nil, nil
	}
}
func (o *streamLoopObservation) checked(d time.Duration) {
	o.authority.add(d)
	if !o.lastEmpty.IsZero() {
		o.lastAuthority = streamMS(d)
	}
}
func (o *streamLoopObservation) cached() {
	o.authorityCacheHits++
	o.lastAuthority = nil
}
func (o *streamLoopObservation) polled(d time.Duration) {
	o.poll.add(d)
	if !o.lastEmpty.IsZero() {
		o.lastPoll = streamMS(d)
	}
}
func (o *streamLoopObservation) fields(readAt time.Time) []any {
	var gap any
	if !o.lastEmpty.IsZero() {
		gap = streamMS(readAt.Sub(o.lastEmpty))
	}
	fields := []any{"timing_scope", "connection_prefix", "empty_read_count", o.emptyReads,
		"authority_cache_hit_count", o.authorityCacheHits, "first_text_list_ms", o.currentList,
		"last_empty_to_first_read_ms", gap, "last_poll_wait_ms", o.lastPoll,
		"last_authority_check_ms", o.lastAuthority, "last_list_events_ms", o.lastList}
	fields = append(fields, o.poll.fields("poll_wait")...)
	fields = append(fields, o.authority.fields("authority_check")...)
	return append(fields, o.list.fields("list_events")...)
}
func (o *streamTextObservation) finish(outcome string) {
	if o.textObserved {
		return
	}
	fields := []any{"session_id", o.session, "connection_id", o.connection, "start_cursor", o.cursor,
		"outcome", outcome, "scope", "connection_without_text", "text_observed", false}
	fields = append(fields, o.loop.fields(time.Now())...)
	// An ending connection without text has no first-text read boundary.
	for i := 0; i < len(fields); i += 2 {
		if fields[i] == "first_text_list_ms" || fields[i] == "last_empty_to_first_read_ms" {
			fields[i+1] = nil
		}
	}
	obslog.Ctx(o.ctx).Info("session stream observation ended", fields...)
}
