package main

import (
	"context"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/coremetrics"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/execution"
)

func TestHistoryOwnershipObservesWorkerWithoutDatabaseChecks(t *testing.T) {
	owned, notOwned := true, false
	for _, tt := range []struct {
		name      string
		owner     *bool
		status    coremetrics.JobStatus
		available bool
	}{
		{"unobserved", nil, "unknown", false}, {"owned", &owned, "ok", true},
		{"acquired before first poll", &owned, "unknown", true}, {"released", &notOwned, "stopped", false},
		{"draining", &owned, "stopped", false}, {"failed", &owned, "failing", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			owner := runtimeHistoryOwnership{snapshot: func() execution.WorkerMetrics {
				calls++
				return execution.WorkerMetrics{ExecutionOwner: tt.owner, Scheduler: execution.WorkerJobMetrics{Status: tt.status}}
			}}
			if got := owner.CheckOwnership(t.Context()) == nil; got != tt.available {
				t.Fatalf("ownership availability = %v", got)
			}
			if calls != 1 {
				t.Fatal("snapshot was not read once")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if owner.CheckOwnership(ctx) == nil || calls != 1 {
				t.Fatal("cancelled sampling touched the owner")
			}
		})
	}
}
