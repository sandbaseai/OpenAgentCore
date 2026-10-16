package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/deployment"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sandbox"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

type reconcileObservationSessions struct{ sessions.Reader }

func (reconcileObservationSessions) GetSessionRuntimeDevice(context.Context, string, string) (sessions.ExecutionDevice, error) {
	return sessions.ExecutionDevice{}, sessions.ErrNotFound
}

type reconcileObservationProvider struct {
	sandbox.SandboxProvider
	entered, release chan struct{}
}

func (p *reconcileObservationProvider) GetInfo(ctx context.Context, _ sandbox.Reference) (sandbox.Info, error) {
	close(p.entered)
	select {
	case <-p.release:
		return sandbox.Info{}, sandbox.ErrNotFound
	case <-ctx.Done():
		return sandbox.Info{}, ctx.Err()
	}
}

func TestReconcileTraceCorrelatesSlowObservationAndPendingScan(t *testing.T) {
	out := captureExecutionLogs(t)
	parent, carrier := obslog.StartBackgroundTrace(t.Context())
	pending := false
	provider := &reconcileObservationProvider{entered: make(chan struct{}), release: make(chan struct{})}
	reader := &strictDeploymentReader{t: t, lifecycleAllocations: func(context.Context, string, string) ([]deployment.Allocation, error) {
		return []deployment.Allocation{{ID: "old-allocation", SessionID: "old-session", ProviderKey: "installation", ComputePhase: "disabled", State: "running", CreateSettled: true}}, nil
	}, unallocatedEnvironments: func(context.Context, string, string) ([]deployment.UnallocatedEnvironment, error) {
		pending = true
		return nil, nil
	}}
	r := &runtimeLifecycle{ctx: t.Context(), gate: make(chan struct{}, 1), reader: reader, lease: heldLease{}, sessions: reconcileObservationSessions{}, config: RuntimeProvider{InstallationID: "installation", Provider: provider}}
	done := make(chan error, 1)
	go func() { done <- r.reconcile(parent) }()
	select {
	case <-provider.entered:
	case err := <-done:
		t.Fatal("reconcile ended before observation", err)
	case <-time.After(5 * time.Second):
		t.Fatal("observation did not start")
	}
	// No timing threshold: the provider explicitly holds this reconciliation.
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("pending scan was lost")
	}
	stages := map[string]int{}
	roundSpan := ""
	observePosition, pendingPosition := -1, -1
	for i, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row["trace_id"] != carrier.Trace.String() {
			t.Fatal("reconcile lost caller trace", row)
		}
		if roundSpan == "" {
			roundSpan = row["span_id"].(string)
		}
		if row["span_id"] != roundSpan || roundSpan == carrier.Span.String() {
			t.Fatal("round span not shared or not distinct", row)
		}
		if stage, ok := row["stage"].(string); ok {
			stages[stage]++
			if stage == "runtime_observe" {
				observePosition = i
			}
			if stage == "runtime_pending_inventory" {
				pendingPosition = i
			}
		}
	}
	if observePosition < 0 || pendingPosition < 0 {
		t.Fatal("missing observed/pending stages", stages)
	}
	// Their shared identity establishes the ordering of the current serial path.
	// This assertion qualifies the diagnostic, not a requirement to retain HOL.
	if observePosition >= pendingPosition {
		t.Fatal("diagnostic does not match serial execution")
	}
	for _, stage := range []string{"runtime_gate_wait", "runtime_inventory", "runtime_observe", "runtime_pending_inventory", "runtime_pending", "runtime_reconcile", "runtime_reconcile_total"} {
		if stages[stage] != 1 {
			t.Fatal("missing/duplicate stage", stage, stages)
		}
	}
	second := lifecycleTrace(parent)
	other, _ := obslog.TraceFromContext(second)
	if other.Trace != carrier.Trace || other.Span.String() == roundSpan {
		t.Fatal("successive rounds share identity")
	}
}

func TestReconcileEmptyAndFailedInventoryRemainObservable(t *testing.T) {
	for _, scenario := range []string{"empty", "inventory_error", "gate_cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			out := captureExecutionLogs(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := &runtimeLifecycle{ctx: t.Context(), gate: make(chan struct{}, 1), reader: &strictDeploymentReader{t: t, lifecycleAllocations: func(context.Context, string, string) ([]deployment.Allocation, error) {
				if scenario == "inventory_error" {
					return nil, errors.New("private credential")
				}
				return nil, nil
			}, unallocatedEnvironments: func(context.Context, string, string) ([]deployment.UnallocatedEnvironment, error) { return nil, nil }}}
			if scenario == "gate_cancelled" {
				r.gate <- struct{}{}
				cancel()
			}
			err := r.reconcile(ctx)
			if (err != nil) != (scenario != "empty") {
				t.Fatal(err)
			}
			found := false
			for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
				var row map[string]any
				if err := json.Unmarshal(line, &row); err != nil {
					t.Fatal(err)
				}
				if row["trace_id"] == nil || row["span_id"] == nil {
					t.Fatal("missing background identity", row)
				}
				if row["stage"] == "runtime_inventory" {
					if scenario == "empty" && row["allocation_count"] != float64(0) {
						t.Fatal(row)
					}
					if scenario == "inventory_error" && row["allocation_count"] != nil {
						t.Fatal("failed read fabricated empty page", row)
					}
				}
				if row["stage"] == "runtime_reconcile_total" {
					found = true
					want := "ok"
					if scenario == "inventory_error" {
						want = "error"
					}
					if scenario == "gate_cancelled" {
						want = "cancelled"
					}
					if row["status"] != want {
						t.Fatal(row)
					}
				}
			}
			if !found || bytes.Contains(out.Bytes(), []byte("private credential")) {
				t.Fatal("missing envelope or unsafe error")
			}
		})
	}
}

func TestLifecycleTraceKeepsCancellationAndDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(t.Context(), time.Minute)
	child := lifecycleTrace(parent)
	a, _ := parent.Deadline()
	b, _ := child.Deadline()
	if a != b {
		t.Fatal("deadline changed")
	}
	cancel()
	if !errors.Is(child.Err(), context.Canceled) {
		t.Fatal("cancellation changed")
	}
}
