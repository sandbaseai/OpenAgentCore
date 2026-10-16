package execution

import (
	"context"
	"time"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
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
func withArtifactObservation(ctx context.Context, session, turn string) context.Context {
	return textobs.WithArtifact(ctx, session, turn)
}
func observeArtifactWorkspace(ctx context.Context, stage string, started time.Time, err error, attrs ...any) {
	scope, ok := textobs.Artifact(ctx)
	if !ok {
		return
	}
	observeExecutionStage(ctx, stage, started, err, append([]any{"session_id", scope.Session, "turn_id", scope.Turn, "capture_id", scope.Capture}, attrs...)...)
}
func observeArtifactSkip(ctx context.Context, session, turn, reason string) {
	obslog.Ctx(ctx).Info("artifact capture skipped", "session_id", session, "turn_id", turn, "reason", reason)
}
