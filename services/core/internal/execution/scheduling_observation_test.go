package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/deployment"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func TestSchedulingObservationBoundedConcurrentAndOwnerLocal(t *testing.T) {
	logs := captureExecutionLogs(t)
	var observations schedulingObservations
	first := observations.candidate("reservation", "session")
	ctx := schedulingContext(t.Context(), first)
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				observeSchedulingState(ctx, "capability_unavailable")
			}
		}()
	}
	workers.Wait()
	if observations.candidate("reservation", "session") != first {
		t.Fatal("duplicate scope")
	}
	for i := 1; i < schedulingObservationLimit; i++ {
		observations.candidate(fmt.Sprint(i), "session")
	}
	if observations.candidate("overflow", "session") != nil || observations.overflow != 1 {
		t.Fatal("limit not enforced")
	}
	observations.finish(ctx, first, "completed")
	observations.finish(ctx, first, "completed")
	row := singleSchedulingRow(t, logs.String())
	if row["state_count"] != float64(2) || row["notify_to_handler_ms"] != nil || row["observation_complete"] != false {
		t.Fatal(row)
	}
	var next schedulingObservations
	if next.candidate("reservation", "session").owner == first.owner {
		t.Fatal("owner identity reused")
	}
	observations.close(t.Context(), "owner_lost")
	if len(observations.active) != 0 {
		t.Fatal("owner leaked scopes")
	}
}

func TestSchedulingObservationDelayAndInvalidIntervals(t *testing.T) {
	logs := captureExecutionLogs(t)
	var observations schedulingObservations
	scope := observations.candidate("reservation", "session")
	ctx := schedulingContext(t.Context(), scope)
	observeSchedulingState(ctx, "eligible_seen")
	before := scope.times[6]
	time.Sleep(12 * time.Millisecond)
	observeSchedulingState(ctx, "selected")
	observeSchedulingState(ctx, "eligible_seen")
	if scope.times[6] != before {
		t.Fatal("first observation overwritten")
	}
	observations.finish(ctx, scope, "completed")
	row := singleSchedulingRow(t, logs.String())
	if row["eligible_to_selected_ms"].(float64) < 10 {
		t.Fatal("delay excluded", row)
	}
	now := time.Now()
	for _, end := range []time.Time{{}, now.Add(-time.Second), now.Add(schedulingDurationLimit + time.Nanosecond)} {
		if schedulingMilliseconds(now, end) != nil {
			t.Fatal("invalid interval fabricated")
		}
	}
	if schedulingMilliseconds(now, now) != float64(0) {
		t.Fatal("known zero lost")
	}
}

func TestSchedulingObservationNoScopeSilent(t *testing.T) {
	logs := captureExecutionLogs(t)
	observeSchedulingState(t.Context(), "unknown")
	var observations schedulingObservations
	observations.finish(t.Context(), nil, "failed")
	observations.close(t.Context(), "unknown")
	if logs.Len() != 0 {
		t.Fatal("unscoped logs")
	}
}

type schedulingBindingReader struct {
	sessions.Reader
	calls int
}

func (r *schedulingBindingReader) GetSessionEnvironment(context.Context, string, string) (sessions.Environment, error) {
	r.calls++
	return sessions.Environment{ID: "env", Configuration: json.RawMessage(`{"type":"openai_hosted"}`)}, nil
}
func (r *schedulingBindingReader) GetSessionDevice(context.Context, string, string) (sessions.ExecutionDevice, error) {
	r.calls++
	return sessions.ExecutionDevice{}, sessions.ErrNotFound
}

func TestSchedulingObservationExistingReadClassifications(t *testing.T) {
	for _, tc := range []struct {
		name, phase string
		err         error
		state       string
		reads       int
	}{{"allocation absent", "", deployment.ErrNotFound, "allocation_missing", 1}, {"compute waking", "waking", nil, "compute_not_ready", 1}, {"binding absent", "running", nil, "binding_unavailable", 2}, {"read failure", "", fmt.Errorf("SECRET_CANARY"), "unknown", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureExecutionLogs(t)
			reader := &schedulingBindingReader{}
			allocationCalls := 0
			worker := &Worker{dispatcher: &Dispatcher{SessionsReader: reader, DeploymentReader: &strictDeploymentReader{t: t, environmentAllocation: func(context.Context, deployment.AllocationKey) (deployment.Allocation, error) {
				allocationCalls++
				return deployment.Allocation{ComputePhase: tc.phase}, tc.err
			}}}}
			var observations schedulingObservations
			scope := observations.candidate("reservation", "session")
			ctx := schedulingContext(t.Context(), scope)
			ready, err := worker.bindSessionDevice(ctx, sessions.Session{Configuration: json.RawMessage(`{"environment":{"type":"openai_hosted"}}`)}, func(string) bool { t.Fatal("unexpected readiness read"); return false })
			if ready || (tc.err != nil && tc.err != deployment.ErrNotFound && err != tc.err) {
				t.Fatal(ready, err)
			}
			if reader.calls != tc.reads || allocationCalls != 1 {
				t.Fatal("business reads changed", reader.calls, allocationCalls)
			}
			observations.finish(ctx, scope, "unknown")
			row := singleSchedulingRow(t, logs.String())
			if row[tc.state+"_observed"] != true || strings.Contains(logs.String(), "SECRET_CANARY") {
				t.Fatal(row)
			}
		})
	}
}

