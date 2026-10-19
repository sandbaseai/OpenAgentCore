package workspaceobs

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

type timedWriter struct {
	output io.Writer
	scope  *Scope
}

// Writer preserves the original PipeWriter's n/error and never closes or buffers it.
func (s *Scope) Writer(output io.Writer) io.Writer {
	if s == nil {
		return output
	}
	return &timedWriter{output: output, scope: s}
}
func (w *timedWriter) Write(p []byte) (int, error) {
	start := w.scope.Begin(ExportWriter)
	n, err := w.output.Write(p)
	w.scope.End(ExportWriter, start, err)
	return n, err
}

type roundKey struct{}
type Round struct {
	scope           *Scope
	start           time.Time
	done            bool
	responseOutcome string
	Index           int    `json:"round_index"`
	Phase           string `json:"phase"`
	Offset          int64  `json:"offset"`
	Observed        bool   `json:"observed"`
	Outcome         string `json:"outcome"`
	Duration        any    `json:"request_read_to_response_write_ms"`
}

func (s *Scope) RoundContext(ctx, received context.Context, phase string, offset int64) context.Context {
	if s == nil {
		return ctx
	}
	a, ok := received.Value(arrivalKey{}).(arrival)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ctx
	}
	if s.roundCount >= 1000000 {
		s.roundsTruncated = true
		s.roundMetric.overflow = true
		return ctx
	}
	s.roundCount++
	r := &Round{scope: s, Index: s.roundCount, Phase: phase, Offset: offset, Outcome: "unknown"}
	if ok && a.connection == s.connection {
		r.start = a.readReturned
	}
	if len(s.rounds) < 8 {
		s.rounds = append(s.rounds, r)
	} else {
		s.roundsTruncated = true
	}
	s.roundMetric.active++
	return context.WithValue(ctx, roundKey{}, r)
}
func RoundFromContext(ctx context.Context) *Round { r, _ := ctx.Value(roundKey{}).(*Round); return r }

// Written uses the endpoint captured immediately after WriteMessage returns,
// before any observer lock or bookkeeping.
func (r *Round) Written(returned time.Time, err error) {
	if r == nil {
		return
	}
	d := returned.Sub(r.start)
	s := r.scope
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || r.done {
		return
	}
	r.done = true
	s.roundMetric.active--
	if r.start.IsZero() || returned.IsZero() || d < 0 || d > 30*time.Minute {
		if !r.start.IsZero() && !returned.IsZero() {
			s.roundMetric.overflow = true
		}
		s.roundMetric.addUnknown(1)
		return
	}
	r.Observed = true
	r.Duration = float64(d.Microseconds()) / 1000
	r.Outcome = r.responseOutcome
	if r.Outcome == "" {
		r.Outcome = "ok"
	}
	if err != nil {
		r.Outcome = "error"
		s.roundMetric.failed++
	}
	if s.roundMetric.count >= 1000000 || s.roundMetric.total > 30*time.Minute-d {
		s.roundMetric.overflow = true
		s.roundMetric.addUnknown(1)
		return
	}
	s.roundMetric.count++
	s.roundMetric.total += d
	if d > s.roundMetric.max {
		s.roundMetric.max = d
	}
}
func (s *Scope) roundFieldsLocked() []any {
	// Copy values while locked: a queued write may still finish after this snapshot.
	rounds := make([]Round, len(s.rounds))
	for i, r := range s.rounds {
		rounds[i] = *r
		rounds[i].scope = nil
	}
	roundsJSON, _ := json.Marshal(rounds)
	m := s.roundMetric
	m.addUnknown(m.active)
	var total, max any
	if m.count > 0 {
		total = float64(m.total.Microseconds()) / 1000
		max = float64(m.max.Microseconds()) / 1000
	}
	return []any{"rounds_json", string(roundsJSON), "round_count", s.roundCount, "rounds_truncated", s.roundsTruncated, "rounds_overflow", m.overflow, "request_read_to_response_write_count", m.count, "request_read_to_response_write_total_ms", total, "request_read_to_response_write_max_ms", max, "request_read_to_response_write_unknown_count", m.unknown}
}

// SetResponse records a fixed local result kind without retaining the payload.
func (r *Round) SetResponse(outcome string) {
	if r == nil {
		return
	}
	switch outcome {
	case "chunk", "completed", "failed", "rejected":
	default:
		return
	}
	r.scope.mu.Lock()
	defer r.scope.mu.Unlock()
	if !r.scope.closed && !r.done {
		r.responseOutcome = outcome
	}
}
