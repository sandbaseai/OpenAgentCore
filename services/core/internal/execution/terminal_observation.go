package execution

import (
	"context"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
)

func observeTerminalCommit(ctx context.Context, session, turn, requested, returned string, attempt int, started time.Time, eventID string, err error) {
	confirmation := "unconfirmed"
	if err == nil {
		confirmation = "confirmed"
	}
	mapping := "unobserved"
	if eventID != "" {
		mapping = confirmation
	}
	observeExecutionStage(ctx, "turn_completion", started, err, "session_id", session, "turn_id", turn,
		"requested_turn_status", requested, "returned_turn_status", returned, "attempt", attempt, "confirmation", confirmation, "event_id", eventID, "mapping_status", mapping)
}

// Only artifact capture activates these fixed workspace boundaries. Directory
// browsing keeps its existing path without new diagnostic scope or behavior.
type artifactObservationKey struct{}
type artifactObservation struct{ session, turn string }

func withArtifactObservation(ctx context.Context, session, turn string) context.Context {
	return context.WithValue(ctx, artifactObservationKey{}, artifactObservation{session, turn})
}
func observeArtifactWorkspace(ctx context.Context, stage string, started time.Time, err error, attrs ...any) {
	scope, ok := ctx.Value(artifactObservationKey{}).(artifactObservation)
	if !ok {
		return
	}
	observeExecutionStage(ctx, stage, started, err, append([]any{"session_id", scope.session, "turn_id", scope.turn}, attrs...)...)
}
func observeArtifactSkip(ctx context.Context, session, turn, reason string) {
	obslog.Ctx(ctx).Info("artifact capture skipped", "session_id", session, "turn_id", turn, "reason", reason)
}