func TestSchedulingObservationProducerFixtures(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			previous := slog.Default()
			defer slog.SetDefault(previous)
			var handler slog.Handler = slog.NewJSONHandler(&out, nil)
			if format == "text" {
				handler = slog.NewTextHandler(&out, nil)
			}
			slog.SetDefault(slog.New(obslog.NewContextHandler(handler)))
			var observations schedulingObservations
			value := observations.candidate("18b7c958-e3dc-4b34-a723-c14b9c6b7ba8", "633c50ea-1e01-4cb2-a423-965e391c3632")
			ctx := schedulingContext(t.Context(), value)
			for _, state := range []string{"allocation_missing", "compute_not_ready", "binding_unavailable", "peer_unavailable", "capability_unavailable", "eligible_seen", "selected", "prepare_sent", "ready_seen", "promoted", "notification_sent"} {
				observeSchedulingState(ctx, state)
			}
			observations.handlerOutcome(ctx, "633c50ea-1e01-4cb2-a423-965e391c3632", sessions.EnvironmentInputReservation{ID: "18b7c958-e3dc-4b34-a723-c14b9c6b7ba8", State: sessions.EnvironmentInputAdmitted})
			observations.finish(ctx, value, "completed")
			observations.close(ctx, "unknown")
			t.Log(out.String())
		})
	}
}

func TestSchedulingObservationPendingScanDoesNotChangeThrottleOrReads(t *testing.T) {
	captureExecutionLogs(t)
	reader := &initializationWakeReader{committed: true}
	allocations := 0
	worker := &Worker{concurrency: 1, dispatcher: &Dispatcher{SessionsReader: reader, DeploymentReader: &strictDeploymentReader{t: t, environmentAllocation: func(context.Context, deployment.AllocationKey) (deployment.Allocation, error) {
		allocations++
		return deployment.Allocation{ComputePhase: "waking"}, nil
	}}}}
	schedule := workerSchedule{}
	for _, hinted := range []bool{false, false, true, false} {
		work, err := schedule.selectWork(t.Context(), worker, []string{"device"}, map[string]bool{}, hinted)
		if err != nil || len(work) != 0 {
			t.Fatal(work, err)
		}
	}
	if reader.scans != 2 || allocations != 2 || len(worker.schedulingObservations.active) != 1 {
		t.Fatal("observation changed scans", reader.scans, allocations)
	}
	scope := worker.schedulingObservations.active["reservation"]
	if scope.times[2].IsZero() || !scope.times[6].IsZero() {
		t.Fatal("unobserved eligibility fabricated")
	}
	// A full slot budget does not even evaluate this candidate. The observation
	// must not pretend this was a measured capability or allocation wait.
	worker = &Worker{concurrency: 1, dispatcher: worker.dispatcher}
	_, err := schedule.selectWork(t.Context(), worker, []string{"device"}, map[string]bool{"other": true}, true)
	if err != nil || len(worker.schedulingObservations.active) != 0 || allocations != 2 {
		t.Fatal("full slots observed binding", err)
	}
}

func TestSchedulingObservationClosedOwnerAndOverflowSaturation(t *testing.T) {
	logs := captureExecutionLogs(t)
	var observations schedulingObservations
	for i := 0; i < schedulingObservationLimit; i++ {
		observations.candidate(fmt.Sprint(i), "session")
	}
	observations.overflow = ^uint64(0)
	if observations.candidate("overflow", "session") != nil || observations.overflow != ^uint64(0) {
		t.Fatal("overflow wrapped")
	}
	observations.close(t.Context(), "owner_lost")
	size := logs.Len()
	observations.close(t.Context(), "owner_lost")
	if observations.candidate("new", "session") != nil || logs.Len() != size {
		t.Fatal("closed owner reopened")
	}
	if !strings.Contains(logs.String(), `"overflow_saturated":true`) {
		t.Fatal("missing saturation")
	}
}

