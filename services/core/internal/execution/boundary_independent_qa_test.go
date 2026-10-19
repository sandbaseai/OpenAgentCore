package execution

import (
	"context"
	"errors"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgtest"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgunit"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/sessionpg"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/google/uuid"
	"testing"
)

// A terminal Turn may coexist with already queued native output. Its append
// is rejected before insertion by the real production transaction.
func TestQAFirstTextKnownTurnConflictIsNotUnknownCommit(t *testing.T) {
	logs := captureExecutionLogs(t)
	pool := pgtest.OpenIsolated(t, nil)
	tenant, session, turn := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,tenant_id,engine,idempotency_key,request_hash) VALUES($1,$2,'codex','key','hash')`, session, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO turns(id,session_id,status,completed_at) VALUES($1,$2,'completed',now())`, turn, session); err != nil {
		t.Fatal(err)
	}
	lease, err := pgunit.AcquireLease(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close(context.Background()) })
	operations, err := sessions.NewExecutionOperations(sessionpg.NewExecution(lease))
	if err != nil {
		t.Fatal(err)
	}
	j := journal{ctx: t.Context(), writer: operations, next: 1, tenant: tenant, session: session, turn: turn}
	text, _ := proto.NewEnvelope(proto.TypeDelta, turn, proto.DeltaPayload{Delta: "QA-CANARY"})
	if err := j.enqueue(text); err != nil {
		t.Fatal(err)
	}
	if err := j.flushObserved(t.Context(), "deferred_final"); !errors.Is(err, sessions.ErrTurnConflict) {
		t.Fatalf("expected typed rejection: %v", err)
	}
	reader := sessionpg.New(pgunit.NewPool(pool), nil)
	changes, err := reader.ListSessionEvents(t.Context(), tenant, session, 0)
	if err != nil || len(changes) != 0 {
		t.Fatalf("unexpected public effects: %d %v", len(changes), err)
	}
	rows := persistenceRows(t, logs.Bytes())
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	if rows[0]["append_failed_count"] != float64(1) || rows[0]["append_unknown_count"] != float64(0) {
		t.Fatalf("known pre-insert rejection must be failed=1 unknown=0: %v", rows[0])
	}
}
