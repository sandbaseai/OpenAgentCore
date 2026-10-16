package main

import (
	"context"
	"errors"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/execution"
)

// runtimeHistoryOwnership implements the sampler's read-only ownership check.
// The execution worker performs authoritative database checks. Sampling observes
// their result without putting its short-lived contexts on the leased connection.
type runtimeHistoryOwnership struct {
	snapshot func() execution.WorkerMetrics
}

func (o runtimeHistoryOwnership) CheckOwnership(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot := o.snapshot()
	if snapshot.ExecutionOwner == nil || !*snapshot.ExecutionOwner || snapshot.Scheduler.Status == "stopped" || snapshot.Scheduler.Status == "failing" {
		return errors.New("execution ownership is not observed")
	}
	return nil
}
