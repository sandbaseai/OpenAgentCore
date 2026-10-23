package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto/prototest"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/deployment"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

type qaTerminalReservationReader struct {
	sessions.Reader
	calls int
	state string
}

func (r *qaTerminalReservationReader) GetEnvironmentInputReservation(context.Context, string, string, string) (sessions.EnvironmentInputReservation, error) {
	r.calls++
	return sessions.EnvironmentInputReservation{ID: "qa-reservation", State: r.state}, nil
}

// The handler already reads this terminal outcome; observing it must release
// the same owner's finite slot without adding a database read.
func TestQAObservedTerminalOutcomeReleasesSchedulingSlot(t *testing.T) {
	for _, state := range []string{sessions.EnvironmentInputCancelled, sessions.EnvironmentInputFailed, sessions.EnvironmentInputExpired} {
		t.Run(state, func(t *testing.T) {
			logs := captureExecutionLogs(t)
			reader := &qaTerminalReservationReader{state: state}
			w := &Worker{dispatcher: &Dispatcher{SessionsReader: reader}}
			w.schedulingObservations.candidate("qa-reservation", "qa-session")
			got, err := w.environmentInputOutcome(t.Context(), sessions.Session{ID: "qa-session", TenantID: "qa-tenant"}, sessions.EnvironmentInputReservation{ID: "qa-reservation", State: sessions.EnvironmentInputPending, Deadline: time.Now().Add(time.Hour)})
			if err != nil || got.State != state || reader.calls != 1 {
				t.Fatalf("business result/reads changed: %v %v %d", got, err, reader.calls)
			}
			if len(w.schedulingObservations.active) != 0 {
				t.Fatalf("terminal %s already observed by handler, but finite owner slot retained", state)
			}
			if !strings.Contains(logs.String(), `"reservation_id":"qa-reservation"`) {
				t.Fatal("terminal observation missing")
			}
		})
	}
}

// Two individually invalid state anchors cannot become a valid short pair.
func TestQAOutOfRangeAnchorsDoNotProducePairedDuration(t *testing.T) {
	logs := captureExecutionLogs(t)
	var scopes schedulingObservations
	scope := scopes.candidate("qa-reservation", "qa-session")
	scope.times[6] = scope.started.Add(schedulingDurationLimit + time.Second)
	scope.times[7] = scope.times[6].Add(time.Millisecond)
	scopes.finish(t.Context(), scope, "unknown")
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &row); err != nil {
		t.Fatal(err)
	}
	if row["eligible_seen_observed"] != false || row["selected_observed"] != false {
		t.Fatal("out of range anchors accepted")
	}
	if row["eligible_to_selected_ms"] != nil {
		t.Fatalf("unobserved state pair fabricated: %v", row["eligible_to_selected_ms"])
	}
}

func qaObservationRows(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["msg"] == "execution scheduling observation" {
			rows = append(rows, row)
		}
	}
	return rows
}

func TestQAConcurrentHandlerDispatchAndOwnerClose(t *testing.T) {
	logs := captureExecutionLogs(t)
	var scopes schedulingObservations
	const count = 100
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("reservation-%d", i)
		scope := scopes.candidate(id, "session")
		ctx := schedulingContext(t.Context(), scope)
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				switch j {
				case 0:
					observeSchedulingState(ctx, "promoted")
					observeSchedulingState(ctx, "notification_sent")
				case 1:
					scopes.handlerOutcome(ctx, "session", sessions.EnvironmentInputReservation{ID: id, State: sessions.EnvironmentInputAdmitted})
				case 2:
					scopes.handlerOutcome(ctx, "session", sessions.EnvironmentInputReservation{ID: id, State: sessions.EnvironmentInputCancelled})
				case 3:
					scopes.finish(ctx, scope, "completed")
				}
			}(j)
		}
		wg.Wait()
	}
	scopes.close(t.Context(), "owner_lost")
	rows := qaObservationRows(t, logs.String())
	if len(rows) != count || len(scopes.active) != 0 {
		t.Fatalf("summaries/scopes = %d/%d", len(rows), len(scopes.active))
	}
	seen := map[any]bool{}
	for _, r := range rows {
		if seen[r["reservation_id"]] {
			t.Fatal("duplicate summary")
		}
		seen[r["reservation_id"]] = true
		if r["observation_complete"] != false {
			t.Fatal("unknown prefix claimed complete")
		}
	}
	// A later owner must neither inherit anchors nor accept the old pointer.
	var next schedulingObservations
	value := next.candidate("reservation-0", "session")
	if value.owner == rows[0]["owner_observation_id"] {
		t.Fatal("owner identity reused")
	}
	next.finish(t.Context(), scopes.candidate("reservation-0", "session"), "completed")
	if len(next.active) != 1 {
		t.Fatal("closed owner influenced next owner")
	}
	next.close(t.Context(), "owner_lost")
}

