package execution

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
)

// A reservation survives observer disconnects and owner restarts. Its opaque
// identity anchors the execution trace; the submitting HTTP trace is linked in
// the reservation log rather than retained in a process-local map.
func reservationTraceID(reservation string) obslog.TraceID {
	digest := sha256.Sum256([]byte("oac.environment-input:" + reservation))
	var id obslog.TraceID
	copy(id[:], digest[:len(id)])
	return id
}

func reservationTrace(ctx context.Context, reservation string) context.Context {
	return obslog.WithTrace(ctx, obslog.Carrier{Trace: reservationTraceID(reservation), Span: obslog.NewSpanID()})
}

// lifecycleTrace identifies one reconciliation, including waiting for its gate.
// A caller trace is retained with a fresh span; background scans get a trace.
func lifecycleTrace(ctx context.Context) context.Context {
	if carrier, ok := obslog.TraceFromContext(ctx); ok {
		carrier.Span = obslog.NewSpanID()
		return obslog.WithTrace(ctx, carrier)
	}
	ctx, _ = obslog.StartBackgroundTrace(ctx)
	return ctx
}

// Stage intervals use a local monotonic clock. Error prose and request content
// are deliberately excluded: provider errors may contain private configuration.
func observeExecutionStage(ctx context.Context, stage string, started time.Time, err error, attrs ...any) {
	status := "ok"
	switch {
	case errors.Is(err, context.Canceled):
		status = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		status = "timeout"
	case err != nil:
		status = "error"
	}
	fields := []any{"stage", stage, "duration_ms", float64(time.Since(started).Microseconds()) / 1000, "status", status}
	obslog.Ctx(ctx).Info("execution stage", append(fields, attrs...)...)
}

// Initial input is committed by Session creation. Joining its origin to the
// worker's is_initial reservation avoids an additional diagnostic store query.
func recordInitialInputOrigin(ctx context.Context, session string) {
	obslog.Ctx(ctx).Info("session initial input origin", "session_id", session)
}

// Failed inventory reads have an unknown count, never a fabricated empty page.
func observeRuntimeInventory(ctx context.Context, started time.Time, err error, node string, refill bool, count int) {
	var knownCount any
	if err == nil {
		knownCount = count
	}
	observeExecutionStage(ctx, "runtime_inventory", started, err, "node_id", node, "refill", refill, "allocation_count", knownCount)
}
