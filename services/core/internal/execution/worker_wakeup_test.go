package execution

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestSchedulerWakeCoalescesConcurrentAdmissionsAndKeepsNextHint(t *testing.T) {
	worker := &Worker{scheduleWake: make(chan struct{}, 1)}
	var callers sync.WaitGroup
	for range 100 {
		callers.Go(func() {
			for range 100 {
				worker.wakeScheduler()
			}
		})
	}
	callers.Wait()
	if got := len(worker.scheduleWake); got != 1 {
		t.Fatalf("queued wakeups = %d, want one", got)
	}
	<-worker.scheduleWake
	// A commit while a previous scan is running needs a subsequent scan.
	worker.wakeScheduler()
	select {
	case <-worker.scheduleWake:
	default:
		t.Fatal("admission during a scan lost its wakeup")
	}
	select {
	case <-worker.scheduleWake:
		t.Fatal("coalesced submissions caused an extra scan")
	default:
	}
}

func TestWorkerCancelledWithoutGatewayPreservesShutdown(t *testing.T) {
	worker := &Worker{dispatcher: &Dispatcher{}, lease: heldLease{}, stopped: make(chan struct{}), concurrency: 1}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("gateway-free admission worker did not stop", err)
	}
	select {
	case <-worker.stopped:
	default:
		t.Fatal("worker did not publish shutdown")
	}
}