func TestQAHandlerIsolationUnknownAndAdmittedOrdering(t *testing.T) {
	logs := captureExecutionLogs(t)
	var scopes schedulingObservations
	scope := scopes.candidate("reservation", "session")
	ctx := schedulingContext(t.Context(), scope)
	for _, r := range []sessions.EnvironmentInputReservation{{ID: "missing", State: sessions.EnvironmentInputCancelled}, {ID: "reservation", State: sessions.EnvironmentInputPending}, {ID: "reservation", State: "SECRET_CANARY"}} {
		scopes.handlerOutcome(ctx, "session", r)
	}
	scopes.handlerOutcome(ctx, "wrong-session", sessions.EnvironmentInputReservation{ID: "reservation", State: sessions.EnvironmentInputCancelled})
	if logs.Len() != 0 || !scope.times[12].IsZero() {
		t.Fatal("nonmatching result emitted or mutated")
	}
	// Poll can observe durable admission before the notify call: do not invent a
	// negative interval or finalize dispatch before its remaining anchors.
	scopes.handlerOutcome(ctx, "session", sessions.EnvironmentInputReservation{ID: "reservation", State: sessions.EnvironmentInputAdmitted})
	if scope.times[12].IsZero() || len(scopes.active) != 1 {
		t.Fatal("admitted observation missing or prematurely finished")
	}
	observeSchedulingState(ctx, "promoted")
	observeSchedulingState(ctx, "notification_sent")
	scopes.finish(ctx, scope, "completed")
	rows := qaObservationRows(t, logs.String())
	if len(rows) != 1 || rows[0]["notify_to_handler_ms"] != nil || rows[0]["notification_sent_observed"] != true {
		t.Fatal("ordering or remaining anchor lost", rows)
	}
	if strings.Contains(logs.String(), "SECRET_CANARY") {
		t.Fatal("unknown state leaked")
	}
	before := logs.Len()
	scopes.handlerOutcome(ctx, "session", sessions.EnvironmentInputReservation{ID: "reservation", State: sessions.EnvironmentInputCancelled})
	if logs.Len() != before {
		t.Fatal("late read reopened completed scope")
	}
}

func TestQACapacityWithoutTerminalVisibilityRemainsExplicit(t *testing.T) {
	captureExecutionLogs(t)
	var scopes schedulingObservations
	for i := 0; i < 256; i++ {
		scopes.candidate(fmt.Sprint(i), "session")
	}
	// Missing candidates are not proof of terminal state. No TTL or page-based
	// eviction may fabricate completion; this bounds memory but loses new scopes.
	for i := 0; i < 1024; i++ {
		if scopes.candidate("new", "session") != nil {
			t.Fatal("active scope evicted")
		}
	}
	if len(scopes.active) != 256 || scopes.overflow != 1024 {
		t.Fatal("capacity/accounting changed")
	}
	scopes.handlerOutcome(t.Context(), "session", sessions.EnvironmentInputReservation{ID: "0", State: sessions.EnvironmentInputExpired})
	if scopes.candidate("new", "session") == nil || len(scopes.active) != 256 {
		t.Fatal("known terminal slot not reusable")
	}
	scopes.close(t.Context(), "owner_lost")
}

