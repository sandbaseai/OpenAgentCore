package execution

import (
	"context"
	"errors"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

// Journal failures can contain SQL, model output and credentials. Record only
// bounded categories and sizes; the error text and event payload stay private.
func (j *journal) reportFailure(stage, kind string, size int, err error) {
	ctx := j.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	switch kind {
	case proto.TypeDelta, proto.TypeOutputMessage, proto.TypeThinking, proto.TypeToolCall, proto.TypeCommandOutput, proto.TypeUsage,
		proto.TypeError, proto.TypeDone, proto.TypePromptSteerAck, proto.TypeSubagentIdentity, proto.TypeSubagentLifecycle, proto.TypeSubagentTurn, proto.TypeSubagentItem, proto.TypeSubagentCoordination, "cancel_receipt":
	default:
		kind = "unknown"
	}
	reason, state := journalFailureCategory(err)
	obslog.Ctx(ctx).Error("execution journal failed", "project_id", j.tenant, "session_id", j.session, "turn_id", j.turn,
		"stage", stage, "event_kind", kind, "payload_bytes", size, "next_ordinal", j.next,
		"pending_events", len(j.batch), "reason", reason, "sqlstate", state)
}

func journalFailureCategory(err error) (string, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded", ""
	case errors.Is(err, context.Canceled):
		return "cancelled", ""
	case errors.Is(err, sessions.ErrEventLimit):
		return "event_limit", ""
	case errors.Is(err, sessions.ErrInvalidInput):
		return "invalid_event", ""
	case errors.Is(err, sessions.ErrIdempotencyConflict):
		return "idempotency_conflict", ""
	case errors.Is(err, sessions.ErrTurnConflict):
		return "turn_conflict", ""
	}
	var pg interface{ SQLState() string }
	if errors.As(err, &pg) {
		if len(pg.SQLState()) == 5 {
			valid := true
			for _, c := range pg.SQLState() {
				if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
					valid = false
				}
			}
			if valid {
				return "database", pg.SQLState()
			}
		}
		return "database", ""
	}
	return "unknown", ""
}
