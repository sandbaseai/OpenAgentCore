package execution

import (
	"context"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/deployment"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"strings"
	"testing"
)

type reviewStaleReader struct {
	*initializationWakeReader
	terminal func()
}

func (r *reviewStaleReader) ListEnvironmentInputWork(ctx context.Context, cursor string, devices []string) ([]sessions.EnvironmentInputWork, error) {
	rows, err := r.initializationWakeReader.ListEnvironmentInputWork(ctx, cursor, devices)
	r.terminal() // cancellation commits after the pending query snapshot, before candidate consumption
	return rows, err
}
func TestReviewStaleCandidateCannotReopenFinishedReservation(t *testing.T) {
	logs := captureExecutionLogs(t)
	worker := &Worker{concurrency: 1}
	worker.schedulingObservations.candidate("reservation", "session")
	reader := &reviewStaleReader{initializationWakeReader: &initializationWakeReader{committed: true}}
	reader.terminal = func() {
		worker.schedulingObservations.handlerOutcome(t.Context(), "session", sessions.EnvironmentInputReservation{ID: "reservation", State: sessions.EnvironmentInputCancelled})
	}
	worker.dispatcher = &Dispatcher{SessionsReader: reader, DeploymentReader: &strictDeploymentReader{t: t, environmentAllocation: func(context.Context, deployment.AllocationKey) (deployment.Allocation, error) {
		return deployment.Allocation{ComputePhase: "waking"}, nil
	}}}
	schedule := workerSchedule{}
	if _, err := schedule.selectWork(t.Context(), worker, []string{"device"}, map[string]bool{}, true); err != nil {
		t.Fatal(err)
	}
	worker.schedulingObservations.close(t.Context(), "unknown")
	if got := strings.Count(logs.String(), `"msg":"execution scheduling observation"`); got != 1 {
		t.Fatalf("same owner/reservation emitted %d terminal summaries: %s", got, logs.String())
	}
}