func TestQASchedulerActualReadinessHintsAndPollFallback(t *testing.T) {
	captureExecutionLogs(t)
	reader := &initializationWakeReader{committed: true}
	registry := runtimegateway.NewRegistry()
	handler := runtimegateway.NewHandler(runtimegateway.HandlerConfig{Authenticator: runtimegateway.NewAuthenticator(reader), Registry: registry})
	server := httptest.NewServer(http.HandlerFunc(handler.WS))
	defer server.Close()
	allocations := 0
	phase := "waking"
	worker := &Worker{concurrency: 1, dispatcher: &Dispatcher{SessionsReader: reader, Registry: registry, DeploymentReader: &strictDeploymentReader{t: t, environmentAllocation: func(context.Context, deployment.AllocationKey) (deployment.Allocation, error) {
		allocations++
		return deployment.Allocation{ComputePhase: phase}, nil
	}}}}
	schedule := workerSchedule{}
	scan := func(hint bool) []scheduledWork {
		t.Helper()
		work, err := schedule.selectWork(t.Context(), worker, []string{"device"}, map[string]bool{}, hint)
		if err != nil {
			t.Fatal(err)
		}
		return work
	}
	if len(scan(true)) != 0 {
		t.Fatal("waking selected")
	}
	scope := worker.schedulingObservations.active["reservation"]
	if scope == nil || scope.times[2].IsZero() {
		t.Fatal("compute wait not observed")
	}
	phase = "running"
	if len(scan(true)) != 0 || scope.times[4].IsZero() {
		t.Fatal("missing peer classification")
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?device_id=device&version="+proto.Version, http.Header{"Authorization": {"Bearer fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = registry.WaitForDevice(t.Context(), "device", time.Second); err != nil {
		t.Fatal(err)
	}
	if len(scan(true)) != 0 || scope.times[5].IsZero() {
		t.Fatal("unknown capabilities did not block/classify")
	}
	heartbeat, _ := proto.NewEnvelope(proto.TypeHeartbeat, "", proto.HeartbeatPayload{SupportedAgentKinds: []proto.SupportedAgentKind{{Kind: "codex", Available: true, Capabilities: prototest.Capabilities(proto.AgentKindCapabilities{Streaming: proto.CapabilitySupported, DurableTurns: proto.CapabilitySupported, DurableInputReceipts: proto.CapabilitySupported, Preparation: proto.CapabilitySupported, ExecutionControls: proto.CapabilitySupported, ToolObservations: proto.CapabilitySupported, SubagentControl: proto.CapabilitySupported, LocalEnvironment: proto.CapabilitySupported, WorkspaceReadPreparation: proto.CapabilitySupported, WorkspaceOutputExport: proto.CapabilitySupported})}}})
	if err = conn.WriteJSON(heartbeat); err != nil {
		t.Fatal(err)
	}
	// Intentionally consume the hint without forwarding it to the scheduler.
	select {
	case <-registry.CapabilityHints():
	case <-time.After(time.Second):
		t.Fatal("hint absent")
	}
	// Hold an unrelated lifecycle observation while the actual scheduler reads
	// and selects this ready reservation. No duration threshold proves ordering.
	provider := &reconcileObservationProvider{entered: make(chan struct{}), release: make(chan struct{})}
	lifecycle := &runtimeLifecycle{ctx: t.Context(), gate: make(chan struct{}, 1), lease: heldLease{}, sessions: reconcileObservationSessions{}, config: RuntimeProvider{InstallationID: "installation", Provider: provider}, reader: &strictDeploymentReader{t: t,
		lifecycleAllocations: func(context.Context, string, string) ([]deployment.Allocation, error) {
			return []deployment.Allocation{{ID: "old-allocation", SessionID: "old-session", ProviderKey: "installation", ComputePhase: "disabled", State: "running", CreateSettled: true}}, nil
		},
		unallocatedEnvironments: func(context.Context, string, string) ([]deployment.UnallocatedEnvironment, error) { return nil, nil },
	}}
	done := make(chan error, 1)
	go func() { done <- lifecycle.reconcile(t.Context()) }()
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("slow observe did not start")
	}
	defer func() {
		close(provider.release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	reads := reader.scans
	if len(scan(false)) != 0 || reader.scans != reads {
		t.Fatal("observer bypassed scan throttle")
	}
	schedule.nextEnvironmentScan = time.Now().Add(-time.Millisecond)
	selected := scan(false)
	if len(selected) != 1 || scope.times[6].IsZero() || scope.times[7].IsZero() {
		t.Fatal("poll fallback did not select ready reservation")
	}
	select {
	case <-done:
		t.Fatal("slow observe unexpectedly completed before selection")
	default:
	}
	if allocations != 4 {
		t.Fatalf("extra business allocation reads: %d", allocations)
	}
	first := scope.times[6]
	// Late/redundant hint must not duplicate an active session or overwrite time.
	active := map[string]bool{"session": true}
	if work, err := schedule.selectWork(t.Context(), worker, []string{"device"}, active, true); err != nil || len(work) != 0 || scope.times[6] != first {
		t.Fatal("late hint duplicated active selection")
	}
	worker.schedulingObservations.finish(t.Context(), scope, "completed")
}