func TestSchedulingObservationAdmittedHandlerPreservesDispatch(t *testing.T) {
	logs := captureExecutionLogs(t)
	var scopes schedulingObservations
	value := scopes.candidate("reservation", "session")
	ctx := schedulingContext(t.Context(), value)
	var tasks sync.WaitGroup
	for i := 0; i < 32; i++ {
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			scopes.handlerOutcome(ctx, "session", sessions.EnvironmentInputReservation{ID: "reservation", State: sessions.EnvironmentInputAdmitted})
		}()
	}
	tasks.Wait()
	if len(scopes.active) != 1 || value.finished || logs.Len() != 0 {
		t.Fatal("handler ended in-flight admission")
	}
	observeSchedulingState(ctx, "promoted")
	observeSchedulingState(ctx, "notification_sent")
	scopes.finish(ctx, value, "completed")
	row := singleSchedulingRow(t, logs.String())
	if row["handler_observed_observed"] != true || row["promoted_observed"] != true || row["notification_sent_observed"] != true {
		t.Fatal("lost concurrent dispatch milestones", row)
	}
	if row["notify_to_handler_ms"] != nil {
		t.Fatal("reverse ordering fabricated a duration", row)
	}
}

func TestSchedulingObservationHandlerRequiresExactExistingScope(t *testing.T) {
	logs := captureExecutionLogs(t)
	var scopes schedulingObservations
	value := scopes.candidate("reservation", "session")
	scopes.handlerOutcome(t.Context(), "other-session", sessions.EnvironmentInputReservation{ID: "reservation", State: sessions.EnvironmentInputCancelled})
	scopes.handlerOutcome(t.Context(), "session", sessions.EnvironmentInputReservation{ID: "other-reservation", State: sessions.EnvironmentInputCancelled})
	scopes.handlerOutcome(t.Context(), "session", sessions.EnvironmentInputReservation{ID: "reservation", State: sessions.EnvironmentInputPending})
	if len(scopes.active) != 1 || !value.times[12].IsZero() || logs.Len() != 0 {
		t.Fatal("unmatched or pending read changed scope")
	}
	scopes.handlerOutcome(t.Context(), "session", sessions.EnvironmentInputReservation{ID: "reservation", State: sessions.EnvironmentInputCancelled})
	if len(scopes.active) != 0 || !strings.Contains(logs.String(), `"handler_observed_observed":true`) {
		t.Fatal("terminal handler not recorded")
	}
}

func TestSchedulingObservationStalePageFencePreservesExistingAnchors(t *testing.T) {
	captureExecutionLogs(t)
	var scopes schedulingObservations
	terminal := scopes.candidate("terminal", "session")
	retained := scopes.candidate("retained", "session")
	started := retained.started
	scan := time.Now()
	scopes.finish(t.Context(), terminal, "cancelled")
	if scopes.candidateFromScan("terminal", "session", scan) != nil {
		t.Fatal("stale terminal candidate reopened")
	}
	if scopes.candidateFromScan("unseen", "session", scan) != nil {
		t.Fatal("old page fabricated a new scope")
	}
	if got := scopes.candidateFromScan("retained", "session", scan); got != retained || got.started != started {
		t.Fatal("existing first anchor replaced")
	}
	// Only a later authoritative candidate page can establish an unseen scope.
	if scopes.candidateFromScan("unseen", "session", time.Now()) == nil {
		t.Fatal("fresh page was not observable")
	}
	if len(scopes.active) != 2 {
		t.Fatal("unexpected active scope growth")
	}
}

func TestSchedulingObservationTerminalPathsReleaseLocksBeforeLogging(t *testing.T) {
	for _, path := range []string{"handler", "close"} {
		t.Run(path, func(t *testing.T) {
			h := &closureBlockingHandler{entered: make(chan struct{}), release: make(chan struct{})}
			previous := slog.Default()
			slog.SetDefault(slog.New(h))
			defer slog.SetDefault(previous)
			var scopes schedulingObservations
			value := scopes.candidate("reservation", "session")
			done := make(chan struct{})
			go func() {
				defer close(done)
				if path == "handler" {
					scopes.handlerOutcome(context.Background(), "session", sessions.EnvironmentInputReservation{ID: "reservation", State: sessions.EnvironmentInputCancelled})
				} else {
					scopes.close(context.Background(), "owner_lost")
				}
			}()
			<-h.entered
			ownerFree := scopes.mu.TryLock()
			if ownerFree {
				scopes.mu.Unlock()
			}
			scopeFree := value.mu.TryLock()
			if scopeFree {
				value.mu.Unlock()
			}
			close(h.release)
			<-done
			if !ownerFree || !scopeFree {
				t.Fatal("terminal logger retained observation lock", path, ownerFree, scopeFree)
			}
			if len(scopes.active) != 0 || !value.finished {
				t.Fatal("state not finalized before emission")
			}
		})
	}
}
