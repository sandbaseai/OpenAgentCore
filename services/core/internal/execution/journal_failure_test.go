package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestJournalFailureLogsSafeCategoriesAndCorrelation(t *testing.T) {
	out := captureExecutionLogs(t)
	j := journal{ctx: obslog.WithRequestID(reservationTrace(t.Context(), "reservation"), "request-id"), tenant: "project-id", session: "session-id", turn: "turn-id", next: 112}
	for _, tc := range []struct {
		err           error
		reason, state string
	}{
		{fmt.Errorf("secret-canary: %w", sessions.ErrEventLimit), "event_limit", ""},
		{context.DeadlineExceeded, "deadline_exceeded", ""},
		{context.Canceled, "cancelled", ""},
		{sessions.ErrInvalidInput, "invalid_event", ""},
		{sessions.ErrIdempotencyConflict, "idempotency_conflict", ""},
		{sessions.ErrTurnConflict, "turn_conflict", ""},
		{&pgconn.PgError{Code: "22P05", Message: "secret-canary", Detail: "secret-canary"}, "database", "22P05"},
		{&pgconn.PgError{Code: "secret-canary"}, "database", ""},
		{errors.New("secret-canary"), "unknown", ""},
	} {
		reason, state := journalFailureCategory(tc.err)
		if reason != tc.reason || state != tc.state {
			t.Fatalf("category %q %q", reason, state)
		}
		j.reportFailure("flush", proto.TypeToolCall, 536023, tc.err)
	}
	j.reportFailure("enqueue", "secret-canary", 1, errors.New("secret-canary"))
	logs := out.String()
	if strings.Contains(logs, "secret-canary") {
		t.Fatal("raw diagnostic leaked")
	}
	for _, want := range []string{"project-id", "session-id", "turn-id", "request-id", "trace_id", "536023", "112", "event_limit", "22P05"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing correlation/category %q", want)
		}
	}
}
