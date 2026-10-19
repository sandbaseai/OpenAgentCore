package execution

import (
	"context"
	"sync"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

const schedulingObservationLimit = 256
const schedulingDurationLimit = 30 * time.Minute

var schedulingStates = [...]string{"pending_seen", "allocation_missing", "compute_not_ready", "binding_unavailable", "peer_unavailable", "capability_unavailable", "eligible_seen", "selected", "prepare_sent", "ready_seen", "promoted", "notification_sent", "handler_observed", "unknown"}

type schedulingObservationKey struct{}
type schedulingObservation struct {
	mu                          sync.Mutex
	owner, reservation, session string
	started                     time.Time
	times                       [len(schedulingStates)]time.Time
	finished                    bool
}

type schedulingObservations struct {
	mu           sync.Mutex
	owner        string
	active       map[string]*schedulingObservation
	overflow     uint64
	closed       bool
	lastFinished time.Time
}

// Only candidates returned by the existing query are visible. In particular,
// the prefix before a Runtime connects is unknown, not allocation wait.
func (s *schedulingObservations) candidate(reservation, session string) *schedulingObservation {
	return s.candidateFromScan(reservation, session, time.Now())
}

// A query begun before a terminal observation may still return its pending
// snapshot. Conservatively skip new scopes from that page, without retaining
// terminal IDs or changing which candidates the scheduler evaluates.
func (s *schedulingObservations) candidateFromScan(reservation, session string, scanStarted time.Time) *schedulingObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.owner == "" {
		s.owner = obslog.NewSpanID().String()
	}
	if value := s.active[reservation]; value != nil {
		return value
	}
	if !scanStarted.After(s.lastFinished) {
		return nil
	}
	if len(s.active) == schedulingObservationLimit {
		if s.overflow != ^uint64(0) {
			s.overflow++
		}
		return nil
	}
	if s.active == nil {
		s.active = make(map[string]*schedulingObservation)
	}
	value := &schedulingObservation{owner: s.owner, reservation: reservation, session: session, started: time.Now()}
	value.times[0] = value.started
	s.active[reservation] = value
	return value
}

func schedulingContext(ctx context.Context, value *schedulingObservation) context.Context {
	if value == nil {
		return ctx
	}
	return context.WithValue(ctx, schedulingObservationKey{}, value)
}
func observeSchedulingState(ctx context.Context, state string) {
	value, _ := ctx.Value(schedulingObservationKey{}).(*schedulingObservation)
	if value == nil {
		return
	}
	value.mu.Lock()
	defer value.mu.Unlock()
	if value.finished {
		return
	}
	for i, name := range schedulingStates {
		if state == name && value.times[i].IsZero() {
			value.times[i] = time.Now()
			return
		}
	}
}
func schedulingMilliseconds(start, end time.Time) any {
	if start.IsZero() || end.IsZero() {
		return nil
	}
	duration := end.Sub(start)
	if duration < 0 || duration > schedulingDurationLimit {
		return nil
	}
	return float64(duration) / float64(time.Millisecond)
}

type schedulingRecord struct {
	reservation string
	attrs       []any
}

func (r schedulingRecord) emit(ctx context.Context) {
	if r.attrs != nil {
		obslog.Ctx(reservationTrace(ctx, r.reservation)).Info("execution scheduling observation", r.attrs...)
	}
}

// Freeze under observation locks; emit only after all locks have been released.
func (o *schedulingObservation) freeze(outcome string) schedulingRecord {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return schedulingRecord{}
	}
	o.finished = true
	// The pre-candidate prefix remains unmeasured even when a same-worker
	// handler observes the outcome before dispatch ends.
	attrs := []any{"owner_observation_id", o.owner, "reservation_id", o.reservation, "session_id", o.session, "observation_complete", false, "outcome", outcome, "prefix_observed", false, "overflow_count", uint64(0), "overflow_saturated", false}
	count := 0
	for i, state := range schedulingStates {
		offset := schedulingMilliseconds(o.started, o.times[i])
		if offset != nil {
			count++
		}
		attrs = append(attrs, state+"_observed", offset != nil, state+"_offset_ms", offset)
	}
	attrs = append(attrs, "state_count", count)
	for _, pair := range []struct {
		name     string
		from, to int
	}{{"eligible_to_selected_ms", 6, 7}, {"selected_to_prepare_ms", 7, 8}, {"ready_to_promote_ms", 9, 10}, {"promote_to_notify_ms", 10, 11}, {"notify_to_handler_ms", 11, 12}} {
		var duration any
		if schedulingMilliseconds(o.started, o.times[pair.from]) != nil && schedulingMilliseconds(o.started, o.times[pair.to]) != nil {
			duration = schedulingMilliseconds(o.times[pair.from], o.times[pair.to])
		}
		attrs = append(attrs, pair.name, duration)
	}
	return schedulingRecord{reservation: o.reservation, attrs: attrs}
}
func (s *schedulingObservations) finish(ctx context.Context, value *schedulingObservation, outcome string) {
	if value == nil {
		return
	}
	s.mu.Lock()
	if s.active[value.reservation] != value {
		s.mu.Unlock()
		return
	}
	record := value.freeze(outcome)
	s.lastFinished = time.Now()
	delete(s.active, value.reservation)
	s.mu.Unlock()
	record.emit(ctx)
}
func (s *schedulingObservations) close(ctx context.Context, outcome string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	records := make([]schedulingRecord, 0, len(s.active))
	for id, value := range s.active {
		records = append(records, value.freeze(outcome))
		delete(s.active, id)
	}
	owner, overflow := s.owner, s.overflow
	s.mu.Unlock()
	for _, record := range records {
		record.emit(ctx)
	}
	if owner != "" {
		obslog.Ctx(ctx).Info("execution scheduling owner observation", "owner_observation_id", owner, "overflow_count", overflow, "overflow_saturated", overflow == ^uint64(0), "outcome", outcome)
	}
}

// Reuse the submitting handler's existing read without creating a second
// observer. Admitted work is still running: only its dispatch may finalize it.
func (s *schedulingObservations) handlerOutcome(ctx context.Context, session string, reservation sessions.EnvironmentInputReservation) {
	outcome := ""
	switch reservation.State {
	case sessions.EnvironmentInputAdmitted:
	case sessions.EnvironmentInputCancelled:
		outcome = "cancelled"
	case sessions.EnvironmentInputFailed:
		outcome = "failed"
	case sessions.EnvironmentInputExpired:
		outcome = "unknown"
	default:
		return
	}
	s.mu.Lock()
	value := s.active[reservation.ID]
	if value == nil || value.session != session {
		s.mu.Unlock()
		return
	}
	observeSchedulingState(schedulingContext(ctx, value), "handler_observed")
	var record schedulingRecord
	if outcome != "" {
		record = value.freeze(outcome)
		s.lastFinished = time.Now()
		delete(s.active, reservation.ID)
	}
	s.mu.Unlock()
	record.emit(ctx)
}
